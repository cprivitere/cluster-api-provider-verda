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
	"slices"
	"strings"
	"time"

	verdasdk "github.com/verda-cloud/verdacloud-sdk-go/pkg/verda"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1alpha1 "github.com/cprivitere/cluster-api-provider-verda/api/v1alpha1"
	"github.com/cprivitere/cluster-api-provider-verda/pkg/cloud"
)

// controlPlanePort is the port the workload cluster's API server
// serves on — kubeadm's default, and the port KCP advertises.
const controlPlanePort = 6443

// VerdaClusterReconciler reconciles a VerdaCluster object. The
// cluster object creates no cloud resources of its own — instances
// belong to VerdaMachines — so its whole job is the Cluster API
// contract's cluster-level signal: once a control-plane instance is
// running with a public IP, publish that address as the control
// plane endpoint and mark initialization provisioned. Core copies
// the endpoint onto the Cluster at that moment, and KCP/CABPK
// consume it from there. With no load balancer in the Verda
// instance API, the first control-plane instance's address is the
// endpoint.
type VerdaClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Cloud is the Verda Cloud client. It is injected by main; tests
	// substitute cloud.NewFakeClient().
	Cloud cloud.Client
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdaclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdaclusters/status,verbs=get;update;patch

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
		// This controller creates nothing in the cloud, so there is
		// nothing to clean up — and it never adds a finalizer.
		return ctrl.Result{}, nil
	}

	inst, err := r.findControlPlaneInstance(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if inst == nil {
		// Nothing watches the cloud side, so poll while the control
		// plane is still coming up (the machine controller polls the
		// same way).
		log.Info("No running control-plane instance yet")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	endpoint := clusterv1.APIEndpoint{Host: *inst.IP, Port: controlPlanePort}
	if cluster.Spec.ControlPlaneEndpoint != endpoint {
		base := cluster.DeepCopy()
		cluster.Spec.ControlPlaneEndpoint = endpoint
		if err := r.Patch(ctx, cluster, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("writing control plane endpoint: %w", err)
		}
		log.Info("Wrote control plane endpoint", "host", endpoint.Host, "port", endpoint.Port, "instanceID", inst.ID)
	}

	if !ptr.Deref(cluster.Status.Initialization.Provisioned, false) {
		base := cluster.DeepCopy()
		cluster.Status.Initialization.Provisioned = new(true)
		if err := r.Status().Patch(ctx, cluster, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
		}
		// Name the success transition, as the machine controller
		// does: silence at exactly this moment reads like a stall.
		log.Info("Cluster infrastructure is provisioned", "instanceID", inst.ID)
	}
	return ctrl.Result{}, nil
}

// findControlPlaneInstance returns the cluster's endpoint candidate:
// among the instances carrying this cluster's identity tags with the
// control-plane role, the first (by instance ID, so the choice is
// stable) that is running and has a public IP. It returns nil when
// no instance qualifies yet.
func (r *VerdaClusterReconciler) findControlPlaneInstance(ctx context.Context, cluster *infrastructurev1alpha1.VerdaCluster) (*verdasdk.Instance, error) {
	found, err := r.Cloud.FindInstancesByTags(ctx, map[string]string{
		cloud.TagClusterName: clusterKeyFor(cluster),
		cloud.TagRole:        cloud.RoleControlPlane,
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(found, func(a, b verdasdk.Instance) int { return strings.Compare(a.ID, b.ID) })
	for i := range found {
		inst := &found[i]
		if inst.Status == "running" && inst.IP != nil && *inst.IP != "" {
			return inst, nil
		}
	}
	return nil, nil
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
