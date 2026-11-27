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
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// VerdaMachineSpec defines the desired state of VerdaMachine
type VerdaMachineSpec struct {
	// providerID is the Verda instance this machine manages, in the
	// form verda://<location>/<instance-id>. The controller sets it
	// once the instance exists; on later passes it is the identity
	// the reconciler trusts.
	// +optional
	ProviderID *string `json:"providerID,omitempty"`

	// instanceType is the Verda instance type, e.g. CPU.4V.16G.
	InstanceType string `json:"instanceType"`

	// image is the Verda image name, e.g. 26.04.cuda13.4.
	Image string `json:"image"`

	// location is the Verda location code. Empty means the SDK's
	// default location.
	// +optional
	Location string `json:"location,omitempty"`

	// sshKeyIDs are the UUIDs of SSH keys registered in the Verda
	// project that should be granted login on the instance.
	// +optional
	SSHKeyIDs []string `json:"sshKeyIDs,omitempty"`

	// isSpot requests a spot instance. Spot instances can be
	// discontinued by Verda; the reconciler surfaces that on the
	// status instead of silently recreating the machine.
	// +optional
	IsSpot bool `json:"isSpot,omitempty"`
}

// MachineAddressType is the kind of a MachineAddress. The values
// mirror clusterv1.MachineAddressType; the type is local for now so
// the provider does not take the cluster-api dependency until the
// Cluster API composition lands.
type MachineAddressType string

const (
	// MachineExternalIP is the instance's public IP address.
	MachineExternalIP MachineAddressType = "ExternalIP"
	// MachineInternalIP is the instance's private IP address.
	MachineInternalIP MachineAddressType = "InternalIP"
	// MachineHostName is the instance's hostname.
	MachineHostName MachineAddressType = "Hostname"
)

// MachineAddress is one network address of a machine's instance,
// mirroring clusterv1.MachineAddress.
type MachineAddress struct {
	Type    MachineAddressType `json:"type"`
	Address string             `json:"address"`
}

// VerdaMachineStatus defines the observed state of VerdaMachine.
type VerdaMachineStatus struct {
	// ready reports whether the instance exists and is running.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// addresses are the instance's network addresses.
	// +optional
	Addresses []MachineAddress `json:"addresses,omitempty"`

	// failureMessage is set when the instance reaches a state the
	// operator should know about — currently a spot instance being
	// discontinued. The machine is not recreated implicitly in that
	// state; a human decides.
	// +optional
	FailureMessage *string `json:"failureMessage,omitempty"`

	// conditions represent the current state of the VerdaMachine resource.
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

// VerdaMachine is the Schema for the verdamachines API
type VerdaMachine struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of VerdaMachine
	// +required
	Spec VerdaMachineSpec `json:"spec"`

	// status defines the observed state of VerdaMachine
	// +optional
	Status VerdaMachineStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VerdaMachineList contains a list of VerdaMachine
type VerdaMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VerdaMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VerdaMachine{}, &VerdaMachineList{})
		return nil
	})
}
