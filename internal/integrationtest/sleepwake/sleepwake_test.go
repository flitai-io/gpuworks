package sleepwake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestSleepWakeRouteAndProxy exercises the complete L1 path with the real app,
// controller, agent and proxy.  Only the GPU probe and runtime are simulated.
func TestSleepWakeRouteAndProxy(t *testing.T) {
	env := testenv.NewOnDisk(t)
	env.AddNode("sleep-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, "sleep-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "sleep-model", Arch: "qwen3",
		WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "sleep-model", "sleepable": true,
		"quick_params": map[string]any{"max_model_len": 512},
	})
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create deployment: %d %s", resp.StatusCode, b)
	}
	var dep struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dep); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if dep.ID == 0 {
		t.Fatal("deployment id is zero")
	}
	// Judge the deployment's own running row, never the total instance
	// count: the controller can burn a failed attempt before one sticks (a
	// port the previous suite member's engine still holds makes the first
	// bind fail), and counting rows would then wait out every deadline even
	// though the retry is already serving.
	running := func() (store.Instance, bool) {
		instances, _ := env.Store().ListInstances()
		for _, instance := range instances {
			if instance.DeploymentID == dep.ID && instance.State == proto.InstRunning {
				return instance, true
			}
		}
		return store.Instance{}, false
	}
	env.Eventually(t, 30*time.Second, func() bool {
		instance, ok := running()
		return ok && instance.SleepLevel == 0
	})
	if instance, ok := running(); !ok || instance.SleepLevel != 0 {
		instances, _ := env.Store().ListInstances()
		t.Fatalf("instance did not become awake: %+v", instances)
	}

	key, err := env.Store().CreateAPIKey("sleepwake", 0)
	if err != nil {
		t.Fatal(err)
	}
	chat := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"sleep-model","messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		res, reqErr := http.DefaultClient.Do(req)
		if reqErr != nil {
			t.Fatalf("chat: %v", reqErr)
		}
		return res
	}
	res := chat()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("awake chat: %d %s", res.StatusCode, b)
	}
	res.Body.Close()

	sleep, err := api.PostJSON("/api/v1/deployments/"+itoa(dep.ID)+"/sleep?level=1", nil)
	if err != nil || sleep.StatusCode != http.StatusOK {
		if sleep != nil {
			sleep.Body.Close()
		}
		t.Fatalf("sleep: status=%v err=%v", status(sleep), err)
	}
	sleep.Body.Close()
	env.Eventually(t, 10*time.Second, func() bool {
		instance, ok := running()
		return ok && instance.SleepLevel == 1
	})
	// A control-plane restart must adopt the still-sleeping runtime and retain
	// the desired/actual sleep level instead of waking or recreating it.
	env.RestartServer(t)
	env.Eventually(t, 15*time.Second, func() bool {
		instance, ok := running()
		return ok && instance.SleepLevel == 1
	})
	res = chat()
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || !bytes.Contains(b, []byte("asleep")) {
		t.Fatalf("sleeping chat: %d %s", res.StatusCode, b)
	}

	wake, err := api.PostJSON("/api/v1/deployments/"+itoa(dep.ID)+"/wake", nil)
	if err != nil || wake.StatusCode != http.StatusOK {
		if wake != nil {
			wake.Body.Close()
		}
		t.Fatalf("wake: status=%v err=%v", status(wake), err)
	}
	wake.Body.Close()
	env.Eventually(t, 45*time.Second, func() bool {
		instance, ok := running()
		return ok && instance.SleepLevel == 0
	})
	res = chat()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("woken chat: %d %s", res.StatusCode, b)
	}
	res.Body.Close()
}

func itoa(v uint) string {
	// Avoid strconv noise in the assertions while keeping URL construction
	// explicit and portable.
	return strconv.FormatUint(uint64(v), 10)
}

func status(resp *http.Response) any {
	if resp == nil {
		return "<nil>"
	}
	return resp.StatusCode
}
