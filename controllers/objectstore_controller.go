/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	objectv1alpha1 "github.com/redhat-et/rgw-standalone-operator/api/v1alpha1"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ObjectStoreReconciler reconciles a ObjectStore object
type ObjectStoreReconciler struct {
	client.Client
	*runtime.Scheme
	logr.Logger
	*RemotePodCommandExecutor
}

//+kubebuilder:rbac:groups=object.rgw-standalone,resources=objectstores,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=object.rgw-standalone,resources=objectstores/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=object.rgw-standalone,resources=objectstores/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=create;delete;get;list
//+kubebuilder:rbac:groups="",resources=services,verbs=create;delete;get;update;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=list;watch;delete
//+kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
//+kubebuilder:rbac:groups="apps",resources=deployments,verbs=create;delete;get;update;list;watch
//+kubebuilder:rbac:groups="batch",resources=jobs,verbs=create;delete;get;list;watch

// SetupWithManager sets up the controller with the Manager.
func (r *ObjectStoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&objectv1alpha1.ObjectStore{}).
		Complete(r)
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ObjectStoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Logger = ctrl.Log.WithValues("ObjectStore", req.NamespacedName.String())
	r.Logger.Info("reconciling")

	// Fetch the cephObjectStore instance
	objectStore := &objectv1alpha1.ObjectStore{}
	err := r.Client.Get(ctx, req.NamespacedName, objectStore)
	if err != nil {
		if kerrors.IsNotFound(err) {
			r.Logger.Info("cephObjectStore resource not found. Ignoring since object must be deleted.")

			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, fmt.Errorf("failed to get ObjectStore: %w", err)
	}

	// Build finalizer name
	finalizerName := buildFinalizerName(objectStore.GetObjectKind().GroupVersionKind().Kind)

	// DELETE: the CR was deleted
	if !objectStore.GetDeletionTimestamp().IsZero() {
		// updateStatus(r.client, request.NamespacedName, cephv1.ConditionDeleting, buildStatusInfo(cephObjectStore))

		// DO WHATEVER CLEANUP

		// Remove finalizer
		controllerutil.RemoveFinalizer(objectStore, finalizerName)

		// Return and do not requeue. Successful deletion.
		r.Logger.Info("successfully deleted ObjectStore" + req.NamespacedName.String())
		return reconcile.Result{}, nil
	}

	// Main reconcile logic starts here
	if !controllerutil.ContainsFinalizer(objectStore, finalizerName) {
		controllerutil.AddFinalizer(objectStore, finalizerName)
	}

	// Create PVC from provided SC
	err = r.createPVC(ctx, objectStore)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to create PVC: %w", err)
	}

	// Reconcile objectStore service
	serviceIP, err := r.reconcileService(ctx, objectStore)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to reconcile Service: %w", err)
	}

	// Configure multisite will import the realm token from the main site
	if objectStore.Spec.IsMultisite() {
		err = r.configureMultisite(ctx, objectStore, serviceIP)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to configure multisite: %w", err)
		}
	}

	// Reconcile objectStore deployment
	port := int32(8080)
	if objectStore.Spec.Gateway.Port != 0 {
		port = objectStore.Spec.Gateway.Port
	}
	reconcileResult, err := r.createOrUpdateDeployment(ctx, objectStore, fmt.Sprintf("http://%s:%d", serviceIP, port))
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to create or update deployment: %w", err)
	}
	r.Logger.Info("successful deployed", "DeploymentResults", reconcileResult)

	// Wait for the pod to be ready
	pod, err := r.waitForLabeledPodsToRunWithRetries(ctx, objectStore, 5)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to wait for pods to be ready: %w", err)
	}

	// Bootstrap my own realm
	if objectStore.Spec.IsMainSite() {
		err = r.bootstrapRealm(ctx, objectStore, pod, serviceIP)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to bootstrap realm: %w", err)
		}
	}

	r.Logger.Info("successfully reconciled", "ObjectStore", req.NamespacedName.String())
	return ctrl.Result{}, nil
}

// createPVC will create a PVC for the given ObjectStore
// It will be used to store the ObjectStore database
func (r *ObjectStoreReconciler) createPVC(ctx context.Context, objectStore *objectv1alpha1.ObjectStore) error {
	volumeMode := v1.PersistentVolumeFilesystem
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceName(objectStore.Name, objectStore.Namespace),
			Namespace: objectStore.Namespace,
		},
		Spec: objectStore.Spec.VolumeClaimTemplate.Spec,
	}

	// TODO: do not override user's settings
	pvc.Spec.AccessModes = []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}
	pvc.Spec.VolumeMode = &volumeMode

	err := r.Create(ctx, pvc, &client.CreateOptions{})
	if err != nil {
		if kerrors.IsAlreadyExists(err) {
			r.Logger.Info("PVC", pvc.Name, "already exists")
			return nil
		} else {
			return fmt.Errorf("failed to create PVC %q: %w", pvc.Name, err)
		}
	}
	r.Logger.Info("successfully provisioned", "PVC", pvc.Name)

	return nil
}

func (r *ObjectStoreReconciler) configureMultisite(ctx context.Context, objectStore *objectv1alpha1.ObjectStore, serviceIP string) error {
	secret := &v1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: objectStore.Namespace, Name: objectStore.Spec.Multisite.RealmTokenSecretName}, secret)
	if err != nil {
		return fmt.Errorf("failed to get realm token secret %q: %w", objectStore.Spec.Multisite.RealmTokenSecretName, err)
	}

	realmToken := string(secret.Data["token"])
	if realmToken == "" {
		return fmt.Errorf("failed to find realm token secret, 'token' key missing or empty?")
	}

	port := int32(8080)
	if objectStore.Spec.Gateway.Port != 0 {
		port = objectStore.Spec.Gateway.Port
	}

	// Create multisite zone
	// The realm token is stored in the zone secret and mounted in the pod
	err = r.createMultisiteZoneJob(ctx, objectStore, fmt.Sprintf("http://%s:%d", serviceIP, port))
	if err != nil {
		return fmt.Errorf("failed to create multisite zone job: %w", err)
	}

	// Wait for the multisite zone job completion
	err = r.waitForJobCompletion(ctx, multisiteJobMeta(objectStore), time.Minute)
	if err != nil {
		return fmt.Errorf("failed to wait for multisite zone job to complete: %w", err)
	}
	r.Logger.Info("successfully configured multisite")

	return nil
}

// bootstrapRealm bootstrap my own realm in case another gw wants to connect with me
func (r *ObjectStoreReconciler) bootstrapRealm(ctx context.Context, objectStore *objectv1alpha1.ObjectStore, pod v1.Pod, serviceIP string) error {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "object-store-realm-token",
			Namespace: objectStore.Namespace,
		},
	}
	err := controllerutil.SetControllerReference(objectStore, secret, r.Scheme)
	if err != nil {
		return fmt.Errorf("failed to set owner reference to secret %q: %w", secret.Name, err)
	}

	port := int32(8080)
	if objectStore.Spec.Gateway.Port != 0 {
		port = objectStore.Spec.Gateway.Port
	}

	output, stderr, err := r.RemotePodCommandExecutor.ExecCommandInContainerWithFullOutputWithTimeout(
		ctx,
		getLabelString(objectStore.Name),
		"rgw",
		objectStore.Namespace,
		append([]string{
			"rgwam-sqlite"},
			[]string{
				"realm",
				"bootstrap",
				fmt.Sprintf("--realm=%s-%s", objectStore.Name, objectStore.Namespace),
				fmt.Sprintf("--endpoints=http://%s:%d", serviceIP, port),
			}...,
		)...,
	)
	// TODO: re-add this once rgwam-sqlite stops logging to stderr
	// if err != nil || stderr != "" {
	if err != nil {
		if code, err := extractExitCode(err); err == nil {
			if code != int(syscall.EEXIST) {
				r.Logger.Info("realm has been created already")
				return nil
			}
			if code != 0 || strings.Contains(stderr, "ERROR") {
				return fmt.Errorf("failed to bootstrap realm: %w", err)
			}
		}
	}

	// Parse the output to get the token
	outputParse := regexp.MustCompile(`^Realm Token: (\S+)$`).FindStringSubmatch(output)
	if len(outputParse) != 2 {
		return fmt.Errorf("failed to parse realm token from output: %s", output)
	}

	token := outputParse[1]
	if isBase64Encoded(token) {
		// Create secret with the token
		secret.Data = map[string][]byte{"token": []byte(token)}

		err := r.Client.Create(ctx, secret)
		if err != nil {
			if kerrors.IsAlreadyExists(err) {
				r.Logger.Info("Realm Secret", secret.Name, "already exists")
				return nil
			}
			return fmt.Errorf("failed to create realm token secret: %w", err)
		}

	} else {
		return fmt.Errorf("failed to parse realm token")
	}

	r.Logger.Info("deleting pod to restart the gateway and apply realm configuration", "Pod", pod.Name)
	err = r.Client.Delete(ctx, pod.DeepCopy())
	if err != nil {
		return fmt.Errorf("failed to delete pod %q: %w", pod.Name, err)
	}

	// Wait for the pod to be ready
	_, err = r.waitForLabeledPodsToRunWithRetries(ctx, objectStore, 5)
	if err != nil {
		return fmt.Errorf("failed to wait for pods to be ready: %w", err)
	}

	r.Logger.Info("successfully configured realm")
	return nil
}
