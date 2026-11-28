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

	verdasdk "github.com/verda-cloud/verdacloud-sdk-go/pkg/verda"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1alpha1 "github.com/cprivitere/cluster-api-provider-verda/api/v1alpha1"
	"github.com/cprivitere/cluster-api-provider-verda/pkg/cloud"
)

// Shared fixture values for the controller test suites (goconst:
// these literals recur across the package's test files).
const (
	testClusterName   = "test-cluster"
	testMachineName   = "test-machine"
	testBootstrapName = "test-bootstrap"
	testNodeName      = "test-node"
	testNamespace     = "default"
	testLocation      = "FIN-01"
	testInstanceType  = "CPU.4V.16G"
	testImage         = "26.04.cuda13.4"
	testProviderID    = "verda://FIN-01/instance-0001"
	testNetworkID     = "network-0001"
	testSubnetID      = "subnet-0001"
)

func reconcileTestMachine() *infrastructurev1alpha1.VerdaMachine {
	return &infrastructurev1alpha1.VerdaMachine{
		ObjectMeta: metav1.ObjectMeta{Name: testMachineName, Namespace: testNamespace},
		Spec: infrastructurev1alpha1.VerdaMachineSpec{
			InstanceType: testInstanceType,
			Image:        testImage,
			Location:     testLocation,
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
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	machine := &infrastructurev1alpha1.VerdaMachine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if machine.Spec.ProviderID == "" {
		t.Fatal("providerID was not recorded")
	}
	if !machine.Status.Ready {
		t.Error("status.ready is false, want true for a running instance")
	}
	if machine.Status.Initialization.Provisioned == nil || !*machine.Status.Initialization.Provisioned {
		t.Error("status.initialization.provisioned is not true for a running instance (the v1beta2 contract signal)")
	}
	cond := meta.FindStatusCondition(machine.Status.Conditions, infrastructurev1alpha1.ReadyCondition)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != infrastructurev1alpha1.InstanceRunningReason {
		t.Errorf("Ready condition = %+v, want True/%s", cond, infrastructurev1alpha1.InstanceRunningReason)
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
	if instances[0].InstanceType != testInstanceType || instances[0].Image != testImage {
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
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

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
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (create): %v", err)
	}
	stored := &infrastructurev1alpha1.VerdaMachine{}
	if err := r.Get(ctx, req.NamespacedName, stored); err != nil {
		t.Fatalf("Get: %v", err)
	}
	id, ok := instanceIDFromProviderID(stored.Spec.ProviderID)
	if !ok {
		t.Fatalf("providerID %q did not parse", stored.Spec.ProviderID)
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
	cond := meta.FindStatusCondition(final.Status.Conditions, infrastructurev1alpha1.ReadyCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != infrastructurev1alpha1.InstanceDiscontinuedReason {
		t.Errorf("Ready condition = %+v, want False/%s (the contract-visible discontinued signal)", cond, infrastructurev1alpha1.InstanceDiscontinuedReason)
	}
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Errorf("cloud has %d instances after discontinuation, want 1 (no silent recreation)", len(instances))
	}
}

// networkTestReconciler is reconcileTestReconciler with the core
// Cluster API types registered, for tests that seed a Cluster and
// its VerdaCluster alongside the machine.
func networkTestReconciler(t *testing.T, cloudClient cloud.Client, objs ...client.Object) *VerdaMachineReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infrastructurev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (infrastructure): %v", err)
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (core): %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (corev1): %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&infrastructurev1alpha1.VerdaMachine{}, &infrastructurev1alpha1.VerdaCluster{}).
		Build()
	return &VerdaMachineReconciler{Client: c, Scheme: scheme, Cloud: cloudClient}
}

// networkTestObjects seeds the Cluster and VerdaCluster a machine
// belongs to; the VerdaCluster status carries the network the
// cluster controller provisioned (or not, when provisioned is
// false).
func networkTestObjects(machine *infrastructurev1alpha1.VerdaMachine, provisioned bool) []client.Object {
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "infrastructure.cluster.x-k8s.io",
				Kind:     "VerdaCluster",
				Name:     testClusterName,
			},
		},
	}
	infraCluster := &infrastructurev1alpha1.VerdaCluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec:       infrastructurev1alpha1.VerdaClusterSpec{Location: testLocation},
	}
	if provisioned {
		infraCluster.Status.NetworkID = testNetworkID
		infraCluster.Status.SubnetID = testSubnetID
	}
	return []client.Object{cluster, infraCluster, machine, ownerMachine(new(testBootstrapName)), bootstrapSecret("cloud-config")}
}

func theInstance(t *testing.T, ctx context.Context, cloudClient *cloud.FakeClient) verdasdk.CreateInstanceRequest {
	t.Helper()
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("cloud has %d instances, want 1", len(instances))
	}
	createReq, ok := cloudClient.CreateRequestFor(instances[0].ID)
	if !ok {
		t.Fatalf("no create request recorded for instance %s", instances[0].ID)
	}
	return createReq
}

// The control-plane instance joins the cluster network at the
// pinned private address the endpoint proxy forwards to.
func TestReconcileControlPlanePinsPrivateAddress(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	machine := ownedTestMachine()
	machine.Labels = map[string]string{clusterNameLabel: testClusterName, controlPlaneLabel: ""}
	r := networkTestReconciler(t, cloudClient, networkTestObjects(machine, true)...)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	createReq := theInstance(t, ctx, cloudClient)
	if createReq.Network != testNetworkID || createReq.Subnet != testSubnetID {
		t.Errorf("instance created on network %q subnet %q, want network-0001/subnet-0001", createReq.Network, createReq.Subnet)
	}
	if createReq.PrivateIP == nil || string(*createReq.PrivateIP) != cloud.ControlPlanePrivateIP {
		t.Errorf("control-plane private allocation = %v, want the pinned %s", createReq.PrivateIP, cloud.ControlPlanePrivateIP)
	}
	if createReq.PublicIP == nil || *createReq.PublicIP != verdasdk.IPAllocAuto {
		t.Errorf("control-plane public allocation = %v, want auto", createReq.PublicIP)
	}
}

// A worker that names no network in its spec stays off every
// network — even when its cluster has one. It reaches the API
// server through the cluster's public endpoint instead (the
// attachment rule: only control planes join by force).
func TestReconcileWorkerStaysOffNetworkByDefault(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	machine := ownedTestMachine()
	machine.Labels = map[string]string{clusterNameLabel: testClusterName}
	r := networkTestReconciler(t, cloudClient, networkTestObjects(machine, true)...)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	createReq := theInstance(t, ctx, cloudClient)
	if createReq.Network != "" || createReq.Subnet != "" {
		t.Errorf("worker created on network %q subnet %q, want unattached", createReq.Network, createReq.Subnet)
	}
	if createReq.PrivateIP != nil || createReq.PublicIP != nil {
		t.Errorf("worker IP allocations = %v/%v, want both unset (platform default)", createReq.PrivateIP, createReq.PublicIP)
	}
}

// A worker whose spec explicitly names a network and subnet joins
// that network with automatic addresses.
func TestReconcileWorkerJoinsExplicitNetworkFromSpec(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	machine := ownedTestMachine()
	machine.Labels = map[string]string{clusterNameLabel: testClusterName}
	machine.Spec.NetworkID = testNetworkID
	machine.Spec.SubnetID = testSubnetID
	r := networkTestReconciler(t, cloudClient, networkTestObjects(machine, true)...)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	createReq := theInstance(t, ctx, cloudClient)
	if createReq.Network != testNetworkID || createReq.Subnet != testSubnetID {
		t.Errorf("instance created on network %q subnet %q, want the spec's %q/%q",
			createReq.Network, createReq.Subnet, testNetworkID, testSubnetID)
	}
	if createReq.PrivateIP == nil || *createReq.PrivateIP != verdasdk.IPAllocAuto {
		t.Errorf("worker private allocation = %v, want auto", createReq.PrivateIP)
	}
}

// A worker spec naming only one of network/subnet is an error, not
// a half-attached create.
func TestReconcileWorkerRejectsHalfNamedNetwork(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	machine := ownedTestMachine()
	machine.Labels = map[string]string{clusterNameLabel: testClusterName}
	machine.Spec.NetworkID = testNetworkID
	r := networkTestReconciler(t, cloudClient, networkTestObjects(machine, true)...)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("Reconcile succeeded with only spec.networkID set, want an error")
	}
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 0 {
		t.Errorf("cloud has %d instances, want 0 after the rejected spec", len(instances))
	}
}

// While the cluster network is not provisioned yet, a control
// plane must not be created off-network (its attachment is
// enforced): the reconcile errors and no instance appears.
func TestReconcileWaitsForClusterNetwork(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	machine := ownedTestMachine()
	machine.Labels = map[string]string{clusterNameLabel: testClusterName, controlPlaneLabel: ""}
	r := networkTestReconciler(t, cloudClient, networkTestObjects(machine, false)...)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("Reconcile succeeded before the cluster network was provisioned, want an error")
	}
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 0 {
		t.Errorf("cloud has %d instances, want 0 while the network is unprovisioned", len(instances))
	}
}
