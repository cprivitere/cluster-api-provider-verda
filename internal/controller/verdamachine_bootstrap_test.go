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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// bootstrapTestCloudConfig is a small CABPK-shaped document: the
// translator must turn it into a script carrying the kubeadm line
// and its success sentinel.
const bootstrapTestCloudConfig = `## template: jinja
#cloud-config
write_files:
-   path: /run/kubeadm/kubeadm-join-config.yaml
    owner: root:root
    permissions: '0640'
    content: |
      apiVersion: kubeadm.k8s.io/v1beta4
      kind: JoinConfiguration
runcmd:
- "kubeadm join --config /run/kubeadm/kubeadm-join-config.yaml  && echo success > /run/cluster-api/bootstrap-success.complete"
`

// ownedTestMachine is the reconcileTestMachine plus the owner
// reference core Cluster API sets from Machine.spec.infrastructureRef.
func ownedTestMachine() *infrastructurev1alpha1.VerdaMachine {
	machine := reconcileTestMachine()
	machine.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: clusterv1.GroupVersion.String(),
		Kind:       "Machine",
		Name:       testMachineName,
		UID:        types.UID("owner-uid"),
	}}
	return machine
}

func ownerMachine(dataSecretName *string) *clusterv1.Machine {
	return &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: testMachineName, Namespace: testNamespace},
		Spec: clusterv1.MachineSpec{
			Bootstrap: clusterv1.Bootstrap{DataSecretName: dataSecretName},
		},
	}
}

func bootstrapSecret(format string) *corev1.Secret {
	data := map[string][]byte{"value": []byte(bootstrapTestCloudConfig)}
	if format != "" {
		data["format"] = []byte(format)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testBootstrapName, Namespace: testNamespace},
		Data:       data,
	}
}

func bootstrapTestReconciler(t *testing.T, cloudClient cloud.Client, objs ...client.Object) *VerdaMachineReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infrastructurev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (infra): %v", err)
	}
	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (clusterv1): %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (corev1): %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&infrastructurev1alpha1.VerdaMachine{}).
		Build()
	return &VerdaMachineReconciler{Client: c, Scheme: scheme, Cloud: cloudClient}
}

func instanceCount(t *testing.T, ctx context.Context, cloudClient *cloud.FakeClient) int {
	t.Helper()
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	return len(instances)
}

func TestBootstrapScriptDeliveredOnCreate(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := bootstrapTestReconciler(t, cloudClient,
		ownedTestMachine(), ownerMachine(new(testBootstrapName)), bootstrapSecret("cloud-config"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("cloud has %d instances, want 1", len(instances))
	}
	script := cloudClient.StartupScriptFor(instances[0].ID)
	if !strings.HasPrefix(script, "#!/bin/bash") {
		t.Errorf("delivered script does not start with the bash shebang:\n%.80s", script)
	}
	if !strings.Contains(script, "kubeadm join --config /run/kubeadm/kubeadm-join-config.yaml  && echo success > /run/cluster-api/bootstrap-success.complete") {
		t.Errorf("delivered script lost the kubeadm line or its sentinel chain:\n%s", script)
	}
	if !strings.Contains(script, "chmod '0640' '/run/kubeadm/kubeadm-join-config.yaml'") {
		t.Errorf("delivered script lost the write_files entry:\n%s", script)
	}
}

func TestBootstrapWaitsWhenDataSecretNameNil(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := bootstrapTestReconciler(t, cloudClient, ownedTestMachine(), ownerMachine(nil))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("RequeueAfter = %v, want 15s while CABPK has not delivered", result.RequeueAfter)
	}
	if n := instanceCount(t, ctx, cloudClient); n != 0 {
		t.Errorf("cloud has %d instances, want 0 (no instance before bootstrap data)", n)
	}
}

func TestBootstrapWaitsWhenSecretMissing(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := bootstrapTestReconciler(t, cloudClient, ownedTestMachine(), ownerMachine(new(testBootstrapName)))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("RequeueAfter = %v, want 15s while the secret is missing", result.RequeueAfter)
	}
	if n := instanceCount(t, ctx, cloudClient); n != 0 {
		t.Errorf("cloud has %d instances, want 0", n)
	}
}

func TestBootstrapRejectsUnsupportedFormat(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := bootstrapTestReconciler(t, cloudClient,
		ownedTestMachine(), ownerMachine(new(testBootstrapName)), bootstrapSecret("ignition"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Error("Reconcile succeeded with an ignition bootstrap secret, want an error")
	}
	if n := instanceCount(t, ctx, cloudClient); n != 0 {
		t.Errorf("cloud has %d instances, want 0", n)
	}
}

func TestStandaloneMachineGetsNoScript(t *testing.T) {
	ctx := context.Background()
	cloudClient := cloud.NewFakeClient()
	r := reconcileTestReconciler(t, cloudClient, reconcileTestMachine())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: testMachineName, Namespace: testNamespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	instances, err := cloudClient.ListInstances(ctx, nil)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("cloud has %d instances, want 1", len(instances))
	}
	if script := cloudClient.StartupScriptFor(instances[0].ID); script != "" {
		t.Errorf("standalone machine got a startup script, want none:\n%s", script)
	}
}
