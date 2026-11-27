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
	"sync"

	"github.com/verda-cloud/verdacloud-sdk-go/pkg/verda"
)

// FakeClient is an in-memory Client for tests. It stores what was
// created and answers lookups from that store, including tag
// filtering with the API's semantics.
//
// If Err is set, every method returns it, which is enough to drive
// controller error paths.
type FakeClient struct {
	mu        sync.Mutex
	instances map[string]*verda.Instance
	scripts   map[string]*verda.StartupScript
	// inlineScripts records the inline startup script each instance
	// was created with, keyed by instance ID. The SDK's Instance
	// type does not echo the script back (only a script ID), so
	// tests need this side channel to see what was delivered.
	inlineScripts map[string]string
	seq           int

	Err error
}

var _ Client = (*FakeClient)(nil)

// NewFakeClient returns an empty FakeClient.
func NewFakeClient() *FakeClient {
	return &FakeClient{
		instances:     make(map[string]*verda.Instance),
		scripts:       make(map[string]*verda.StartupScript),
		inlineScripts: make(map[string]string),
	}
}

// StartupScriptFor returns the inline startup script the instance
// with the given ID was created with, or "" if it was created
// without one (or does not exist).
func (f *FakeClient) StartupScriptFor(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inlineScripts[id]
}

// SetInstanceStatus overrides a stored instance's status. It exists
// so tests can drive state transitions the real API would produce on
// its own — spot discontinuation above all.
func (f *FakeClient) SetInstanceStatus(id, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[id]
	if !ok {
		return fmt.Errorf("%w: instance %s", ErrNotFound, id)
	}
	inst.Status = status
	return nil
}

// SetInstanceIP overrides a stored instance's public IP. It exists
// for the same reason as SetInstanceStatus: the real API assigns an
// address on its own schedule, and tests need to drive that.
func (f *FakeClient) SetInstanceIP(id, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.instances[id]
	if !ok {
		return fmt.Errorf("%w: instance %s", ErrNotFound, id)
	}
	inst.IP = &ip
	return nil
}

func (f *FakeClient) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-%04d", prefix, f.seq)
}

func (f *FakeClient) CreateInstance(_ context.Context, req verda.CreateInstanceRequest) (*verda.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	inst := &verda.Instance{
		ID:              f.nextID("instance"),
		Status:          "running",
		InstanceType:    req.InstanceType,
		Image:           req.Image,
		Hostname:        req.Hostname,
		Location:        req.LocationCode,
		SSHKeyIDs:       append([]string(nil), req.SSHKeyIDs...),
		StartupScriptID: req.StartupScriptID,
		IsSpot:          req.IsSpot,
	}
	for _, t := range req.Tags {
		inst.Tags = append(inst.Tags, verda.Tag{Key: t.Key, Value: t.Value})
	}
	f.instances[inst.ID] = inst
	if req.StartupScript != "" {
		f.inlineScripts[inst.ID] = req.StartupScript
	}
	return copyInstance(inst), nil
}

func (f *FakeClient) GetInstance(_ context.Context, id string) (*verda.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	inst, ok := f.instances[id]
	if !ok {
		return nil, fmt.Errorf("%w: instance %s", ErrNotFound, id)
	}
	return copyInstance(inst), nil
}

func (f *FakeClient) ListInstances(_ context.Context, opts *verda.ListInstancesOptions) ([]verda.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	out := []verda.Instance{}
	for _, inst := range f.instances {
		if opts != nil {
			if opts.Status != "" && inst.Status != opts.Status {
				continue
			}
			if !instanceHasTags(inst, opts.Tags) {
				continue
			}
		}
		out = append(out, *copyInstance(inst))
	}
	return out, nil
}

func (f *FakeClient) FindInstancesByTags(ctx context.Context, tags map[string]string) ([]verda.Instance, error) {
	return f.ListInstances(ctx, &verda.ListInstancesOptions{Tags: TagFilters(tags)})
}

func (f *FakeClient) DeleteInstance(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	if _, ok := f.instances[id]; !ok {
		return fmt.Errorf("%w: instance %s", ErrNotFound, id)
	}
	delete(f.instances, id)
	return nil
}

func (f *FakeClient) DeleteInstanceWithVolumes(ctx context.Context, id string) error {
	// Same contract as the real client: deleting what is already
	// gone is success, not an error.
	if err := f.DeleteInstance(ctx, id); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

func (f *FakeClient) CreateStartupScript(_ context.Context, name, script string) (*verda.StartupScript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	s := &verda.StartupScript{
		ID:     f.nextID("script"),
		Name:   name,
		Script: script,
	}
	f.scripts[s.ID] = s
	cp := *s
	return &cp, nil
}

func (f *FakeClient) GetStartupScript(_ context.Context, id string) (*verda.StartupScript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	s, ok := f.scripts[id]
	if !ok {
		return nil, fmt.Errorf("%w: startup script %s", ErrNotFound, id)
	}
	cp := *s
	return &cp, nil
}

func (f *FakeClient) DeleteStartupScript(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	if _, ok := f.scripts[id]; !ok {
		return fmt.Errorf("%w: startup script %s", ErrNotFound, id)
	}
	delete(f.scripts, id)
	return nil
}

func copyInstance(inst *verda.Instance) *verda.Instance {
	cp := *inst
	cp.SSHKeyIDs = append([]string(nil), inst.SSHKeyIDs...)
	cp.Tags = append([]verda.Tag(nil), inst.Tags...)
	cp.VolumeIDs = append([]string(nil), inst.VolumeIDs...)
	return &cp
}
