/*
Copyright 2026.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// InitializationStatus is the status.initialization block of the
// Cluster API v1beta2 infrastructure contract, shared by the
// VerdaCluster and VerdaMachine statuses: provisioned flips true
// exactly once, when the object's infrastructure exists, and core
// Cluster API takes the object from there.
type InitializationStatus struct {
	// provisioned reports whether the infrastructure is provisioned.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// VerdaClusterSpec defines the desired state of VerdaCluster
type VerdaClusterSpec struct {
	// controlPlaneEndpoint is the API server endpoint of the
	// workload cluster. The VerdaCluster controller writes it (the
	// first control-plane instance's public IP, port 6443); core
	// copies it onto the Cluster once initialization.provisioned
	// is set, and KCP/CABPK consume it from there.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty"`

	// location is the Verda location code the cluster's instances
	// live in. Stated explicitly on purpose: the Verda SDK silently
	// defaults an empty location to FIN-03, while the instance
	// families this platform uses (CPU.*, Tesla V100) exist only
	// in FIN-01.
	// +optional
	Location string `json:"location,omitempty"`
}

// VerdaClusterStatus defines the observed state of VerdaCluster.
type VerdaClusterStatus struct {
	// initialization carries the Cluster API v1beta2 contract's
	// provisioning signal: provisioned flips true once the
	// control-plane endpoint is written, and core surfaces the
	// endpoint onto the Cluster at the same moment.
	// +optional
	Initialization InitializationStatus `json:"initialization,omitempty"`

	// conditions represent the current state of the VerdaCluster resource.
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

// VerdaCluster is the Schema for the verdaclusters API
type VerdaCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of VerdaCluster
	// +required
	Spec VerdaClusterSpec `json:"spec"`

	// status defines the observed state of VerdaCluster
	// +optional
	Status VerdaClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VerdaClusterList contains a list of VerdaCluster
type VerdaClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VerdaCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VerdaCluster{}, &VerdaClusterList{})
		return nil
	})
}
