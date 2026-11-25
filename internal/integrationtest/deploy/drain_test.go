package deploy

import (
	"bytes"
	"fmt"
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

// gatedEngines serves the fake engine per instance and records which
// instance each chat request reached; while held, chat requests wait until
// release, like generations still decoding.
type gatedEngines struct {
	mu       sync.Mutex
	held     bool
	release  chan struct{}
	arrivals []string // container names, in arrival order
}

func (g *gatedEngines) handler(inst testenv.EngineInstance) http.Handler {
	container := inst.Container
	engine := fakehandler.New(fakehandler.Options{Model: inst.Model})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			g.mu.Lock()
			g.arrivals = append(g.arrivals, container)
			held, release := g.held, g.release
			g.mu.Unlock()
			if held {
				<-release
			}
		}
		engine.ServeHTTP(w, r)
	})
}

func (g *gatedEngines) hold() {
	g.mu.Lock()
	g.held, g.release = true, make(chan struct{})
	g.mu.Unlock()
}

func (g *gatedEngines) open() {
	g.mu.Lock()
	if g.held {
		close(g.release)
	}
	g.held = false
	g.mu.Unlock()
}

func (g *gatedEngines) arrived() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.arrivals...)
}

func containerOf(uuid string) string {
	if len(uuid) > 12 {
		uuid = uuid[:12]
	}
	return "gpuworks-" + uuid
}

func chat(t *testing.T, env *testenv.Env, key string) (int, []byte) {
	t.Helper()
	return chatModel(t, env, key, "lifecycle-model")
}

func chatModel(t *testing.T, env *testenv.Env, key, model string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func instanceState(env *testenv.Env, uuid string) string {
	inst, err := env.Store().InstanceByUUID(uuid)
	if err != nil {
		return "gone"
	}
	return inst.State
}

// Scaling down while the named replica answers a request: the replica drains
// (no new request reaches it, the one it holds completes) and is retired
// only after that, leaving the other replica serving.
func TestScaleDownDrainsTheNamedReplicaWithoutCuttingItsRequest(t *testing.T) {
	gates := &gatedEngines{}
	driver := testenv.NewFakeDriver("docker")
	driver.SetEngineHandler(gates.handler)
	env, api, depID := lifecycleEnv(t, driver)
	running := waitRunning(t, env, depID, 2, nil)
	key, err := env.Store().CreateAPIKey("drain", 0)
	if err != nil {
		t.Fatal(err)
	}

	gates.hold()
	held := make(chan int, 1)
	go func() {
		status, _ := chat(t, env, key)
		held <- status
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(gates.arrived()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the held request never reached an engine")
		}
		time.Sleep(10 * time.Millisecond)
	}
	busy, idle := running[0], running[1]
	if gates.arrived()[0] == containerOf(running[1]) {
		busy, idle = running[1], running[0]
	}

	resp, err := api.Do(mustRequest(t, http.MethodPut, env.URL()+fmt.Sprintf("/api/v1/deployments/%d/replicas", depID), fmt.Sprintf(`{"replicas":1,"remove":[%q]}`, busy)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scale down = %d", resp.StatusCode)
	}
	if got := instanceState(env, busy); got != proto.InstDraining {
		t.Fatalf("named replica = %s, want draining", got)
	}

	// New work goes to the replica that stays, while the drained one still
	// holds its request.
	gates.open()
	for i := 0; i < 3; i++ {
		if status, body := chat(t, env, key); status != http.StatusOK {
			t.Fatalf("chat during drain = %d %s", status, body)
		}
	}
	if status := <-held; status != http.StatusOK {
		t.Fatalf("the drained replica's request ended %d", status)
	}
	for _, container := range gates.arrived()[1:] {
		if container != containerOf(idle) {
			t.Fatalf("a request reached the draining replica: %v", gates.arrived())
		}
	}

	// Once idle it is retired, and the deployment settles on one replica
	// with no failed attempt and no replacement.
	deadline = time.Now().Add(30 * time.Second)
	for instanceState(env, busy) != "gone" {
		if time.Now().After(deadline) {
			t.Fatalf("drained replica is %s after the drain", instanceState(env, busy))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if left := waitRunning(t, env, depID, 1, []string{busy}); left[0] != idle {
		t.Fatalf("remaining replica = %v, want %s", left, idle)
	}
}

// Restarting one replica through its instance drains it while the
// replacement starts, and costs the replica no retry.
func TestRestartOneReplicaReplacesItAndSparesTheOther(t *testing.T) {
	env, api, depID := lifecycleEnv(t, testenv.NewFakeDriver("docker"))
	running := waitRunning(t, env, depID, 2, nil)
	postAction(t, api, fmt.Sprintf("/api/v1/instances/%s/restart", running[0]), ``, http.StatusAccepted)
	if got := instanceState(env, running[0]); got != proto.InstDraining {
		t.Fatalf("restarted replica = %s, want draining", got)
	}
	after := waitRunning(t, env, depID, 2, []string{running[0]})
	kept := false
	for _, uuid := range after {
		kept = kept || uuid == running[1]
	}
	if !kept {
		t.Fatalf("the other replica was replaced too: before %v after %v", running, after)
	}
	insts, _ := env.Store().InstancesForDeployment(depID)
	for _, inst := range insts {
		if inst.State == proto.InstFailed {
			t.Fatalf("a restart left a failed attempt: %+v", inst)
		}
	}
	if event, found, _ := env.Store().LatestScheduleEvent(depID); !found || event.Code != store.EventReplicaRestarted {
		t.Fatalf("event = %+v", event)
	}
}

func mustRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// A rolling restart replaces the replicas one at a time: at every moment at
// least one replica serves and at most one drains, every replica ends on a new
// generation, and the restart record is cleared once done.
func TestRollingRestartReplacesOneReplicaAtATime(t *testing.T) {
	env, api, depID := lifecycleEnv(t, testenv.NewFakeDriver("docker"))
	first := waitRunning(t, env, depID, 2, nil)
	postAction(t, api, fmt.Sprintf("/api/v1/deployments/%d/restart", depID), ``, http.StatusAccepted)

	deadline := time.Now().Add(60 * time.Second)
	for {
		insts, _ := env.Store().InstancesForDeployment(depID)
		running, draining, old := 0, 0, 0
		for _, inst := range insts {
			switch inst.State {
			case proto.InstRunning:
				running++
			case proto.InstDraining:
				draining++
			}
			for _, uuid := range first {
				if inst.UUID == uuid {
					old++
				}
			}
		}
		if running == 0 {
			t.Fatalf("no replica served during the rolling restart: %+v", insts)
		}
		if draining > 1 {
			t.Fatalf("more than one replica drained at once: %+v", insts)
		}
		dep, _ := env.Store().GetDeployment(depID)
		if old == 0 && running == 2 && len(insts) == 2 && dep.RestartRequestedAt == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rolling restart did not finish: restart=%v instances=%+v", dep.RestartRequestedAt, insts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if event, found, _ := env.Store().LatestScheduleEvent(depID); !found || event.Code != store.EventDeploymentRollingRestarted {
		t.Fatalf("event = %+v", event)
	}
}
