package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

// A run names the request priority class its requests ask the gateway for;
// only a run whose requests go through the gateway may, and only a class
// that exists. The run keeps the name and says it back.
func TestBenchmarkPriorityClassNeedsTheGateway(t *testing.T) {
	st, err := store.Open("file:benchmark_priority?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("prio-node", store.HashToken("token"), "prio-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "chat", ModelID: benchmarkChatModel(t, st, "chat").ID, ServedName: "chat-model", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "chat-0", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 31001, GPUIndexes: `[0]`}); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("prio-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	box, _ := auth.NewSecretbox("prio-test")
	h := &apiHandlers{d: Deps{Store: st, AuthSigner: signer, AISecretbox: box, Server: StaticServerSettings{Loopback: "http://127.0.0.1:1"}}, benchmarkExecutions: map[uint]*benchmarkExecution{}}
	var createAs func(bearer, target string) (int, map[string]any)
	user, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "user", Role: "user"})
	create := func(target string) (int, map[string]any) {
		return createAs(token, target)
	}
	createAs = func(bearer, target string) (int, map[string]any) {
		body := `{"name":"prio",` + target + `,"dataset":{"kind":"random","input_tokens":8,"output_tokens":4},"mode":"grid","search":{"concurrency":[1],"isl":[8],"osl":[4],"requests_per_point_multiplier":1},"allow_conflict":true}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		h.createBenchmark(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	managed := fmt.Sprintf(`"target_kind":"instance","deployment_id":%d`, dep.ID)

	status, out := create(managed + `,"priority_class":" High "`)
	if status != http.StatusAccepted || out["priority_class"] != "high" || out["load_origin"] != "server" {
		t.Fatalf("server origin = %d %v", status, out)
	}
	run, err := st.GetBenchmarkRun(uint(out["id"].(float64)))
	if err != nil || run.PriorityClass != "high" {
		t.Fatalf("stored = %+v err=%v", run, err)
	}
	// The snapshot says whether the class could order anything: this
	// instance was launched without priority scheduling.
	if snapshot, _ := out["target_snapshot"].(map[string]any); snapshot["priority_scheduling"] != false {
		t.Fatalf("snapshot = %v", out["target_snapshot"])
	}
	// Only an administrator chooses a class; anyone else may still run.
	if status, _ := createAs(user, managed+`,"priority_class":"high"`); status != http.StatusForbidden {
		t.Fatalf("user = %d", status)
	}
	if status, _ := createAs(user, managed); status != http.StatusAccepted {
		t.Fatalf("user without a class = %d", status)
	}
	// None named: the key is absent and the requests ask for nothing.
	if status, out := create(managed); status != http.StatusAccepted || out["priority_class"] != nil {
		t.Fatalf("none = %d %v", status, out)
	}
	for _, tc := range []struct {
		name, target, code string
	}{
		{"no such class", managed + `,"priority_class":"platinum"`, messages.ErrorPriorityClassNotFound},
		{"a node origin calls the instance directly", managed + fmt.Sprintf(`,"load_origin":%q,"priority_class":"high"`, node.UUID), ErrorBenchmarkPriorityClassGateway},
		{"one instance is measured directly", managed + `,"instance_uuid":"chat-0","priority_class":"high"`, ErrorBenchmarkPriorityClassGateway},
		// Refused before the endpoint is probed: the closed port would fail otherwise.
		{"an external endpoint is no gateway of ours", `"target_kind":"external","base_url":"http://127.0.0.1:1","model":"m","priority_class":"high"`, ErrorBenchmarkPriorityClassGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, out := create(tc.target)
			if status != http.StatusBadRequest || out["code"] != tc.code {
				t.Fatalf("= %d %v, want 400 %s", status, out, tc.code)
			}
		})
	}
}

// Every request of a server-origin run says which run it is, so the gateway
// records it as an evaluation's, and asks for the run's class when it has one.
func TestBenchmarkRequestHeader(t *testing.T) {
	h := benchmarkRequestHeader(12, "", true)
	if h.Get(loadgen.BenchmarkRunHeader) != "12" || h.Get(loadgen.PriorityClassHeader) != "" {
		t.Fatalf("no class = %v", h)
	}
	if h := benchmarkRequestHeader(12, "high", true); h.Get(loadgen.PriorityClassHeader) != "high" || h.Get(loadgen.BenchmarkRunHeader) != "12" {
		t.Fatalf("class = %v", h)
	}
	// An external endpoint is not told the run.
	if h := benchmarkRequestHeader(12, "high", false); h.Get(loadgen.BenchmarkRunHeader) != "" || h.Get(loadgen.PriorityClassHeader) != "high" {
		t.Fatalf("external with class = %v", h)
	}
	if h := benchmarkRequestHeader(12, "", false); h != nil {
		t.Fatalf("external = %v, want none", h)
	}
}
