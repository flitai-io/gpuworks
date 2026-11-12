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

// cardDefaultNode is a node whose cards are given one by one, so a case can
// state the hardware its expectation is about.
func cardDefaultNode(t *testing.T, st *store.Store, uuid string, gpus ...proto.GPUInfo) {
	t.Helper()
	if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"},
		GPUs: gpus,
	}); err != nil {
		t.Fatal(err)
	}
}

func nvidiaCard(index int, name, cc string, totalGiB int64) proto.GPUInfo {
	return proto.GPUInfo{
		Index: index, Name: name, Vendor: "nvidia", ComputeCapability: cc,
		MemoryTotalBytes: totalGiB << 30, Healthy: true,
	}
}

// The form is answered by evaluate and the deployment is written by create, so
// a share derived from the card has to come out of both the same. A difference
// here is the failure this rule is most exposed to: the deployer would approve
// one number and run another.
func TestEvaluateAndCreateAgreeOnTheCardDerivedShare(t *testing.T) {
	for _, tc := range []struct {
		name string
		gpu  proto.GPUInfo
		want float64
	}{
		{"a 12 GiB card", nvidiaCard(0, "GeForce RTX 3080", "8.6", 12), 0.83},
		{"a unified-memory card", nvidiaCard(0, "GB10", "12.1", 128), 0.8},
		{"an 80 GiB card is unchanged", nvidiaCard(0, "A100-SXM4-80GB", "8.0", 80), 0.9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:card_default_"+strings.ReplaceAll(tc.name, " ", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			cardDefaultNode(t, st, "card-node", tc.gpu)
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "org/qwen", Revision: "main", Name: "org/qwen",
				WeightBytes: 2 << 30,
				Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			handler := &apiHandlers{d: Deps{Store: st}}
			body := fmt.Sprintf(`{"name":"card-share","model_id":%d,"node_uuid":"card-node","engine":"vllm","image":"vllm/vllm-openai:latest","quick_params":{"max_model_len":512}}`, model.ID)

			rec := httptest.NewRecorder()
			handler.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("evaluate=%d: %s", rec.Code, rec.Body.String())
			}
			var evaluated struct {
				FormValues      map[string]any `json:"form_values"`
				EffectiveParams map[string]any `json:"effective_params"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &evaluated); err != nil {
				t.Fatal(err)
			}
			// form_values is what the form sets its fields from, and
			// effective_params is what the estimate was taken against; the
			// deployer reads the first and trusts the second.
			for field, values := range map[string]map[string]any{"form_values": evaluated.FormValues, "effective_params": evaluated.EffectiveParams} {
				if got := values["gpu_memory_utilization"]; got != tc.want {
					t.Errorf("evaluate %s = %v, want %v", field, got, tc.want)
				}
			}

			created := httptest.NewRecorder()
			handler.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if created.Code != http.StatusCreated {
				t.Fatalf("create=%d: %s", created.Code, created.Body.String())
			}
			var deployment dto.Deployment
			if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
				t.Fatal(err)
			}
			stored, err := st.GetDeployment(deployment.ID)
			if err != nil {
				t.Fatal(err)
			}
			var quick map[string]any
			if err := json.Unmarshal([]byte(stored.QuickParams), &quick); err != nil {
				t.Fatalf("stored quick_params %q: %v", stored.QuickParams, err)
			}
			if got := quick["gpu_memory_utilization"]; got != tc.want {
				t.Fatalf("the created deployment stored %v, want the %v evaluate showed", got, tc.want)
			}
		})
	}
}

// Pinning the GPUs says which cards will run the deployment, so the share is
// taken against those and not against the node's smallest card. On a mixed
// node the difference is the whole point: the big card keeps its share.
func TestPinnedGPUsDecideTheShareOnAMixedNode(t *testing.T) {
	st, err := store.Open("file:card_default_mixed?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cardDefaultNode(t, st, "mixed-node",
		nvidiaCard(0, "Quadro RTX 8000", "7.5", 48),
		nvidiaCard(1, "GeForce RTX 2080 Ti", "7.5", 11),
	)
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "org/qwen", Revision: "main", Name: "org/qwen",
		WeightBytes: 2 << 30,
		Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := &apiHandlers{d: Deps{Store: st}}
	for _, tc := range []struct {
		name      string
		placement string
		want      float64
	}{
		{"pinned to the 48 GiB card", `,"placement":{"gpus":[0],"tp":1}`, 0.9},
		{"pinned to the 11 GiB card", `,"placement":{"gpus":[1],"tp":1}`, 0.81},
		// Nothing pinned: the deployment may land on either card, so the
		// smaller one binds. It reserves more than the big card needs, which
		// is the side to err on - the other way round leaves the small card
		// short of the reserve.
		{"nothing pinned", "", 0.81},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"name":"mixed","model_id":%d,"node_uuid":"mixed-node","engine":"vllm","image":"vllm/vllm-openai:latest","quick_params":{"max_model_len":512}%s}`, model.ID, tc.placement)
			rec := httptest.NewRecorder()
			handler.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("evaluate=%d: %s", rec.Code, rec.Body.String())
			}
			var evaluated struct {
				FormValues map[string]any `json:"form_values"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &evaluated); err != nil {
				t.Fatal(err)
			}
			if got := evaluated.FormValues["gpu_memory_utilization"]; got != tc.want {
				t.Fatalf("form_values = %v, want %v", got, tc.want)
			}
		})
	}
}
