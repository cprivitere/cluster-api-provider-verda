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

// Package cloud wraps the Verda Cloud Go SDK behind a small interface
// covering the instance and startup-script lifecycle the provider's
// controllers need. Controllers depend on Client, never on the SDK
// directly, so they can be tested against FakeClient.
//
// The interface deliberately returns SDK types (verda.Instance and
// friends): translating every SDK struct into a local DTO would add a
// second model to keep in sync without buying isolation the provider
// can use at this stage.
package cloud

import (
	"context"
	"errors"

	"github.com/verda-cloud/verdacloud-sdk-go/pkg/verda"
)

// ErrNotFound is returned (wrapped) when the requested resource does
// not exist, so reconcilers can distinguish "already gone" from a
// failed call without inspecting SDK error strings.
var ErrNotFound = errors.New("not found")

// Client is the Verda Cloud surface the provider depends on.
type Client interface {
	// CreateInstance creates one instance from the given request.
	CreateInstance(ctx context.Context, req verda.CreateInstanceRequest) (*verda.Instance, error)
	// GetInstance returns the instance with the given ID, or an error
	// wrapping ErrNotFound.
	GetInstance(ctx context.Context, id string) (*verda.Instance, error)
	// ListInstances returns instances matching the given filters; a
	// nil opts lists every instance in the project.
	ListInstances(ctx context.Context, opts *verda.ListInstancesOptions) ([]verda.Instance, error)
	// FindInstancesByTags returns instances carrying every given tag.
	// It exists because instance creation has no idempotency key:
	// reconcilers must look up by identity tags before creating, or a
	// retried reconcile can provision a duplicate instance.
	FindInstancesByTags(ctx context.Context, tags map[string]string) ([]verda.Instance, error)
	// DeleteInstance permanently deletes one instance. Attached
	// volumes, including the OS volume, are left untouched; volume
	// lifetime is a separate policy decision.
	DeleteInstance(ctx context.Context, id string) error
	// DeleteInstanceWithVolumes permanently deletes one instance and
	// every volume attached to it, including its OS volume.
	DeleteInstanceWithVolumes(ctx context.Context, id string) error

	// CreateStartupScript stores a startup script (the bootstrap-data
	// carrier for machines that cannot take inline scripts) and
	// returns it with its assigned ID.
	CreateStartupScript(ctx context.Context, name, script string) (*verda.StartupScript, error)
	// GetStartupScript returns the startup script with the given ID.
	GetStartupScript(ctx context.Context, id string) (*verda.StartupScript, error)
	// DeleteStartupScript deletes one startup script.
	DeleteStartupScript(ctx context.Context, id string) error

	// ListPrivateNetworks returns every private network in the
	// project. Networks are global; their subnets are per-location.
	ListPrivateNetworks(ctx context.Context) ([]verda.PrivateNetwork, error)
	// CreatePrivateNetwork creates one private network.
	CreatePrivateNetwork(ctx context.Context, req verda.CreatePrivateNetworkRequest) (*verda.PrivateNetwork, error)
	// DeletePrivateNetwork deletes one private network by ID, or an
	// error wrapping ErrNotFound.
	DeletePrivateNetwork(ctx context.Context, id string) error
	// ListSubnets returns the subnets of one private network.
	ListSubnets(ctx context.Context, networkID string) ([]verda.Subnet, error)
	// CreateSubnet creates one subnet in a private network.
	CreateSubnet(ctx context.Context, networkID string, req verda.CreateSubnetRequest) (*verda.Subnet, error)
	// DeleteSubnet deletes one subnet from a private network, or an
	// error wrapping ErrNotFound.
	DeleteSubnet(ctx context.Context, networkID, subnetID string) error
}
