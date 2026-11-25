package sleepwake

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestSleepMakesRoomAndWakeIsRefusedWhileTheCoTenantRuns is the reason sleep
// mode exists: sleeping A frees its card for B, and A cannot come back until B
// is gone. It runs the real app, controller, agent, ledger and proxy against
// the fake runtime and fake engine.
func TestSleepMakesRoomAndWakeIsRefusedWhileTheCoTenantRuns(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("cotenant-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "cotenant-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	node := nodes[0]
	api := env.API(t)

	newModel := func(name string) *store.Model {
		model, err := env.Store().UpsertModel(&store.Model{
			Source: "huggingface", SourceID: "zstackai/" + name, Revision: "main", Name: name, Arch: "qwen3",
			WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "model"+name)}); err != nil {
			t.Fatal(err)
		}
		return model
	}
	deploy := func(model *store.Model, served string, sleepable bool) (uint, int, string) {
		body, _ := json.Marshal(map[string]any{
			"model_id": model.ID, "node_uuid": node.UUID, "served_name": served, "sleepable": sleepable,
			"quick_params": map[string]any{"max_model_len": 512},
		})
		resp, err := api.PostJSON("/api/v1/deployments", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var dep struct {
			ID uint `json:"id"`
		}
		_ = json.Unmarshal(raw, &dep)
		return dep.ID, resp.StatusCode, string(raw)
	}
	instanceOf := func(depID uint) *store.Instance {
		insts, _ := env.Store().InstancesForDeployment(depID)
		for i := range insts {
			if insts[i].State != proto.InstStopped && insts[i].State != proto.InstFailed {
				return &insts[i]
			}
		}
		return nil
	}
	awake := func(depID uint) bool {
		in := instanceOf(depID)
		return in != nil && in.State == proto.InstRunning && in.SleepLevel == 0
	}
	asleep := func(depID uint, level int) bool {
		in := instanceOf(depID)
		return in != nil && in.State == proto.InstRunning && in.SleepLevel == level
	}

	// A takes the only card.
	a, code, raw := deploy(newModel("model-a"), "model-a", true)
	if code != http.StatusCreated {
		t.Fatalf("create A: %d %s", code, raw)
	}
	env.Eventually(t, 30*time.Second, func() bool { return awake(a) })

	// B cannot be placed while A is awake, and the evaluation says which
	// deployment to sleep for it.
	modelB := newModel("model-b")
	evalBody, _ := json.Marshal(map[string]any{"model_id": modelB.ID, "node_uuid": node.UUID, "target_vendor": "nvidia", "engine": "vllm", "quick_params": map[string]any{"max_model_len": 512}})
	evalResp, err := api.PostJSON("/api/v1/evaluate", evalBody)
	if err != nil {
		t.Fatal(err)
	}
	var eval struct {
		OK       bool `json:"ok"`
		Eviction []struct {
			DeploymentID uint    `json:"deployment_id"`
			Deployment   string  `json:"deployment"`
			FreesGiB     float64 `json:"frees_gib"`
		} `json:"eviction"`
	}
	if err := json.NewDecoder(evalResp.Body).Decode(&eval); err != nil {
		t.Fatal(err)
	}
	evalResp.Body.Close()
	if eval.OK || len(eval.Eviction) != 1 || eval.Eviction[0].DeploymentID != a || eval.Eviction[0].FreesGiB <= 0 || eval.Eviction[0].FreesGiB >= 24 {
		t.Fatalf("evaluation with A awake = %+v, want no fit with A as the eviction candidate freeing less than the whole card", eval)
	}

	// Sleep A the way the suggestion would: L1, finishing in-flight requests.
	sleepResp, err := api.PostJSON("/api/v1/deployments/"+itoa(a)+"/sleep?level=1&mode=wait", nil)
	if err != nil || sleepResp.StatusCode != http.StatusOK {
		t.Fatalf("sleep A: status=%v err=%v", status(sleepResp), err)
	}
	sleepResp.Body.Close()
	env.Eventually(t, 15*time.Second, func() bool { return asleep(a, 1) })
	if in := instanceOf(a); !strings.Contains(in.DesiredStateJSON, `"sleep_mode":"wait"`) {
		t.Fatalf("A's desired state does not carry the pause mode: %s", in.DesiredStateJSON)
	}

	// The card is free now: B is placed and runs alongside the sleeping A.
	b, code, raw := deploy(modelB, "model-b", false)
	if code != http.StatusCreated {
		t.Fatalf("create B with A asleep: %d %s", code, raw)
	}
	env.Eventually(t, 30*time.Second, func() bool { return awake(b) })
	if in := instanceOf(b); in.GPUIndexes != "[0]" {
		t.Fatalf("B landed on %s, want the card A released", in.GPUIndexes)
	}

	// A cannot come back while B holds the card, and the refusal names B.
	wakeResp, err := api.PostJSON("/api/v1/deployments/"+itoa(a)+"/wake", nil)
	if err != nil {
		t.Fatal(err)
	}
	wakeRaw, _ := io.ReadAll(wakeResp.Body)
	wakeResp.Body.Close()
	if wakeResp.StatusCode != http.StatusConflict || !strings.Contains(string(wakeRaw), `"code":"WAKE_INSUFFICIENT_GPU_MEMORY"`) || !strings.Contains(string(wakeRaw), "model-b") {
		t.Fatalf("wake A over B: %d %s", wakeResp.StatusCode, wakeRaw)
	}
	if dep, _ := env.Store().GetDeployment(a); dep.DesiredSleep != 1 {
		t.Fatalf("refused wake changed A's desired level to %d", dep.DesiredSleep)
	}

	// Stop B; once its instance is gone the same wake goes through and A serves.
	stopResp, err := api.PostJSON("/api/v1/deployments/"+itoa(b)+"/stop", nil)
	if err != nil || stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop B: status=%v err=%v", status(stopResp), err)
	}
	stopResp.Body.Close()
	env.Eventually(t, 30*time.Second, func() bool { return instanceOf(b) == nil })
	wakeResp, err = api.PostJSON("/api/v1/deployments/"+itoa(a)+"/wake", nil)
	if err != nil || wakeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(wakeResp.Body)
		t.Fatalf("wake A after B stopped: status=%v err=%v body=%s", status(wakeResp), err, body)
	}
	wakeResp.Body.Close()
	env.Eventually(t, 45*time.Second, func() bool { return awake(a) })
}

// TestDeepSleepWakesWithReloadedWeights drives the L2 path end to end: the
// fake engine answers garbage after an L2 wake that skipped the reload, so a
// correct answer after the wake proves the agent issued the three orders.
func TestDeepSleepWakesWithReloadedWeights(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("deep-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "deep-node")
	nodes, _ := env.Store().ListNodes()
	node := nodes[0]
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/deep-model", Revision: "main", Name: "deep-model", Arch: "qwen3",
		WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "deep")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": node.UUID, "served_name": "deep-model", "sleepable": true, "quick_params": map[string]any{"max_model_len": 512}})
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	var dep struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dep)
	resp.Body.Close()
	level := func(want int) func() bool {
		return func() bool {
			insts, _ := env.Store().ListInstances()
			return len(insts) == 1 && insts[0].State == proto.InstRunning && insts[0].SleepLevel == want
		}
	}
	env.Eventually(t, 30*time.Second, level(0))

	key, err := env.Store().CreateAPIKey("deep", 0)
	if err != nil {
		t.Fatal(err)
	}
	answer := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", strings.NewReader(`{"model":"deep-model","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, string(raw)
	}
	stableAnswer := func(raw string) map[string]any {
		t.Helper()
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var reply map[string]any
		if err := decoder.Decode(&reply); err != nil || reply == nil {
			t.Fatalf("invalid completion object %q: %v", raw, err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			t.Fatalf("trailing completion data %q: %v", raw, err)
		}
		created, ok := reply["created"].(json.Number)
		if !ok {
			t.Fatalf("missing or invalid completion timestamp: %s", raw)
		}
		if _, err := created.Int64(); err != nil {
			t.Fatalf("invalid completion timestamp %q: %v", created, err)
		}
		for _, field := range []string{"id", "model", "object"} {
			if value, ok := reply[field].(string); !ok || value == "" {
				t.Fatalf("missing or invalid completion %s: %s", field, raw)
			}
		}
		if choices, ok := reply["choices"].([]any); !ok || len(choices) == 0 {
			t.Fatalf("missing or invalid completion choices: %s", raw)
		}
		// Every response has its own creation instant. Keep all remaining
		// fields, including nested choices and usage, in the reload assertion.
		delete(reply, "created")
		return reply
	}
	code, before := answer()
	if code != http.StatusOK {
		t.Fatalf("awake chat: %d %s", code, before)
	}
	beforeReply := stableAnswer(before)

	sleep, err := api.PostJSON("/api/v1/deployments/"+itoa(dep.ID)+"/sleep?level=2", nil)
	if err != nil || sleep.StatusCode != http.StatusOK {
		t.Fatalf("deep sleep: status=%v err=%v", status(sleep), err)
	}
	sleep.Body.Close()
	env.Eventually(t, 15*time.Second, level(2))
	if code, raw := answer(); code != http.StatusServiceUnavailable || !strings.Contains(raw, "asleep") {
		t.Fatalf("chat while deep asleep: %d %s", code, raw)
	}

	wake, err := api.PostJSON("/api/v1/deployments/"+itoa(dep.ID)+"/wake", nil)
	if err != nil || wake.StatusCode != http.StatusOK {
		t.Fatalf("wake: status=%v err=%v", status(wake), err)
	}
	wake.Body.Close()
	env.Eventually(t, 45*time.Second, level(0))
	code, after := answer()
	if code != http.StatusOK || !reflect.DeepEqual(stableAnswer(after), beforeReply) {
		t.Fatalf("answer after deep wake differs:\nbefore: %s\nafter:  %d %s", before, code, after)
	}
}

// TestWakeFailureRollsBackWithoutAutomaticRetry verifies that a failed wake
// restores the sleeping intent and requires an explicit retry.
func TestWakeFailureRollsBackWithoutAutomaticRetry(t *testing.T) {
	env := testenv.New(t)
	env.AddNode("retry-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, "retry-node")
	nodes, _ := env.Store().ListNodes()
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/retry-model", Revision: "main", Name: "retry-model", Arch: "qwen3",
		WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "retry")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "retry-model", "sleepable": true, "quick_params": map[string]any{"max_model_len": 512}})
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment: status=%v err=%v", status(resp), err)
	}
	var dep struct {
		ID uint `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&dep)
	resp.Body.Close()
	level := func(want int) bool {
		instances, _ := env.Store().InstancesForDeployment(dep.ID)
		return len(instances) == 1 && instances[0].State == proto.InstRunning && instances[0].SleepLevel == want
	}
	env.Eventually(t, 30*time.Second, func() bool { return level(0) })

	sleep, err := api.PostJSON("/api/v1/deployments/"+itoa(dep.ID)+"/sleep?level=1", nil)
	if err != nil || sleep.StatusCode != http.StatusOK {
		t.Fatalf("sleep: status=%v err=%v", status(sleep), err)
	}
	sleep.Body.Close()
	env.Eventually(t, 15*time.Second, func() bool { return level(1) })
	instances, _ := env.Store().InstancesForDeployment(dep.ID)
	if len(instances) != 1 || instances[0].DirectAddr == "" {
		t.Fatalf("sleeping instance has no direct address: %+v", instances)
	}

	fail, err := http.Post("http://"+instances[0].DirectAddr+"/debug/wake-failures?count=1", "", nil)
	if err != nil || fail.StatusCode != http.StatusOK {
		t.Fatalf("inject wake failure: status=%v err=%v", status(fail), err)
	}
	fail.Body.Close()
	wake, err := api.PostJSON("/api/v1/deployments/"+itoa(dep.ID)+"/wake", nil)
	if err != nil || wake.StatusCode != http.StatusOK {
		t.Fatalf("wake: status=%v err=%v", status(wake), err)
	}
	wake.Body.Close()

	// The failed wake rolls the desired level back to the observed sleeping
	// level. No background retry should change it before a new order.
	env.Eventually(t, 15*time.Second, func() bool {
		got, _ := env.Store().GetDeployment(dep.ID)
		return got.DesiredSleep == 1 && level(1)
	})
	time.Sleep(2 * time.Second)
	if !level(1) {
		t.Fatal("failed wake was retried automatically")
	}
	instances, _ = env.Store().InstancesForDeployment(dep.ID)
	events, err := env.Store().InstanceEvents(instances[0].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	statusEvents := 0
	for _, event := range events {
		if event.Code == "INSTANCE_STATUS_UPDATED" && strings.Contains(event.Data, `"phase":"sleep"`) {
			statusEvents++
			if !strings.Contains(event.Data, `"detail":"sleep level`) {
				t.Fatalf("sleep failure event lost its detail: %+v", event)
			}
		}
	}
	if statusEvents != 1 {
		t.Fatalf("sleep status events = %d, want one: %+v", statusEvents, events)
	}

	wake, err = api.PostJSON("/api/v1/deployments/"+itoa(dep.ID)+"/wake", nil)
	if err != nil || wake.StatusCode != http.StatusOK {
		t.Fatalf("explicit wake retry: status=%v err=%v", status(wake), err)
	}
	wake.Body.Close()
	env.Eventually(t, 45*time.Second, func() bool { return level(0) })
}
