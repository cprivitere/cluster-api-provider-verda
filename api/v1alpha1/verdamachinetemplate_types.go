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

// VerdaMachineTemplateSpec defines the desired state of VerdaMachineTemplate
type VerdaMachineTemplateSpec struct {
	// template is the template for the VerdaMachines this
	// template creates.
	// +required
	Template VerdaMachineTemplateResource `json:"template"`
}

// VerdaMachineTemplateResource is the template body: the object
// metadata and spec stamped onto every created VerdaMachine. The
// metadata is the Cluster API template contract's restricted
// ObjectMeta — labels and annotations only. Templates carry no
// status.
type VerdaMachineTemplateResource struct {
	// metadata is the object metadata the created VerdaMachine
	// carries.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the VerdaMachine spec the created object carries.
	// +required
	Spec VerdaMachineSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// VerdaMachineTemplate is the Schema for the verdamachinetemplates API
type VerdaMachineTemplate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of VerdaMachineTemplate
	// +required
	Spec VerdaMachineTemplateSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// VerdaMachineTemplateList contains a list of VerdaMachineTemplate
type VerdaMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VerdaMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VerdaMachineTemplate{}, &VerdaMachineTemplateList{})
		return nil
	})
}
