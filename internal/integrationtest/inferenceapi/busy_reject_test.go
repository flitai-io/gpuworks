package inferenceapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	fakehandler "gpuworks.ai/gpuworks/test/fake-engine/handler"
)

// heldEngine is the fake engine with each chat held until let() lets one
// go, as a generation still decoding holds its run slot.
type heldEngine struct {
	entered chan struct{}
	release chan struct{}
	stop    sync.Once
	mu      sync.Mutex
	// declarations are the X-GPUWorks-If-Busy values the engine got.
	declarations []string
}

func (e *heldEngine) handler(inst testenv.EngineInstance) http.Handler {
	engine := fakehandler.New(fakehandler.Options{Model: inst.Model})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			e.mu.Lock()
			e.declarations = append(e.declarations, r.Header.Values("X-GPUWorks-If-Busy")...)
			e.mu.Unlock()
			e.entered <- struct{}{}
			<-e.release
		}
		engine.ServeHTTP(w, r)
	})
}

func (e *heldEngine) letOne() { e.release <- struct{}{} }

func (e *heldEngine) letAll() { e.stop.Do(func() { close(e.release) }) }

type answer struct {
	status     int
	retryAfter string
	body       []byte
}

func chatDeclared(t *testing.T, env *testenv.Env, key string, declare bool) answer {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"busy-model","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	if declare {
		req.Header.Set("X-GPUWorks-If-Busy", "reject")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return answer{}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: body}
}

// deployBusyModel deploys busy-model, vLLM of two run slots with request
// priority on (the default), on one node
// whose engine is engine, and waits until its instance runs. It returns the
// deployment's id.
func deployBusyModel(t *testing.T, env *testenv.Env, engine *heldEngine) uint {
	t.Helper()
	driver := testenv.NewFakeDriver("docker")
	driver.SetEngineHandler(engine.handler)
	env.AddNode("busy-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(driver))
	env.WaitNodeOnline(t, "busy-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "busy-model", Revision: "main", Name: "busy-model",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "busy")}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "busy-model", "engine": "vllm",
		"quick_params": map[string]any{"max_model_len": 256, "max_num_seqs": 2},
	})
	created, err := env.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	if created.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(created.Body)
		created.Body.Close()
		t.Fatalf("create deployment status=%d body=%s", created.StatusCode, data)
	}
	var dep struct {
		ID uint `json:"id"`
	}
	err = json.NewDecoder(created.Body).Decode(&dep)
	created.Body.Close()
	if err != nil || dep.ID == 0 {
		t.Fatalf("created deployment = %+v %v", dep, err)
	}
	env.Eventually(t, 30*time.Second, func() bool {
		instances, err := env.Store().ListInstances()
		return err == nil && len(instances) == 1 && instances[0].State == proto.InstRunning && instances[0].APIPort != 0
	})
	return dep.ID
}

// A deployment launched with two run slots, both held by the engine: a
// third request that asked not to wait is refused with 429 MODEL_BUSY well
// inside 100 ms, through the real router, key authentication, route table
// and agent tunnel; once one slot frees, the next declared request is taken.
//
//	go test ./internal/integrationtest/inferenceapi/ -run TestBusyDeploymentRefusesADeclaredRequestThroughTheControlPlane -count=1
func TestBusyDeploymentRefusesADeclaredRequestThroughTheControlPlane(t *testing.T) {
	engine := &heldEngine{entered: make(chan struct{}, 8), release: make(chan struct{})}
	env := testenv.New(t)
	t.Cleanup(engine.letAll)
	deployBusyModel(t, env, engine)
	key, err := env.Store().CreateAPIKey("busy", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The route table refreshes asynchronously: wait until a request is
	// routed, then hold it and one more.
	held := make(chan answer, 4)
	go func() { held <- chatDeclared(t, env, key, true) }()
	select {
	case <-engine.entered:
	case got := <-held:
		t.Fatalf("the first request was answered %d %s", got.status, got.body)
	case <-time.After(30 * time.Second):
		t.Fatal("the first request never reached the engine")
	}
	go func() { held <- chatDeclared(t, env, key, false) }()
	select {
	case <-engine.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the second request never reached the engine")
	}

	started := time.Now()
	refused := chatDeclared(t, env, key, true)
	elapsed := time.Since(started)
	if refused.status != http.StatusTooManyRequests || refused.retryAfter != "1" || !bytes.Contains(refused.body, []byte(`"code":"MODEL_BUSY"`)) {
		t.Fatalf("third declared request = %d Retry-After %q %s, want 429 MODEL_BUSY with Retry-After 1", refused.status, refused.retryAfter, refused.body)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("the refusal took %s, want under 100ms", elapsed)
	}

	engine.letOne()
	if got := <-held; got.status != http.StatusOK {
		t.Fatalf("a held request = %d %s", got.status, got.body)
	}
	// The slot frees when the gateway has given the replica back, just
	// after the client read the answer.
	for deadline := time.Now().Add(time.Second); ; {
		go func() { held <- chatDeclared(t, env, key, true) }()
		select {
		case <-engine.entered:
		case got := <-held:
			if got.status != http.StatusTooManyRequests || time.Now().After(deadline) {
				t.Fatalf("with a slot free the declared request = %d %s, want it taken", got.status, got.body)
			}
			time.Sleep(10 * time.Millisecond)
			continue
		case <-time.After(10 * time.Second):
			t.Fatal("with a slot free the declared request never reached the engine")
		}
		break
	}
	engine.letAll()
	for range 2 {
		if got := <-held; got.status != http.StatusOK {
			t.Fatalf("a held request = %d %s", got.status, got.body)
		}
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.declarations) != 0 {
		t.Fatalf("the engine got X-GPUWorks-If-Busy: %q", engine.declarations)
	}
}
