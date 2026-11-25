package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestDeploymentRunsThroughRealAppAndFakeDriver(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("deploy-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	var ran runtime.WorkloadSpec
	var pulled atomic.Int32
	node.Driver.AfterRun(func(ws runtime.WorkloadSpec) { ran = ws })
	node.Driver.AfterPullImage(func(string) { pulled.Add(1) })
	env.WaitNodeOnline(t, "deploy-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %+v, err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "zstackai/qwen3-4b-fake", Revision: "main", Name: "qwen3-4b-fake", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "fake")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "fake", "quick_params": map[string]any{"max_model_len": 512}})
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment status = %d", resp.StatusCode)
	}
	var created struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == 0 {
		t.Fatal("create deployment returned no id")
	}
	instances, _ := env.Store().ListInstances()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && (len(instances) != 1 || instances[0].State != proto.InstRunning) {
		time.Sleep(100 * time.Millisecond)
		instances, _ = env.Store().ListInstances()
	}
	if len(instances) != 1 || instances[0].State != proto.InstRunning {
		t.Fatalf("instance did not reach running: %+v", instances)
	}
	if ran.Name == "" || ran.Image == "" || len(ran.Mounts) == 0 || ran.Mounts[0].Target != "/model" {
		t.Fatalf("AfterRun did not observe assembled workload: %+v", ran)
	}
	if pulled.Load() == 0 {
		t.Fatal("deployment skipped the fake runtime image-pull path")
	}
	instances, _ = env.Store().ListInstances()
	if instances[0].ContainerID == "" || instances[0].DirectAddr == "" {
		t.Fatalf("running instance missing runtime details: %+v", instances[0])
	}
	key, err := env.Store().CreateAPIKey("deploy", 0)
	if err != nil {
		t.Fatal(err)
	}
	modelsReq, _ := http.NewRequest(http.MethodGet, env.URL()+"/v1/models", nil)
	modelsReq.Header.Set("Authorization", "Bearer "+key)
	modelsResp, err := http.DefaultClient.Do(modelsReq)
	if err != nil {
		t.Fatal(err)
	}
	modelsBody, _ := io.ReadAll(modelsResp.Body)
	modelsResp.Body.Close()
	if modelsResp.StatusCode != http.StatusOK || !bytes.Contains(modelsBody, []byte(`"id":"fake"`)) {
		t.Fatalf("models status=%d body=%s", modelsResp.StatusCode, modelsBody)
	}
	streamReq, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"fake","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	streamReq.Header.Set("Authorization", "Bearer "+key)
	streamReq.Header.Set("Content-Type", "application/json")
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	streamBody, _ := io.ReadAll(streamResp.Body)
	streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK || !strings.HasPrefix(streamResp.Header.Get("Content-Type"), "text/event-stream") || !bytes.Contains(streamBody, []byte("data: [DONE]")) {
		t.Fatalf("stream status=%d content-type=%q body=%s", streamResp.StatusCode, streamResp.Header.Get("Content-Type"), streamBody)
	}
	chatReq, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"fake","messages":[{"role":"user","content":"hi"}]}`)))
	chatReq.Header.Set("Authorization", "Bearer "+key)
	chatReq.Header.Set("Content-Type", "application/json")
	chat, err := http.DefaultClient.Do(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	if chat.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(chat.Body)
		chat.Body.Close()
		t.Fatalf("chat proxy: %d %s", chat.StatusCode, body)
	}
	chat.Body.Close()
	badReq, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{}`)))
	badReq.Header.Set("Authorization", "Bearer gw-invalid-key")
	bad, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid proxy key status=%d", bad.StatusCode)
	}
	stop, err := api.PostJSON(fmt.Sprintf("/api/v1/deployments/%d/stop", created.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	stop.Body.Close()
	stopDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(stopDeadline) {
		current, _ := env.Store().ListInstances()
		if len(current) == 0 || (len(current) > 0 && current[0].State == proto.InstStopped) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	current, _ := env.Store().ListInstances()
	t.Fatalf("instance not stopped: %+v", current)
}

// A deployment's own container overrides are the whole customization layer:
// what the preview shows is what the create stores and what the runtime runs,
// including the /dev/shm size and environment removals. The catalog row of the
// image contributes nothing but its reference (ADR 0031).
func TestContainerOverridesPreviewCreateAndRuntimeAgree(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("runspec-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	var ran runtime.WorkloadSpec
	node.Driver.AfterRun(func(ws runtime.WorkloadSpec) { ran = ws })
	env.WaitNodeOnline(t, "runspec-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %+v, err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "runspec/model", Revision: "main", Name: "runspec-model", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "runspec")}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertImage(&store.Image{
		ImageRef: "example/runspec:v1",
		Source:   "community",
		Engine:   "vllm",
		Enabled:  true,
	}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"model_id":     model.ID,
		"node_uuid":    nodes[0].UUID,
		"served_name":  "runspec",
		"engine":       "vllm",
		"image":        "example/runspec:v1",
		"quick_params": map[string]any{"max_model_len": 512},
		"container_overrides": map[string]any{
			"model_mount_path":   "/override-model",
			"env":                map[string]string{"OVERRIDE_ENV": "yes"},
			"docker_args":        []string{"--cap-add=SYS_NICE"},
			"extra_mounts":       []map[string]any{{"host_path": "/kv-cache", "container_path": "/kv", "purpose": "kv-cache"}},
			"entrypoint_display": `["custom-engine"]`,
			"cmd_display":        `["serve", "/model"]`,
			"shm_bytes":          32 << 30,
			"env_unset":          []string{"HF_HUB_OFFLINE"},
		},
	})
	previewResp, err := api.PostJSON("/api/v1/evaluate", body)
	if err != nil {
		t.Fatal(err)
	}
	if previewResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(previewResp.Body)
		previewResp.Body.Close()
		t.Fatalf("evaluate status = %d body=%s", previewResp.StatusCode, raw)
	}
	var preview struct {
		Command     []string          `json:"command"`
		Env         map[string]string `json:"env"`
		CommandMode string            `json:"command_mode"`
		ShmBytes    int64             `json:"shm_bytes"`
		Mounts      []struct {
			Target string `json:"target"`
		} `json:"mounts"`
	}
	if err := json.NewDecoder(previewResp.Body).Decode(&preview); err != nil {
		previewResp.Body.Close()
		t.Fatal(err)
	}
	previewResp.Body.Close()
	if preview.CommandMode != "engine" {
		t.Fatalf("preview command mode = %q", preview.CommandMode)
	}
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create deployment status = %d body=%s", resp.StatusCode, body)
	}
	var created struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	deadline := time.Now().Add(20 * time.Second)
	var instances []store.Instance
	for time.Now().Before(deadline) {
		instances, _ = env.Store().ListInstances()
		if len(instances) == 1 && instances[0].State == proto.InstRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(instances) != 1 || instances[0].State != proto.InstRunning {
		t.Fatalf("instance did not reach running: %+v", instances)
	}
	if _, unset := ran.Env["HF_HUB_OFFLINE"]; ran.Image != "example/runspec:v1" || unset || ran.Env["OVERRIDE_ENV"] != "yes" {
		t.Fatalf("runtime image/env = %q / %+v", ran.Image, ran.Env)
	}
	if ran.ShmBytes != 32<<30 || preview.ShmBytes != ran.ShmBytes {
		t.Fatalf("shm preview=%d runtime=%d, want 32 GiB in both", preview.ShmBytes, ran.ShmBytes)
	}
	if len(ran.Mounts) != 2 || !containsMountTarget(ran.Mounts, "/override-model") || !containsMountTarget(ran.Mounts, "/kv") {
		t.Fatalf("runtime mounts = %+v", ran.Mounts)
	}
	if !reflect.DeepEqual(ran.DockerArgs, []string{"--cap-add=SYS_NICE"}) {
		t.Fatalf("runtime docker args = %+v", ran.DockerArgs)
	}
	if !reflect.DeepEqual(ran.Entrypoint, []string{"custom-engine"}) || !reflect.DeepEqual(ran.Cmd, []string{"serve", "/override-model"}) {
		t.Fatalf("runtime command did not consume the container overrides: entrypoint=%+v cmd=%+v", ran.Entrypoint, ran.Cmd)
	}
	if !reflect.DeepEqual(append(append([]string{}, ran.Entrypoint...), ran.Cmd...), preview.Command) {
		t.Fatalf("preview/runtime command mismatch: preview=%v runtime=%v/%v", preview.Command, ran.Entrypoint, ran.Cmd)
	}
	if _, previewed := preview.Env["HF_HUB_OFFLINE"]; previewed || ran.Env["OVERRIDE_ENV"] != preview.Env["OVERRIDE_ENV"] {
		t.Fatalf("preview/runtime env mismatch: preview=%v runtime=%v", preview.Env, ran.Env)
	}
	if len(preview.Mounts) != 1 {
		t.Fatalf("preview mounts = %+v", preview.Mounts)
	}
	if !containsString(ran.Entrypoint, "/override-model") && !containsString(ran.Cmd, "/override-model") && !containsArgPair(ran.Cmd, "--model", "/override-model") && !containsArgPair(ran.Cmd, "--model-path", "/override-model") {
		t.Fatalf("runtime model argument did not follow the overridden mount path: entrypoint=%+v cmd=%+v", ran.Entrypoint, ran.Cmd)
	}
	var scheduleExplain map[string]any
	if err := json.Unmarshal([]byte(instances[0].ScheduleExpl), &scheduleExplain); err != nil {
		t.Fatalf("schedule explain JSON = %v", err)
	}
	if _, ok := scheduleExplain["image"]; !ok {
		t.Fatalf("schedule explain omitted image resolution: %s", instances[0].ScheduleExpl)
	}
	_ = created
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsArgPair(values []string, flag, value string) bool {
	for i := 0; i+1 < len(values); i++ {
		if values[i] == flag && values[i+1] == value {
			return true
		}
	}
	return false
}

func containsMountTarget(values []runtime.Mount, wanted string) bool {
	for _, value := range values {
		if value.Target == wanted {
			return true
		}
	}
	return false
}
