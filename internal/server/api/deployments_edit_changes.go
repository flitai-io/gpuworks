package api

import (
	"encoding/json"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// keepEquivalentJSON keeps a stored JSON column when the edited value says
// the same thing, so an edit that did not touch it does not rewrite it.
func keepEquivalentJSON(stored, edited string) string {
	if sameJSON(stored, edited) {
		return stored
	}
	return edited
}

// sameJSON compares two JSON documents (or their decoded values) by what
// they say; an empty or null document is the same as none.
func sameJSON(a, b any) bool {
	return equalDecoded(normalizeJSON(a), normalizeJSON(b))
}

func normalizeJSON(value any) any {
	var raw []byte
	switch typed := value.(type) {
	case string:
		raw = []byte(strings.TrimSpace(typed))
	case nil:
		return nil
	default:
		var err error
		if raw, err = json.Marshal(typed); err != nil {
			return value
		}
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw)
	}
	if object, ok := out.(map[string]any); ok && len(object) == 0 {
		return nil
	}
	return out
}

func equalDecoded(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

// sameRequirementSets compares two requirement sets as sets.
func sameRequirementSets(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, values := range a {
		other, ok := b[key]
		if !ok || len(values) != len(other) {
			return false
		}
		left, right := slices.Clone(values), slices.Clone(other)
		slices.Sort(left)
		slices.Sort(right)
		if !slices.Equal(left, right) {
			return false
		}
	}
	return true
}

// deploymentChanges lists what an edit changes, field by field in the
// request's own names, each with the layer it takes effect in.
func deploymentChanges(st *store.Store, dep *store.Deployment, reqs map[string][]string, next *store.Deployment, nextReqs map[string][]string) []dto.DeploymentChange {
	var out []dto.DeploymentChange
	add := func(field, layer string, from, to any) {
		out = append(out, dto.DeploymentChange{Field: field, Layer: layer, From: changeText(from), To: changeText(to)})
	}
	live, restart := dto.DeploymentChangeLive, dto.DeploymentChangeRestart
	objectChanges := func(prefix, before, after string) {
		was, _ := storedObject(before)
		now, _ := storedObject(after)
		for _, key := range unionKeys(was, now) {
			if prefix == "container_overrides" && key == "env" {
				wasEnv, _ := was[key].(map[string]any)
				nowEnv, _ := now[key].(map[string]any)
				for _, name := range unionKeys(wasEnv, nowEnv) {
					if !sameJSON(wasEnv[name], nowEnv[name]) {
						out = append(out, dto.DeploymentChange{Field: prefix + ".env." + name, Layer: restart, Masked: true})
					}
				}
				continue
			}
			if !sameJSON(was[key], now[key]) {
				add(prefix+"."+key, restart, was[key], now[key])
			}
		}
	}
	objectChanges("quick_params", dep.QuickParams, next.QuickParams)
	if dep.ExtraArgs != next.ExtraArgs {
		add("extra_args", restart, dep.ExtraArgs, next.ExtraArgs)
	}
	if dep.Image != next.Image {
		add("image", restart, dep.Image, next.Image)
	}
	objectChanges("container_overrides", dep.ContainerOverrides, next.ContainerOverrides)
	objectChanges("placement", dep.Placement, next.Placement)
	if dep.Sleepable != next.Sleepable {
		add("sleepable", restart, dep.Sleepable, next.Sleepable)
	}
	if dep.PriorityScheduling != next.PriorityScheduling {
		add("priority_scheduling", restart, dep.PriorityScheduling, next.PriorityScheduling)
	}
	if dep.VRAMLimitBytes != next.VRAMLimitBytes {
		add("vram_limit_gib", restart, float64(dep.VRAMLimitBytes)/(1<<30), float64(next.VRAMLimitBytes)/(1<<30))
	}
	if dep.PartitionBackend != next.PartitionBackend {
		add("partition_backend", restart, dep.PartitionBackend, next.PartitionBackend)
	}
	if max(dep.Replicas, 1) != max(next.Replicas, 1) {
		add("replicas", live, max(dep.Replicas, 1), max(next.Replicas, 1))
	}
	if dep.RoutingPolicy != next.RoutingPolicy {
		add("routing_policy", live, dep.RoutingPolicy, next.RoutingPolicy)
	}
	if dep.HealthCheck != next.HealthCheck {
		add("health_check", live, dep.HealthCheck, next.HealthCheck)
	}
	if dep.HealthCheckThresholdSec != next.HealthCheckThresholdSec {
		add("health_check_threshold_sec", live, dep.HealthCheckThresholdSec, next.HealthCheckThresholdSec)
	}
	if dep.TokenizeDisabled != next.TokenizeDisabled {
		add("tokenize_enabled", live, !dep.TokenizeDisabled, !next.TokenizeDisabled)
	}
	if dep.StartTimeoutSec != next.StartTimeoutSec {
		add("start_timeout_sec", live, dep.StartTimeoutSec, next.StartTimeoutSec)
	}
	if dep.PlacementStrategy != next.PlacementStrategy {
		add("placement_strategy", live, string(dep.PlacementStrategy), string(next.PlacementStrategy))
	}
	if !sameRequirementSets(reqs, nextReqs) {
		add("node_requirements", live, reqs, nextReqs)
	}
	if dep.NodeID != next.NodeID {
		add("node_uuid", live, nodeUUIDOf(st, dep.NodeID), nodeUUIDOf(st, next.NodeID))
	}
	return out
}

// changeText renders a changed value for display: a string as itself,
// anything else as its JSON, nothing as "".
func changeText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func unionKeys(maps ...map[string]any) []string {
	var keys []string
	for _, m := range maps {
		for key := range m {
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)
	return keys
}
