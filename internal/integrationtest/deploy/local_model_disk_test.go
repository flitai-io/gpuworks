package deploy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// Existing bytes alone are not a registered model copy. Exercise the actual
// evaluate -> register-path RPC -> evaluate -> deploy HTTP flow so a disk
// refusal both explains how to proceed and stops reserving a second copy once
// the operator has adopted the directory under the original model identity.
func TestExistingModelDirectoryCanBeRegisteredAfterDiskRefusal(t *testing.T) {
	const (
		repo        = "fixture/local-existing"
		weightBytes = int64(1 << 30)
		freeBytes   = int64(1 << 20)
		config      = `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`
	)
	var weightRequests atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/" + repo + "/tree/main":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":%d},{"type":"file","path":"model.safetensors","size":%d}]`, len(config), weightBytes)
		case "/" + repo + "/resolve/main/config.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, config)
		case "/" + repo + "/resolve/main/model.safetensors":
			weightRequests.Add(1)
			http.Error(w, "existing local weights must not be downloaded", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	env := testenv.NewWithModelEndpoint(t, hub.URL)
	node := env.AddNode("local-disk-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("fake"))
	env.WaitNodeOnline(t, node.Name)
	api := env.API(t)
	post := func(path string, body any, status int) []byte {
		t.Helper()
		response, err := api.PostJSON(path, mustJSON(t, body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status {
			t.Fatalf("POST %s status=%d want=%d body=%s err=%v", path, response.StatusCode, status, data, err)
		}
		return data
	}
	get := func(path string, out any) {
		t.Helper()
		response, err := api.Get(path)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status=%d", path, response.StatusCode)
		}
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	var storage, model struct {
		ID uint `json:"id"`
	}
	storageRoot := t.TempDir()
	downloadTarget := filepath.Join(storageRoot, "huggingface", "fixture_local-existing-main")
	if err := json.Unmarshal(post("/api/v1/storages", map[string]any{
		"name": "existing-weights", "type": "local", "path": storageRoot, "all_nodes": true, "enabled": true,
	}, http.StatusCreated), &storage); err != nil || storage.ID == 0 {
		t.Fatalf("storage id=%d err=%v", storage.ID, err)
	}
	if err := json.Unmarshal(post("/api/v1/models", map[string]any{
		"source": "huggingface", "source_id": repo, "revision": "main", "name": "existing-model",
		"browser_metadata": true, "config_raw": config, "weight_bytes": weightBytes,
	}, http.StatusCreated), &model); err != nil || model.ID == 0 {
		t.Fatalf("model id=%d err=%v", model.ID, err)
	}
	// The directory predates the model-file registration and can live outside
	// the managed download root. A sparse file retains the real declared length
	// without consuming a GiB of test-host disk or requiring a public download.
	existingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(existingDir, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	weightsPath := filepath.Join(existingDir, "model.safetensors")
	weights, err := os.OpenFile(weightsPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	header := `{"weight":{"dtype":"U8","shape":[1073741736],"data_offsets":[0,1073741736]}}`
	header += strings.Repeat(" ", (8-len(header)%8)%8)
	prefix := make([]byte, 8+len(header))
	binary.LittleEndian.PutUint64(prefix[:8], uint64(len(header)))
	copy(prefix[8:], header)
	// The aligned header and length prefix take 88 bytes; the tensor payload
	// fills the remainder of the exact 1 GiB length advertised by the fixture.
	fileBytes := weightBytes
	if err := weights.Truncate(fileBytes); err != nil {
		_ = weights.Close()
		t.Fatal(err)
	}
	if _, err := weights.WriteAt(prefix, 0); err != nil {
		_ = weights.Close()
		t.Fatal(err)
	}
	if err := weights.Close(); err != nil {
		t.Fatal(err)
	}
	var beforeRegistration struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	get(fmt.Sprintf("/api/v1/models/%d", model.ID), &beforeRegistration)
	if len(beforeRegistration.Nodes) != 0 {
		t.Fatalf("physical files unexpectedly became registered copies: %s", beforeRegistration.Nodes)
	}
	before, err := os.Stat(weightsPath)
	if err != nil {
		t.Fatal(err)
	}
	// Supply a last observed node status through the same store boundary that
	// consumes agent status.node. The real agent/RPC remains in place; only the
	// external capacity fact is constrained, never the host filesystem.
	lowDisk := func() {
		t.Helper()
		gpus, err := node.Prober.Probe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := env.Store().UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUs: gpus, PathUsage: []proto.PathUsage{
			{Kind: "models", Path: storageRoot, TotalBytes: 2 << 30, AvailableBytes: freeBytes},
			{Kind: "storage", StorageID: storage.ID, Path: storageRoot, TotalBytes: 2 << 30, AvailableBytes: freeBytes},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	type reason struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	type evaluation struct {
		OK   bool `json:"ok"`
		Plan struct {
			Message reason `json:"message"`
		} `json:"plan"`
		Candidates []struct {
			OK       bool   `json:"ok"`
			NodeUUID string `json:"node_uuid"`
			Reason   reason `json:"reason"`
		} `json:"candidates"`
		Diagnostics struct {
			Checks []struct {
				Message reason `json:"message"`
			} `json:"checks"`
		} `json:"diagnostics"`
		Acquisition struct {
			Nodes []struct {
				WeightsPresent bool   `json:"weights_present"`
				WeightsPlan    string `json:"weights_plan"`
			} `json:"nodes"`
		} `json:"acquisition"`
	}
	body := map[string]any{"model_id": model.ID, "node_uuid": node.UUID, "engine": "vllm", "image": "example/local-existing:v1",
		"model_source": map[string]any{"storage_id": storage.ID, "provider": "huggingface"}, "placement_mode": "manual", "target_vendor": "nvidia", "quick_params": map[string]any{"max_model_len": 512}}
	evaluate := func() evaluation {
		t.Helper()
		lowDisk()
		var out evaluation
		if err := json.Unmarshal(post("/api/v1/evaluate", body, http.StatusOK), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	refused := evaluate()
	if refused.OK || refused.Plan.Message.Code != "insufficient_disk" {
		t.Fatalf("unregistered local directory did not produce disk refusal: %+v", refused)
	}
	want := map[string]any{"required_bytes": float64(weightBytes), "available_bytes": float64(freeBytes), "node": node.Name, "node_uuid": node.UUID,
		"model": "existing-model", "model_id": float64(model.ID), "storage": "existing-weights", "storage_id": float64(storage.ID), "storage_type": "local",
		"path": downloadTarget}
	assertFacts := func(label string, got map[string]any) {
		t.Helper()
		for key, value := range want {
			if got[key] != value {
				t.Fatalf("%s %s=%v want=%v; params=%+v", label, key, got[key], value, got)
			}
		}
	}
	assertFacts("plan disk refusal", refused.Plan.Message.Params)
	for _, check := range refused.Diagnostics.Checks {
		if check.Message.Params["reason"] == "insufficient_disk" || check.Message.Code == "insufficient_disk" {
			assertFacts("diagnostic disk refusal", check.Message.Params)
		}
	}
	// The automatic node picker displays candidate.reason, rather than the
	// manual preview's plan.message; both public wire paths must retain facts.
	autoBody := make(map[string]any, len(body))
	for key, value := range body {
		if key != "node_uuid" {
			autoBody[key] = value
		}
	}
	autoBody["placement_mode"] = "auto"
	lowDisk()
	var automatic evaluation
	if err := json.Unmarshal(post("/api/v1/evaluate", autoBody, http.StatusOK), &automatic); err != nil {
		t.Fatal(err)
	}
	if len(automatic.Candidates) != 1 || automatic.Candidates[0].OK || automatic.Candidates[0].NodeUUID != node.UUID || automatic.Candidates[0].Reason.Code != "insufficient_disk" {
		t.Fatalf("automatic disk refusal candidates=%+v ok=%v", automatic.Candidates, automatic.OK)
	}
	assertFacts("candidate disk refusal", automatic.Candidates[0].Reason.Params)
	if got := string(post(fmt.Sprintf("/api/v1/models/%d/register-path", model.ID), map[string]any{
		"node_uuid": node.UUID, "storage_id": storage.ID, "path": existingDir,
	}, http.StatusOK)); got != "{\"status\":\"registered\"}\n" {
		t.Fatalf("register-path body=%s", got)
	}
	ready := evaluate()
	if !ready.OK || len(ready.Acquisition.Nodes) != 1 || !ready.Acquisition.Nodes[0].WeightsPresent || ready.Acquisition.Nodes[0].WeightsPlan != "present" {
		t.Fatalf("registered existing directory still reserves a download: %+v", ready)
	}
	var registered struct {
		ID       uint   `json:"id"`
		Source   string `json:"source"`
		SourceID string `json:"source_id"`
		Revision string `json:"revision"`
		Nodes    []struct {
			Path  string `json:"path"`
			State string `json:"state"`
		} `json:"nodes"`
	}
	get(fmt.Sprintf("/api/v1/models/%d", model.ID), &registered)
	if registered.ID != model.ID || registered.Source != "huggingface" || registered.SourceID != repo || registered.Revision != "main" || len(registered.Nodes) != 1 || registered.Nodes[0].Path != existingDir || registered.Nodes[0].State != "registered" {
		t.Fatalf("registration changed identity or adopted a different path: %+v", registered)
	}
	var launched atomic.Pointer[runtime.WorkloadSpec]
	node.Driver.AfterRun(func(spec runtime.WorkloadSpec) { launched.Store(&spec) })
	body["name"], body["served_name"] = "reuse-existing-model", "reuse-existing-model"
	lowDisk()
	var created struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(post("/api/v1/deployments", body, http.StatusCreated), &created); err != nil || created.ID == 0 {
		t.Fatalf("deployment id=%d err=%v", created.ID, err)
	}
	var detail struct {
		ModelID   uint `json:"model_id"`
		Instances []struct {
			State string `json:"state"`
		} `json:"instances"`
	}
	env.Eventually(t, 30*time.Second, func() bool {
		get(fmt.Sprintf("/api/v1/deployments/%d", created.ID), &detail)
		return len(detail.Instances) == 1 && detail.Instances[0].State == "running"
	})
	if detail.ModelID != model.ID || launched.Load() == nil {
		t.Fatalf("deployment identity=%d want=%d launch=%v", detail.ModelID, model.ID, launched.Load())
	}
	mounted := false
	for _, mount := range launched.Load().Mounts {
		if mount.Source == existingDir && mount.Target == "/model" {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("workload did not mount existing directory: %+v", launched.Load().Mounts)
	}
	after, err := os.Stat(weightsPath)
	if err != nil || !os.SameFile(before, after) || after.Size() != fileBytes {
		t.Fatalf("registration/deployment replaced or copied existing weights: stat=%v err=%v", after, err)
	}
	if _, err := os.Stat(downloadTarget); !os.IsNotExist(err) {
		t.Fatalf("deployment created a redundant managed download target: %s err=%v", downloadTarget, err)
	}
	if weightRequests.Load() != 0 {
		t.Fatalf("existing registered weights caused %d forbidden weight requests", weightRequests.Load())
	}
}
