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

//+kubebuilder:rbac:groups=object.rook-s3-nano,resources=objectstores,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=object.rook-s3-nano,resources=objectstores/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=object.rook-s3-nano,resources=objectstores/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=create;delete;get;list
//+kubebuilder:rbac:groups="",resources=services,verbs=create;delete;get;update;list;watch
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
	r.Logger.Info("reconciling", "ObjectStore", req.NamespacedName.String())

	// Fetch the cephObjectStore instance
	objectStore := &objectv1alpha1.ObjectStore{}
	err := r.Client.Get(ctx, req.NamespacedName, objectStore)
	if err != nil {
		if kerrors.IsNotFound(err) {
			r.Logger.Info("cephObjectStore resource not found. Ignoring since object must be deleted.", "ObjectStore", req.NamespacedName.String())

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
	_, err = r.reconcileService(ctx, objectStore)
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
	err = r.waitForLabeledPodsToRunWithRetries(ctx, getLabels(objectStore.Name), 5)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to wait for pods to be ready: %w", err)
	}

	// Try remote executor
	output, stderr, err := r.RemotePodCommandExecutor.ExecCommandInContainerWithFullOutputWithTimeout(
		ctx,
		getLabelString(objectStore.Name),
		"rgw",
		objectStore.Namespace,
		append([]string{"radosgw-admin-sqlite"}, buildFinalCLIArgs([]string{"user", "list"})...)...,
	)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to execute radosgw-admin-sqlite user list: %s. %w", stderr, err)
	}
	r.Logger.Info("radosgw-admin-sqlite user list output: \n" + output)
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
