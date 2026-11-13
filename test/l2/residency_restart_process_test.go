//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// restartableRig is the docker automation rig with a server that can be
// stopped (SIGINT, its graceful path) or killed (SIGKILL, no shutdown at all)
// and started again on the same port and data directory, the agent and its
// containers living on.
type restartableRig struct {
	base, token, node string
	modelID           uint
	stop, kill        func()
	start             func()
}

func startRestartableRig(t *testing.T, name string) *restartableRig {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil || exec.Command("docker", "info").Run() != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker required in strict mode")
		}
		t.Skip("docker unavailable")
	}
	root := repoRoot()
	if err := ensureFakeEngineImage(t, root); err != nil {
		t.Fatal(err)
	}
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	dataDir, logDir := t.TempDir(), t.TempDir()
	rig := &restartableRig{base: base}
	var server *exec.Cmd
	starts := 0
	first := true
	rig.start = func() {
		t.Helper()
		starts++
		var cmd *exec.Cmd
		if first {
			cmd = serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir)
			first = false
		} else {
			cmd = exec.Command(serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", dataDir, "--log-level", "warn")
			cmd.Env = os.Environ()
		}
		f, err := os.Create(filepath.Join(logDir, fmt.Sprintf("server-%d.log", starts)))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = f, f
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		server = cmd
		waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout,
			func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized })
		rig.token = login(t, base)
		if rig.node != "" {
			waitNode(t, base, rig.token, func(n processNode) bool { return n.UUID == rig.node && n.State == "online" })
		}
	}
	rig.stop = func() {
		t.Helper()
		if err := server.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := server.Process.Wait(); done <- err }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			_ = server.Process.Kill()
			t.Fatal("the server did not stop within 20s of SIGINT")
		}
	}
	rig.kill = func() {
		t.Helper()
		_ = server.Process.Kill()
		_, _ = server.Process.Wait()
	}
	t.Cleanup(func() {
		if server != nil && server.ProcessState == nil {
			stopProcess(server)
		}
	})
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for i := 1; i <= starts; i++ {
			if data, err := os.ReadFile(filepath.Join(logDir, fmt.Sprintf("server-%d.log", i))); err == nil {
				t.Logf("server log %d:\n%s", i, data)
			}
		}
	})
	rig.start()
	seed, nodeToken := createNode(t, base, rig.token, name+"-node")
	agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", filepath.Join(t.TempDir(), "agent-state"))
	agent.Env = append(os.Environ(), "GPUWORKS_FAKE_GPU=A100x1,24576", "GPUWORKS_FAKE_GPU_NO_INJECT=1", "GPUWORKS_RUNTIME_DRIVER=docker", "GPUWORKS_LOG_LEVEL=warn")
	agent.Stdout, agent.Stderr = io.Discard, io.Discard
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(agent) })
	rig.node = seed.UUID
	waitNode(t, base, rig.token, func(n processNode) bool { return n.UUID == seed.UUID && n.State == "online" })
	rig.modelID = registerFixtureModel(t, base, rig.token, seed.UUID, name)
	return rig
}

// chatReply is one /v1 chat completion as the client saw it: a response, or
// the transport error that ended it.
type chatReply struct {
	status     int
	retryAfter string
	body       string
	err        error
}

func (r *restartableRig) chat(key, model string, within time.Duration) chatReply {
	req, _ := http.NewRequest(http.MethodPost, r.base+"/v1/chat/completions",
		bytes.NewBufferString(`{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: within}).Do(req)
	if err != nil {
		return chatReply{err: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return chatReply{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: string(b)}
}

// eventsSince are the deployment's AUTO_SLEEP and AUTO_WAKE events after at,
// oldest first, as "CODE trigger".
func (r *restartableRig) eventsSince(t *testing.T, id uint, at time.Time) []string {
	t.Helper()
	_, data := authedDo(t, http.MethodGet, r.base, r.token, fmt.Sprintf("/api/v1/deployments/%d/events?limit=200", id), nil)
	var ev struct {
		Items []struct {
			Code      string         `json:"code"`
			CreatedAt time.Time      `json:"created_at"`
			Params    map[string]any `json:"params"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("events of %d: %v (%s)", id, err, data)
	}
	var out []string
	for i := len(ev.Items) - 1; i >= 0; i-- {
		e := ev.Items[i]
		if e.CreatedAt.After(at) && (e.Code == "AUTO_SLEEP" || e.Code == "AUTO_WAKE") {
			out = append(out, fmt.Sprintf("%s %v", e.Code, e.Params["trigger"]))
		}
	}
	return out
}

func (r *restartableRig) setYieldPolicy(t *testing.T, id uint, body string) {
	t.Helper()
	if resp, data := authedDo(t, http.MethodPut, r.base, r.token, fmt.Sprintf("/api/v1/deployments/%d/yield-policy", id), []byte(body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("yield policy %s: %d %s", body, resp.StatusCode, data)
	}
}

// TestProcessResidencyAcrossAServerStopAndKill drives the residency arbiter
// across real restarts of the server process; the agent and the fake-engine
// containers live on.
//
//   - A graceful stop answers a request held for a wake with a 503, a
//     Retry-After that covers a restart, and a message saying to send it
//     again; sent again to the new process, it is held and answered.
//   - A kill drops a held request's connection. Sent again, it is held and
//     answered: the wake that waited for room is in the store, and the holder
//     yields once idle - idle counted from the new process's start.
//   - A deployment put to sleep for another is on the resume list after a
//     kill: once its cards are free it takes them back without a request.
//
// Skipped without Docker.
//
//	go test -tags=multiproc -run TestProcessResidencyAcrossAServerStopAndKill -timeout 20m ./test/l2/
func TestProcessResidencyAcrossAServerStopAndKill(t *testing.T) {
	r := startRestartableRig(t, "restart")
	_, keyData := authedDo(t, http.MethodPost, r.base, r.token, "/api/v1/apikeys", []byte(`{"name":"restart"}`))
	var key struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(keyData, &key); err != nil || key.Key == "" {
		t.Fatalf("api key: %s", keyData)
	}
	create := func(served string, policy map[string]any) (uint, processInstance) {
		return createFakeSleepable(t, r.base, r.token, r.node, r.modelID, served, map[string]any{"yield_policy": policy})
	}
	// A (time-sliced) asleep, its wake waiting for the card H (idle-policy,
	// 600 s, so it does not yield on its own here) holds awake.
	a, aInst := create("restart-a", map[string]any{"policy": "timeslice"})
	sleepForTheTaking(t, r.base, r.token, a, aInst.UUID)
	h, hInst := create("restart-h", map[string]any{"policy": "idle", "idle_sec": 600})
	wakeWhenThereIsRoom(t, r.base, r.token, a)

	sendHeld := func() <-chan chatReply {
		got := make(chan chatReply, 1)
		go func() { got <- r.chat(key.Key, "restart-a", 5*time.Minute) }()
		time.Sleep(3 * time.Second) // held: H does not yield for 600 s
		return got
	}

	// Graceful stop while a request is held.
	held := sendHeld()
	r.stop()
	got := <-held
	if got.err != nil || got.status != http.StatusServiceUnavailable || got.retryAfter != "10" ||
		got.body != `{"error":{"message":"the server is restarting; send the request again","type":"gpuworks_proxy"}}`+"\n" {
		t.Fatalf("held request on a graceful stop = %+v, want 503 Retry-After 10 and the restart message", got)
	}
	r.start()

	// Kill while a request is held: the connection drops.
	held = sendHeld()
	r.kill()
	if got := <-held; got.err == nil {
		t.Fatalf("held request on a kill = %+v, want its connection dropped", got)
	}
	r.start()
	restartedAt := time.Now()
	// H now yields once idle 15 s; idle counts from this process's start.
	r.setYieldPolicy(t, h, `{"policy":"idle","idle_sec":15}`)
	if got := r.chat(key.Key, "restart-a", 3*time.Minute); got.err != nil || got.status != http.StatusOK {
		t.Fatalf("the request sent again after the kill = %+v, want it held and answered", got)
	}
	answered := time.Since(restartedAt)
	if answered < 14*time.Second {
		t.Fatalf("answered %v after the restart: H yielded before it was idle 15 s since the start", answered)
	}
	if ev := r.eventsSince(t, h, restartedAt); len(ev) == 0 || ev[0] != "AUTO_SLEEP preempt" {
		t.Fatalf("H's events after the restart = %v, want a yield", ev)
	}
	if ev := r.eventsSince(t, a, restartedAt); len(ev) == 0 || ev[len(ev)-1] != "AUTO_WAKE request" {
		t.Fatalf("A's events after the restart = %v, want a wake for the request", ev)
	}
	t.Logf("held request sent again after the kill answered %v after the restart", answered.Round(time.Millisecond))

	// A request for H (idle ranks above time-sliced) makes A yield at once:
	// A goes on the resume list.
	yieldedAt := time.Now()
	if got := r.chat(key.Key, "restart-h", 3*time.Minute); got.err != nil || got.status != http.StatusOK {
		t.Fatalf("request for H = %+v", got)
	}
	waitSleepLevelWithin(t, r.base, r.token, aInst.UUID, 1, time.Minute)
	if ev := r.eventsSince(t, a, yieldedAt); len(ev) == 0 || ev[len(ev)-1] != "AUTO_SLEEP preempt" {
		t.Fatalf("A's events after the request for H = %v, want a yield", ev)
	}

	// Kill; then free the card with an operator's held sleep of H. A takes
	// it back with no request.
	r.kill()
	r.start()
	resumedFrom := time.Now()
	if resp, data := authedDo(t, http.MethodPost, r.base, r.token, fmt.Sprintf("/api/v1/deployments/%d/sleep?level=2&wake_on_request=false", h), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("operator sleep of H: %d %s", resp.StatusCode, data)
	}
	waitSleepLevelWithin(t, r.base, r.token, hInst.UUID, 2, time.Minute)
	waitSleepLevelWithin(t, r.base, r.token, aInst.UUID, 0, time.Minute)
	if ev := r.eventsSince(t, a, resumedFrom); len(ev) != 1 || ev[0] != "AUTO_WAKE resume" {
		t.Fatalf("A's events after the kill = %v, want it resumed with no request", ev)
	}
	if engineSleeping(t, aInst.APIPort) {
		t.Fatal("A reads awake but its engine sleeps")
	}
}
