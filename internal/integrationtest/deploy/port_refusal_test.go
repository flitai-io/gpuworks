package deploy

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// A launch the agent refuses because another process holds its host port is
// retried at once on another port, under the default 15 s retry backoff
// (the budget side is covered in the controller's tests).
//
//	CGO_ENABLED=0 go test -count=1 -run TestARefusedPortIsRetriedWithoutTheBackoff ./internal/integrationtest/deploy/
func TestARefusedPortIsRetriedWithoutTheBackoff(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("refusal-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "refusal-model", Revision: "main", Name: "refusal-model",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "refusal")}); err != nil {
		t.Fatal(err)
	}
	// The next pre-create port check finds its port taken, as a process that
	// started listening after the node's last report would make it.
	node.Driver.FailNext("BindHostPort", &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)})
	body, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "refusal-model",
		"quick_params": map[string]any{"max_model_len": 256},
	})
	started := time.Now()
	resp, err := env.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, data)
	}
	var dep struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(data, &dep); err != nil {
		t.Fatal(err)
	}
	var refused, running *store.Instance
	ok := env.EventuallyNoFail(30*time.Second, func() bool {
		rows, err := env.Store().InstancesForDeployment(dep.ID)
		if err != nil {
			return false
		}
		refused, running = nil, nil
		for i := range rows {
			switch {
			case rows[i].State == proto.InstFailed && rows[i].RuntimeFailure != "" && rows[i].Failure().Code == proto.ErrorEnginePortRefused:
				refused = &rows[i]
			case rows[i].State == proto.InstRunning:
				running = &rows[i]
			}
		}
		return refused != nil && running != nil
	})
	if !ok {
		rows, _ := env.Store().InstancesForDeployment(dep.ID)
		t.Fatalf("want one refused attempt and one running retry: %+v", rows)
	}
	if took := time.Since(started); took >= 15*time.Second {
		t.Fatalf("running after %s: the refused launch waited out the retry backoff", took)
	}
	if refused.Ports == running.Ports {
		t.Fatalf("retry ports %s = the refused attempt's", running.Ports)
	}
}
