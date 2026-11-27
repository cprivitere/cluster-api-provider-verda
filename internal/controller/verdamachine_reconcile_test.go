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

package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1alpha1 "github.com/cprivitere/cluster-api-provider-verda/api/v1alpha1"
	"github.com/cprivitere/cluster-api-provider-verda/pkg/cloud"
)

func reconcileTestMachine() *infrastructurev1alpha1.VerdaMachine {
	return &infrastructurev1alpha1.VerdaMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Namespace: "default"},
		Spec: infrastructurev1alpha1.VerdaMachineSpec{
			InstanceType: "CPU.4V.16G",
			Image:        "26.04.cuda13.4",
			SSHKeyIDs:    []string{"key-1"},
		},
	}
}

func reconcileTestReconciler(t *testing.T, cloudClient cloud.Client, objs ...client.Object) *VerdaMachineReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infrastructurev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&infrastructurev1alpha1.VerdaMachine{}).
		Build()
	return &VerdaMachineReconciler{Client: c, Scheme: scheme, Cloud: cloudClient}
}

func TestReconcileCreatesOneInstanceAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := reconcileTestReconciler(t, cloudClient, reconcileTestMachine())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine", Namespace: "default"}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	machine := &infrastructurev1alpha1.VerdaMachine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if machine.Spec.ProviderID == nil || *machine.Spec.ProviderID == "" {
		t.Fatal("providerID was not recorded")
	}
	if !machine.Status.Ready {
		t.Error("status.ready is false, want true for a running instance")
	}
	foundFinalizer := false
	for _, f := range machine.Finalizers {
		if f == verdaMachineFinalizer {
			foundFinalizer = true
		}
	}
	if !foundFinalizer {
		t.Error("finalizer was not added")
	}

	// A second pass must not create a duplicate instance.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("cloud has %d instances after two reconciles, want 1", len(instances))
	}
	if instances[0].InstanceType != "CPU.4V.16G" || instances[0].Image != "26.04.cuda13.4" {
		t.Errorf("instance created from wrong spec: %+v", instances[0])
	}
	tagged, err := cloudClient.FindInstancesByTags(ctx, map[string]string{cloud.TagMachineName: "default/test-machine"})
	if err != nil {
		t.Fatalf("FindInstancesByTags: %v", err)
	}
	if len(tagged) != 1 {
		t.Errorf("identity tags found %d instances, want 1", len(tagged))
	}
}

func TestReconcileDeleteRemovesInstanceAndFinalizer(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	machine := reconcileTestMachine()
	r := reconcileTestReconciler(t, cloudClient, machine)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine", Namespace: "default"}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (create): %v", err)
	}
	stored := &infrastructurev1alpha1.VerdaMachine{}
	if err := r.Get(ctx, req.NamespacedName, stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := r.Delete(ctx, stored); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (delete): %v", err)
	}

	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 0 {
		t.Errorf("cloud has %d instances after delete, want 0", len(instances))
	}
	// With the finalizer removed, the object itself is gone — the
	// API server (and the fake client) deletes it once the last
	// finalizer drops. A lingering object here would mean the
	// finalizer was never removed.
	final := &infrastructurev1alpha1.VerdaMachine{}
	if err := r.Get(ctx, req.NamespacedName, final); !apierrors.IsNotFound(err) {
		t.Errorf("Get after delete returned %v, want NotFound (object should be fully deleted)", err)
	}
}

func TestReconcileSurfacesDiscontinuedSpot(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	machine := reconcileTestMachine()
	machine.Spec.IsSpot = true
	r := reconcileTestReconciler(t, cloudClient, machine)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine", Namespace: "default"}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (create): %v", err)
	}
	stored := &infrastructurev1alpha1.VerdaMachine{}
	if err := r.Get(ctx, req.NamespacedName, stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	id, ok := instanceIDFromProviderID(*stored.Spec.ProviderID)
	if !ok {
		t.Fatalf("providerID %q did not parse", *stored.Spec.ProviderID)
	}
	if err := cloudClient.SetInstanceStatus(id, instanceStatusDiscontinued); err != nil {
		t.Fatalf("SetInstanceStatus: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (discontinued): %v", err)
	}
	final := &infrastructurev1alpha1.VerdaMachine{}
	if err := r.Get(ctx, req.NamespacedName, final); err != nil {
		t.Fatalf("Get after discontinued: %v", err)
	}
	if final.Status.Ready {
		t.Error("status.ready is true for a discontinued instance, want false")
	}
	if final.Status.FailureMessage == nil {
		t.Error("status.failureMessage is not set for a discontinued instance")
	}
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Errorf("cloud has %d instances after discontinuation, want 1 (no silent recreation)", len(instances))
	}
}
