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
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	controlplanev1beta2 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1alpha1 "github.com/cprivitere/cluster-api-provider-verda/api/v1alpha1"
	"github.com/cprivitere/cluster-api-provider-verda/pkg/cloud"
)

const (
	testControlPlaneName = "test-control-plane"
	testCPTemplateName   = "test-cp-template"
)

// testSSHKeyIDs stands in for the project's real key IDs (UUID-shaped,
// as the API requires).
var testSSHKeyIDs = []string{
	"11111111-2222-4333-8444-555555555555",
	"66666666-7777-4888-8999-000000000000",
}

// reconcileTestControlPlaneChain returns the objects the cluster
// controller's SSH-key lookup walks: the owner Cluster, its
// KubeadmControlPlane, and the control-plane machine template
// carrying the keys.
func reconcileTestControlPlaneChain() []client.Object {
	owner := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterName, Namespace: testNamespace},
		Spec: clusterv1.ClusterSpec{
			ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: "controlplane.cluster.x-k8s.io",
				Kind:     "KubeadmControlPlane",
				Name:     testControlPlaneName,
			},
		},
	}
	controlPlane := &controlplanev1beta2.KubeadmControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: testControlPlaneName, Namespace: testNamespace},
		Spec: controlplanev1beta2.KubeadmControlPlaneSpec{
			MachineTemplate: controlplanev1beta2.KubeadmControlPlaneMachineTemplate{
				Spec: controlplanev1beta2.KubeadmControlPlaneMachineTemplateSpec{
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: "infrastructure.cluster.x-k8s.io",
						Kind:     "VerdaMachineTemplate",
						Name:     testCPTemplateName,
					},
				},
			},
		},
	}
	template := &infrastructurev1alpha1.VerdaMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: testCPTemplateName, Namespace: testNamespace},
		Spec: infrastructurev1alpha1.VerdaMachineTemplateSpec{
			Template: infrastructurev1alpha1.VerdaMachineTemplateResource{
				Spec: infrastructurev1alpha1.VerdaMachineSpec{SSHKeyIDs: testSSHKeyIDs},
			},
		},
	}
	return []client.Object{owner, controlPlane, template}
}

// clusterWithChain returns the reconciler inputs for a cluster whose
// control-plane chain exists: the VerdaCluster plus the lookup chain.
func clusterWithChain() []client.Object {
	return append([]client.Object{reconcileTestCluster()}, reconcileTestControlPlaneChain()...)
}

func reconcileTestCluster() *infrastructurev1alpha1.VerdaCluster {
	return &infrastructurev1alpha1.VerdaCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
			Labels:    map[string]string{clusterNameLabel: testClusterName},
		},
		Spec: infrastructurev1alpha1.VerdaClusterSpec{Location: testLocation},
	}
}

func clusterTestReconciler(t *testing.T, cloudClient cloud.Client, objs ...client.Object) *VerdaClusterReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infrastructurev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (core): %v", err)
	}
	if err := controlplanev1beta2.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (controlplane): %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&infrastructurev1alpha1.VerdaCluster{}).
		Build()
	return &VerdaClusterReconciler{Client: c, Scheme: scheme, Cloud: cloudClient}
}

func getCluster(t *testing.T, ctx context.Context, r *VerdaClusterReconciler) *infrastructurev1alpha1.VerdaCluster {
	t.Helper()
	cluster := &infrastructurev1alpha1.VerdaCluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: testClusterName, Namespace: testNamespace}, cluster); err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	return cluster
}

// theProxy returns the cluster's one endpoint-proxy instance from
// the fake cloud, failing the test unless there is exactly one.
func theProxy(t *testing.T, ctx context.Context, cloudClient *cloud.FakeClient) (string, string) {
	t.Helper()
	found, err := cloudClient.FindInstancesByTags(ctx, map[string]string{
		cloud.TagClusterName: testNamespace + "/" + testClusterName,
		cloud.TagRole:        cloud.RoleEndpointProxy,
	})
	if err != nil {
		t.Fatalf("FindInstancesByTags: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("cloud has %d endpoint proxies, want 1", len(found))
	}
	if found[0].IP == nil {
		t.Fatalf("endpoint proxy %s has no public IP", found[0].ID)
	}
	return found[0].ID, *found[0].IP
}

// One reconcile builds the whole endpoint path — private network,
// subnet, proxy instance — and publishes the proxy's address as the
// control-plane endpoint with provisioned set: the state KCP and
// CABPK are waiting for before any machine can exist.
func TestClusterReconcileProvisionsNetworkProxyAndEndpoint(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := clusterTestReconciler(t, cloudClient, clusterWithChain()...)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: testNamespace}}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 once the endpoint is published", result.RequeueAfter)
	}

	networks, err := cloudClient.ListPrivateNetworks(ctx)
	if err != nil {
		t.Fatalf("ListPrivateNetworks: %v", err)
	}
	if len(networks) != 1 || networks[0].Name != "capve-default-test-cluster" {
		t.Fatalf("networks = %+v, want one capve-default-test-cluster", networks)
	}
	subnets, err := cloudClient.ListSubnets(ctx, networks[0].ID)
	if err != nil {
		t.Fatalf("ListSubnets: %v", err)
	}
	if len(subnets) != 1 || subnets[0].CIDR != cloud.SubnetCIDR || subnets[0].Location.Code != testLocation {
		t.Fatalf("subnets = %+v, want one %s subnet in %s", subnets, cloud.SubnetCIDR, testLocation)
	}

	proxyID, proxyIP := theProxy(t, ctx, cloudClient)
	createReq, ok := cloudClient.CreateRequestFor(proxyID)
	if !ok {
		t.Fatalf("no create request recorded for proxy %s", proxyID)
	}
	if createReq.Network != networks[0].ID || createReq.Subnet != subnets[0].ID {
		t.Errorf("proxy created on network %q subnet %q, want %q/%q", createReq.Network, createReq.Subnet, networks[0].ID, subnets[0].ID)
	}
	if createReq.PrivateIP == nil || string(*createReq.PrivateIP) != cloud.ProxyPrivateIP {
		t.Errorf("proxy private allocation = %v, want %s", createReq.PrivateIP, cloud.ProxyPrivateIP)
	}
	if !slices.Equal(createReq.SSHKeyIDs, testSSHKeyIDs) {
		t.Errorf("proxy SSH keys = %v, want the control-plane template's %v", createReq.SSHKeyIDs, testSSHKeyIDs)
	}
	script := cloudClient.StartupScriptFor(proxyID)
	if !strings.Contains(script, "server control-plane "+cloud.ControlPlanePrivateIP+":6443") {
		t.Errorf("proxy startup script does not forward to the control plane:\n%s", script)
	}

	cluster := getCluster(t, ctx, r)
	if cluster.Status.NetworkID != networks[0].ID || cluster.Status.SubnetID != subnets[0].ID {
		t.Errorf("status network/subnet = %q/%q, want %q/%q", cluster.Status.NetworkID, cluster.Status.SubnetID, networks[0].ID, subnets[0].ID)
	}
	if cluster.Spec.ControlPlaneEndpoint.Host != proxyIP || cluster.Spec.ControlPlaneEndpoint.Port != controlPlanePort {
		t.Errorf("endpoint = %+v, want %s:%d", cluster.Spec.ControlPlaneEndpoint, proxyIP, controlPlanePort)
	}
	if cluster.Status.Initialization.Provisioned == nil || !*cluster.Status.Initialization.Provisioned {
		t.Error("status.initialization.provisioned is not true (the v1beta2 contract signal)")
	}
	hasFinalizer := false
	for _, f := range cluster.Finalizers {
		if f == verdaClusterFinalizer {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Error("finalizer was not added")
	}
}

// Without the control-plane chain, the proxy's SSH keys cannot be
// resolved — and the reconcile must fail loudly rather than create
// a keyless instance the API rejects (the Nov 27, 2026 live
// failure: a best-effort lookup swallowed a missing-scheme error
// and the create went out with no keys at all).
func TestClusterReconcileFailsLoudlyWithoutControlPlaneChain(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := clusterTestReconciler(t, cloudClient, reconcileTestCluster())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("Reconcile succeeded without a control-plane chain, want an error")
	}
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 0 {
		t.Errorf("cloud has %d instances after the failed reconcile, want 0", len(instances))
	}
}

// A second reconcile must not duplicate the network, subnet, or
// proxy: find-or-create by name and tag is the idempotency story
// (the API has no idempotency keys).
func TestClusterReconcileIsIdempotent(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := clusterTestReconciler(t, cloudClient, clusterWithChain()...)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: testNamespace}}

	for i := range 2 {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}
	networks, err := cloudClient.ListPrivateNetworks(ctx)
	if err != nil {
		t.Fatalf("ListPrivateNetworks: %v", err)
	}
	if len(networks) != 1 {
		t.Fatalf("networks = %d after two reconciles, want 1", len(networks))
	}
	subnets, err := cloudClient.ListSubnets(ctx, networks[0].ID)
	if err != nil {
		t.Fatalf("ListSubnets: %v", err)
	}
	if len(subnets) != 1 {
		t.Errorf("subnets = %d after two reconciles, want 1", len(subnets))
	}
	theProxy(t, ctx, cloudClient)
}

// Deleting the cluster removes the proxy, the subnet, and the
// network, and releases the finalizer.
func TestClusterReconcileDeleteRemovesNetworkAndProxy(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := clusterTestReconciler(t, cloudClient, clusterWithChain()...)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	theProxy(t, ctx, cloudClient)

	cluster := getCluster(t, ctx, r)
	if err := r.Delete(ctx, cluster); err != nil {
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
	networks, err := cloudClient.ListPrivateNetworks(ctx)
	if err != nil {
		t.Fatalf("ListPrivateNetworks: %v", err)
	}
	if len(networks) != 0 {
		t.Errorf("cloud has %d networks after delete, want 0", len(networks))
	}
	// With the finalizer released, the object itself is gone.
	cluster = &infrastructurev1alpha1.VerdaCluster{}
	err = r.Get(ctx, types.NamespacedName{Name: testClusterName, Namespace: testNamespace}, cluster)
	if err == nil {
		for _, f := range cluster.Finalizers {
			if f == verdaClusterFinalizer {
				t.Error("finalizer was not removed")
			}
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("Get after delete: %v", err)
	}
}
