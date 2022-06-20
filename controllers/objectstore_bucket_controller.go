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

	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/go-logr/logr"
	"github.com/kube-object-storage/lib-bucket-provisioner/pkg/provisioner"
	"github.com/kube-object-storage/lib-bucket-provisioner/pkg/provisioner/api"
	objectv1alpha1 "github.com/leseb/rook-s3-nano/api/v1alpha1"
)

// ObjectStoreBucketReconciler reconciles a ObjectStore bucket object
type ObjectStoreBucketReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RestConfig *rest.Config
	Logger     logr.Logger
}

type Provisioner struct{}

var (
	ImmediateRetryResult                  = ctrl.Result{Requeue: true}
	bucketProvisionerName                 = "s3.rook.io/bucket"
	_                     api.Provisioner = &Provisioner{}
)

//+kubebuilder:rbac:groups=objectbucket.io,resources=objectbuckets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=objectbucket.io,resources=objectbucketclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=object.rook-s3-nano,resources=objectstores/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ObjectStoreBucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Logger.Info("reconciling lib bucket provisioner")

	/* TODO:
	- create an S3 user to use by the provisioner to create users
	- do the creation by exec'ing into the object store pod
	- run the provisioner
	*/

	// Start the object bucket provisioner
	// note: the error return below is ignored and is expected to be removed from the
	//   bucket library's `NewProvisioner` function
	const allNamespaces = ""
	p := Provisioner{}
	bucketController, _ := provisioner.NewProvisioner(r.RestConfig, bucketProvisionerName, p, allNamespaces)

	// RunWithContext() blocks and waits for the context to be Done. So the controller never
	// finishes its reconcile loop.
	// It's fine since we don't need reconcile that block, it does not reconcile anything, just run the bucket controller.
	err := bucketController.RunWithContext(ctx)
	if err != nil {
		return ImmediateRetryResult, fmt.Errorf("failed to run bucket controller: %w", err)
	}

	// Because of the above behavior, this following is unreachable.
	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ObjectStoreBucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&objectv1alpha1.ObjectStore{}).
		WithEventFilter(predicateController()).
		Complete(r)
}

// predicateController is the predicate function to trigger reconcile on events for the ObjectStoreBucketReconciler.
func predicateController() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
	}
}
