package sleepwake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
	fakehandler "gpuworks.ai/gpuworks/test/fake-engine/handler"
)

// arrivals records which engine each chat request reached.
type arrivals struct {
	mu  sync.Mutex
	got []string // container names
}

func (a *arrivals) handler(inst testenv.EngineInstance) http.Handler {
	container := inst.Container
	engine := fakehandler.New(fakehandler.Options{Model: inst.Model})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			a.mu.Lock()
			a.got = append(a.got, container)
			a.mu.Unlock()
		}
		engine.ServeHTTP(w, r)
	})
}

func (a *arrivals) take() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.got
	a.got = nil
	return out
}

func containerOf(uuid string) string {
	if len(uuid) > 12 {
		uuid = uuid[:12]
	}
	return "gpuworks-" + uuid
}

// One replica of a two-replica deployment sleeps on its own order: the other
// keeps answering every request, the sleeper takes none, a server restart
// keeps each at its own level, and waking the one brings it back into
// routing. The real app, controller, agent, ledger and proxy run against the
// fake runtime and fake engine.
func TestOneReplicaSleepsWhileTheOtherServes(t *testing.T) {
	seen := &arrivals{}
	driver := testenv.NewFakeDriver("docker")
	driver.SetEngineHandler(seen.handler)
	env := testenv.NewOnDisk(t)
	env.AddNode("replica-sleep-node", testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(driver))
	env.WaitNodeOnline(t, "replica-sleep-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/replica-sleep", Revision: "main", Name: "replica-sleep", Arch: "qwen3",
		WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "replica-sleep")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	body, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "replica-sleep", "sleepable": true, "replicas": 2,
		"quick_params": map[string]any{"max_model_len": 512},
	})
	resp, err := api.PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create deployment: %d %s", resp.StatusCode, raw)
	}
	var dep struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(raw, &dep); err != nil || dep.ID == 0 {
		t.Fatalf("create response %s: %v", raw, err)
	}
	levels := func() map[string]int {
		insts, _ := env.Store().InstancesForDeployment(dep.ID)
		out := map[string]int{}
		for _, in := range insts {
			if in.State == proto.InstRunning {
				out[in.UUID] = in.SleepLevel
			}
		}
		return out
	}
	env.Eventually(t, 45*time.Second, func() bool { return len(levels()) == 2 })
	var sleeper, server string
	for uuid := range levels() {
		if sleeper == "" {
			sleeper = uuid
		} else {
			server = uuid
		}
	}

	key, err := env.Store().CreateAPIKey("replica-sleep", 0)
	if err != nil {
		t.Fatal(err)
	}
	chat := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"replica-sleep","messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	post := func(path string, want int) {
		t.Helper()
		resp, err := api.PostJSON(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("POST %s = %d %s, want %d", path, resp.StatusCode, b, want)
		}
	}
	// servesOnly sends requests and checks every one is answered by the
	// named replica.
	servesOnly := func(uuid string) {
		t.Helper()
		seen.take()
		for i := 0; i < 8; i++ {
			if status, b := chat(); status != http.StatusOK {
				insts, _ := env.Store().InstancesForDeployment(dep.ID)
				for _, in := range insts {
					t.Logf("instance %s state=%s level=%d port=%d headless=%v role=%q intent=%d", in.UUID, in.State, in.SleepLevel, in.APIPort, in.Headless, in.Role, planning.InstanceSleepIntent(&in))
				}
				t.Fatalf("chat %d: %d %s", i, status, b)
			}
		}
		for _, container := range seen.take() {
			if container != containerOf(uuid) {
				t.Fatalf("a request reached %s, want only %s", container, containerOf(uuid))
			}
		}
	}

	env.Eventually(t, 15*time.Second, func() bool { status, _ := chat(); return status == http.StatusOK })
	post(fmt.Sprintf("/api/v1/instances/%s/sleep?level=1", sleeper), http.StatusOK)
	// The order takes the sleeper out of routing at once, before it reports.
	servesOnly(server)
	env.Eventually(t, 15*time.Second, func() bool { l := levels(); return l[sleeper] == 1 && l[server] == 0 })
	servesOnly(server)
	if got, _ := env.Store().GetDeployment(dep.ID); got.DesiredSleep != 0 {
		t.Fatalf("one replica's order put the deployment under a sleep order: %d", got.DesiredSleep)
	}

	// A control-plane restart re-pushes nothing that would wake the sleeper
	// or sleep the other.
	env.RestartServer(t)
	env.WaitNodeOnline(t, "replica-sleep-node")
	env.Eventually(t, 15*time.Second, func() bool { l := levels(); return l[sleeper] == 1 && l[server] == 0 })
	servesOnly(server)

	post(fmt.Sprintf("/api/v1/instances/%s/wake", sleeper), http.StatusOK)
	env.Eventually(t, 45*time.Second, func() bool { l := levels(); return l[sleeper] == 0 && l[server] == 0 })
	seen.take()
	env.Eventually(t, 15*time.Second, func() bool {
		if status, _ := chat(); status != http.StatusOK {
			return false
		}
		for _, container := range seen.take() {
			if container == containerOf(sleeper) {
				return true
			}
		}
		return false
	})

	// With every replica ordered to sleep the model is asleep.
	post(fmt.Sprintf("/api/v1/deployments/%d/sleep", dep.ID), http.StatusOK)
	if status, b := chat(); status != http.StatusServiceUnavailable || !strings.Contains(b, "asleep") {
		t.Fatalf("chat with every replica asleep: %d %s", status, b)
	}
}
