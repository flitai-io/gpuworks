package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployment refused because the weight-derived memory estimate does not fit
// says so with memory_contract_hint: the template could declare the engine's
// real per-card memory (a placement resolver's gpu_memory_mib_per_device),
// which matters for engines that offload weights or quantize at load time.
// The server decides when the hint applies; refusals that are not the
// estimate's - a card count, a resolver that turns the cards down, a figure
// the template already declared - never carry it.
func TestMemoryRefusalNamesThePlacementContractOnlyWhenTheEstimateIsWhatRefused(t *testing.T) {
	st, err := store.Open("file:memory_contract_hint?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("a40", store.HashToken("token"), "a40"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("a40", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "a40", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA A40", Vendor: "nvidia", ArchID: "sm_86", ComputeCapability: "8.6", MemoryTotalBytes: 48 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	const weightBytes = 68026093024 // 63 GiB on a 48 GiB card
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/offload-8b", Name: "offload", WeightBytes: weightBytes})
	if err != nil {
		t.Fatal(err)
	}
	resolver := func(body string) string {
		raw, err := json.Marshal(map[string]string{"language": "starlark", "resolver": "def resolve(gpu):\n" + body})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	spec := func(name, placement string) store.InferenceSpecRow {
		return store.InferenceSpecRow{
			Name: name, Scenario: "balanced", Engine: "vllm", Image: "example/vllm:test", Quantization: "bf16",
			WeightBytes: weightBytes, Placement: placement,
			WeightSources: `[{"provider":"huggingface","repo_id":"acme/offload-8b"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=7.5"}`,
		}
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/offload", DisplayTitle: "Offload", Source: "upstream", Architecture: "qwen4", ContextLength: 32768,
		Specs: []store.InferenceSpecRow{
			spec("estimate", ""),
			spec("contract-too-large", resolver("    return accept(gpu_memory_mib_per_device=61440)\n")),
			spec("resolver-rejects", resolver("    return reject(\"needs a different card\")\n")),
		},
	}}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID("acme/offload")
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	body := func(specName, extra string) string {
		return fmt.Sprintf(`{"name":%q,"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":%q,"engine":"vllm","image":"example/vllm:test","node_uuid":"a40","placement_mode":"manual","quick_params":{}%s}`,
			specName, model.ID, template.ID, specName, extra)
	}
	type refusal struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	create := func(specName, extra string) refusal {
		t.Helper()
		rec := httptest.NewRecorder()
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body(specName, extra))))
		var got refusal
		if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Code != ErrorDeploymentNeverFits {
			t.Fatalf("create %s = %d %s, want %s", specName, rec.Code, rec.Body.String(), ErrorDeploymentNeverFits)
		}
		return got
	}
	// The evaluation's node list refuses the node with the same reason.
	evaluatedReason := func(specName, extra string) refusal {
		t.Helper()
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body(specName, extra))))
		var answer struct {
			EligibleNodes []struct {
				UUID    string    `json:"uuid"`
				Reasons []refusal `json:"reasons"`
			} `json:"eligible_nodes"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &answer) != nil {
			t.Fatalf("evaluate %s = %d %s", specName, rec.Code, rec.Body.String())
		}
		for _, node := range answer.EligibleNodes {
			if node.UUID == "a40" && len(node.Reasons) > 0 {
				return node.Reasons[len(node.Reasons)-1]
			}
		}
		t.Fatalf("evaluate %s: the a40 node carries no refusal: %s", specName, rec.Body.String())
		return refusal{}
	}

	for _, tc := range []struct {
		name, spec, extra string
		reason            string // the node's reason code
		hint              bool
	}{
		{name: "estimate alone", spec: "estimate", reason: "no_fit", hint: true},
		{name: "contract already declared", spec: "contract-too-large", reason: "no_fit"},
		{name: "resolver rejects the card", spec: "resolver-rejects", reason: "placement_rejected"},
		{name: "more cards than the node has", spec: "estimate", extra: `,"placement":{"tp":2}`, reason: "insufficient_empty_gpus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused := create(tc.spec, tc.extra)
			if refused.Params["reason"] != tc.reason || refused.Params["node"] != "a40" {
				t.Fatalf("params = %v, want node a40 refused for %s", refused.Params, tc.reason)
			}
			hint, hinted := refused.Params["memory_contract_hint"]
			if hinted != tc.hint || (hinted && hint != true) {
				t.Fatalf("create params = %v, want memory_contract_hint=%v", refused.Params, tc.hint)
			}
			reasonParams, _ := refused.Params["reason_params"].(map[string]any)
			nested, nestedHint := reasonParams["memory_contract_hint"]
			if nestedHint != tc.hint || (nestedHint && nested != true) {
				t.Fatalf("reason_params = %v, want memory_contract_hint=%v", refused.Params["reason_params"], tc.hint)
			}
			node := evaluatedReason(tc.spec, tc.extra)
			if node.Code != tc.reason {
				t.Fatalf("evaluate reason = %+v, want %s", node, tc.reason)
			}
			if _, ok := node.Params["memory_contract_hint"]; ok != tc.hint {
				t.Fatalf("evaluate reason params = %v, want memory_contract_hint=%v", node.Params, tc.hint)
			}
		})
	}

	// The wire form of the refusal a client renders the hint from.
	refused := create("estimate", "")
	raw, _ := json.Marshal(refused.Params["reason_params"])
	if !strings.Contains(string(raw), `"memory_contract_hint":true`) {
		t.Fatalf("reason_params = %s", raw)
	}
}
