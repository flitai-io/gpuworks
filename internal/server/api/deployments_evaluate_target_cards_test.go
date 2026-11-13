package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type targetEligibleNode struct {
	UUID     string `json:"uuid"`
	Eligible bool   `json:"eligible"`
	Reasons  []struct {
		Code string `json:"code"`
	} `json:"reasons"`
}

func targetEligibleWire(t *testing.T, st *store.Store, body string) map[string]targetEligibleNode {
	t.Helper()
	signer, err := auth.NewTokenSigner("amd-target-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("evaluate status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		EligibleNodes []targetEligibleNode `json:"eligible_nodes"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]targetEligibleNode, len(response.EligibleNodes))
	for _, node := range response.EligibleNodes {
		out[node.UUID] = node
	}
	return out
}

func TestEvaluateEligibleNodesAMDSleepWire(t *testing.T) {
	h, _ := eligibleNodeFixture(t)
	uuid := "amd-sleep-card"
	if _, err := h.d.Store.RegisterNode(uuid, store.HashToken("amd-sleep-token"), uuid); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Store.UpdateNodeStatus(uuid, proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: uuid, OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUVendor: "amd"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "AMD Instinct MI300X", Vendor: "amd", ArchID: "gfx942", MemoryTotalBytes: 96 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	model, err := h.d.Store.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-FP8", Revision: "main", Name: "Acme/Deploy-FP8", WeightBytes: 16 << 30})
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","sleepable":true}`, model.ID)
	nodes := targetEligibleWire(t, h.d.Store, body)
	node, ok := nodes[uuid]
	// vLLM sleep mode runs on ROCm, so a sleepable draft keeps the AMD card.
	if !ok || !node.Eligible || len(node.Reasons) != 0 {
		t.Fatalf("AMD eligible_nodes=%+v", node)
	}
}

func TestEvaluateEligibleNodesMixedTemplateRuntimeWire(t *testing.T) {
	h, _ := eligibleNodeFixture(t)
	uuid := "amd-nvidia-mixed-card"
	if _, err := h.d.Store.RegisterNode(uuid, store.HashToken("mixed-target-token"), uuid); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Store.UpdateNodeStatus(uuid, proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: uuid, OS: "linux", Arch: "amd64", GPUVendor: "", RuntimeDriver: ""},
		GPUs: []proto.GPUInfo{
			{Index: 0, Name: "AMD Instinct MI300X", Vendor: "amd", ArchID: "gfx942", MemoryTotalBytes: 96 << 30, Healthy: true},
			{Index: 1, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	catalog := deploymentFlatFixture()
	catalog.ModelID = "acme/mixed-target-cards"
	catalog.Specs[0].Name = "amd-only"
	catalog.Specs[0].GPUs = `{"vendor":"amd"}`
	nvidia := catalog.Specs[0]
	nvidia.Name = "nvidia-only"
	nvidia.GPUs = `{"vendor":"nvidia"}`
	catalog.Specs = append(catalog.Specs, nvidia)
	if err := h.d.Store.ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	model, err := h.d.Store.InferenceModelByModelID(catalog.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		spec     string
		eligible bool
		reason   string
	}{
		{"amd-only", false, "runtime_unreported"}, {"nvidia-only", true, ""},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			body := fmt.Sprintf(`{"template_id":%d,"template_spec_name":%q,"engine":"vllm","placement_mode":"auto"}`, model.ID, tc.spec)
			nodes := targetEligibleWire(t, h.d.Store, body)
			node, ok := nodes[uuid]
			if !ok || node.Eligible != tc.eligible {
				t.Fatalf("%s eligible_nodes=%+v", tc.spec, node)
			}
			if tc.reason != "" {
				if len(node.Reasons) == 0 || node.Reasons[0].Code != tc.reason {
					t.Fatalf("%s reasons=%+v", tc.spec, node.Reasons)
				}
			} else if len(node.Reasons) != 0 {
				t.Fatalf("%s unexpected reasons=%+v", tc.spec, node.Reasons)
			}
		})
	}
}
