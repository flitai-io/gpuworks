package deploy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestReplicasAreCreatedAndRoutableThroughThePublicAPI covers the ordinary
// (non-distributed) multi-instance path. Each replica receives its own port
// and runtime identity, while /v1 routing resolves the shared served name to
// one of the healthy instances.
func TestReplicasAreCreatedAndRoutableThroughThePublicAPI(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("replica-node", testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "replica-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "replica-model", Revision: "main", Name: "replica-model",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "replica")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "replica-model",
		"replicas": 2, "quick_params": map[string]any{"max_model_len": 256},
	})
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create replicas status=%d body=%s", resp.StatusCode, data)
	}
	var dep struct {
		ID       uint `json:"id"`
		Replicas int  `json:"replicas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if dep.ID == 0 || dep.Replicas != 2 {
		t.Fatalf("deployment response=%+v", dep)
	}

	var running []store.Instance
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		rows, listErr := env.Store().ListInstances()
		if listErr == nil && len(rows) == 2 {
			ready := true
			for _, row := range rows {
				if row.State != proto.InstRunning || row.APIPort <= 0 || row.ContainerID == "" {
					ready = false
				}
			}
			if ready && rows[0].APIPort != rows[1].APIPort && rows[0].UUID != rows[1].UUID {
				running = rows
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(running) != 2 {
		depRow, _ := env.Store().GetDeployment(dep.ID)
		rows, _ := env.Store().ListInstances()
		t.Fatalf("replicas did not converge: deployment=%+v instances=%+v", depRow, rows)
	}

	key, err := env.Store().CreateAPIKey("replicas", 0)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"replica-model","messages":[{"role":"user","content":"hi"}]}`)))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	chat, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(chat.Body)
	chat.Body.Close()
	if chat.StatusCode != http.StatusOK || !bytes.Contains(data, []byte(`"choices"`)) {
		t.Fatalf("replica proxy status=%d body=%s instances=%+v", chat.StatusCode, data, running)
	}
}
