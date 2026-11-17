package deploy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestComposeVLLMKVTransfer(t *testing.T) {
	pd := &kvContribution{object: map[string]any{"kv_connector": "NixlConnector", "kv_role": "kv_producer"}, role: "kv_producer"}
	disk := &kvContribution{object: map[string]any{"kv_connector": "OffloadingConnector", "kv_role": "kv_both"}}
	wrapped := &kvContribution{object: map[string]any{"kv_connector": "X", "kv_role": "kv_producer"}, wrapper: map[string]any{"kv_connector_module_path": "m", "kv_role": "kv_consumer", "kv_connector": "Y"}}
	cpu := &kvContribution{object: map[string]any{"kv_connector": "OffloadingConnector", "kv_role": "kv_both", "cpu": true}, standaloneAsFlags: true}
	for _, tc := range []struct {
		name        string
		pd, offload *kvContribution
		policy      string
		want        string
	}{
		{"nothing", nil, nil, "fail", ""},
		{"cpu tier alone stays on its flags", nil, cpu, "", ""},
		{"offload object alone", nil, disk, "fail", `{"kv_connector":"OffloadingConnector","kv_role":"kv_both"}`},
		{"pd alone", pd, nil, "fail", `{"kv_connector":"NixlConnector","kv_load_failure_policy":"fail","kv_role":"kv_producer"}`},
		{"pd and cpu", pd, cpu, "", `{"kv_connector":"MultiConnector","kv_connector_extra_config":{"connectors":[{"kv_connector":"NixlConnector","kv_role":"kv_producer"},{"cpu":true,"kv_connector":"OffloadingConnector","kv_role":"kv_both"}]},"kv_role":"kv_producer"}`},
		{"pd and disk with policy on top", pd, disk, "recompute", `{"kv_connector":"MultiConnector","kv_connector_extra_config":{"connectors":[{"kv_connector":"NixlConnector","kv_role":"kv_producer"},{"kv_connector":"OffloadingConnector","kv_role":"kv_both"}]},"kv_load_failure_policy":"recompute","kv_role":"kv_producer"}`},
		{"wrapper keys reach the MultiConnector, the composer's own win", pd, wrapped, "", `{"kv_connector":"MultiConnector","kv_connector_extra_config":{"connectors":[{"kv_connector":"NixlConnector","kv_role":"kv_producer"},{"kv_connector":"X","kv_role":"kv_producer"}]},"kv_connector_module_path":"m","kv_role":"kv_producer"}`},
		{"wrapper keys need a wrapper", nil, wrapped, "", `{"kv_connector":"X","kv_role":"kv_producer"}`},
	} {
		got, ok := composeVLLMKVTransfer(tc.pd, tc.offload, tc.policy)
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, ok, tc.want)
		}
	}
	// The parts are not mutated: the same PD object composes again unchanged.
	if !reflect.DeepEqual(pd.object, map[string]any{"kv_connector": "NixlConnector", "kv_role": "kv_producer"}) {
		raw, _ := json.Marshal(pd.object)
		t.Fatalf("composer mutated the PD part: %s", raw)
	}
}
