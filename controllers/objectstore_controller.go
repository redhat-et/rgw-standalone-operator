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
	"encoding/json"
	"fmt"
	"regexp"

	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	objectv1alpha1 "github.com/leseb/rook-s3-nano/api/v1alpha1"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ObjectStoreReconciler reconciles a ObjectStore object
type ObjectStoreReconciler struct {
	client.Client
	*runtime.Scheme
	logr.Logger
	*RemotePodCommandExecutor
}

type realmSpec struct {
	Realms []string `json:"realms"`
}

//+kubebuilder:rbac:groups=object.rook-s3-nano,resources=objectstores,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=object.rook-s3-nano,resources=objectstores/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=object.rook-s3-nano,resources=objectstores/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=create;delete;get;list
//+kubebuilder:rbac:groups="",resources=services,verbs=create;delete;get;update;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=list;watch
//+kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
//+kubebuilder:rbac:groups="apps",resources=deployments,verbs=create;delete;get;update;list;watch

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

	// Reconcile objectStore deployment
	reconcileResult, err := r.createOrUpdateDeployment(ctx, objectStore)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to create or update deployment: %w", err)
	}
	r.Logger.Info("successful deployment " + string(reconcileResult))

	// Wait for the pod to be ready
	err = r.waitForLabeledPodsToRunWithRetries(ctx, objectStore, 5)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to wait for pods to be ready: %w", err)
	}

	// Configure multisite will import the realm token from the main site
	if objectStore.Spec.IsMultisite() {
		err = r.configureMultisite(ctx, objectStore, serviceIP)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to configure multisite: %w", err)
		}
	}

	// Bootstrap my own realm
	if objectStore.Spec.IsMainSite() {
		err = r.bootstrapRealm(ctx, objectStore, serviceIP)
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

	// Create zone within realm
	output, stderr, err := r.RemotePodCommandExecutor.ExecCommandInContainerWithFullOutputWithTimeout(
		ctx,
		getLabelString(objectStore.Name),
		"rgw",
		objectStore.Namespace,
		append([]string{"rgwam-sqlite"}, []string{"zone", "create", fmt.Sprintf("--realm-token=%s", realmToken), fmt.Sprintf("--endpoints=http://%s:%d", serviceIP, port)}...)...,
	)
	// TODO: re-add this once rgwam-sqlite stops logging to stderr
	// if err != nil || stderr != "" {
	if err != nil {
		if code, err := extractExitCode(err); err == nil && code != 0 {
			return fmt.Errorf("failed to create zone: %s. %w", stderr, err)
		}
	}
	r.Logger.Info(output)

	return nil
}

// bootstrapRealm bootstrap my own realm in case another gw wants to connect with me
func (r *ObjectStoreReconciler) bootstrapRealm(ctx context.Context, objectStore *objectv1alpha1.ObjectStore, serviceIP string) error {
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

	// Create zone within realm
	realms, err := r.getRealms(ctx, objectStore)
	if err != nil {
		return fmt.Errorf("failed to get realms: %w", err)
	}
	if len(realms) > 0 {
		r.Logger.Info("realm already exists", "realm", realms[0])
		return nil
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
		append([]string{"rgwam-sqlite"}, []string{"realm", "bootstrap", fmt.Sprintf("--endpoints=http://%s:%d", serviceIP, port)}...)...,
	)
	// TODO: re-add this once rgwam-sqlite stops logging to stderr
	// if err != nil || stderr != "" {
	if err != nil {
		if code, err := extractExitCode(err); err == nil && code != 0 {
			return fmt.Errorf("failed to bootstrap realm: %s. %w", stderr, err)
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

	return nil
}

func (r *ObjectStoreReconciler) getRealms(ctx context.Context, objectStore *objectv1alpha1.ObjectStore) ([]string, error) {
	output, stderr, err := r.RemotePodCommandExecutor.ExecCommandInContainerWithFullOutputWithTimeout(
		ctx,
		getLabelString(objectStore.Name),
		"rgw",
		objectStore.Namespace,
		append([]string{"radosgw-admin-sqlite"}, buildFinalCLIArgs([]string{"realm", "list"})...)...,
	)
	if err != nil || stderr != "" {
		if code, err := extractExitCode(err); err == nil && code != 0 {
			return nil, fmt.Errorf("failed to list realm: %s. %w", stderr, err)
		}
	}

	var realm realmSpec
	err = json.Unmarshal([]byte(output), &realm)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal realms %w", err)
	}

	return realm.Realms, nil
}
