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

package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/verda-cloud/verdacloud-sdk-go/pkg/verda"
)

// Environment variables the SDK credentials are read from.
const (
	EnvClientID     = "VERDA_CLIENT_ID"
	EnvClientSecret = "VERDA_CLIENT_SECRET"
)

// verdaClient is the SDK-backed Client implementation.
type verdaClient struct {
	sdk *verda.Client
}

var _ Client = (*verdaClient)(nil)

// NewClient builds a Client from explicit OAuth2 client credentials,
// as issued in the Verda console under Project > Keys > Cloud API
// Credentials.
func NewClient(clientID, clientSecret string) (Client, error) {
	sdk, err := verda.NewClient(
		verda.WithClientID(clientID),
		verda.WithClientSecret(clientSecret),
	)
	if err != nil {
		return nil, fmt.Errorf("creating Verda client: %w", err)
	}
	return &verdaClient{sdk: sdk}, nil
}

// NewClientFromEnv builds a Client from the VERDA_CLIENT_ID and
// VERDA_CLIENT_SECRET environment variables.
func NewClientFromEnv() (Client, error) {
	clientID, idOK := os.LookupEnv(EnvClientID)
	clientSecret, secretOK := os.LookupEnv(EnvClientSecret)
	if !idOK || clientID == "" || !secretOK || clientSecret == "" {
		return nil, fmt.Errorf("credentials not set: %s and %s must both be present", EnvClientID, EnvClientSecret)
	}
	return NewClient(clientID, clientSecret)
}

func (c *verdaClient) CreateInstance(ctx context.Context, req verda.CreateInstanceRequest) (*verda.Instance, error) {
	return c.sdk.Instances.Create(ctx, req)
}

// isNotFoundError reports whether err is a Verda API 404.
func isNotFoundError(err error) bool {
	var apiErr *verda.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func (c *verdaClient) GetInstance(ctx context.Context, id string) (*verda.Instance, error) {
	inst, err := c.sdk.Instances.GetByID(ctx, id)
	if err != nil {
		if isNotFoundError(err) {
			return nil, fmt.Errorf("%w: instance %s", ErrNotFound, id)
		}
		return nil, err
	}
	return inst, nil
}

func (c *verdaClient) ListInstances(ctx context.Context, opts *verda.ListInstancesOptions) ([]verda.Instance, error) {
	return c.sdk.Instances.List(ctx, opts)
}

func (c *verdaClient) FindInstancesByTags(ctx context.Context, tags map[string]string) ([]verda.Instance, error) {
	return c.sdk.Instances.List(ctx, &verda.ListInstancesOptions{Tags: TagFilters(tags)})
}

func (c *verdaClient) DeleteInstance(ctx context.Context, id string) error {
	return c.sdk.Instances.Delete(ctx, []string{id}, nil, true)
}

func (c *verdaClient) DeleteInstanceWithVolumes(ctx context.Context, id string) error {
	inst, err := c.GetInstance(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	volumeIDs := make([]string, 0, len(inst.VolumeIDs)+1)
	if inst.OSVolumeID != nil && *inst.OSVolumeID != "" {
		volumeIDs = append(volumeIDs, *inst.OSVolumeID)
	}
	volumeIDs = append(volumeIDs, inst.VolumeIDs...)

	// Delete the instance together with its volumes.
	err = c.sdk.Instances.Delete(ctx, []string{id}, volumeIDs, true)
	if err == nil {
		return nil
	}
	// The combined call races the platform's own teardown: deleting
	// the instance retires its OS volume platform-side, so the
	// explicit volume list can 404 ("Volume ID doesn't exist") even
	// though the deletion is proceeding — observed live Nov 26,
	// 2026. Verify the end state instead of trusting the call: if
	// the instance is gone, sweep any surviving volumes individually
	// and tolerate the ones the platform already took. If the
	// instance is still there, the failure is real and the caller
	// should see it.
	if _, getErr := c.GetInstance(ctx, id); !errors.Is(getErr, ErrNotFound) {
		return err
	}
	for _, volumeID := range volumeIDs {
		if volErr := c.sdk.Volumes.DeleteVolume(ctx, volumeID, true); volErr != nil && !isNotFoundError(volErr) {
			return fmt.Errorf("deleting volume %s: %w", volumeID, volErr)
		}
	}
	return nil
}

func (c *verdaClient) CreateStartupScript(ctx context.Context, name, script string) (*verda.StartupScript, error) {
	return c.sdk.StartupScripts.AddStartupScript(ctx, &verda.CreateStartupScriptRequest{Name: name, Script: script})
}

func (c *verdaClient) GetStartupScript(ctx context.Context, id string) (*verda.StartupScript, error) {
	return c.sdk.StartupScripts.GetStartupScriptByID(ctx, id)
}

func (c *verdaClient) DeleteStartupScript(ctx context.Context, id string) error {
	return c.sdk.StartupScripts.DeleteStartupScript(ctx, id)
}
