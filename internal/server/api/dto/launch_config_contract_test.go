package dto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// The deploy form reads its whole state back from these fields, so their names
// and shapes are pinned against literal wire JSON rather than against the Go
// structs that produced them.
func TestEvaluateLaunchConfigWireContract(t *testing.T) {
	const wire = `{"ok":true,"argv":["--gpu-memory-utilization","0.92"],` +
		`"image":"vllm/vllm-openai:latest","warnings":[],"template_deviated":false,` +
		`"effective_params":{"dtype":"auto","gpu_memory_utilization":0.92,"max_model_len":8192},` +
		`"moved":[{"key":"max_model_len","flag":"--max-model-len","value":8192,"source":"extra_args"}],` +
		`"template_params":{"kv_cache_dtype":"fp8"},` +
		`"template_residual_args":["--no-enable-flashinfer-autotune"]}`

	var decoded EvaluateResponse
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.EffectiveParams["dtype"] != "auto" || decoded.EffectiveParams["gpu_memory_utilization"] != 0.92 || decoded.EffectiveParams["max_model_len"] != float64(8192) {
		t.Fatalf("effective_params=%#v", decoded.EffectiveParams)
	}
	want := deploy.MovedFlag{Key: "max_model_len", Flag: "--max-model-len", Value: float64(8192), Source: "extra_args"}
	if len(decoded.Moved) != 1 || decoded.Moved[0] != want {
		t.Fatalf("moved=%#v, want %#v", decoded.Moved, want)
	}
	if decoded.TemplateParams["kv_cache_dtype"] != "fp8" {
		t.Fatalf("template_params=%#v", decoded.TemplateParams)
	}
	if !reflect.DeepEqual(decoded.TemplateResidualArgs, []string{"--no-enable-flashinfer-autotune"}) {
		t.Fatalf("template_residual_args=%#v", decoded.TemplateResidualArgs)
	}

	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err := json.Unmarshal([]byte(wire), &original); err != nil {
		t.Fatal(err)
	}
	for key, value := range original {
		if !reflect.DeepEqual(round[key], value) {
			t.Fatalf("round trip changed %q: %#v, want %#v", key, round[key], value)
		}
	}
}

// The retired injected-args ledger must never reappear on the instance or rank
// wire shapes: the instance reads its desired-state snapshot (entrypoint +
// argv + command_mode), and a rank command carries `command` only when set.
func TestInstanceAndRankCommandWireNeverCarryInjectedLedger(t *testing.T) {
	instance, err := json.Marshal(Instance{
		UUID: "inst-1", DeploymentID: 3, State: "running", GPUIndexes: []int{0}, Ports: []int{30000},
		APIPort: 30000, Image: "vllm/vllm-openai:latest", Message: "",
		Entrypoint: []string{"/bin/sh"}, Argv: []string{"-c", "set -a\nvllm serve /model"}, CommandMode: "manual",
	})
	if err != nil {
		t.Fatal(err)
	}
	var instanceWire map[string]any
	if err := json.Unmarshal(instance, &instanceWire); err != nil {
		t.Fatal(err)
	}
	assertWireValues(t, instanceWire, map[string]any{
		"uuid": "inst-1", "command_mode": "manual",
		"entrypoint": []any{"/bin/sh"}, "argv": []any{"-c", "set -a\nvllm serve /model"},
	})
	for key := range instanceWire {
		if strings.Contains(key, "injected") {
			t.Fatalf("instance wire carries injected key %q: %s", key, instance)
		}
	}

	rank, err := json.Marshal(EvaluateRankCommand{
		NodeUUID: "n1", Role: "standalone", Image: "vllm/vllm-openai:latest",
		Entrypoint: []string{"vllm", "serve", "/model"}, Argv: []string{"--port", "30000"},
		Command: []string{"vllm", "serve", "/model", "--port", "30000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var rankWire map[string]any
	if err := json.Unmarshal(rank, &rankWire); err != nil {
		t.Fatal(err)
	}
	assertWireValues(t, rankWire, map[string]any{
		"command": []any{"vllm", "serve", "/model", "--port", "30000"},
	})
	for key := range rankWire {
		if strings.Contains(key, "injected") {
			t.Fatalf("rank command wire carries injected key %q: %s", key, rank)
		}
	}
	bare, err := json.Marshal(EvaluateRankCommand{NodeUUID: "n1", Role: "standalone", Argv: []string{"--x"}})
	if err != nil {
		t.Fatal(err)
	}
	var bareWire map[string]any
	if err := json.Unmarshal(bare, &bareWire); err != nil {
		t.Fatal(err)
	}
	if _, present := bareWire["command"]; present {
		t.Fatalf("command must be omitted when there is nothing to report: %s", bare)
	}
}

func assertWireValues(t *testing.T, object, want map[string]any) {
	t.Helper()
	for key, wantValue := range want {
		if got, ok := object[key]; !ok || !reflect.DeepEqual(got, wantValue) {
			t.Errorf("JSON field %q=%#v (present=%v), want %#v; object=%#v", key, got, ok, wantValue, object)
		}
	}
}

// The four launch-configuration fields are additive: a response that carries no
// materialization (an evaluation that failed before it) must not put empty
// objects on the wire, because the form reads presence as "the server told me
// what the configuration is".
func TestEvaluateLaunchConfigFieldsAreOmittedWhenAbsent(t *testing.T) {
	encoded, err := json.Marshal(EvaluateResponse{OK: false})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"effective_params", "moved", "template_params", "template_residual_args"} {
		if _, present := wire[key]; present {
			t.Fatalf("%s must be omitted when there is nothing to report: %s", key, encoded)
		}
	}
}

// A deployment read API exposes the stored launch configuration under the same
// keys the create request used.
func TestDeploymentLaunchConfigWireContract(t *testing.T) {
	const wire = `{"id":7,"name":"d","engine":"vllm","model_id":1,"model_name":"m","node_uuid":"n","served_name":"d",` +
		`"desired_state":"active","runtime_state":"running","sleepable":false,"replicas":1,"operations":[],` +
		`"template_deviated":false,"created_at":"2026-09-15T00:00:00Z",` +
		`"quick_params":{"gpu_memory_utilization":0.92,"max_model_len":8192},"extra_args":"--foo bar"}`
	var decoded Deployment
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.QuickParams["gpu_memory_utilization"] != 0.92 || decoded.QuickParams["max_model_len"] != float64(8192) {
		t.Fatalf("quick_params=%#v", decoded.QuickParams)
	}
	if decoded.ExtraArgs != "--foo bar" {
		t.Fatalf("extra_args=%q", decoded.ExtraArgs)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round["quick_params"], map[string]any{"gpu_memory_utilization": 0.92, "max_model_len": float64(8192)}) {
		t.Fatalf("round trip quick_params=%#v", round["quick_params"])
	}
	if round["extra_args"] != "--foo bar" {
		t.Fatalf("round trip extra_args=%#v", round["extra_args"])
	}
}

// The KV disk tier's ceiling is a platform parameter, so it never appears on a
// command line - the deployment read API is the only place a client can learn
// what the platform is enforcing, and the instance panel reads it from there.
func TestDeploymentCarriesTheKVDiskTierConfiguration(t *testing.T) {
	const wire = `{"id":7,"name":"d","engine":"vllm","model_id":1,"model_name":"m","node_uuid":"n","served_name":"d",` +
		`"desired_state":"active","runtime_state":"running","sleepable":false,"replicas":1,"operations":[],` +
		`"template_deviated":false,"created_at":"2026-09-15T00:00:00Z",` +
		`"quick_params":{"kv_offloading_size":8,"kv_disk_offload":true,"shared_kv_dir":"/mnt/kv","kv_disk_limit_gib":32}}`
	var decoded Deployment
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"kv_offloading_size": float64(8),
		"kv_disk_offload":    true,
		"shared_kv_dir":      "/mnt/kv",
		"kv_disk_limit_gib":  float64(32),
	}
	if !reflect.DeepEqual(decoded.QuickParams, want) {
		t.Fatalf("quick_params=%#v, want %#v", decoded.QuickParams, want)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round["quick_params"], want) {
		t.Fatalf("round trip quick_params=%#v", round["quick_params"])
	}
	// A deployment that configured no tier says nothing about one.
	plain, err := json.Marshal(Deployment{ID: 7, QuickParams: map[string]any{"max_model_len": 8192}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "kv_disk_limit_gib") {
		t.Fatalf("an unconfigured deployment advertised the ceiling: %s", plain)
	}
}

func TestLaunchPreparationWireRoundTrip(t *testing.T) {
	for _, wire := range []string{
		`{"pre_start_script":"echo ready\n","env":{"SGLANG_LOGGING_CONFIG_PATH":"/tmp/logging.json"}}`,
		`{"pre_start_argv":["prepare","a b"]}`,
	} {
		for _, target := range []any{&EvaluateResponse{}, &EvaluateRankCommand{}, &Instance{}} {
			if err := json.Unmarshal([]byte(wire), target); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(target)
			if err != nil {
				t.Fatal(err)
			}
			var original, round map[string]any
			_ = json.Unmarshal([]byte(wire), &original)
			_ = json.Unmarshal(encoded, &round)
			for key, value := range original {
				if !reflect.DeepEqual(round[key], value) {
					t.Fatalf("%T lost %s: %s", target, key, encoded)
				}
			}
			for _, key := range []string{"pre_start_script", "pre_start_argv"} {
				if _, present := original[key]; !present {
					if _, emitted := round[key]; emitted {
						t.Fatalf("%T emitted absent %s", target, key)
					}
				}
			}
		}
	}
}

// An instance names the KV offload backend it keeps only when it keeps one:
// an instance of the engine's own tiers carries no key at all.
func TestInstanceKVOffloadBackendWire(t *testing.T) {
	raw, err := json.Marshal(Instance{UUID: "i", KVOffloadBackend: &KVOffloadBackendRef{ID: "lmcache", Label: "LMCache"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"kv_offload_backend":{"id":"lmcache","label":"LMCache"}`) {
		t.Fatalf("wire = %s", raw)
	}
	var round Instance
	if err := json.Unmarshal(raw, &round); err != nil || round.KVOffloadBackend == nil || *round.KVOffloadBackend != (KVOffloadBackendRef{ID: "lmcache", Label: "LMCache"}) {
		t.Fatalf("round trip = %+v, %v", round.KVOffloadBackend, err)
	}
	raw, _ = json.Marshal(Instance{UUID: "i"})
	if strings.Contains(string(raw), "kv_offload_backend") {
		t.Fatalf("native instance wire = %s", raw)
	}
}
