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

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectStoreSpec defines the desired state of ObjectStore
type ObjectStoreSpec struct {
	// Image is the container image to use for the ObjectStore.
	Image string `json:"image"`

	// Important: Run "make" to regenerate code after modifying this file
	// The rgw pod info
	// +optional
	// +nullable
	Gateway GatewaySpec `json:"gateway"`

	// Multisite is the multisite configuration
	// +optional
	Multisite *MultisiteSpec `json:"multisite,omitempty"`

	// VolumeClaimTemplate is the PVC definition
	VolumeClaimTemplate *v1.PersistentVolumeClaim `json:"volumeClaimTemplate,omitempty"`
}

// GatewaySpec represents the specification of Ceph Object Store Gateway
type GatewaySpec struct {
	// The port the rgw service will be listening on (http)
	// +optional
	Port int32 `json:"port,omitempty"`
}

type MultisiteSpec struct {
	// IsMainSite is true if this is the main site of the multisite
	IsMainSite bool `json:"isMainSite,omitempty"`

	// RealmTokenSecretName is the name of the Kubernetes Secret that contains the realm token
	// It is used to bootstrap the Zone
	// +optional
	RealmTokenSecretName string `json:"realmTokenSecretName,omitempty"`
}

// ObjectStoreStatus defines the observed state of ObjectStore
type ObjectStoreStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// ObjectStore is the Schema for the objectstores API
type ObjectStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectStoreSpec   `json:"spec,omitempty"`
	Status ObjectStoreStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ObjectStoreList contains a list of ObjectStore
type ObjectStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectStore `json:"items"`
}

func init() {
	//SchemeBuilder.Register(addKnownTypes)
	SchemeBuilder.Register(
		&ObjectStore{},
		&ObjectStoreList{},
	)
}

func (o *ObjectStoreSpec) IsMultisite() bool {
	return o.Multisite != nil && o.Multisite.RealmTokenSecretName != ""
}

func (o *ObjectStoreSpec) IsMainSite() bool {
	return o.Multisite != nil && o.Multisite.IsMainSite
}
