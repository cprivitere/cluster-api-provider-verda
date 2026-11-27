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

	verdasdk "github.com/verda-cloud/verdacloud-sdk-go/pkg/verda"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1alpha1 "github.com/cprivitere/cluster-api-provider-verda/api/v1alpha1"
	"github.com/cprivitere/cluster-api-provider-verda/pkg/cloud"
)

func reconcileTestCluster() *infrastructurev1alpha1.VerdaCluster {
	return &infrastructurev1alpha1.VerdaCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
			Labels:    map[string]string{clusterNameLabel: "test-cluster"},
		},
		Spec: infrastructurev1alpha1.VerdaClusterSpec{Location: "FIN-01"},
	}
}

func clusterTestReconciler(t *testing.T, cloudClient cloud.Client, objs ...client.Object) *VerdaClusterReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infrastructurev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&infrastructurev1alpha1.VerdaCluster{}).
		Build()
	return &VerdaClusterReconciler{Client: c, Scheme: scheme, Cloud: cloudClient}
}

// seedInstance creates one instance in the fake cloud with this
// cluster's identity tags, as the machine controller would have.
func seedInstance(t *testing.T, ctx context.Context, cloudClient *cloud.FakeClient, role, ip string) *verdasdk.Instance {
	t.Helper()
	inst, err := cloudClient.CreateInstance(ctx, verdasdk.CreateInstanceRequest{
		InstanceType: "CPU.4V.16G",
		Image:        "26.04.cuda13.4",
		LocationCode: "FIN-01",
		Tags:         cloud.MachineTags("default/test-cluster", "default/test-machine", role),
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if ip != "" {
		if err := cloudClient.SetInstanceIP(inst.ID, ip); err != nil {
			t.Fatalf("SetInstanceIP: %v", err)
		}
	}
	return inst
}

func getCluster(t *testing.T, ctx context.Context, r *VerdaClusterReconciler) *infrastructurev1alpha1.VerdaCluster {
	t.Helper()
	cluster := &infrastructurev1alpha1.VerdaCluster{}
	if err := r.Get(ctx, types.NamespacedName{Name: "test-cluster", Namespace: "default"}, cluster); err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	return cluster
}

func TestClusterReconcilePublishesEndpoint(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	seedInstance(t, ctx, cloudClient, cloud.RoleControlPlane, "203.0.113.10")
	r := clusterTestReconciler(t, cloudClient, reconcileTestCluster())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-cluster", Namespace: "default"}}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 once the endpoint is published", result.RequeueAfter)
	}
	cluster := getCluster(t, ctx, r)
	if cluster.Spec.ControlPlaneEndpoint.Host != "203.0.113.10" || cluster.Spec.ControlPlaneEndpoint.Port != controlPlanePort {
		t.Errorf("endpoint = %+v, want 203.0.113.10:%d", cluster.Spec.ControlPlaneEndpoint, controlPlanePort)
	}
	if cluster.Status.Initialization.Provisioned == nil || !*cluster.Status.Initialization.Provisioned {
		t.Error("status.initialization.provisioned is not true (the v1beta2 contract signal)")
	}

	// A second pass is a no-op: same endpoint, still provisioned.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile (second pass): %v", err)
	}
	cluster = getCluster(t, ctx, r)
	if cluster.Spec.ControlPlaneEndpoint.Host != "203.0.113.10" {
		t.Errorf("endpoint after second pass = %+v, want unchanged", cluster.Spec.ControlPlaneEndpoint)
	}
}

func TestClusterReconcileWaitsForControlPlane(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := clusterTestReconciler(t, cloudClient, reconcileTestCluster())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-cluster", Namespace: "default"}}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("RequeueAfter = %v, want 15s while no control-plane instance exists", result.RequeueAfter)
	}
	cluster := getCluster(t, ctx, r)
	if !cluster.Spec.ControlPlaneEndpoint.IsZero() {
		t.Errorf("endpoint = %+v, want zero with no instances", cluster.Spec.ControlPlaneEndpoint)
	}
	if cluster.Status.Initialization.Provisioned != nil && *cluster.Status.Initialization.Provisioned {
		t.Error("provisioned is true with no control-plane instance")
	}
}

func TestClusterReconcileIgnoresWorkers(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	seedInstance(t, ctx, cloudClient, cloud.RoleWorker, "203.0.113.20")
	r := clusterTestReconciler(t, cloudClient, reconcileTestCluster())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-cluster", Namespace: "default"}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	cluster := getCluster(t, ctx, r)
	if !cluster.Spec.ControlPlaneEndpoint.IsZero() {
		t.Errorf("endpoint = %+v, want zero when only a worker exists", cluster.Spec.ControlPlaneEndpoint)
	}
}

func TestClusterReconcileWaitsForRunningInstance(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	inst := seedInstance(t, ctx, cloudClient, cloud.RoleControlPlane, "203.0.113.10")
	if err := cloudClient.SetInstanceStatus(inst.ID, "provisioning"); err != nil {
		t.Fatalf("SetInstanceStatus: %v", err)
	}
	r := clusterTestReconciler(t, cloudClient, reconcileTestCluster())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-cluster", Namespace: "default"}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	cluster := getCluster(t, ctx, r)
	if !cluster.Spec.ControlPlaneEndpoint.IsZero() {
		t.Errorf("endpoint = %+v, want zero while the control-plane instance is not running", cluster.Spec.ControlPlaneEndpoint)
	}
}
