package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestPDDeploymentServesThroughTheGatewayTwoStage is the product-level PD
// contract: the real API, controller, agent and gateway start a prefill and
// a decode instance, and a chat request through /v1 is prefilled on one and
// decoded on the other, the decoder pulling the KV from the prefill
// instance's side channel. Only GPUs and the container runtime are fake; the
// engines speak the PD dialect recorded against real vLLM.
func TestPDDeploymentServesThroughTheGatewayTwoStage(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("pd-node", testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	runs := make(chan runtime.WorkloadSpec, 4)
	node.Driver.AfterRun(func(spec runtime.WorkloadSpec) { captureWorkload(runs, spec) })
	env.WaitNodeOnline(t, node.Name)
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-pd-fake", Revision: "main", Name: "qwen3-pd-fake", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := env.Store().NodeByUUID(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "qwen3-pd-fake")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	payload, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "served_name": "pd-fake", "engine": "vllm", "image": "gpuworks/fake-engine:test",
		"quick_params": map[string]any{"max_model_len": 512},
		"placement": map[string]any{"pd": map[string]any{"mode": "vllm-nixl", "roles": map[string]any{
			"prefill": map[string]any{"units": []any{map[string]any{"node_uuid": node.UUID, "gpus": []int{0}}}},
			"decode":  map[string]any{"units": []any{map[string]any{"node_uuid": node.UUID, "gpus": []int{1}}}},
		}}},
	})
	resp, err := api.PostJSON("/api/v1/deployments", payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}
	var created dto.Deployment
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	deployment := waitPDRunning(t, api, created.ID)
	var prefill, decode dto.Instance
	for _, inst := range deployment.Instances {
		switch inst.Role {
		case "prefill":
			prefill = inst
		case "decode":
			decode = inst
		}
	}
	if prefill.UUID == "" || decode.UUID == "" || prefill.GPUIndexes[0] != 0 || decode.GPUIndexes[0] != 1 {
		t.Fatalf("instances = %+v", deployment.Instances)
	}
	if len(prefill.Ports) != 2 || len(decode.Ports) != 2 || prefill.Ports[1] == decode.Ports[1] {
		t.Fatalf("ports: prefill %v decode %v", prefill.Ports, decode.Ports)
	}
	producer := ""
	for i := 0; i < 2; i++ {
		spec := waitCapturedWorkload(t, runs, "pd instance")
		argv := strings.Join(spec.Cmd, " ")
		if !strings.Contains(argv, "NixlConnector") || spec.Env["VLLM_NIXL_SIDE_CHANNEL_PORT"] == "" {
			t.Fatalf("workload = %v %v", spec.Cmd, spec.Env)
		}
		if strings.Contains(argv, "kv_producer") {
			producer = spec.Name
		}
	}
	if producer == "" {
		t.Fatal("no prefill workload ran")
	}

	key, err := env.Store().CreateAPIKey("pd-fake", 0)
	if err != nil {
		t.Fatal(err)
	}
	chat := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"pd-fake","messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	metric := func(port int, series string) string {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		for _, line := range strings.Split(string(body), "\n") {
			// The engine labels series with its own model name; match the family.
			if strings.HasPrefix(line, series+"{") {
				return line[strings.LastIndex(line, " ")+1:]
			}
		}
		return ""
	}
	const transfers = "vllm:nixl_bytes_transferred_count"
	// The server verifies the prefill instance's handoff by itself and
	// records it on the instance's timeline: same node, so KV stayed local.
	verified := waitHandoffEvent(t, env.Store(), prefill.UUID)
	if verified.Code != "PD_HANDOFF_VERIFIED" || !strings.Contains(verified.Data, `"transport":"local"`) {
		t.Fatalf("handoff event = %+v", verified)
	}
	before := metric(decode.APIPort, transfers)
	if status, body := chat(); status != http.StatusOK {
		t.Fatalf("chat = %d %s", status, body)
	}
	after := metric(decode.APIPort, transfers)
	if before == "" || after == before {
		t.Fatalf("decode transfers %q -> %q: the chat request pulled no KV", before, after)
	}

	// The prefill engine goes away: the gateway falls back to the decoder,
	// which computes the prefill itself, and the client still gets its answer.
	if err := node.Driver.Stop(context.Background(), producer); err != nil {
		t.Fatal(err)
	}
	if status, body := chat(); status != http.StatusOK {
		t.Fatalf("chat without prefill = %d %s", status, body)
	}
	if got := metric(decode.APIPort, transfers); got != after {
		t.Fatalf("a fallback request transferred KV: %q -> %q", after, got)
	}
}

func waitPDRunning(t *testing.T, api *testenv.APIClient, id uint) dto.Deployment {
	t.Helper()
	return waitPDRunningWithin(t, api, id, 30*time.Second)
}

// waitPDRunningWithin is waitPDRunning with the time a case allows: one that
// rebuilds a unit first waits out the retry backoff and a second launch.
func waitPDRunningWithin(t *testing.T, api *testenv.APIClient, id uint, within time.Duration) dto.Deployment {
	t.Helper()
	last, ok := pollPDRunning(api, id, within)
	if !ok {
		t.Fatalf("PD deployment did not reach running: %+v", last.PD)
	}
	return last
}

// pollPDRunning polls the deployment until its PD state is running with every
// role complete, or within passes. It returns the last response read either
// way, so a caller that keeps waiting for something else can report it.
func pollPDRunning(api *testenv.APIClient, id uint, within time.Duration) (dto.Deployment, bool) {
	deadline := time.Now().Add(within)
	var last dto.Deployment
	for time.Now().Before(deadline) {
		resp, err := api.Get(fmt.Sprintf("/api/v1/deployments/%d", id))
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				last = dto.Deployment{}
				_ = json.NewDecoder(resp.Body).Decode(&last)
			}
			resp.Body.Close()
		}
		if last.PD != nil && last.PD.State == "running" && !last.PD.Short {
			return last, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last, false
}

func waitHandoffEvent(t *testing.T, st *store.Store, uuid string) store.InstanceEvent {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		events, _ := st.InstanceEventsByUUID(uuid, 0)
		for _, ev := range events {
			if strings.HasPrefix(ev.Code, "PD_HANDOFF_") {
				return ev
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no handoff verification recorded for %s", uuid)
	return store.InstanceEvent{}
}

// TestPDScalesPrefillWhileServing changes the prefill role 2 -> 1 -> 0 -> 1
// under continuous traffic: removed instances drain before they stop, a
// deployment without prefill instances serves from its decoder, and no
// request fails along the way (plan phase 3).
func TestPDScalesPrefillWhileServing(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("pd-scale-node", testenv.WithGPUs(3, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, node.Name)
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "zstackai/qwen3-pd-scale", Revision: "main", Name: "qwen3-pd-scale", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	row, _ := env.Store().NodeByUUID(node.UUID)
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "qwen3-pd-scale")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	unit := func(gpu int) map[string]any { return map[string]any{"node_uuid": node.UUID, "gpus": []int{gpu}} }
	payload, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "served_name": "pd-scale", "engine": "vllm", "image": "gpuworks/fake-engine:test",
		"placement": map[string]any{"pd": map[string]any{"mode": "vllm-nixl", "roles": map[string]any{
			"prefill": map[string]any{"units": []any{unit(0), unit(1)}},
			"decode":  map[string]any{"units": []any{unit(2)}},
		}}},
	})
	resp, err := api.PostJSON("/api/v1/deployments", payload)
	if err != nil {
		t.Fatal(err)
	}
	var created dto.Deployment
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	waitPDRunning(t, api, created.ID)
	key, _ := env.Store().CreateAPIKey("pd-scale", 0)

	stop := make(chan struct{})
	failures := make(chan string, 64)
	sent := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			req, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"pd-scale","messages":[{"role":"user","content":"hi"}]}`)))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			sent++
			if err != nil {
				failures <- err.Error()
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				failures <- fmt.Sprintf("%d %s", resp.StatusCode, body)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	scale := func(units ...map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"roles": map[string]any{"prefill": map[string]any{"units": append([]map[string]any{}, units...)}}})
		req, _ := http.NewRequest(http.MethodPut, "/api/v1/deployments/"+fmt.Sprint(created.ID)+"/pd", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := api.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("scale = %d %s", resp.StatusCode, raw)
		}
	}
	prefillCount := func() int {
		insts, _ := env.Store().InstancesForDeployment(created.ID)
		n := 0
		for _, inst := range insts {
			if inst.Role == "prefill" && inst.State != proto.InstFailed {
				n++
			}
		}
		return n
	}
	waitPrefill := func(want int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) && prefillCount() != want {
			time.Sleep(100 * time.Millisecond)
		}
		if got := prefillCount(); got != want {
			t.Fatalf("prefill instances = %d, want %d", got, want)
		}
	}
	scale(unit(0))
	waitPrefill(1)
	scale()
	waitPrefill(0)
	scale(unit(1))
	waitPrefill(1)
	waitPDRunning(t, api, created.ID)
	time.Sleep(300 * time.Millisecond)
	close(stop)
	<-done
	close(failures)
	for failure := range failures {
		t.Errorf("request failed while scaling: %s", failure)
	}
	if sent < 20 {
		t.Fatalf("only %d requests were sent while scaling", sent)
	}
}

// TestSGLangPDDeploymentPairsByBootstrapRoom runs the concurrent wire end to
// end: SGLang prefill and decode instances, a chat request through /v1, and
// the server's own handoff check, which for an engine without transfer
// counters is judged by the decoder's answer.
func TestSGLangPDDeploymentPairsByBootstrapRoom(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("pd-sglang-node", testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, node.Name)
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "zstackai/qwen3-pd-sglang", Revision: "main", Name: "qwen3-pd-sglang", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	row, _ := env.Store().NodeByUUID(node.UUID)
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "qwen3-pd-sglang")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	payload, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "served_name": "pd-sglang", "engine": "sglang", "image": "gpuworks/fake-engine:test",
		"placement": map[string]any{"pd": map[string]any{"mode": "sglang-nixl", "roles": map[string]any{
			"prefill": map[string]any{"units": []any{map[string]any{"node_uuid": node.UUID, "gpus": []int{0}}}},
			"decode":  map[string]any{"units": []any{map[string]any{"node_uuid": node.UUID, "gpus": []int{1}}}},
		}}},
	})
	resp, err := api.PostJSON("/api/v1/deployments", payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}
	var created dto.Deployment
	_ = json.Unmarshal(raw, &created)
	deployment := waitPDRunning(t, api, created.ID)
	var prefill string
	for _, inst := range deployment.Instances {
		if inst.Role == "prefill" {
			prefill = inst.UUID
			if len(inst.Ports) != 2 {
				t.Fatalf("prefill ports = %v, want the API port and a bootstrap port", inst.Ports)
			}
		} else if len(inst.Ports) != 1 {
			t.Fatalf("decode ports = %v, want the API port only", inst.Ports)
		}
	}
	verified := waitHandoffEvent(t, env.Store(), prefill)
	if verified.Code != "PD_HANDOFF_VERIFIED" || !strings.Contains(verified.Data, `"transport":"unmeasured"`) {
		t.Fatalf("handoff = %+v", verified)
	}
	key, _ := env.Store().CreateAPIKey("pd-sglang", 0)
	req, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"pd-sglang","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	chat, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(chat.Body)
	chat.Body.Close()
	if chat.StatusCode != http.StatusOK {
		t.Fatalf("chat = %d %s", chat.StatusCode, body)
	}
}
