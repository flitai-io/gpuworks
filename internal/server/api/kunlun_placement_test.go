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
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An incompatible host may be first in node order or outnumber the trial
// budget. Neither may stop the default draft from finding a Docker host.
func TestKunlunDefaultPlacementSkipsIncompatibleRuntime(t *testing.T) {
	st, err := store.Open("file:kunlun_default_placement?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for index := 0; index < 10; index++ {
		uuid, driver := fmt.Sprintf("p800-%02d", index), "containerd"
		if index == 9 {
			driver = "docker"
		}
		if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: uuid, OS: "linux", Arch: "amd64", RuntimeDriver: driver, GPUVendor: "kunlunxin", Framework: "xre"},
			GPUs: []proto.GPUInfo{{Index: 0, Name: "P800 PCIe", Vendor: "kunlunxin", ArchID: "kl3", MemoryTotalBytes: 96 << 30, Healthy: true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B", WeightBytes: 1519182438,
		Meta: `{"model_type":"qwen3","num_hidden_layers":28,"hidden_size":1024,"num_attention_heads":16,"num_key_value_heads":8,"max_position_embeddings":40960}`})
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	body := fmt.Sprintf(`{"name":"p800-default","model_id":%d,"engine":"sglang","target_vendor":"kunlunxin","image":"example/kunlun:sglang","placement_mode":"auto","quick_params":{}}`, model.ID)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	var result dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !result.OK || result.SelectedNode == nil || result.SelectedNode.UUID != "p800-09" {
		t.Fatalf("default evaluation=%d %s", rec.Code, rec.Body.String())
	}
	for _, node := range result.EligibleNodes {
		if node.UUID == "p800-09" {
			if !node.Eligible {
				t.Fatalf("Docker host excluded: %+v", node)
			}
			continue
		}
		if node.Eligible || len(node.Reasons) == 0 || node.Reasons[0].Code != "runtime_unsupported" {
			t.Fatalf("unsupported runtime offered: %+v", node)
		}
		if node.Reasons[0].Params["required_runtime"] != "docker/nerdctl/kube" || node.Reasons[0].Params["os"] != "linux" || node.Reasons[0].Params["arch"] != "amd64" || node.Reasons[0].Params["runtime"] != "containerd" {
			t.Fatalf("runtime reason params = %+v", node.Reasons[0])
		}
	}
	var request map[string]any
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal(err)
	}
	request["node_uuid"] = result.SelectedNode.UUID
	raw, _ := json.Marshal(request)
	created := httptest.NewRecorder()
	h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(string(raw))))
	if created.Code != http.StatusCreated {
		t.Fatalf("create after default evaluation=%d %s", created.Code, created.Body.String())
	}
	var dep dto.Deployment
	if err := json.Unmarshal(created.Body.Bytes(), &dep); err != nil {
		t.Fatal(err)
	}
	if dep.NodeUUID != "p800-09" || dep.Engine != "sglang" {
		t.Fatalf("created deployment=%+v", dep)
	}
}
