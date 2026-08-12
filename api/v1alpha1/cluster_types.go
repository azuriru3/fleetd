/*
Copyright 2026 Azril.

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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterSpec defines the desired state of Cluster
type ClusterSpec struct {
	// kubeconfigSecretRef points at a Secret, in the fleetd-system
	// namespace, holding the kubeconfig used to reach this cluster. The
	// credentials themselves never live on the Cluster object.
	// +required
	KubeconfigSecretRef corev1.LocalObjectReference `json:"kubeconfigSecretRef"`
}

// ClusterStatus defines the observed state of Cluster.
type ClusterStatus struct {
	// allocatable is the sum of allocatable resources across this
	// cluster's nodes, as observed on the last successful reconcile.
	// +optional
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`

	// lastHeartbeatTime is when the controller last successfully wrote
	// to this cluster, proving both read and write access.
	// +optional
	LastHeartbeatTime metav1.Time `json:"lastHeartbeatTime,omitzero"`

	// conditions represent the current state of the Cluster resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// Cluster is the Schema for the clusters API
type Cluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Cluster
	// +required
	Spec ClusterSpec `json:"spec"`

	// status defines the observed state of Cluster
	// +optional
	Status ClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterList contains a list of Cluster
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Cluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Cluster{}, &ClusterList{})
		return nil
	})
}
