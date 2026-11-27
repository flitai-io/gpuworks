package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/priority"
	"gpuworks.ai/gpuworks/internal/server/proxy/storeplane"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// launchSpec is the desired state JSON the store keeps for an instance
// launched with argv, as json.Marshal writes it.
func launchSpec(t testing.TB, argv ...string) string {
	t.Helper()
	raw, err := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{Entrypoint: argv[:1], Argv: argv[1:]}, UUID: "spec"})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// newRunSlotsRig serves three deployments from one engine stub, which holds
// each request until release is closed: "seq" (vLLM, --max-num-seqs 64),
// "run" (SGLang, --max-running-requests=32) and "bare" (vLLM stating no cap).
func newRunSlotsRig(t *testing.T) (rig *priorityRig, entered chan struct{}, release chan struct{}) {
	t.Helper()
	entered, release = make(chan struct{}, 8), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(upstream.Close)
	st, err := store.Open("file:proxy_run_slots_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-slots", store.HashToken("node"), "node-slots")
	if err != nil {
		t.Fatal(err)
	}
	rig = &priorityRig{st: st, deployments: map[string]uint{}}
	for i, d := range []struct {
		name, engine string
		spec         string
	}{
		{"seq", "vllm", launchSpec(t, "vllm", "serve", "/models/m", "--max-num-seqs", "64")},
		{"run", "sglang", launchSpec(t, "python3", "-m", "sglang.launch_server", "--model-path", "/models/m", "--max-running-requests=32")},
		{"bare", "vllm", launchSpec(t, "vllm", "serve", "/models/m")},
	} {
		dep, err := st.CreateDeployment(&store.Deployment{Name: d.name, ModelID: model.ID, NodeID: node.ID, ServedName: d.name, Engine: d.engine, State: "active"})
		if err != nil {
			t.Fatal(err)
		}
		rig.deployments[d.name] = dep.ID
		if _, err := st.CreateInstance(&store.Instance{UUID: d.name + "-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000 + i, Ports: "[30000]", DesiredStateJSON: d.spec}); err != nil {
			t.Fatal(err)
		}
	}
	addr := strings.TrimPrefix(upstream.URL, "http://")
	rig.p = New(storeplane.New(st), storeNetworks(st), dialTestHub{addr: addr}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(rig.p.Close)
	return rig, entered, release
}

func TestCandidatesCarryTheirRunSlots(t *testing.T) {
	rig, _, _ := newRunSlotsRig(t)
	want := map[string]int{"seq": 64, "run": 32, "bare": 0}
	// From the route snapshot.
	for name, slots := range want {
		deps, ok := rig.p.routes.snapshot(name)
		if !ok || len(deps) != 1 || len(deps[0].candidates) != 1 {
			t.Fatalf("%s is not routed with one candidate: %v", name, deps)
		}
		if got := deps[0].candidates[0].slots; got != slots {
			t.Fatalf("%s snapshot slots = %d, want %d", name, got, slots)
		}
	}
	// From the rows a request reads when the snapshot has no candidate.
	for name, slots := range want {
		dep, err := rig.st.GetDeployment(rig.deployments[name])
		if err != nil {
			t.Fatal(err)
		}
		tgt, rejection, _ := rig.p.selectDeploymentInstances(dep, name, "", admission{})
		if rejection != nil {
			t.Fatalf("%s refused: %+v", name, rejection)
		}
		if got := tgt.lease.c.slots; got != slots {
			t.Fatalf("%s fresh slots = %d, want %d", name, got, slots)
		}
		rig.p.releaseTarget(tgt)
	}
}

// A request holds its replica under the value of the class it runs at, from
// selection to the end of its answer.
func TestRequestHoldsItsReplicaAtItsClass(t *testing.T) {
	rig, entered, release := newRunSlotsRig(t)
	high := rig.key(t, store.APIKeyPriority{Class: priority.High})
	normal := rig.key(t, store.APIKeyPriority{})
	done := make(chan int, 3)
	send := func(key, asked string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"seq","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		if asked != "" {
			req.Header.Set(priorityClassHeader, asked)
		}
		rec := httptest.NewRecorder()
		rig.p.Handler()(rec, req)
		done <- rec.Code
	}
	go send(high, "")
	go send(normal, "")
	go send(high, priority.Batch)
	for range 3 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the requests did not reach the engine")
		}
	}
	counts := rig.p.balancer.counts("seq-instance")
	if counts.total != 3 || counts.atLeast(20) != 1 || counts.atLeast(0) != 2 || counts.atLeast(-40) != 3 || counts.atLeast(21) != 0 {
		t.Fatalf("in flight on seq = %+v, want one high, one normal and one batch", counts)
	}
	close(release)
	for range 3 {
		if code := <-done; code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
	}
	if counts := rig.p.balancer.counts("seq-instance"); counts.total != 0 || len(counts.byValue) != 0 {
		t.Fatalf("after the answers: %+v, want nothing left", counts)
	}
}
