package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The spec declares two mirrors; one is already registered and the server
// suggests the ModelScope one. Both facts travel with the option, and the
// registration choice itself stays a client action against the register API.
func TestEvaluateWeightSourcesWire(t *testing.T) {
	h, templateID := eligibleNodeFixture(t)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","node_uuid":"eligible-node"}`, templateID))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		WeightSources []dto.WeightSourceOption `json:"weight_sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.WeightSources) != 2 {
		t.Fatalf("weight_sources = %+v, want the two declared mirrors", response.WeightSources)
	}
	byProvider := map[string]dto.WeightSourceOption{}
	for _, option := range response.WeightSources {
		byProvider[option.Provider] = option
	}
	registered := byProvider["modelscope"]
	if registered.RepoID != "Acme/Deploy-FP8" || !registered.Registered {
		t.Fatalf("the seeded modelscope row must be registered: %+v", registered)
	}
	other := byProvider["huggingface"]
	if other.RepoID != "Acme/Deploy-FP8" {
		t.Fatalf("both mirrors publish the same repository id: %+v", other)
	}
	// Repository identity is what registration means: the seeded modelscope
	// row serves the repo, so every mirror of it reports registered.
	if !other.Registered {
		t.Fatalf("a registered repo id serves every mirror: %+v", other)
	}
	if !registered.Suggested && !other.Suggested {
		t.Fatalf("exactly the server's suggestion carries suggested: %+v", response.WeightSources)
	}
	encoded, err := json.Marshal(dto.WeightSourceOption{Provider: "modelscope", RepoID: "acme/x", Suggested: true, Registered: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"provider":"modelscope","repo_id":"acme/x","suggested":true,"registered":true}` {
		t.Fatalf("wire form = %s", encoded)
	}
}

// A single-node draft answers no preflight at all; a multi-node draft answers
// the structural summary, and the summary names the concrete rule that failed.
func TestEvaluateInlinePreflightWire(t *testing.T) {
	st, err := store.Open("file:evaluate_preflight_inline?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/preflight", Revision: "main", Name: "preflight", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"preflight-a", "preflight-b", "preflight-c"} {
		launchConfigNode(t, st, uuid)
		if err := st.UpdateNodeStatus(uuid, nodeStatusTwoGPUs(uuid, i)); err != nil {
			t.Fatal(err)
		}
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	h := &apiHandlers{d: Deps{Store: st}}
	evaluate := func(body string) (bool, *dto.PreflightSummary) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		var response struct {
			OK        bool                  `json:"ok"`
			Preflight *dto.PreflightSummary `json:"preflight"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.OK, response.Preflight
	}
	single := fmt.Sprintf(`{"model_id":%d,"node_uuid":"preflight-a","engine":"vllm"}`, model.ID)
	if ok, preflight := evaluate(single); !ok || preflight != nil {
		t.Fatalf("a single-node draft answers no preflight: ok=%v preflight=%+v", ok, preflight)
	}
	ok, preflight := evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["preflight-a","preflight-b"],"placement":{"tp":2}}`, model.ID))
	if !ok || preflight == nil || !preflight.OK || len(preflight.Checks) != 0 {
		t.Fatalf("two healthy nodes preflight clean: ok=%v %+v", ok, preflight)
	}
	ok, preflight = evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["preflight-a","preflight-a"],"placement":{"tp":2}}`, model.ID))
	if ok || preflight == nil || preflight.OK {
		t.Fatalf("a duplicate node must fail the summary: ok=%v %+v", ok, preflight)
	}
	if preflight.Checks[0].Code != ErrorPreflightDuplicateNode || preflight.Checks[0].Params["node"] != "preflight-a" {
		t.Fatalf("duplicate check = %+v", preflight.Checks[0])
	}
	ok, preflight = evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","nnodes":2,"node_uuids":["preflight-a","preflight-b"],"master_port":99,"placement":{"tp":2}}`, model.ID))
	if ok || preflight == nil || preflight.OK || preflight.Checks[0].Code != ErrorPreflightPortInvalid {
		t.Fatalf("an out-of-range master port must fail the summary: ok=%v %+v", ok, preflight)
	}
}

func nodeStatusTwoGPUs(hostname string, index int) proto.NodeStatus {
	gpus := func() []proto.GPUInfo {
		out := []proto.GPUInfo{}
		for index := 0; index < 2; index++ {
			gpu := proto.GPUInfo{Index: index, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}
			out = append(out, gpu)
		}
		return out
	}()
	return proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: hostname, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"}, Network: testManagement(fmt.Sprintf("10.0.0.%d", index+1)),
		GPUs: gpus,
	}
}
