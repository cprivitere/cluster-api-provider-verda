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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1alpha1 "github.com/cprivitere/cluster-api-provider-verda/api/v1alpha1"
	"github.com/cprivitere/cluster-api-provider-verda/pkg/cloud"
)

const nodeTestIP = "203.0.113.10"

// labeledTestMachine is the reconcileTestMachine carrying the
// cluster-name label core Cluster API sets, which is how the node
// step finds its workload cluster.
func labeledTestMachine() *infrastructurev1alpha1.VerdaMachine {
	machine := reconcileTestMachine()
	machine.Labels = map[string]string{clusterv1.ClusterNameLabel: testClusterName}
	return machine
}

func nodeTestReconciler(t *testing.T, cloudClient cloud.Client, workloadClient client.Client, machine *infrastructurev1alpha1.VerdaMachine) *VerdaMachineReconciler {
	t.Helper()
	r := bootstrapTestReconciler(t, cloudClient, machine)
	r.ClusterCache = clustercache.NewFakeClusterCache(workloadClient,
		client.ObjectKey{Namespace: testNamespace, Name: testClusterName})
	return r
}

func workloadClientWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (corev1): %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func uninitializedNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{
				Key:    uninitializedTaintKey,
				Value:  "true",
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
				{Type: corev1.NodeExternalIP, Address: nodeTestIP},
			},
		},
	}
}

// primeInstance runs one reconcile so the instance exists, then
// gives it the address the test node carries.
func primeInstance(t *testing.T, ctx context.Context, r *VerdaMachineReconciler, cloudClient *cloud.FakeClient, req ctrl.Request) {
	t.Helper()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("priming Reconcile: %v", err)
	}
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("cloud has %d instances after priming, want 1", len(instances))
	}
	if err := cloudClient.SetInstanceIP(instances[0].ID, nodeTestIP); err != nil {
		t.Fatalf("SetInstanceIP: %v", err)
	}
}

func TestNodeGetsProviderIDAndTaintCleared(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	workload := workloadClientWith(t, uninitializedNode())
	r := nodeTestReconciler(t, cloudClient, workload, labeledTestMachine())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}
	primeInstance(t, ctx, r, cloudClient, req)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	node := &corev1.Node{}
	if err := workload.Get(ctx, types.NamespacedName{Name: testNodeName}, node); err != nil {
		t.Fatalf("Get node: %v", err)
	}
	if want := testProviderID; node.Spec.ProviderID != want {
		t.Errorf("node providerID = %q, want %q", node.Spec.ProviderID, want)
	}
	if len(node.Spec.Taints) != 0 {
		t.Errorf("node taints = %v, want the uninitialized taint cleared", node.Spec.Taints)
	}
}

func TestNodeStepWaitsForNodeToRegister(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	workload := workloadClientWith(t)
	r := nodeTestReconciler(t, cloudClient, workload, labeledTestMachine())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}
	primeInstance(t, ctx, r, cloudClient, req)

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("RequeueAfter = %v, want 15s while the node has not registered", result.RequeueAfter)
	}
}

func TestNodeStepQuietWhenAlreadyInitialized(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	node := uninitializedNode()
	node.Spec.ProviderID = testProviderID
	node.Spec.Taints = nil
	workload := workloadClientWith(t, node)
	r := nodeTestReconciler(t, cloudClient, workload, labeledTestMachine())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}
	primeInstance(t, ctx, r, cloudClient, req)

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want no requeue once the node is initialized", result.RequeueAfter)
	}
}

// nodeWithUUID is the uninitialized test node carrying a system
// UUID and a chosen external address.
func nodeWithUUID(name, systemUUID, externalIP string) *corev1.Node {
	node := uninitializedNode()
	node.Name = name
	node.Status.NodeInfo.SystemUUID = systemUUID
	node.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
		{Type: corev1.NodeExternalIP, Address: externalIP},
	}
	return node
}

// The node's addresses deliberately do not include the instance's
// IP, so only the systemUUID match can find it. (The pre-existing
// TestNodeGetsProviderIDAndTaintCleared covers the IP fallback:
// its node reports no systemUUID.)
func TestNodeMatchedBySystemUUID(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	workload := workloadClientWith(t, nodeWithUUID(testNodeName, "instance-0001", "203.0.113.99"))
	r := nodeTestReconciler(t, cloudClient, workload, labeledTestMachine())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}
	primeInstance(t, ctx, r, cloudClient, req)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	node := &corev1.Node{}
	if err := workload.Get(ctx, types.NamespacedName{Name: testNodeName}, node); err != nil {
		t.Fatalf("Get node: %v", err)
	}
	if want := testProviderID; node.Spec.ProviderID != want {
		t.Errorf("node providerID = %q, want %q", node.Spec.ProviderID, want)
	}
	if len(node.Spec.Taints) != 0 {
		t.Errorf("node taints = %v, want the uninitialized taint cleared", node.Spec.Taints)
	}
}

// With one node matching by address only and another matching by
// systemUUID, the UUID match wins: identity beats a reusable
// address.
func TestNodeUUIDMatchWinsOverIPMatch(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	ipNode := nodeWithUUID("ip-node", "instance-9999", nodeTestIP)
	uuidNode := nodeWithUUID("uuid-node", "instance-0001", "203.0.113.99")
	workload := workloadClientWith(t, ipNode, uuidNode)
	r := nodeTestReconciler(t, cloudClient, workload, labeledTestMachine())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}
	primeInstance(t, ctx, r, cloudClient, req)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	node := &corev1.Node{}
	if err := workload.Get(ctx, types.NamespacedName{Name: "uuid-node"}, node); err != nil {
		t.Fatalf("Get uuid-node: %v", err)
	}
	if want := testProviderID; node.Spec.ProviderID != want {
		t.Errorf("uuid-node providerID = %q, want %q", node.Spec.ProviderID, want)
	}
	if len(node.Spec.Taints) != 0 {
		t.Errorf("uuid-node taints = %v, want the uninitialized taint cleared", node.Spec.Taints)
	}
	other := &corev1.Node{}
	if err := workload.Get(ctx, types.NamespacedName{Name: "ip-node"}, other); err != nil {
		t.Fatalf("Get ip-node: %v", err)
	}
	if other.Spec.ProviderID != "" {
		t.Errorf("ip-node providerID = %q, want it untouched", other.Spec.ProviderID)
	}
	if len(other.Spec.Taints) != 1 {
		t.Errorf("ip-node taints = %v, want the uninitialized taint intact", other.Spec.Taints)
	}
}

func TestNodeStepSkippedWithoutClusterCache(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := bootstrapTestReconciler(t, cloudClient, labeledTestMachine())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want no requeue when the node step is disabled", result.RequeueAfter)
	}
}
