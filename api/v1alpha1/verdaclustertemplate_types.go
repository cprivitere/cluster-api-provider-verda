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

// VerdaClusterTemplateSpec defines the desired state of VerdaClusterTemplate
type VerdaClusterTemplateSpec struct {
	// template is the template for the VerdaClusters this
	// template creates.
	// +required
	Template VerdaClusterTemplateResource `json:"template"`
}

// VerdaClusterTemplateResource is the template body: the object
// metadata and spec stamped onto every created VerdaCluster. The
// metadata is the Cluster API template contract's restricted
// ObjectMeta — labels and annotations only. Templates carry no
// status.
type VerdaClusterTemplateResource struct {
	// metadata is the object metadata the created VerdaCluster
	// carries.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the VerdaCluster spec the created object carries.
	// +required
	Spec VerdaClusterSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// VerdaClusterTemplate is the Schema for the verdaclustertemplates API
type VerdaClusterTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of VerdaClusterTemplate
	// +required
	Spec VerdaClusterTemplateSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// VerdaClusterTemplateList contains a list of VerdaClusterTemplate
type VerdaClusterTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VerdaClusterTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VerdaClusterTemplate{}, &VerdaClusterTemplateList{})
		return nil
	})
}
