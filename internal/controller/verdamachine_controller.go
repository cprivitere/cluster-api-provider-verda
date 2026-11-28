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
	"errors"
	"fmt"
	"strings"
	"time"

	verdasdk "github.com/verda-cloud/verdacloud-sdk-go/pkg/verda"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1alpha1 "github.com/cprivitere/cluster-api-provider-verda/api/v1alpha1"
	"github.com/cprivitere/cluster-api-provider-verda/pkg/bootstrap"
	"github.com/cprivitere/cluster-api-provider-verda/pkg/cloud"
)

// verdaMachineFinalizer guards the Verda instance: the VerdaMachine
// cannot disappear until its instance has been deleted.
const verdaMachineFinalizer = "infrastructure.cluster.x-k8s.io/verdamachine"

// Label keys the reconciler reads for identity tags. Both are the
// standard Cluster API labels; a standalone VerdaMachine can set them
// directly, and once core Cluster API objects own the VerdaMachine
// they arrive the same way.
const (
	clusterNameLabel  = "cluster.x-k8s.io/cluster-name"
	controlPlaneLabel = "cluster.x-k8s.io/control-plane"

	// instanceStatusDiscontinued is the Verda status of a spot
	// instance that has been discontinued.
	instanceStatusDiscontinued = "discontinued"
)

// VerdaMachineReconciler reconciles a VerdaMachine object: one object,
// one Verda instance, kept in sync through pkg/cloud.
type VerdaMachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Cloud is the Verda Cloud client. It is injected by main; tests
	// substitute cloud.NewFakeClient().
	Cloud cloud.Client

	// ClusterCache provides clients for workload clusters, used to
	// finish node initialization there (see reconcileNode). Nil
	// disables the node step — standalone use has no workload
	// cluster to talk to.
	ClusterCache clustercache.ClusterCache
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdamachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdamachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdamachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdaclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *VerdaMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if r.Cloud == nil {
		return ctrl.Result{}, errors.New("cloud client is not configured")
	}

	machine := &infrastructurev1alpha1.VerdaMachine{}
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !machine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, machine)
	}

	if controllerutil.AddFinalizer(machine, verdaMachineFinalizer) {
		if err := r.Update(ctx, machine); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	inst, err := r.findOrCreateInstance(ctx, machine)
	if err != nil {
		if errors.Is(err, errBootstrapNotReady) {
			// The owning Machine exists but CABPK has not
			// delivered its bootstrap data yet. Per the contract
			// the Machine stays Pending; creating the instance
			// now would strand a node that can never join.
			log.Info("Waiting for bootstrap data before creating the instance")
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Reflect the instance onto the status. The status is written
	// with a patch, not an update: a patch carries no resourceVersion
	// precondition, so it cannot lose the optimistic-concurrency race
	// when watch events fire reconciles in quick succession against a
	// lagging cache (observed live Nov 26, 2026: "the object has been
	// modified" from Status().Update).
	base := machine.DeepCopy()
	wasReady := machine.Status.Ready
	machine.Status.Addresses = addressesOf(inst)
	machine.Status.Initialization.Provisioned = new(true)
	machine.Status.Interruptible = new(machine.Spec.IsSpot)
	switch inst.Status {
	case "running":
		machine.Status.Ready = true
		machine.Status.FailureMessage = nil
		meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:    infrastructurev1alpha1.ReadyCondition,
			Status:  metav1.ConditionTrue,
			Reason:  infrastructurev1alpha1.InstanceRunningReason,
			Message: "Instance is running",
		})
	case instanceStatusDiscontinued:
		// A discontinued spot instance still exists as an object, so
		// it never falls into the recreate path below: surface it and
		// let a human decide whether the machine should be replaced.
		// The Ready condition is the contract-visible signal (core
		// mirrors it into the Machine's InfrastructureReady
		// condition); failureMessage is its human-readable companion.
		machine.Status.Ready = false
		msg := fmt.Sprintf("instance %s was discontinued", inst.ID)
		machine.Status.FailureMessage = &msg
		meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:    infrastructurev1alpha1.ReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  infrastructurev1alpha1.InstanceDiscontinuedReason,
			Message: msg,
		})
	default:
		machine.Status.Ready = false
		meta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:    infrastructurev1alpha1.ReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  infrastructurev1alpha1.InstanceProvisioningReason,
			Message: fmt.Sprintf("Instance is %s", inst.Status),
		})
	}
	if err := r.Status().Patch(ctx, machine, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	if machine.Status.Ready && !wasReady {
		// Name the success transition: without this line the log
		// goes quiet at exactly the moment the machine comes up,
		// which reads like a stall (observed live Nov 26, 2026).
		log.Info("Instance is ready", "instanceID", inst.ID)
	}

	if !machine.Status.Ready && machine.Status.FailureMessage == nil {
		// Nothing watches the cloud side, so poll while the instance
		// is still coming up.
		log.Info("Instance is not ready yet", "instanceID", inst.ID, "status", inst.Status)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if machine.Status.Ready && r.ClusterCache != nil {
		done, err := r.reconcileNode(ctx, machine, inst)
		if err != nil {
			// The workload cluster may simply not be reachable
			// yet — its API server is still coming up while KCP
			// initializes the control plane. That is a wait, not
			// a machine failure: log it and poll.
			log.Info("Node step is waiting on the workload cluster", "error", err.Error())
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if !done {
			// The node has not registered yet; kubeadm is still
			// joining. Poll until it appears.
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}
	if machine.Status.Ready && machine.Spec.IsSpot {
		// A spot instance can be discontinued underneath a Ready
		// machine, and nothing watches the cloud side — poll slowly
		// so a discontinuation surfaces (the Ready condition going
		// False) instead of going unnoticed indefinitely.
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	return ctrl.Result{}, nil
}

// findOrCreateInstance returns the instance backing this machine,
// creating it if none exists yet. Creation has no idempotency key in
// the Verda API, so the lookup order is: the recorded providerID,
// then the identity tags (covers a crash between create and recording
// the providerID), and only then a create.
func (r *VerdaMachineReconciler) findOrCreateInstance(ctx context.Context, machine *infrastructurev1alpha1.VerdaMachine) (*verdasdk.Instance, error) {
	log := logf.FromContext(ctx)

	if machine.Spec.ProviderID != "" {
		id, ok := instanceIDFromProviderID(machine.Spec.ProviderID)
		if !ok {
			return nil, fmt.Errorf("providerID %q is not a verda:// provider ID", machine.Spec.ProviderID)
		}
		inst, err := r.Cloud.GetInstance(ctx, id)
		if err == nil {
			return inst, nil
		}
		if !errors.Is(err, cloud.ErrNotFound) {
			return nil, err
		}
		log.Info("Recorded instance is gone, creating a new one", "instanceID", id)
	} else {
		found, err := r.Cloud.FindInstancesByTags(ctx, map[string]string{
			cloud.TagMachineName: machine.Namespace + "/" + machine.Name,
		})
		if err != nil {
			return nil, err
		}
		if len(found) > 0 {
			inst := found[0]
			log.Info("Adopting existing instance found by tags", "instanceID", inst.ID)
			if err := r.recordProviderID(ctx, machine, &inst); err != nil {
				return nil, err
			}
			return &inst, nil
		}
	}

	script, err := r.bootstrapScript(ctx, machine)
	if err != nil {
		return nil, err
	}

	createReq := verdasdk.CreateInstanceRequest{
		InstanceType:  machine.Spec.InstanceType,
		Image:         machine.Spec.Image,
		LocationCode:  machine.Spec.Location,
		SSHKeyIDs:     machine.Spec.SSHKeyIDs,
		IsSpot:        machine.Spec.IsSpot,
		Hostname:      machine.Name,
		StartupScript: script,
		Tags:          cloud.MachineTags(clusterNameFor(machine), machine.Namespace+"/"+machine.Name, roleFor(machine)),
	}

	// Private-network placement follows the attachment rule (see
	// networkAttachment): control planes always join their cluster's
	// network at the pinned private address the endpoint proxy
	// forwards to; workers join only a network their own spec names,
	// with an automatic address. The API requires the private and
	// public allocations to be set together, so the public side asks
	// for automatic alongside. An unattached machine keeps the
	// platform default placement and reaches the API server through
	// the cluster's public endpoint.
	networkID, subnetID, err := r.networkAttachment(ctx, machine)
	if err != nil {
		return nil, err
	}
	if networkID != "" {
		private := verdasdk.IPAllocAuto
		if roleFor(machine) == cloud.RoleControlPlane {
			pinned, err := verdasdk.IPAllocIPv4(cloud.ControlPlanePrivateIP)
			if err != nil {
				return nil, fmt.Errorf("control plane private address: %w", err)
			}
			private = pinned
		}
		public := verdasdk.IPAllocAuto
		createReq.Network = networkID
		createReq.Subnet = subnetID
		createReq.PrivateIP = &private
		createReq.PublicIP = &public
	}

	inst, err := r.Cloud.CreateInstance(ctx, createReq)
	if err != nil {
		return nil, fmt.Errorf("creating instance: %w", err)
	}
	log.Info("Created instance", "instanceID", inst.ID)
	if err := r.recordProviderID(ctx, machine, inst); err != nil {
		return nil, err
	}
	return inst, nil
}

// errBootstrapNotReady marks the create path's wait state: an owner
// Machine exists but its bootstrap data is not available yet.
var errBootstrapNotReady = errors.New("bootstrap data is not ready yet")

// bootstrapScript resolves the instance's startup script from the
// Cluster API bootstrap data of the owning Machine: the Machine's
// spec.bootstrap.dataSecretName names a Secret whose "value" key
// holds CABPK's cloud-config, which pkg/bootstrap translates into
// the bash script Verda's startup mechanism runs.
//
// A VerdaMachine with no owner Machine (standalone use) gets no
// script — bootstrap delivery is a composition feature. An owner
// whose data is not ready yet yields errBootstrapNotReady: per the
// contract the Machine stays Pending until CABPK delivers, and
// creating the instance before that would strand a node that can
// never join.
func (r *VerdaMachineReconciler) bootstrapScript(ctx context.Context, machine *infrastructurev1alpha1.VerdaMachine) (string, error) {
	owner, err := util.GetOwnerMachine(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		return "", fmt.Errorf("getting owner Machine: %w", err)
	}
	if owner == nil {
		return "", nil
	}
	if owner.Spec.Bootstrap.DataSecretName == nil {
		return "", errBootstrapNotReady
	}
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: machine.Namespace, Name: *owner.Spec.Bootstrap.DataSecretName}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", errBootstrapNotReady
		}
		return "", fmt.Errorf("getting bootstrap secret: %w", err)
	}
	if format := string(secret.Data["format"]); format != "" && format != "cloud-config" {
		return "", fmt.Errorf("unsupported bootstrap format %q in secret %s", format, secretKey)
	}
	data, ok := secret.Data["value"]
	if !ok {
		return "", fmt.Errorf("bootstrap secret %s has no \"value\" key", secretKey)
	}
	script, err := bootstrap.Translate(data)
	if err != nil {
		return "", fmt.Errorf("translating bootstrap data: %w", err)
	}
	return script, nil
}

// uninitializedTaintKey is the taint kubelet applies to a node that
// registered with an external cloud provider before any cloud
// controller has initialized it (k8s.io/cloud-provider's
// TaintExternalCloudProvider). With no Verda CCM, this provider
// clears it — the same job the reference Docker provider does.
const uninitializedTaintKey = "node.cloudprovider.kubernetes.io/uninitialized"

// reconcileNode finishes node initialization in the workload
// cluster (decision 13): once the instance's node has registered,
// set the node's providerID to this machine's and clear the
// uninitialized taint, so core Cluster API can correlate the
// Machine with its Node. The node is matched by system UUID
// first and by address second — see findNodeForInstance.
//
// done=false means "not yet": the node has not registered. Errors
// are the caller's to soften — a workload cluster that isn't up
// yet is a wait, not a machine failure.
func (r *VerdaMachineReconciler) reconcileNode(ctx context.Context, machine *infrastructurev1alpha1.VerdaMachine, inst *verdasdk.Instance) (bool, error) {
	clusterName := machine.Labels[clusterv1.ClusterNameLabel]
	if clusterName == "" {
		// Standalone machine: no workload cluster to patch.
		return true, nil
	}
	workloadClient, err := r.ClusterCache.GetClient(ctx, client.ObjectKey{Namespace: machine.Namespace, Name: clusterName})
	if err != nil {
		return false, fmt.Errorf("getting workload cluster client: %w", err)
	}
	nodes := &corev1.NodeList{}
	if err := workloadClient.List(ctx, nodes); err != nil {
		return false, fmt.Errorf("listing workload cluster nodes: %w", err)
	}
	node := findNodeForInstance(nodes, inst)
	if node == nil {
		return false, nil
	}
	if node.Spec.ProviderID == machine.Spec.ProviderID && !hasTaint(node, uninitializedTaintKey) {
		return true, nil
	}
	base := node.DeepCopy()
	node.Spec.ProviderID = machine.Spec.ProviderID
	node.Spec.Taints = removeTaint(node.Spec.Taints, uninitializedTaintKey)
	if err := workloadClient.Patch(ctx, node, client.MergeFrom(base)); err != nil {
		return false, fmt.Errorf("patching node %s: %w", node.Name, err)
	}
	logf.FromContext(ctx).Info("Set providerID on node", "node", node.Name, "providerID", machine.Spec.ProviderID)
	return true, nil
}

// findNodeForInstance returns the node that belongs to the
// instance, or nil.
//
// The primary key is the node's system UUID: Verda sets the
// guest's SMBIOS/DMI UUID to the instance ID (verified live
// 2026-11-27 — /sys/class/dmi/id/product_uuid inside a guest
// prints exactly the instance ID), and kubelet reports that UUID
// as status.nodeInfo.systemUUID. Matching on it ties the node to
// the instance's identity rather than to an address that could
// later be reused by a different instance.
//
// The fallback is address matching — the node's internal or
// external IP among the instance's public/private IPs — for
// nodes whose systemUUID is empty or unreported.
func findNodeForInstance(nodes *corev1.NodeList, inst *verdasdk.Instance) *corev1.Node {
	if inst.ID != "" {
		for i := range nodes.Items {
			if strings.EqualFold(nodes.Items[i].Status.NodeInfo.SystemUUID, inst.ID) {
				return &nodes.Items[i]
			}
		}
	}
	ips := make(map[string]bool)
	if inst.IP != nil {
		ips[*inst.IP] = true
	}
	if inst.PrivateIP != nil {
		ips[*inst.PrivateIP] = true
	}
	if len(ips) == 0 {
		return nil
	}
	for i := range nodes.Items {
		for _, addr := range nodes.Items[i].Status.Addresses {
			if (addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP) && ips[addr.Address] {
				return &nodes.Items[i]
			}
		}
	}
	return nil
}

func hasTaint(node *corev1.Node, key string) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == key {
			return true
		}
	}
	return false
}

func removeTaint(taints []corev1.Taint, key string) []corev1.Taint {
	kept := make([]corev1.Taint, 0, len(taints))
	for _, taint := range taints {
		if taint.Key != key {
			kept = append(kept, taint)
		}
	}
	return kept
}

func (r *VerdaMachineReconciler) reconcileDelete(ctx context.Context, machine *infrastructurev1alpha1.VerdaMachine) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(machine, verdaMachineFinalizer) {
		return ctrl.Result{}, nil
	}

	inst, err := r.findInstance(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if inst != nil {
		log.Info("Deleting instance", "instanceID", inst.ID)
		if err := r.Cloud.DeleteInstanceWithVolumes(ctx, inst.ID); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting instance: %w", err)
		}
	}

	controllerutil.RemoveFinalizer(machine, verdaMachineFinalizer)
	if err := r.Update(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// findInstance locates the backing instance without creating one, for
// the delete path. It returns nil when there is nothing to delete.
func (r *VerdaMachineReconciler) findInstance(ctx context.Context, machine *infrastructurev1alpha1.VerdaMachine) (*verdasdk.Instance, error) {
	if machine.Spec.ProviderID != "" {
		id, ok := instanceIDFromProviderID(machine.Spec.ProviderID)
		if !ok {
			return nil, fmt.Errorf("providerID %q is not a verda:// provider ID", machine.Spec.ProviderID)
		}
		inst, err := r.Cloud.GetInstance(ctx, id)
		if errors.Is(err, cloud.ErrNotFound) {
			return nil, nil
		}
		return inst, err
	}
	found, err := r.Cloud.FindInstancesByTags(ctx, map[string]string{
		cloud.TagMachineName: machine.Namespace + "/" + machine.Name,
	})
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}
	return &found[0], nil
}

func (r *VerdaMachineReconciler) recordProviderID(ctx context.Context, machine *infrastructurev1alpha1.VerdaMachine, inst *verdasdk.Instance) error {
	pid := providerIDFor(inst)
	if machine.Spec.ProviderID == pid {
		return nil
	}
	base := machine.DeepCopy()
	machine.Spec.ProviderID = pid
	if err := r.Patch(ctx, machine, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("recording providerID: %w", err)
	}
	return nil
}

// providerIDFor renders the Cluster API provider ID for an instance:
// verda://<location>/<instance-id>.
func providerIDFor(inst *verdasdk.Instance) string {
	return fmt.Sprintf("verda://%s/%s", inst.Location, inst.ID)
}

// instanceIDFromProviderID extracts the instance ID from a
// verda://<location>/<instance-id> provider ID.
func instanceIDFromProviderID(providerID string) (string, bool) {
	rest, ok := strings.CutPrefix(providerID, "verda://")
	if !ok {
		return "", false
	}
	if i := strings.LastIndex(rest, "/"); i >= 0 {
		rest = rest[i+1:]
	}
	return rest, rest != ""
}

// clusterNameFor identifies the owning cluster for the identity tags:
// the cluster-name label when present, the object itself otherwise
// (standalone smoke tests).
func clusterNameFor(machine *infrastructurev1alpha1.VerdaMachine) string {
	if name := machine.Labels[clusterNameLabel]; name != "" {
		return machine.Namespace + "/" + name
	}
	return machine.Namespace + "/" + machine.Name
}

// networkAttachment decides the private-network placement of a
// machine's instance, per the attachment rule (Chris's design, Nov
// 27, 2026): control planes MUST join their cluster's network — the
// endpoint proxy forwards to the control plane's pinned private
// address, so a control plane off that network is a broken cluster.
// Workers join a network only when their own spec explicitly names
// one (spec.networkID/spec.subnetID); a worker that names none is
// created unattached and reaches the API server through the
// cluster's public endpoint — the flexible-join path, e.g. a GPU
// worker in another location, whose subnet cannot be the cluster's.
// (Production posture is the reverse default for workers — attach
// everything and keep traffic private; the public join exists for
// placement flexibility in this demo.)
func (r *VerdaMachineReconciler) networkAttachment(ctx context.Context, machine *infrastructurev1alpha1.VerdaMachine) (string, string, error) {
	if roleFor(machine) == cloud.RoleControlPlane {
		return r.clusterAttachment(ctx, machine)
	}
	if (machine.Spec.NetworkID == "") != (machine.Spec.SubnetID == "") {
		return "", "", fmt.Errorf("spec.networkID and spec.subnetID must be set together")
	}
	return machine.Spec.NetworkID, machine.Spec.SubnetID, nil
}

// clusterAttachment returns the private network and subnet IDs of
// the machine's cluster, recorded in the VerdaCluster status by the
// cluster controller. It is the control-plane path of
// networkAttachment. A standalone machine (no cluster-name label,
// no Cluster, or no infrastructure reference) gets empty strings
// and the platform default placement. A cluster whose network is
// not recorded yet is an error rather than an off-network create
// the instance would be stranded on, so the reconcile retries until
// the cluster controller has provisioned the network.
func (r *VerdaMachineReconciler) clusterAttachment(ctx context.Context, machine *infrastructurev1alpha1.VerdaMachine) (string, string, error) {
	clusterName := machine.Labels[clusterNameLabel]
	if clusterName == "" {
		return "", "", nil
	}
	cluster := &clusterv1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: clusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("reading cluster: %w", err)
	}
	if cluster.Spec.InfrastructureRef.Name == "" {
		return "", "", nil
	}
	infraCluster := &infrastructurev1alpha1.VerdaCluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: cluster.Spec.InfrastructureRef.Name}, infraCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("reading infrastructure cluster: %w", err)
	}
	if infraCluster.Status.NetworkID == "" || infraCluster.Status.SubnetID == "" {
		return "", "", fmt.Errorf("cluster %s network is not provisioned yet", clusterName)
	}
	return infraCluster.Status.NetworkID, infraCluster.Status.SubnetID, nil
}

// roleFor reads the control-plane label the way Cluster API marks
// control plane machines; anything else is a worker.
func roleFor(machine *infrastructurev1alpha1.VerdaMachine) string {
	if _, ok := machine.Labels[controlPlaneLabel]; ok {
		return cloud.RoleControlPlane
	}
	return cloud.RoleWorker
}

func addressesOf(inst *verdasdk.Instance) []clusterv1.MachineAddress {
	var out []clusterv1.MachineAddress
	if inst.IP != nil && *inst.IP != "" {
		out = append(out, clusterv1.MachineAddress{Type: clusterv1.MachineExternalIP, Address: *inst.IP})
	}
	if inst.PrivateIP != nil && *inst.PrivateIP != "" {
		out = append(out, clusterv1.MachineAddress{Type: clusterv1.MachineInternalIP, Address: *inst.PrivateIP})
	}
	if inst.Hostname != "" {
		out = append(out, clusterv1.MachineAddress{Type: clusterv1.MachineHostName, Address: inst.Hostname})
	}
	return out
}

// SetupWithManager sets up the controller with the Manager.
func (r *VerdaMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha1.VerdaMachine{}).
		Named("verdamachine").
		Complete(r)
}
