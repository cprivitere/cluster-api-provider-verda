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
	"time"

	verdasdk "github.com/verda-cloud/verdacloud-sdk-go/pkg/verda"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	controlplanev1beta2 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1alpha1 "github.com/cprivitere/cluster-api-provider-verda/api/v1alpha1"
	"github.com/cprivitere/cluster-api-provider-verda/pkg/cloud"
)

// controlPlanePort is the port the workload cluster's API server
// serves on — kubeadm's default, and the port KCP advertises.
const controlPlanePort = 6443

// verdaClusterFinalizer guards the cloud resources this controller
// creates (private network, subnet, endpoint proxy): the
// VerdaCluster cannot disappear until they have been deleted.
const verdaClusterFinalizer = "infrastructure.cluster.x-k8s.io/verdacluster"

// The endpoint proxy's shape. It only forwards TCP, so the smallest
// instance family the private-network location offers is plenty;
// it runs the nodes' image so the platform has one image to reason
// about.
const (
	proxyInstanceType = "CPU-TURIN.4V.16G"
	proxyImage        = "26.04.cuda13.4"
)

// proxyStartupScript installs HAProxy as a TCP passthrough for the
// API server: listen on 6443, forward to the control-plane
// instance's private address (the %s). The address is a provider
// constant (see pkg/cloud), so the configuration is complete before
// the control plane exists. Verda runs the script as root at the
// end of cloud-init.
const proxyStartupScript = `#!/bin/bash
# CAPVE endpoint proxy: HAProxy in TCP passthrough mode, forwarding
# 6443 to the control-plane instance's private address.
apt-get update
apt-get install -y haproxy
cat > /etc/haproxy/haproxy.cfg <<'HAPROXY_EOF'
global
    maxconn 2048

defaults
    mode tcp
    timeout connect 5s
    timeout client 60s
    timeout server 60s

frontend k8s-api
    bind :6443
    default_backend k8s-control-plane

backend k8s-control-plane
    server control-plane %s:6443
HAPROXY_EOF
systemctl enable --now haproxy
`

// VerdaClusterReconciler reconciles a VerdaCluster object.
//
// The Cluster API contract requires a valid control-plane endpoint
// on the Cluster before KCP creates any Machine and before CABPK
// generates any bootstrap data. Verda has no load balancer and no
// reservable public addresses, so the endpoint cannot come from
// the control-plane instance itself — its address only exists once
// it does. This controller therefore builds the endpoint first: a
// private network and subnet for the cluster's nodes, plus a small
// dedicated proxy instance on that subnet whose public address
// becomes the endpoint. The proxy forwards to the control plane's
// pinned private address (see pkg/cloud), so its configuration is
// static and complete before the control plane exists. Once the
// proxy is running, the controller publishes its address as the
// endpoint and marks initialization provisioned; core copies the
// endpoint onto the Cluster and the ordinary machine flow takes
// over from there.
type VerdaClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Cloud is the Verda Cloud client. It is injected by main; tests
	// substitute cloud.NewFakeClient().
	Cloud cloud.Client
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdaclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdaclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdaclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdamachinetemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kubeadmcontrolplanes,verbs=get;list;watch

func (r *VerdaClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if r.Cloud == nil {
		return ctrl.Result{}, errors.New("cloud client is not configured")
	}

	cluster := &infrastructurev1alpha1.VerdaCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster)
	}

	if !controllerutil.ContainsFinalizer(cluster, verdaClusterFinalizer) {
		base := cluster.DeepCopy()
		controllerutil.AddFinalizer(cluster, verdaClusterFinalizer)
		if err := r.Patch(ctx, cluster, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	networkID, subnetID, err := r.ensureNetwork(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster.Status.NetworkID != networkID || cluster.Status.SubnetID != subnetID {
		base := cluster.DeepCopy()
		cluster.Status.NetworkID = networkID
		cluster.Status.SubnetID = subnetID
		if err := r.Status().Patch(ctx, cluster, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("recording network in status: %w", err)
		}
		log.Info("Recorded cluster network in status", "networkID", networkID, "subnetID", subnetID)
	}

	proxy, err := r.ensureProxy(ctx, cluster, networkID, subnetID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if proxy.Status != "running" || proxy.IP == nil || *proxy.IP == "" {
		// Nothing watches the cloud side, so poll while the proxy
		// is still coming up (the machine controller polls the
		// same way).
		log.Info("Endpoint proxy is not ready yet", "instanceID", proxy.ID, "status", proxy.Status)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	endpoint := clusterv1.APIEndpoint{Host: *proxy.IP, Port: controlPlanePort}
	if cluster.Spec.ControlPlaneEndpoint != endpoint {
		base := cluster.DeepCopy()
		cluster.Spec.ControlPlaneEndpoint = endpoint
		if err := r.Patch(ctx, cluster, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("writing control plane endpoint: %w", err)
		}
		log.Info("Wrote control plane endpoint", "host", endpoint.Host, "port", endpoint.Port, "proxyInstanceID", proxy.ID)
	}

	if !ptr.Deref(cluster.Status.Initialization.Provisioned, false) {
		base := cluster.DeepCopy()
		cluster.Status.Initialization.Provisioned = new(true)
		if err := r.Status().Patch(ctx, cluster, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
		}
		// Name the success transition, as the machine controller
		// does: silence at exactly this moment reads like a stall.
		log.Info("Cluster infrastructure is provisioned", "proxyInstanceID", proxy.ID)
	}
	return ctrl.Result{}, nil
}

// reconcileDelete removes what the controller created — the proxy
// instance first (it holds an address on the subnet), then the
// subnet and the network — and releases the finalizer. Lookups are
// by name and tag rather than by status, so cleanup still works if
// a status write was lost.
func (r *VerdaClusterReconciler) reconcileDelete(ctx context.Context, cluster *infrastructurev1alpha1.VerdaCluster) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(cluster, verdaClusterFinalizer) {
		return ctrl.Result{}, nil
	}

	proxies, err := r.Cloud.FindInstancesByTags(ctx, map[string]string{
		cloud.TagClusterName: clusterKeyFor(cluster),
		cloud.TagRole:        cloud.RoleEndpointProxy,
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range proxies {
		if err := r.Cloud.DeleteInstanceWithVolumes(ctx, proxies[i].ID); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting endpoint proxy: %w", err)
		}
		log.Info("Deleted endpoint proxy", "instanceID", proxies[i].ID)
	}

	network, err := r.findNetwork(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if network != nil {
		subnets, err := r.Cloud.ListSubnets(ctx, network.ID)
		if err != nil {
			return ctrl.Result{}, err
		}
		for i := range subnets {
			if err := r.Cloud.DeleteSubnet(ctx, network.ID, subnets[i].ID); err != nil && !errors.Is(err, cloud.ErrNotFound) {
				return ctrl.Result{}, fmt.Errorf("deleting subnet: %w", err)
			}
		}
		if err := r.Cloud.DeletePrivateNetwork(ctx, network.ID); err != nil && !errors.Is(err, cloud.ErrNotFound) {
			return ctrl.Result{}, fmt.Errorf("deleting private network: %w", err)
		}
		log.Info("Deleted cluster network", "networkID", network.ID)
	}

	base := cluster.DeepCopy()
	controllerutil.RemoveFinalizer(cluster, verdaClusterFinalizer)
	if err := r.Patch(ctx, cluster, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// ensureNetwork finds or creates the cluster's private network and
// its node subnet in the cluster's location, and returns their IDs.
func (r *VerdaClusterReconciler) ensureNetwork(ctx context.Context, cluster *infrastructurev1alpha1.VerdaCluster) (string, string, error) {
	log := logf.FromContext(ctx)

	network, err := r.findNetwork(ctx, cluster)
	if err != nil {
		return "", "", err
	}
	if network == nil {
		network, err = r.Cloud.CreatePrivateNetwork(ctx, verdasdk.CreatePrivateNetworkRequest{
			Name: networkNameFor(cluster),
			Mode: verdasdk.NetworkModeCustom,
		})
		if err != nil {
			return "", "", fmt.Errorf("creating private network: %w", err)
		}
		log.Info("Created private network", "networkID", network.ID, "name", network.Name)
	}

	subnets, err := r.Cloud.ListSubnets(ctx, network.ID)
	if err != nil {
		return "", "", err
	}
	for i := range subnets {
		if subnets[i].Location.Code == cluster.Spec.Location && subnets[i].CIDR == cloud.SubnetCIDR {
			return network.ID, subnets[i].ID, nil
		}
	}
	subnet, err := r.Cloud.CreateSubnet(ctx, network.ID, verdasdk.CreateSubnetRequest{
		LocationCode: cluster.Spec.Location,
		Name:         "nodes",
		CIDR:         cloud.SubnetCIDR,
	})
	if err != nil {
		return "", "", fmt.Errorf("creating subnet: %w", err)
	}
	log.Info("Created subnet", "networkID", network.ID, "subnetID", subnet.ID, "cidr", subnet.CIDR)
	return network.ID, subnet.ID, nil
}

// findNetwork returns the cluster's private network, identified by
// its deterministic name, or nil when it does not exist yet.
func (r *VerdaClusterReconciler) findNetwork(ctx context.Context, cluster *infrastructurev1alpha1.VerdaCluster) (*verdasdk.PrivateNetwork, error) {
	networks, err := r.Cloud.ListPrivateNetworks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range networks {
		if networks[i].Name == networkNameFor(cluster) {
			return &networks[i], nil
		}
	}
	return nil, nil
}

// ensureProxy finds or creates the cluster's endpoint proxy
// instance on the cluster's subnet.
func (r *VerdaClusterReconciler) ensureProxy(ctx context.Context, cluster *infrastructurev1alpha1.VerdaCluster, networkID, subnetID string) (*verdasdk.Instance, error) {
	log := logf.FromContext(ctx)

	found, err := r.Cloud.FindInstancesByTags(ctx, map[string]string{
		cloud.TagClusterName: clusterKeyFor(cluster),
		cloud.TagRole:        cloud.RoleEndpointProxy,
	})
	if err != nil {
		return nil, err
	}
	if len(found) > 0 {
		return &found[0], nil
	}

	private, err := verdasdk.IPAllocIPv4(cloud.ProxyPrivateIP)
	if err != nil {
		return nil, fmt.Errorf("proxy private address: %w", err)
	}
	sshKeyIDs, err := r.controlPlaneSSHKeyIDs(ctx, cluster)
	if err != nil {
		return nil, err
	}

	public := verdasdk.IPAllocAuto
	log.Info("Creating endpoint proxy", "instanceType", proxyInstanceType,
		"networkID", networkID, "subnetID", subnetID, "privateIP", cloud.ProxyPrivateIP,
		"sshKeyCount", len(sshKeyIDs))
	inst, err := r.Cloud.CreateInstance(ctx, verdasdk.CreateInstanceRequest{
		InstanceType:  proxyInstanceType,
		Image:         proxyImage,
		LocationCode:  cluster.Spec.Location,
		Hostname:      cluster.Name + "-proxy",
		SSHKeyIDs:     sshKeyIDs,
		Network:       networkID,
		Subnet:        subnetID,
		PrivateIP:     &private,
		PublicIP:      &public,
		StartupScript: fmt.Sprintf(proxyStartupScript, cloud.ControlPlanePrivateIP),
		Tags:          cloud.MachineTags(clusterKeyFor(cluster), clusterKeyFor(cluster), cloud.RoleEndpointProxy),
	})
	if err != nil {
		return nil, fmt.Errorf("creating endpoint proxy: %w", err)
	}
	log.Info("Created endpoint proxy", "instanceID", inst.ID)
	return inst, nil
}

// controlPlaneSSHKeyIDs returns the SSH keys of the cluster's
// control-plane machine template, so the proxy — the one instance
// an operator may need to log into when the API is unreachable —
// accepts the same keys as the control plane it fronts.
//
// The lookup is deliberately loud, not best-effort: the Verda API
// rejects instance creates that carry no ssh_key_ids (a 400 whose
// message claims the field may be omitted), so a silent nil here
// once sent the proxy create to the API keyless and produced a
// baffling "malformed SSH keys" error (Nov 27, 2026 — compounded
// by cmd/main.go not registering the KubeadmControlPlane scheme,
// which made every read fail). Any missing link is an error and
// the reconcile retries until the chain exists.
func (r *VerdaClusterReconciler) controlPlaneSSHKeyIDs(ctx context.Context, cluster *infrastructurev1alpha1.VerdaCluster) ([]string, error) {
	clusterName := cluster.Labels[clusterNameLabel]
	if clusterName == "" {
		clusterName = cluster.Name
	}
	owner := &clusterv1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: clusterName}, owner); err != nil {
		return nil, fmt.Errorf("reading owner Cluster for SSH keys: %w", err)
	}
	if owner.Spec.ControlPlaneRef.Name == "" {
		return nil, fmt.Errorf("owner Cluster %s has no control plane ref yet", clusterName)
	}
	controlPlane := &controlplanev1beta2.KubeadmControlPlane{}
	key := types.NamespacedName{Namespace: cluster.Namespace, Name: owner.Spec.ControlPlaneRef.Name}
	if err := r.Get(ctx, key, controlPlane); err != nil {
		return nil, fmt.Errorf("reading control plane for SSH keys: %w", err)
	}
	templateName := controlPlane.Spec.MachineTemplate.Spec.InfrastructureRef.Name
	if templateName == "" {
		return nil, fmt.Errorf("control plane %s has no machine template yet", owner.Spec.ControlPlaneRef.Name)
	}
	template := &infrastructurev1alpha1.VerdaMachineTemplate{}
	key = types.NamespacedName{Namespace: cluster.Namespace, Name: templateName}
	if err := r.Get(ctx, key, template); err != nil {
		return nil, fmt.Errorf("reading control-plane machine template for SSH keys: %w", err)
	}
	sshKeyIDs := template.Spec.Template.Spec.SSHKeyIDs
	if len(sshKeyIDs) == 0 {
		return nil, fmt.Errorf("control-plane machine template %s carries no SSH keys", templateName)
	}
	return sshKeyIDs, nil
}

// networkNameFor is the cluster's private network name. Networks
// are project-global and carry no tags, so the name is the identity
// find-or-create and cleanup both look up.
func networkNameFor(cluster *infrastructurev1alpha1.VerdaCluster) string {
	return "capve-" + cluster.Namespace + "-" + cluster.Name
}

// clusterKeyFor renders the capve.cluster tag value the machine
// controller stamps on this cluster's instances: the cluster-name
// label when present, the object itself otherwise (standalone use).
// It mirrors clusterNameFor on the machine side.
func clusterKeyFor(cluster *infrastructurev1alpha1.VerdaCluster) string {
	if name := cluster.Labels[clusterNameLabel]; name != "" {
		return cluster.Namespace + "/" + name
	}
	return cluster.Namespace + "/" + cluster.Name
}

// SetupWithManager sets up the controller with the Manager.
func (r *VerdaClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha1.VerdaCluster{}).
		Named("verdacluster").
		Complete(r)
}
