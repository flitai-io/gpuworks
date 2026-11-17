package deploy

import (
	"fmt"
	"slices"
	"testing"

	enginenames "gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/kvbackend"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// kvBackendSetProblems are the rules every selectable row is held to beyond
// what kvbackend.New can see: an engine adapter renders it, it replaces only
// keys of the engine's own offload or its dictionary, and it replaces every
// key of the engine's own offload, because the adapter renders only the
// selected row, never the engine's own tiers beside it (kv_compose.go). The
// deploy form and the instance panel rely on it: a selected backend darkens
// both engine tiers.
func kvBackendSetProblems(set kvBackendSet) []string {
	var out []string
	for _, engine := range enginenames.DialectNames() {
		for _, row := range set.registry.Selectable(engine) {
			if _, ok := set.vllmBackend(row.ID); engine != EngineVLLM || !ok {
				out = append(out, fmt.Sprintf("%s/%s: no renderer (only the vLLM adapter renders KV offload backends)", engine, row.ID))
			}
			native := nativeKVKeys(engine)
			for _, key := range row.Replaces {
				if _, ok := engineParams(engine)[key]; !ok && !slices.Contains(native, key) {
					out = append(out, fmt.Sprintf("%s/%s: replaces %q, which is neither a dictionary key nor a native offload key", engine, row.ID, key))
				}
			}
			for _, key := range native {
				if !slices.Contains(row.Replaces, key) {
					out = append(out, fmt.Sprintf("%s/%s: a selectable row must replace native key %q", engine, row.ID, key))
				}
			}
		}
	}
	return out
}

// Every selectable row of the shipped catalog, and of the test catalog, is
// rendered, replaces the whole native offload when external, and launches
// without PD when the launch is not PD.
func TestKVBackendSetRendersEverySelectableRow(t *testing.T) {
	check := func() {
		for _, problem := range kvBackendSetProblems(kvBackends) {
			t.Error(problem)
		}
		for _, row := range selectableKVBackends(EngineVLLM) {
			launch, err := resolveKVTest(kvTestParams(LaunchConfig{KVOffloadBackend: row.ID}, firstVendor(row)), CollectTargetCapabilities)
			if err != nil || launch.RequestedFeatures.PD || !launch.RequestedFeatures.CommandKnown {
				t.Errorf("%s: non-PD launch features = %+v, %v", row.ID, launch.RequestedFeatures, err)
			}
		}
	}
	check()
	withKVBackends(t)
	check()

	native, _ := kvbackend.Builtin().Resolve(EngineVLLM, "")
	prefillOnly := []string{pdmode.RolePrefill}
	for name, row := range map[string]kvbackend.Backend{
		"a SGLang row has no adapter": {ID: "hicache", Engine: EngineSGLang, Label: "HiCache", External: true, PDRoles: prefillOnly},
		"a partial replacement":       {ID: "partial", Engine: EngineVLLM, Label: "Partial", External: true, Replaces: []string{"kv_disk_offload"}, PDRoles: prefillOnly},
		"a key nobody owns":           {ID: "stray", Engine: EngineVLLM, Label: "Stray", Replaces: append(slices.Clone(testExternalKVBackendRow().Replaces), "no_such_key"), PDRoles: prefillOnly},
		"an internal partial row":     {ID: "inner", Engine: EngineVLLM, Label: "Inner", Replaces: []string{"kv_disk_offload"}, PDRoles: prefillOnly},
	} {
		registry, err := kvbackend.New(native, row)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		set := kvBackendSet{registry: registry, vllmOffload: map[string]vllmOffloadBackend{row.ID: {render: testExternalVLLMOffload}}}
		if problems := kvBackendSetProblems(set); len(problems) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}

func firstVendor(row kvbackend.Backend) string {
	if len(row.Vendors) == 0 {
		return ""
	}
	return row.Vendors[0]
}

// A role that does not keep the backend launches with none of the engine's
// own offload either, and nothing a backend owns survives on it: no
// dictionary key depends on the selector until PDRoleLaunchConfig learns to
// drop such keys.
func TestPDRoleLaunchConfigDropsEveryOffloadKey(t *testing.T) {
	q := LaunchConfig{KVOffloadingSize: 8, KVDiskOffload: true, SharedKVDir: "/kv", KVDiskLimitGiB: 4, KVOffloadReadThreads: 2, KVOffloadWriteThreads: 2, KVOffloadBlockSize: 64}
	decode := PDRoleLaunchConfig(EngineVLLM, q, pdmode.RoleDecode)
	values, err := launchConfigValues(decode)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range nativeKVKeys(EngineVLLM) {
		if _, stated := values[key]; stated {
			t.Errorf("decode keeps %s = %v", key, values[key])
		}
	}
	for _, engine := range enginenames.DialectNames() {
		for key, spec := range engineParams(engine) {
			if spec.DependsOn == paramKVOffloadBackend {
				t.Errorf("%s/%s depends on %s: PDRoleLaunchConfig must drop it on a role that does not keep the backend", engine, key, paramKVOffloadBackend)
			}
		}
	}
}

// A backend's renderer sees the launch, and the environment it sets reaches
// the engine process; the adapter merges it in one place.
func TestKVBackendRendererEnvReachesTheLaunch(t *testing.T) {
	withKVBackends(t)
	p := kvTestParams(selectTestExternal(), "nvidia")
	p.Quick.DP, p.AssignedGPUs = 2, 2
	launch := vLLMEngine{}.Build(p)
	if launch.Env[testExternalEnv] != "2" {
		t.Fatalf("env = %v", launch.Env)
	}
	if launch := (vLLMEngine{}).Build(kvTestParams(selectTestExternal(), "nvidia")); launch.Env[testExternalEnv] != "" {
		t.Fatalf("env without DP = %v", launch.Env)
	}
}

// The roles that keep KV offload under PD come from the selected row: the
// engine's own tiers and the test row are kept on prefill, and a launch with
// no offload has none.
func TestKVOffloadPDRoles(t *testing.T) {
	withKVBackends(t)
	prefill := []string{pdmode.RolePrefill}
	for name, tc := range map[string]struct {
		engine string
		q      LaunchConfig
		want   []string
	}{
		"no offload":       {EngineVLLM, LaunchConfig{}, nil},
		"native cpu tier":  {EngineVLLM, LaunchConfig{KVOffloadingSize: 8}, prefill},
		"native disk tier": {EngineVLLM, LaunchConfig{KVOffloadingSize: 8, KVDiskOffload: true, SharedKVDir: "/kv"}, prefill},
		"external backend": {EngineVLLM, selectTestExternal(), prefill},
		"sglang cpu tier":  {EngineSGLang, LaunchConfig{KVOffloadingSize: 8}, prefill},
		"unknown backend":  {EngineVLLM, LaunchConfig{KVOffloadBackend: "missing"}, nil},
	} {
		if got := KVOffloadPDRoles(tc.engine, tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("%s: roles = %v, want %v", name, got, tc.want)
		}
	}
}

// A backend whose object carries its own values is recognized by its own
// reader, and the values it reads come back as dictionary values beside the
// selection, so a template exported with non-default values selects the
// backend again instead of leaving an opaque connector in the residual.
func TestKVBackendRecognizerReturnsTheBackendsValues(t *testing.T) {
	withKVBackends(t)
	const segment = "testext_segment_gib"
	object := map[string]any{"kv_connector": "ValuedConnector", "kv_role": "kv_both", "kv_connector_extra_config": map[string]any{"segment_gib": float64(12)}}
	kvBackends.vllmOffload["valued"] = vllmOffloadBackend{
		render: func(Params) *kvContribution { return &kvContribution{object: object} },
		recognize: func(stated map[string]any) (map[string]any, bool) {
			extra, _ := stated["kv_connector_extra_config"].(map[string]any)
			if stated["kv_connector"] != "ValuedConnector" || extra == nil {
				return nil, false
			}
			return map[string]any{segment: extra["segment_gib"]}, true
		},
	}
	native, _ := kvbackend.Builtin().Resolve(EngineVLLM, "")
	registry, err := kvbackend.New(native, testExternalKVBackendRow(), kvbackend.Backend{
		ID: "valued", Engine: EngineVLLM, Label: "Valued", External: true,
		Replaces: testExternalKVBackendRow().Replaces, PDRoles: []string{pdmode.RolePrefill},
	})
	if err != nil {
		t.Fatal(err)
	}
	kvBackends.registry = registry
	stated := `{"kv_connector":"ValuedConnector","kv_role":"kv_both","kv_connector_extra_config":{"segment_gib":24}}`
	params, residual, err := ReverseArgs(EngineVLLM, []string{"--kv-transfer-config", stated})
	if err != nil || params[paramKVOffloadBackend] != "valued" || params[segment] != float64(24) || len(residual) != 0 {
		t.Fatalf("reverse = %v, %v, %v", params, residual, err)
	}
	// The test row has no reader of its own: exactly its rendering is its
	// selection, and nothing else.
	params, _, _ = ReverseArgs(EngineVLLM, []string{"--kv-transfer-config", testExternalKVTransfer})
	if params[paramKVOffloadBackend] != testExternalKVBackend || len(params) != 1 {
		t.Fatalf("test row read as %v", params)
	}
}

// The backend an instance keeps is the deployment's selection as the
// instance's PD role scopes it: what the instance panel names.
func TestKVOffloadBackendOn(t *testing.T) {
	withKVBackends(t)
	for name, tc := range map[string]struct {
		q    LaunchConfig
		role string
		want string
	}{
		"ordinary replica": {selectTestExternal(), "", testExternalKVBackend},
		"prefill keeps it": {selectTestExternal(), pdmode.RolePrefill, testExternalKVBackend},
		"decode drops it":  {selectTestExternal(), pdmode.RoleDecode, ""},
		"engine's own":     {LaunchConfig{KVOffloadingSize: 8}, "", ""},
		"unknown id":       {LaunchConfig{KVOffloadBackend: "missing"}, "", ""},
	} {
		id, label, ok := KVOffloadBackendOn(EngineVLLM, tc.q, tc.role)
		if id != tc.want || ok != (tc.want != "") || (ok && label != "Test External") {
			t.Errorf("%s: %q %q %v, want %q", name, id, label, ok, tc.want)
		}
	}
}
