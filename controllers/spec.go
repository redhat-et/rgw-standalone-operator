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
	"reflect"
	"time"

	objectv1alpha1 "github.com/redhat-et/rgw-standalone-operator/api/v1alpha1"

	apps "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	controllerclient "sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	rgwPortInternalPort      int32 = 7480
	appName                        = "rgw"
	podNameEnvVar                  = "POD_NAME"
	objectStoreDataDirectory       = "/var/lib/ceph/radosgw/data"
	x
)

var (
	cephGID int64 = 167
	CephUID int64 = 167
)

func (r *ObjectStoreReconciler) createOrUpdateDeployment(ctx context.Context, objectStore *objectv1alpha1.ObjectStore, endpoint string) (controllerutil.OperationResult, error) {
	deploy := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceName(objectStore.Name, objectStore.Namespace),
			Namespace: objectStore.Namespace,
			Labels:    getLabels(objectStore.Name),
		},
	}

	// Set ObjectStore instance as the owner and controller of the Deployment.
	err := controllerutil.SetControllerReference(objectStore, deploy, r.Scheme)
	if err != nil {
		return "", fmt.Errorf("failed to set owner reference to deployment %q: %w", deploy.Name, err)
	}

	mutateFunc := func() error {
		pod, err := r.makeRGWPodSpec(objectStore, endpoint)
		if err != nil {
			return err
		}

		deploy.Spec = apps.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: getLabels(objectStore.Name),
			},
			Template: pod,
		}

		return nil
	}

	return controllerutil.CreateOrUpdate(ctx, r.Client, deploy, mutateFunc)
}

func (r *ObjectStoreReconciler) makeRGWPodSpec(objectStore *objectv1alpha1.ObjectStore, endpoint string) (v1.PodTemplateSpec, error) {
	rgwDaemonContainer := r.makeDaemonContainer(objectStore)
	if reflect.DeepEqual(rgwDaemonContainer, v1.Container{}) {
		return v1.PodTemplateSpec{}, fmt.Errorf("got empty container for RGW daemon")
	}
	podSpec := v1.PodSpec{
		InitContainers: []v1.Container{
			// We must chown the data directory since some csi drivers do not honour the FSGroup policy
			// We need to make sure the object store data directory is owned by the ceph user
			chownCephDataDirsInitContainer(objectStore.Spec.Image, []v1.VolumeMount{daemonVolumeMountPVC()}, podSecurityContext()),
		},
		Containers:    []v1.Container{rgwDaemonContainer},
		RestartPolicy: v1.RestartPolicyAlways,
		SecurityContext: &v1.PodSecurityContext{
			RunAsUser:  &CephUID,
			RunAsGroup: &cephGID,
			FSGroup:    &CephUID,
		},
		Volumes: []v1.Volume{DaemonVolumesDataPVC(instanceName(objectStore.Name, objectStore.Namespace))},

		// TODO: add a proper service account decoupled from the operator's SA
		// ServiceAccountName: appName,
	}

	podTemplateSpec := v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Name:   instanceName(objectStore.Name, objectStore.Namespace),
			Labels: getLabels(objectStore.Name),
		},
		Spec: podSpec,
	}

	if objectStore.Spec.IsMultisite() {
		podSpec.InitContainers = append(podSpec.InitContainers, createZoneContainer(objectStore, endpoint))

	}

	return podTemplateSpec, nil
}

func (r *ObjectStoreReconciler) makeDaemonContainer(objectStore *objectv1alpha1.ObjectStore) v1.Container {
	// start the rgw daemon in the foreground
	container := v1.Container{
		Name:  "rgw",
		Image: objectStore.Spec.Image,
		Command: []string{
			"radosgw-sqlite",
		},
		Args: append(
			defaultDaemonFlag(),
			// Use a hash otherwise the socket name might be too long
			newFlag("id", hash(ContainerEnvVarReference(podNameEnvVar))),
			newFlag("host", ContainerEnvVarReference(podNameEnvVar)),
			// TODO: remove me one day? - currently it's helpful to see the DB's initialization progress
			newFlag("debug rgw", "15"),
			// TODO: remove me once caching is fixed
			newFlag("rgw cache enabled", "false"),
		),
		VolumeMounts: []v1.VolumeMount{daemonVolumeMountPVC()},
		Env:          DaemonEnvVars(objectStore.Spec.Image),
	}

	return container
}

func (r *ObjectStoreReconciler) generateService(objectStore *objectv1alpha1.ObjectStore) *v1.Service {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceName(objectStore.Name, objectStore.Namespace),
			Namespace: objectStore.Namespace,
			Labels:    getLabels(objectStore.Name),
		},
	}

	return svc
}

func (r *ObjectStoreReconciler) reconcileService(ctx context.Context, objectStore *objectv1alpha1.ObjectStore) (string, error) {
	service := r.generateService(objectStore)

	err := controllerutil.SetControllerReference(objectStore, service, r.Scheme)
	if err != nil {
		return "", fmt.Errorf("failed to set owner reference to service %q: %w", service.Name, err)
	}

	port := int32(8080)
	if objectStore.Spec.Gateway.Port != 0 {
		port = objectStore.Spec.Gateway.Port
	}

	// Create mutate function to update the service
	mutateFunc := func() error {
		// If the cluster is not external we add the Selector
		service.Spec = v1.ServiceSpec{
			Selector: getLabels(objectStore.Name),
		}

		addPort(service, "http", port, rgwPortInternalPort)
		return nil
	}

	// Create or update the service
	opResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, mutateFunc)
	if err != nil {
		return "", fmt.Errorf("failed to create or update object store %q service %q: %w", objectStore.Name, opResult, err)
	}
	r.Logger.Info("object store gateway service ", "opResult", opResult, "at", service.Spec.ClusterIP, "port", port)

	return service.Spec.ClusterIP, nil
}

func addPort(service *v1.Service, name string, port, destPort int32) {
	if port == 0 || destPort == 0 {
		return
	}
	service.Spec.Ports = append(service.Spec.Ports, v1.ServicePort{
		Name:       name,
		Port:       port,
		TargetPort: intstr.FromInt(int(destPort)),
		Protocol:   v1.ProtocolTCP,
	})
}

func getLabels(name string) map[string]string {
	return map[string]string{
		"object_store": name,
	}
}

func getLabelString(name string) string {
	return fmt.Sprintf("object_store=%s", name)
}

// chownCephDataDirsInitContainer returns an init container which `chown`s the given data
// directories as the `ceph:ceph` user in the container.
// Doing a chown in a post start lifecycle hook does not reliably complete before the daemon
// process starts, which can cause the pod to fail without the lifecycle hook's chown command
// completing. It can take an arbitrarily long time for a pod restart to successfully chown the
// directory. This is a race condition for all daemons; therefore, do this in an init container.
func chownCephDataDirsInitContainer(
	containerImage string,
	volumeMounts []v1.VolumeMount,
	securityContext *v1.SecurityContext,
) v1.Container {
	args := make([]string, 0, 5)
	args = append(args,
		"--verbose",
		"--recursive",
		"ceph:ceph",
		objectStoreDataDirectory,
	)
	return v1.Container{
		Name:            "chown-container-data-dir",
		Command:         []string{"chown"},
		Args:            args,
		Image:           containerImage,
		VolumeMounts:    volumeMounts,
		SecurityContext: securityContext,
	}
}

func createZoneContainer(objectStore *objectv1alpha1.ObjectStore, endpoint string) v1.Container {
	return v1.Container{
		Name:         "object-store-multisite-create-zone",
		Image:        objectStore.Spec.Image,
		Command:      []string{"rgwam-sqlite"},
		Args:         []string{"zone", "create", fmt.Sprintf("--zone=%s-%s", objectStore.Name, objectStore.Namespace), "--realm-token=$(REALM_TOKEN)", fmt.Sprintf("--endpoints=%s", endpoint)},
		VolumeMounts: []v1.VolumeMount{daemonVolumeMountPVC()},
		Env:          append(DaemonEnvVars(objectStore.Spec.Image), realmTokenSecretEnv(objectStore.Spec.Multisite.RealmTokenSecretName)),
	}
}

// podSecurityContextPrivileged returns a privileged PodSecurityContext.
func podSecurityContext() *v1.SecurityContext {
	var root int64 = 0
	privileged := true
	return &v1.SecurityContext{
		Privileged: &privileged,
		RunAsUser:  &root,
	}
}

func (r *ObjectStoreReconciler) waitForLabeledPodsToRunWithRetries(ctx context.Context, objectStore *objectv1alpha1.ObjectStore, retries int) (v1.Pod, error) {
	const retryInterval = 30
	labels := getLabels(objectStore.Name)
	opts := []controllerclient.ListOption{
		controllerclient.InNamespace(objectStore.Namespace),
		controllerclient.MatchingLabels(labels),
	}
	podList := &v1.PodList{}
	for i := 0; i < retries; i++ {
		err := r.Client.List(ctx, podList, opts...)
		lastStatus := ""
		running := 0
		if err == nil && len(podList.Items) > 0 {
			for _, pod := range podList.Items {
				if pod.Status.Phase == "Running" {
					running++
				}
				lastStatus = string(pod.Status.Phase)
			}
			if running == len(podList.Items) {
				r.Logger.Info("all pod(s) running", "Pods", len(podList.Items), "label", labels)
				return podList.Items[0], nil
			}
		}
		r.Logger.Info("waiting for", "pod(s) with label", labels, "status", lastStatus, "running", running, "numPod", len(podList.Items), "Error", err)
		time.Sleep(retryInterval * time.Second)
	}

	return v1.Pod{}, fmt.Errorf("giving up waiting for pod with label %v to be running", labels)
}

func (r *ObjectStoreReconciler) waitForJobCompletion(ctx context.Context, job *batchv1.Job, timeout time.Duration) error {
	r.Logger.Info("waiting for job to complete...", "job", job.Name)
	return wait.Poll(5*time.Second, timeout, func() (bool, error) {
		err := r.Client.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, job)
		if err != nil {
			return false, fmt.Errorf("failed to get job %s. %v", job.Name, err)
		}

		// if the job is still running, allow it to continue to completion
		if job.Status.Active > 0 {
			r.Logger.Info("job is still running", "Status", job.Status)
			return false, nil
		}
		if job.Status.Failed > 0 {
			// TODO: show logs in the op
			// We need to use client-go for this not the controller-runtime client...
			return false, fmt.Errorf("job %s failed", job.Name)
		}
		if job.Status.Succeeded > 0 {
			return true, nil
		}
		r.Logger.Info("job is still initializing")
		return false, nil
	})
}

func multisiteJobMeta(objectStore *objectv1alpha1.ObjectStore) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "object-store-multisite-zone-job",
			Namespace: objectStore.Namespace,
		},
	}
}

// Create multisite zone job
func (r *ObjectStoreReconciler) createMultisiteZoneJob(ctx context.Context, objectStore *objectv1alpha1.ObjectStore, endpoint string) error {
	job := multisiteJobMeta(objectStore)
	backoffLimit := int32(600)
	job.Spec = batchv1.JobSpec{
		Template: v1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app": "object-store-multisite-zone-job",
				},
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:         "object-store-multisite-zone-job",
						Image:        objectStore.Spec.Image,
						Command:      []string{"rgwam-sqlite"},
						Args:         []string{"zone", "create", fmt.Sprintf("--zone=%s-%s", objectStore.Name, objectStore.Namespace), "--realm-token=$(REALM_TOKEN)", fmt.Sprintf("--endpoints=%s", endpoint)},
						VolumeMounts: []v1.VolumeMount{daemonVolumeMountPVC()},
						Env:          append(DaemonEnvVars(objectStore.Spec.Image), realmTokenSecretEnv(objectStore.Spec.Multisite.RealmTokenSecretName)),
					},
				},
				Volumes: []v1.Volume{
					DaemonVolumesDataPVC(instanceName(objectStore.Name, objectStore.Namespace)),
				},
				RestartPolicy: v1.RestartPolicyOnFailure,
			},
		},
		BackoffLimit: &backoffLimit,
	}

	// Set ObjectStore instance as the owner and controller of the Job.
	err := controllerutil.SetControllerReference(objectStore, job, r.Scheme)
	if err != nil {
		return fmt.Errorf("failed to set owner reference to job %q: %w", job.Name, err)
	}

	err = r.Client.Create(ctx, job)
	if err != nil {
		if kerrors.IsAlreadyExists(err) {
			// TODO: delete it and create it again?
			if job.Status.Failed > 0 {
				// Recreate it
			} else if job.Status.Succeeded > 0 {
				r.Logger.Info("Multisite Zone Job", job.Name, "already exists")
				return nil
			}
		}
		return fmt.Errorf("failed to create multisite zone job: %w", err)
	}

	return nil
}
