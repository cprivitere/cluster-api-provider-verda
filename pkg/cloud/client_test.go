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
	"testing"

	"github.com/verda-cloud/verdacloud-sdk-go/pkg/verda"
)

func TestFakeClientInstanceLifecycle(t *testing.T) {
	ctx := context.Background()
	c := NewFakeClient()

	tags := MachineTags("default/test-cluster", "default/test-machine-0", RoleControlPlane)
	inst, err := c.CreateInstance(ctx, verda.CreateInstanceRequest{
		InstanceType: "1CPU-4GB",
		Image:        "ubuntu-24.04",
		Hostname:     "test-machine-0",
		LocationCode: "FIN-03",
		Tags:         tags,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if inst.ID == "" {
		t.Fatal("CreateInstance returned an instance with no ID")
	}

	got, err := c.GetInstance(ctx, inst.ID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.Hostname != "test-machine-0" || got.InstanceType != "1CPU-4GB" {
		t.Errorf("GetInstance returned %+v, want the created instance", got)
	}

	found, err := c.FindInstancesByTags(ctx, map[string]string{
		TagClusterName: "default/test-cluster",
		TagRole:        RoleControlPlane,
	})
	if err != nil {
		t.Fatalf("FindInstancesByTags: %v", err)
	}
	if len(found) != 1 || found[0].ID != inst.ID {
		t.Errorf("FindInstancesByTags found %d instances, want exactly the created one", len(found))
	}

	missed, err := c.FindInstancesByTags(ctx, map[string]string{TagClusterName: "default/other"})
	if err != nil {
		t.Fatalf("FindInstancesByTags (miss): %v", err)
	}
	if len(missed) != 0 {
		t.Errorf("FindInstancesByTags (miss) found %d instances, want 0", len(missed))
	}

	if err := c.DeleteInstance(ctx, inst.ID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if _, err := c.GetInstance(ctx, inst.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetInstance after delete returned %v, want ErrNotFound", err)
	}
	if err := c.DeleteInstance(ctx, inst.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteInstance of a missing instance returned %v, want ErrNotFound", err)
	}
}

func TestFakeClientStartupScripts(t *testing.T) {
	ctx := context.Background()
	c := NewFakeClient()

	script, err := c.CreateStartupScript(ctx, "bootstrap-0", "#!/bin/bash\necho hi")
	if err != nil {
		t.Fatalf("CreateStartupScript: %v", err)
	}
	got, err := c.GetStartupScript(ctx, script.ID)
	if err != nil {
		t.Fatalf("GetStartupScript: %v", err)
	}
	if got.Script != script.Script || got.Name != script.Name {
		t.Errorf("GetStartupScript returned %+v, want %+v", got, script)
	}
	if err := c.DeleteStartupScript(ctx, script.ID); err != nil {
		t.Fatalf("DeleteStartupScript: %v", err)
	}
	if _, err := c.GetStartupScript(ctx, script.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetStartupScript after delete returned %v, want ErrNotFound", err)
	}
}

func TestFakeClientErrorInjection(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	c := NewFakeClient()
	c.Err = boom

	if _, err := c.CreateInstance(ctx, verda.CreateInstanceRequest{}); !errors.Is(err, boom) {
		t.Errorf("CreateInstance returned %v, want the injected error", err)
	}
	if _, err := c.ListInstances(ctx, nil); !errors.Is(err, boom) {
		t.Errorf("ListInstances returned %v, want the injected error", err)
	}
	if err := c.DeleteInstance(ctx, "instance-0001"); !errors.Is(err, boom) {
		t.Errorf("DeleteInstance returned %v, want the injected error", err)
	}
}
