package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	serverestimate "gpuworks.ai/gpuworks/internal/server/estimate"
)

func TestFrontendDTOContractUsesExactNodeWireKeys(t *testing.T) {
	node := dto.Node{
		UUID:        "node-1",
		State:       "online",
		GPUCount:    2,
		GPUVendor:   "nvidia",
		ConnectMode: "server_dial",
		GPUs: []dto.GPU{{
			Index:            1,
			UUID:             "gpu-1",
			MemoryTotalBytes: 24 << 30,
			Schedulable:      true,
		}},
	}

	object := marshalJSONObject(t, node)
	assertJSONValues(t, object, map[string]any{
		"uuid":         "node-1",
		"state":        "online",
		"gpu_count":    float64(2),
		"gpu_vendor":   "nvidia",
		"connect_mode": "server_dial",
	})
	assertJSONKeysAbsent(t, object, "UUID", "GPUCount", "GPUVendor", "ConnectMode")

	gpus, ok := object["gpus"].([]any)
	if !ok || len(gpus) != 1 {
		t.Fatalf("gpus wire value=%#v, want one-element JSON array", object["gpus"])
	}
	gpu, ok := gpus[0].(map[string]any)
	if !ok {
		t.Fatalf("gpus[0]=%#v, want JSON object", gpus[0])
	}
	assertJSONValues(t, gpu, map[string]any{
		"index":              float64(1),
		"uuid":               "gpu-1",
		"memory_total_bytes": float64(24 << 30),
		"schedulable":        true,
	})
	// Whether a card serves is one field, schedulable; a second boolean
	// beside it could only repeat it or contradict it.
	assertJSONKeysAbsent(t, gpu, "healthy")
}

func TestEvaluateBreakdownWireIncludesStartupAndInformationalKVFields(t *testing.T) {
	resp := dto.EvaluateResponse{OK: true, Breakdown: serverestimate.Breakdown{
		WeightBytes: 8 << 30, StartupFixedBytes: 10 << 30, PerGPUBytes: 10 << 30,
		VRAMLimitBytes: 16 << 30, CapacityBytes: 14 << 30, KVPoolBytes: 4 << 30, EstimatedKVTokens: 4096,
		KVEstimateConfidence: "measured", KVEstimateReason: "", Fits: true,
	}}
	object := marshalJSONObject(t, resp)
	breakdown, ok := object["breakdown"].(map[string]any)
	if !ok {
		t.Fatalf("breakdown wire = %#v", object["breakdown"])
	}
	for _, key := range []string{"weight_bytes", "startup_fixed_bytes", "vram_limit_bytes", "capacity_bytes", "kv_pool_bytes", "estimated_kv_tokens", "kv_estimate_confidence", "fits"} {
		if _, ok := breakdown[key]; !ok {
			t.Fatalf("breakdown omitted %q: %#v", key, breakdown)
		}
	}
}

func TestEvaluateResponseWireIncludesTypedEvictionSuggestion(t *testing.T) {
	resp := dto.EvaluateResponse{OK: false, Eviction: []dto.EvictionSuggestion{{
		InstanceUUID: "inst-1", DeploymentID: 7, Deployment: "sleepable", GPUIndexes: []int{0, 2}, FreesGiB: 18.5,
	}}}
	object := marshalJSONObject(t, resp)
	evictions, ok := object["eviction"].([]any)
	if !ok || len(evictions) != 1 {
		t.Fatalf("eviction wire=%#v", object["eviction"])
	}
	eviction := evictions[0].(map[string]any)
	assertJSONValues(t, eviction, map[string]any{
		"instance_uuid": "inst-1", "deployment_id": float64(7), "deployment": "sleepable", "gpu_indexes": []any{float64(0), float64(2)}, "frees_gib": 18.5,
	})
}

func TestFrontendDTOContractAcceptsLiteralDeploymentPayload(t *testing.T) {
	const wirePayload = `{
		"id": 7,
		"template_id": 11,
		"template_spec_source": "upstream",
		"template_spec_name": "balanced",
		"template_deviated": false,
		"name": "qwen",
		"engine": "vllm",
		"model_id": 3,
		"model_name": "Qwen",
		"node_uuid": "node-1",
		"node_uuids": ["node-1", "node-2"],
		"served_name": "qwen",
		"desired_state": "active",
		"runtime_state": "running",
		"state": "running",
		"sleepable": false,
		"replicas": 1,
		"nnodes": 2,
		"master_addr": "10.0.0.1",
		"master_port": 29500,
		"created_at": "2026-08-29T08:00:00Z"
	}`

	var got dto.Deployment
	if err := json.Unmarshal([]byte(wirePayload), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || got.TemplateID != 11 || got.TemplateSpecSource != "upstream" || got.TemplateSpecName != "balanced" || got.ModelID != 3 || got.NodeUUID != "node-1" ||
		!reflect.DeepEqual(got.NodeUUIDs, []string{"node-1", "node-2"}) ||
		got.DesiredState != "active" || got.RuntimeState != "running" || got.NNodes != 2 ||
		got.MasterAddr != "10.0.0.1" || got.MasterPort != 29500 {
		t.Fatalf("literal frontend payload decoded incorrectly: %+v", got)
	}

	object := marshalJSONObject(t, got)
	assertJSONValues(t, object, map[string]any{
		"template_spec_source": "upstream",
		"template_spec_name":   "balanced",
		"model_id":             float64(3),
		"node_uuid":            "node-1",
		"runtime_state":        "running",
		"desired_state":        "active",
		"state":                "running",
		"nnodes":               float64(2),
		"master_addr":          "10.0.0.1",
		"master_port":          float64(29500),
		"template_deviated":    false,
	})
	assertJSONKeysAbsent(t, object, "blueprint_deviated", "template_profile_key")
	assertJSONKeysAbsent(t, object, "ModelID", "NodeUUID", "NodeUUIDs", "DesiredState", "RuntimeState", "MasterAddr", "MasterPort")
}

func TestFrontendDTOContractOmitsEmptyOptionalFields(t *testing.T) {
	object := marshalJSONObject(t, dto.Node{UUID: "node-1", State: "online"})
	assertJSONKeysAbsent(t, object, "token", "gpu_vendor", "connect_mode", "gpu_probe_error", "install_command")
}

func TestEvaluateRequestPreservesServedNameWireContract(t *testing.T) {
	const wire = `{"model_id":3,"target_vendor":"ascend","node_uuid":"node-1","served_name":"public-h3","engine":"sglang","quick_params":{},"extra_args":"","placement":{},"vram_limit_gib":0}`
	var request dto.EvaluateRequest
	if err := json.Unmarshal([]byte(wire), &request); err != nil {
		t.Fatal(err)
	}
	if request.ServedName != "public-h3" {
		t.Fatalf("served_name=%q", request.ServedName)
	}
	object := marshalJSONObject(t, request)
	assertJSONValues(t, object, map[string]any{"served_name": "public-h3", "model_id": float64(3), "target_vendor": "ascend", "node_uuid": "node-1"})
	assertJSONKeysAbsent(t, object, "ServedName", "TargetVendor")
}

func TestEvaluateRequestSharesTheCreateBodyWireContract(t *testing.T) {
	// The preview request is the create body plus target_vendor. sleepable
	// historically existed only on the create side, so the preview never saw
	// the platform's sleep injection; the wire test pins it on both shapes.
	const wire = `{"model_id":3,"target_vendor":"nvidia","sleepable":true,"node_uuid":"node-1","quick_params":{},"extra_args":"","placement":{}}`
	var request dto.EvaluateRequest
	if err := json.Unmarshal([]byte(wire), &request); err != nil {
		t.Fatal(err)
	}
	if !request.Sleepable || request.TargetVendor != "nvidia" || request.NodeUUID != "node-1" {
		t.Fatalf("request=%+v", request.DeployRequest)
	}
	object := marshalJSONObject(t, request)
	assertJSONValues(t, object, map[string]any{"sleepable": true, "target_vendor": "nvidia", "node_uuid": "node-1"})
	assertJSONKeysAbsent(t, object, "DeployRequest")
}

func TestAPIKeyDTOContractCannotExposeSecret(t *testing.T) {
	created := time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC)
	object := marshalJSONObject(t, dto.APIKey{Name: "automation", Prefix: "gw_abcd", Created: created, AllDeployments: true, Deployments: []dto.APIKeyDeployment{}})
	want := map[string]any{
		"name":            "automation",
		"prefix":          "gw_abcd",
		"created_at":      "2026-08-29T08:00:00Z",
		"all_deployments": true,
		"deployments":     []any{},
		"priority":        map[string]any{"class": "", "ceiling": "", "policy": ""},
	}
	if !reflect.DeepEqual(object, want) {
		t.Fatalf("API key response contract=%#v, want exactly %#v", object, want)
	}
	scoped := marshalJSONObject(t, dto.APIKey{Name: "a", Prefix: "gw_a", Created: created, Deployments: []dto.APIKeyDeployment{{ID: 3, Name: "qwen", ServedName: "qwen-chat"}}})
	if !reflect.DeepEqual(scoped["deployments"], []any{map[string]any{"id": float64(3), "name": "qwen", "served_name": "qwen-chat"}}) || scoped["all_deployments"] != false {
		t.Fatalf("scoped key contract=%#v", scoped)
	}
}

func TestFlatSpecAndMatchedByDTOUseLiteralWireJSON(t *testing.T) {
	specWire := `{"id":7,"source":"upstream","name":"blackwell","display_name_zh_cn":"Blackwell · NVFP4","display_name_en":"Blackwell · NVFP4","engine":"vllm","image":"vllm:latest","weight_sources":[{"provider":"huggingface","repo_id":"org/model","revision":"commit","subfolder":"runtime"}],"weight_bytes":2000,"runtime_weight_bytes":1000,"shm_bytes":429496729600,"memlock_unlimited":true,"gpus":{"vendor":"nvidia","cc":">=10.0"}}`
	var spec dto.InferenceSpecDTO
	if err := json.Unmarshal([]byte(specWire), &spec); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(specWire), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flat spec DTO wire=%s, want literal %s", raw, specWire)
	}
	// A spec that states no shm_bytes leaves the key out rather than sending 0.
	spec.ShmBytes = 0
	if raw, err := json.Marshal(spec); err != nil || strings.Contains(string(raw), "shm_bytes") {
		t.Fatalf("flat spec DTO without shm_bytes wire=%s err=%v", raw, err)
	}
	// Likewise memlock_unlimited is left out unless the spec asks for it.
	spec.MemlockUnlimited = false
	if raw, err := json.Marshal(spec); err != nil || strings.Contains(string(raw), "memlock_unlimited") {
		t.Fatalf("flat spec DTO without memlock_unlimited wire=%s err=%v", raw, err)
	}

	evaluateWire := `{"replicas":1,"plan_scope":"per_replica","ok":true,"matched_by":"spec:blackwell gpus:{vendor=nvidia cc=>=10.0}","template_deviated":false,"argv":null,"image":"vllm:latest","warnings":null}`
	var evaluate dto.EvaluateResponse
	if err := json.Unmarshal([]byte(evaluateWire), &evaluate); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(evaluate)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(evaluateWire), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("matched_by DTO wire=%s, want literal %s", raw, evaluateWire)
	}
}

func marshalJSONObject(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode marshaled JSON %s: %v", raw, err)
	}
	return object
}

func assertJSONValues(t *testing.T, object, want map[string]any) {
	t.Helper()
	for key, wantValue := range want {
		if got, ok := object[key]; !ok || !reflect.DeepEqual(got, wantValue) {
			t.Errorf("JSON field %q=%#v (present=%v), want %#v; object=%#v", key, got, ok, wantValue, object)
		}
	}
}

func assertJSONKeysAbsent(t *testing.T, object map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if value, ok := object[key]; ok {
			t.Errorf("JSON field %q unexpectedly present with value %#v; object=%#v", key, value, object)
		}
	}
}

// Why a node cannot hand a container its accelerators travels as a stable
// code beside the plugin's display string, and is absent from the wire
// when there is nothing wrong — the client shows the cordon warning on its
// presence, so an empty string must not reach it.
func TestNodeWireCarriesTheRuntimeGPUReasonAsACodeAndOmitsItWhenThereIsNone(t *testing.T) {
	blocked := marshalJSONObject(t, dto.Node{
		UUID: "node-1", State: "online", Scheduling: "cordoned",
		RuntimeDriver: "docker", RuntimePlugin: "NVIDIA Container Toolkit 1.17.3",
		RuntimeGPUBlocked: accelerator.RuntimeGPUBlockedRuntimeUnconfigured,
	})
	assertJSONValues(t, blocked, map[string]any{
		"scheduling":          "cordoned",
		"runtime_driver":      "docker",
		"runtime_plugin":      "NVIDIA Container Toolkit 1.17.3",
		"runtime_gpu_blocked": "gpu_runtime_unconfigured",
	})
	assertJSONKeysAbsent(t, blocked, "RuntimeGPUBlocked", "runtime_gpu_ready")

	var back dto.Node
	raw, err := json.Marshal(dto.Node{UUID: "node-1", RuntimeGPUBlocked: accelerator.RuntimeGPUBlockedPluginMissing})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.RuntimeGPUBlocked != accelerator.RuntimeGPUBlockedPluginMissing {
		t.Fatalf("round trip lost the reason: %q", back.RuntimeGPUBlocked)
	}

	assertJSONKeysAbsent(t, marshalJSONObject(t, dto.Node{UUID: "node-1", State: "online"}), "runtime_gpu_blocked")
}

// The runtime a node passed over travels by exact key with its reason and
// the plugin it needs, round-trips, and is absent when there is none.
func TestNodeWireCarriesTheRuntimePassedOver(t *testing.T) {
	object := marshalJSONObject(t, dto.Node{
		UUID: "node-1", RuntimeDriver: "udocker",
		RuntimeFallbackFrom: "docker", RuntimeFallbackReason: accelerator.RuntimeGPUReady,
		RuntimeFallbackPlugin: accelerator.PluginNVIDIAContainerToolkit,
	})
	assertJSONValues(t, object, map[string]any{
		"runtime_driver":          "udocker",
		"runtime_fallback_from":   "docker",
		"runtime_fallback_reason": "gpu_ready",
		"runtime_fallback_plugin": "NVIDIA Container Toolkit",
	})
	assertJSONKeysAbsent(t, object, "RuntimeFallbackFrom", "runtime_gpu_blocked")

	raw, err := json.Marshal(dto.Node{UUID: "node-1", RuntimeFallbackFrom: "docker", RuntimeFallbackReason: accelerator.RuntimeGPUBlockedPluginMissing})
	if err != nil {
		t.Fatal(err)
	}
	var back dto.Node
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.RuntimeFallbackFrom != "docker" || back.RuntimeFallbackReason != accelerator.RuntimeGPUBlockedPluginMissing {
		t.Fatalf("round trip = %q/%q", back.RuntimeFallbackFrom, back.RuntimeFallbackReason)
	}
	assertJSONKeysAbsent(t, marshalJSONObject(t, dto.Node{UUID: "node-1"}),
		"runtime_fallback_from", "runtime_fallback_reason", "runtime_fallback_plugin")
}

// The sleep keys answer "is this deployment asleep?", which is the question
// this contract exists for, so their wire shape is pinned rather than left to
// whichever fixture happens to run. `state` is always present and never empty
// — a client reading it must not fall back to runtime_state for a deployment
// the server already classified. `asleep_replicas` and `sleep_level` are
// omitted when there is no sleep to report, so a zero on the wire always means
// a counted zero and never "the server forgot the field".
func TestDeploymentSleepKeysWireContract(t *testing.T) {
	awake := marshalJSONObject(t, dto.Deployment{RuntimeState: "running", State: "running"})
	if got, ok := awake["state"]; !ok || got != "running" {
		t.Fatalf("state = %#v (present %v), want the literal \"running\"", got, ok)
	}
	assertJSONKeysAbsent(t, awake, "asleep_replicas", "sleep_level")

	// A deployment that is asleep carries both, and asleep_replicas counts
	// replicas rather than being a flag.
	asleep := marshalJSONObject(t, dto.Deployment{RuntimeState: "running", State: "asleep", AsleepReplicas: 2, SleepLevel: 1})
	if got := asleep["state"]; got != "asleep" {
		t.Fatalf("state = %#v, want \"asleep\"", got)
	}
	if got := asleep["asleep_replicas"]; got != float64(2) {
		t.Fatalf("asleep_replicas = %#v, want 2", got)
	}
	if got := asleep["sleep_level"]; got != float64(1) {
		t.Fatalf("sleep_level = %#v, want 1", got)
	}

	// A partly-asleep deployment keeps state "running" and still reports the
	// number that sleeps, which is the case the whole ticket is about: the row
	// reads running while some replicas sleep behind it.
	partial := marshalJSONObject(t, dto.Deployment{RuntimeState: "running", State: "running", AsleepReplicas: 1})
	if got := partial["asleep_replicas"]; got != float64(1) {
		t.Fatalf("asleep_replicas = %#v, want 1 on a partly asleep deployment", got)
	}

	// The list row carries the same three keys: the row is where the operator
	// reads the state, and a list-only shape that dropped them would leave the
	// table falling back to runtime_state.
	row := marshalJSONObject(t, dto.DeploymentListItem{RuntimeState: "running", State: "asleep", AsleepReplicas: 1, SleepLevel: 1})
	if got := row["state"]; got != "asleep" {
		t.Fatalf("list row state = %#v, want \"asleep\"", got)
	}
	if got := row["asleep_replicas"]; got != float64(1) {
		t.Fatalf("list row asleep_replicas = %#v, want 1", got)
	}
	if got := row["sleep_level"]; got != float64(1) {
		t.Fatalf("list row sleep_level = %#v, want 1", got)
	}
}
