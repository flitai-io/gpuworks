package engines

import (
	"slices"
	"testing"
)

// TestVLLMKVTransferConfigClassifiesPDByConnector: a --kv-transfer-config is
// a PD request only when its connector moves KV between instances. vLLM's
// offload connector also states a kv_role (kv_both), and a KV tier on one
// instance is not prefill/decode disaggregation.
func TestVLLMKVTransferConfigClassifiesPDByConnector(t *testing.T) {
	offload := `{"kv_connector":"OffloadingConnector","kv_role":"kv_both","kv_connector_extra_config":{"cpu_bytes_to_use":8589934592}}`
	nixlProducer := `{"kv_connector":"NixlConnector","kv_role":"kv_producer"}`
	for _, tc := range []struct {
		name   string
		config string
		pd     bool
		known  bool
	}{
		{"offload kv_both", offload, false, true},
		{"offload kv_producer", `{"kv_connector":"OffloadingConnector","kv_role":"kv_producer"}`, false, true},
		{"multi of offload only", `{"kv_connector":"MultiConnector","kv_role":"kv_both","kv_connector_extra_config":{"connectors":[` + offload + `]}}`, false, true},
		{"multi of PD and offload", `{"kv_connector":"MultiConnector","kv_role":"kv_producer","kv_connector_extra_config":{"connectors":[` + nixlProducer + `,` + offload + `]}}`, true, true},
		{"multi without children", `{"kv_connector":"MultiConnector","kv_role":"kv_both"}`, true, true},
		{"nixl producer", nixlProducer, true, true},
		{"nixl consumer", `{"kv_connector":"NixlConnector","kv_role":"kv_consumer"}`, true, true},
		{"nixl kv_both", `{"kv_connector":"NixlConnector","kv_role":"kv_both"}`, true, true},
		{"third-party kv_both", `{"kv_connector":"LMCacheConnectorV1","kv_role":"kv_both"}`, true, true},
		{"mooncake producer", `{"kv_connector":"MooncakeConnector","kv_role":"kv_producer"}`, true, true},
		{"no connector", `{"kv_role":"kv_both"}`, true, true},
		{"connector not a string", `{"kv_connector":7,"kv_role":"kv_both"}`, true, true},
		{"children not a list", `{"kv_connector":"MultiConnector","kv_role":"kv_both","kv_connector_extra_config":{"connectors":"OffloadingConnector"}}`, true, true},
		{"extra config not an object", `{"kv_connector":"OffloadingConnector","kv_role":"kv_both","kv_connector_extra_config":"x"}`, true, true},
		{"nested multi", `{"kv_connector":"MultiConnector","kv_role":"kv_both","kv_connector_extra_config":{"connectors":[{"kv_connector":"MultiConnector","kv_role":"kv_both"}]}}`, true, true},
		{"offload without role", `{"kv_connector":"OffloadingConnector"}`, false, false},
		{"offload with invalid role", `{"kv_connector":"OffloadingConnector","kv_role":"both"}`, false, false},
		{"nixl without role", `{"kv_connector":"NixlConnector"}`, false, false},
		{"not json", `kv_both`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, args := range [][]string{
				{"vllm", "serve", "/model", "--kv-transfer-config", tc.config},
				{"vllm", "serve", "/model", "--kv-transfer-config=" + tc.config},
			} {
				features, known := CommandFeatures(VLLM, args)
				if known != tc.known || slices.Contains(features, PD) != tc.pd {
					t.Fatalf("%v: features=%v known=%v, want PD=%v known=%v", args, features, known, tc.pd, tc.known)
				}
			}
		})
	}
}

// SGLang's PD flag is unaffected: the classification is vLLM's connector.
func TestSGLangDisaggregationModeStaysPD(t *testing.T) {
	features, known := CommandFeatures(SGLang, []string{"python3", "-m", "sglang.launch_server", "--disaggregation-mode", "prefill"})
	if !known || !slices.Contains(features, PD) {
		t.Fatalf("features=%v known=%v", features, known)
	}
}
