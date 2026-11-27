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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1alpha1 "github.com/cprivitere/cluster-api-provider-verda/api/v1alpha1"
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
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdamachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdamachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=verdamachines/finalizers,verbs=update

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
		return ctrl.Result{}, err
	}

	// Reflect the instance onto the status.
	machine.Status.Addresses = addressesOf(inst)
	switch inst.Status {
	case "running":
		machine.Status.Ready = true
		machine.Status.FailureMessage = nil
	case instanceStatusDiscontinued:
		// A discontinued spot instance still exists as an object, so
		// it never falls into the recreate path below: surface it and
		// let a human decide whether the machine should be replaced.
		machine.Status.Ready = false
		msg := fmt.Sprintf("instance %s was discontinued", inst.ID)
		machine.Status.FailureMessage = &msg
	default:
		machine.Status.Ready = false
	}
	if err := r.Status().Update(ctx, machine); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	if !machine.Status.Ready && machine.Status.FailureMessage == nil {
		// Nothing watches the cloud side, so poll while the instance
		// is still coming up.
		log.Info("Instance is not ready yet", "instanceID", inst.ID, "status", inst.Status)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
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

	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
		id, ok := instanceIDFromProviderID(*machine.Spec.ProviderID)
		if !ok {
			return nil, fmt.Errorf("providerID %q is not a verda:// provider ID", *machine.Spec.ProviderID)
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

	inst, err := r.Cloud.CreateInstance(ctx, verdasdk.CreateInstanceRequest{
		InstanceType: machine.Spec.InstanceType,
		Image:        machine.Spec.Image,
		LocationCode: machine.Spec.Location,
		SSHKeyIDs:    machine.Spec.SSHKeyIDs,
		IsSpot:       machine.Spec.IsSpot,
		Hostname:     machine.Name,
		Tags:         cloud.MachineTags(clusterNameFor(machine), machine.Namespace+"/"+machine.Name, roleFor(machine)),
	})
	if err != nil {
		return nil, fmt.Errorf("creating instance: %w", err)
	}
	log.Info("Created instance", "instanceID", inst.ID)
	if err := r.recordProviderID(ctx, machine, inst); err != nil {
		return nil, err
	}
	return inst, nil
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
	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID != "" {
		id, ok := instanceIDFromProviderID(*machine.Spec.ProviderID)
		if !ok {
			return nil, fmt.Errorf("providerID %q is not a verda:// provider ID", *machine.Spec.ProviderID)
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
	if machine.Spec.ProviderID != nil && *machine.Spec.ProviderID == pid {
		return nil
	}
	machine.Spec.ProviderID = &pid
	if err := r.Update(ctx, machine); err != nil {
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

// roleFor reads the control-plane label the way Cluster API marks
// control plane machines; anything else is a worker.
func roleFor(machine *infrastructurev1alpha1.VerdaMachine) string {
	if _, ok := machine.Labels[controlPlaneLabel]; ok {
		return cloud.RoleControlPlane
	}
	return cloud.RoleWorker
}

func addressesOf(inst *verdasdk.Instance) []infrastructurev1alpha1.MachineAddress {
	var out []infrastructurev1alpha1.MachineAddress
	if inst.IP != nil && *inst.IP != "" {
		out = append(out, infrastructurev1alpha1.MachineAddress{Type: infrastructurev1alpha1.MachineExternalIP, Address: *inst.IP})
	}
	if inst.PrivateIP != nil && *inst.PrivateIP != "" {
		out = append(out, infrastructurev1alpha1.MachineAddress{Type: infrastructurev1alpha1.MachineInternalIP, Address: *inst.PrivateIP})
	}
	if inst.Hostname != "" {
		out = append(out, infrastructurev1alpha1.MachineAddress{Type: infrastructurev1alpha1.MachineHostName, Address: inst.Hostname})
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
