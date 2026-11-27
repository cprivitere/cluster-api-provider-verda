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
	"slices"

	"github.com/verda-cloud/verdacloud-sdk-go/pkg/verda"
)

// Tag keys the provider stamps on every instance it creates. They are
// the identity a reconciler looks up before creating (the API has no
// idempotency key), so they are part of the provider's contract with
// itself: never rename them casually. (The one rename, capv.* ->
// capve.*, happened Nov 27 before any cluster had been created with
// them: CAPV is Cluster API Provider vSphere's abbreviation, so this
// provider goes by CAPVE.)
const (
	// TagClusterName identifies the Cluster API Cluster an instance
	// belongs to, as "<namespace>/<name>".
	TagClusterName = "capve.cluster"
	// TagMachineName identifies the Cluster API Machine an instance
	// belongs to, as "<namespace>/<name>".
	TagMachineName = "capve.machine"
	// TagRole distinguishes control plane from worker instances.
	TagRole = "capve.role"
)

// Role values for TagRole.
const (
	RoleControlPlane = "control-plane"
	RoleWorker       = "worker"
)

// MachineTags returns the identity tags for one machine's instance.
func MachineTags(clusterName, machineName, role string) []verda.TagRequest {
	return []verda.TagRequest{
		{Key: TagClusterName, Value: clusterName},
		{Key: TagMachineName, Value: machineName},
		{Key: TagRole, Value: role},
	}
}

// TagFilters converts a key/value map into SDK tag filters, sorted by
// key so callers get deterministic requests.
func TagFilters(tags map[string]string) []verda.TagFilter {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	filters := make([]verda.TagFilter, 0, len(tags))
	for _, k := range keys {
		filters = append(filters, verda.TagFilter{Key: k, Value: tags[k]})
	}
	return filters
}

// instanceHasTags reports whether inst carries every filter. A filter
// with an empty value matches the key with any value, mirroring the
// API's semantics.
func instanceHasTags(inst *verda.Instance, filters []verda.TagFilter) bool {
	for _, f := range filters {
		found := false
		for _, t := range inst.Tags {
			if t.Key == f.Key && (f.Value == "" || t.Value == f.Value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
