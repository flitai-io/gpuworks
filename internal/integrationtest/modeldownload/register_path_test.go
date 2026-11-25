package modeldownload

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/testenv"
)

// Registration adopts an existing node directory; a local storage records
// ownership but does not restrict where those already-present bytes live.
// Exercise the real HTTP -> node RPC -> filesystem path, with no RPC stub.
func TestRegisterPathOutsideStorageThroughRealAgent(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("external-model", testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, node.Name)
	api := env.API(t)
	post := func(t *testing.T, path string, body any, status int) []byte {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := api.PostJSON(path, payload)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != status {
			t.Fatalf("POST %s status=%d want=%d body=%s err=%v", path, resp.StatusCode, status, data, err)
		}
		return data
	}
	var storage, model struct {
		ID uint `json:"id"`
	}
	data := post(t, "/api/v1/storages", map[string]any{
		"name": "registration-owner", "type": "local", "path": t.TempDir(), "all_nodes": true, "enabled": true,
	}, http.StatusCreated)
	if err := json.Unmarshal(data, &storage); err != nil || storage.ID == 0 {
		t.Fatalf("storage=%s err=%v", data, err)
	}
	data = post(t, "/api/v1/models", map[string]any{
		"source": "huggingface", "source_id": "fixture/external-model", "revision": "main",
		"browser_metadata": true, "config_raw": `{}`, "weight_bytes": 7,
	}, http.StatusCreated)
	if err := json.Unmarshal(data, &model); err != nil || model.ID == 0 {
		t.Fatalf("model=%s err=%v", data, err)
	}
	outside := t.TempDir()
	// Metadata must identify the selected model directory.
	if err := os.WriteFile(filepath.Join(outside, "model.safetensors"), []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	const config = `{"model_type":"qwen3"}`
	if err := os.WriteFile(filepath.Join(outside, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint := fmt.Sprintf("/api/v1/models/%d/register-path", model.ID)
	register := func(t *testing.T, path string, status int) []byte {
		return post(t, endpoint, map[string]any{"node_uuid": node.UUID, "storage_id": storage.ID, "path": path}, status)
	}
	if got := string(register(t, outside, http.StatusOK)); got != "{\"status\":\"registered\"}\n" {
		t.Fatalf("registration response=%s", got)
	}
	assertRegistered := func(t *testing.T) {
		t.Helper()
		resp, err := api.Get(fmt.Sprintf("/api/v1/models/%d", model.ID))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var detail struct {
			Nodes []struct {
				NodeUUID  string `json:"node_uuid"`
				StorageID uint   `json:"storage_id"`
				Path      string `json:"path"`
				State     string `json:"state"`
				SizeBytes int64  `json:"size_bytes"`
			} `json:"nodes"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("model detail status=%d err=%v", resp.StatusCode, err)
		}
		if len(detail.Nodes) != 1 || detail.Nodes[0].NodeUUID != node.UUID || detail.Nodes[0].StorageID != storage.ID || detail.Nodes[0].Path != outside || detail.Nodes[0].State != "registered" || detail.Nodes[0].SizeBytes != int64(7+len(config)) {
			t.Fatalf("registered location=%+v", detail.Nodes)
		}
	}
	assertRegistered(t)
	empty := t.TempDir()
	unfinished := t.TempDir()
	if err := os.WriteFile(filepath.Join(unfinished, "model.part"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	child := filepath.Join(parent, "model")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"config.json": config, "model.safetensors": "child weights"} {
		if err := os.WriteFile(filepath.Join(child, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nonmodel := t.TempDir()
	if err := os.WriteFile(filepath.Join(nonmodel, "notes.txt"), []byte("not a model"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		status     int
	}{
		{"parent of model directory", parent, http.StatusConflict},
		{"non-model directory", nonmodel, http.StatusConflict},
		{"missing path parameter", "", http.StatusBadRequest},
		{"missing directory", filepath.Join(outside, "absent"), http.StatusConflict},
		{"empty directory", empty, http.StatusConflict},
		{"unfinished download", unfinished, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			register(t, tc.path, tc.status)
			assertRegistered(t) // A rejected path must not replace the adopted copy.
		})
	}
}
