package proxy

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/priority"
	"gpuworks.ai/gpuworks/internal/server/proxy/storeplane"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// declared is the admission of a request of priority value that asked to be
// refused rather than wait.
func declared(value int) admission { return admission{priority: value, rejectBusy: true} }

// leaseOf is the lease of a pick, whatever it says of busy.
func leaseOf(l *lease, _ bool) *lease { return l }

// Two requests that asked not to wait race for a replica's last free run
// slot: exactly one takes it and the other is refused, every time.
func TestTwoDeclaredRequestsRaceForTheLastSlot(t *testing.T) {
	for round := range 200 {
		b := newBalancer(nil)
		replica := []candidate{{instanceUUID: "a", nodeUUID: "n", slots: 2}}
		if l, busy := b.pick(replica, nil, allOnline, admission{}); l == nil || busy {
			t.Fatalf("round %d: the first request was not taken", round)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		taken, refused := 0, 0
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				l, busy := b.pick(replica, nil, allOnline, declared(0))
				mu.Lock()
				defer mu.Unlock()
				switch {
				case l != nil && !busy:
					taken++
				case l == nil && busy:
					refused++
				default:
					t.Errorf("round %d: lease %v with busy %v", round, l, busy)
				}
			}()
		}
		close(start)
		wg.Wait()
		if taken != 1 || refused != 1 {
			t.Fatalf("round %d: %d taken and %d refused, want one of each", round, taken, refused)
		}
		if got := b.counts("a").total; got != 2 {
			t.Fatalf("round %d: %d leases held, want the 2 slots", round, got)
		}
	}
}

// On a replica that schedules by priority only the requests of the declaring
// one's class or above hold its slots: the engine runs a lower one after it.
func TestBusyCountsTheRequestsOfItsClassOrAbove(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inflight []int
		value    int
		busy     bool
	}{
		{"two normal ahead of a normal", []int{0, 0}, 0, true},
		{"two batch below a normal", []int{-40, -40}, 0, false},
		{"two normal below a high", []int{0, 0}, 20, false},
		{"two high ahead of a high", []int{20, 20}, 20, true},
		{"a high and a batch around a normal", []int{20, -40}, 0, false},
		{"a critical and a high ahead of a batch", []int{40, 20}, -40, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBalancer(nil)
			replica := []candidate{{instanceUUID: "a", nodeUUID: "n", slots: 2, prioritized: true}}
			for _, v := range tc.inflight {
				if l, busy := b.pick(replica, nil, allOnline, admission{priority: v}); l == nil || busy {
					t.Fatal("an undeclared request was not taken")
				}
			}
			l, busy := b.pick(replica, nil, allOnline, declared(tc.value))
			if busy != tc.busy || (l == nil) != tc.busy {
				t.Fatalf("lease %v busy %v, want busy %v", l, busy, tc.busy)
			}
			if !tc.busy && l.priority != tc.value {
				t.Fatalf("leased at %d, want %d", l.priority, tc.value)
			}
		})
	}
}

// Under every routing policy a declared request whose preferred replica is
// full takes another with room, and is refused only when none has; an
// undeclared one still takes the preferred replica.
func TestEveryRoutingPolicyTakesAReplicaWithRoom(t *testing.T) {
	// full is preferred by each policy: least_request scores it lowest (one
	// lease against two), failover lists it first, and the affinity key
	// ranks it first.
	full := candidate{instanceUUID: "full", nodeUUID: "n", slots: 1, uuidHash: 1}
	roomy := candidate{instanceUUID: "roomy", nodeUUID: "n", slots: 8, uuidHash: 2}
	key := ""
	for i := 0; key == ""; i++ {
		k := "conversation-" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		if mix64(fnv64([]byte(k))^full.uuidHash) > mix64(fnv64([]byte(k))^roomy.uuidHash) {
			key = k
		}
	}
	for _, policy := range []string{deploy.LeastRequest, deploy.Failover, deploy.PrefixAffinity} {
		t.Run(policy, func(t *testing.T) {
			b := newBalancer(nil)
			candidates := []candidate{full, roomy}
			b.take(full, admission{})
			b.take(roomy, admission{})
			b.take(roomy, admission{})
			plain, busy := b.pickBy(policy, candidates, key, nil, allOnline, admission{})
			if plain == nil || busy || plain.c.instanceUUID != "full" {
				t.Fatalf("undeclared pick = %v busy %v, want the preferred full replica", plain, busy)
			}
			plain.release(outcomeCanceled)
			l, busy := b.pickBy(policy, candidates, key, nil, allOnline, declared(0))
			if l == nil || busy || l.c.instanceUUID != "roomy" {
				t.Fatalf("declared pick = %v busy %v, want the replica with room", l, busy)
			}
			for range 5 {
				b.take(roomy, admission{})
			}
			if l, busy := b.pickBy(policy, candidates, key, nil, allOnline, declared(0)); l != nil || !busy {
				t.Fatalf("with both full the declared pick = %v busy %v, want refused", l, busy)
			}
			// Passed over or offline replicas are not eligible: no room
			// among the others is no room.
			if l, busy := b.pickBy(policy, candidates, key, map[string]bool{"roomy": true}, allOnline, declared(0)); l != nil || !busy {
				t.Fatalf("with roomy excluded = %v busy %v, want refused", l, busy)
			}
			if l, busy := b.pickBy(policy, candidates, key, nil, func(string) bool { return false }, declared(0)); l != nil || busy {
				t.Fatalf("with no replica online = %v busy %v, want none and not busy", l, busy)
			}
		})
	}
}

// A replica whose launch states no run slots always has room: a deployment
// with one is never refused for being busy.
func TestUnknownSlotsNeverRefuse(t *testing.T) {
	b := newBalancer(nil)
	bare := []candidate{{instanceUUID: "bare", nodeUUID: "n"}}
	for range 100 {
		b.take(bare[0], admission{priority: 40})
	}
	if l, busy := b.pick(bare, nil, allOnline, declared(-40)); l == nil || busy {
		t.Fatalf("declared pick on a replica of unknown slots = %v busy %v", l, busy)
	}
}

// A resend after a connection failure is never refused: a replica already
// took the request, so the backup is leased even when it is full.
func TestResendAfterAConnectionFailureIsNotRefused(t *testing.T) {
	b := newBalancer(nil)
	candidates := []candidate{{instanceUUID: "primary", nodeUUID: "n", modelName: "m", slots: 1}, {instanceUUID: "backup", nodeUUID: "n", modelName: "m", slots: 1}}
	b.take(candidates[1], admission{})
	first, busy := b.pickFailover(candidates, nil, allOnline, declared(0))
	if first == nil || busy || first.c.instanceUUID != "primary" {
		t.Fatalf("first pick = %v busy %v", first, busy)
	}
	transport := &retryTransport{balancer: b, hub: dialTestHub{}}
	attempt := &routeAttempt{candidates: candidates, current: first, tried: map[string]bool{"primary": true}, attempts: 1, modelName: "m", policy: deploy.Failover, admission: declared(0)}
	first.release(outcomeConnFailure)
	next := transport.pickNext(attempt, false)
	if next == nil || next.c.instanceUUID != "backup" {
		t.Fatalf("resend = %v, want the full backup", next)
	}
	if got := b.counts("backup").total; got != 2 {
		t.Fatalf("backup holds %d leases, want 2", got)
	}
}

// busyRig serves the busy-rejection deployments from one engine stub that
// holds each chat until let() lets one go:
//
//	two    vLLM, one replica of --max-num-seqs 2
//	bare   vLLM, one replica stating no cap
//	mixed  vLLM, a replica of --max-num-seqs 1 and one stating no cap
//	prio   vLLM with priority scheduling, one replica of --max-num-seqs 2
//	nap    vLLM, one replica of --max-num-seqs 2, ordered to sleep
//	wide   vLLM over two nodes (a leader of --max-num-seqs 1 and its follower)
//	idle   vLLM of --max-num-seqs 2 with no running instance
type busyRig struct {
	*priorityRig
	acct    *Accountant
	entered chan struct{}
	release chan struct{}
	stop    sync.Once
	mu      sync.Mutex
	// declarations are the X-GPUWorks-If-Busy values the engine got.
	declarations []string
	records      []uxrecord.Request
}

func newBusyRig(t *testing.T) *busyRig {
	t.Helper()
	rig := &busyRig{entered: make(chan struct{}, 16), release: make(chan struct{})}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		rig.mu.Lock()
		rig.declarations = append(rig.declarations, r.Header.Values(ifBusyHeader)...)
		rig.mu.Unlock()
		rig.entered <- struct{}{}
		<-rig.release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(rig.letAll)
	st, err := store.Open("file:proxy_busy_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-busy", store.HashToken("node"), "node-busy")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.RegisterNode("node-busy-2", store.HashToken("node-2"), "node-busy-2")
	if err != nil {
		t.Fatal(err)
	}
	capped := func(n string) string { return launchSpec(t, "vllm", "serve", "/models/m", "--max-num-seqs", n) }
	rig.priorityRig = &priorityRig{st: st, deployments: map[string]uint{}}
	port := 30000
	for _, d := range []struct {
		name   string
		nnodes int
		insts  []store.Instance
		prio   bool
	}{
		{"two", 1, []store.Instance{{DesiredStateJSON: capped("2")}}, false},
		{"prio", 1, []store.Instance{{DesiredStateJSON: launchSpec(t, "vllm", "serve", "/models/m", "--max-num-seqs", "2", "--scheduling-policy", "priority")}}, true},
		{"bare", 1, []store.Instance{{DesiredStateJSON: launchSpec(t, "vllm", "serve", "/models/m")}}, false},
		{"mixed", 1, []store.Instance{{DesiredStateJSON: capped("1")}, {DesiredStateJSON: launchSpec(t, "vllm", "serve", "/models/m")}}, false},
		{"nap", 1, []store.Instance{{DesiredStateJSON: capped("2")}}, false},
		{"wide", 2, []store.Instance{{DesiredStateJSON: capped("1")}, {DesiredStateJSON: capped("1"), Headless: true, NodeID: other.ID}}, false},
		{"idle", 1, nil, false},
	} {
		dep, err := st.CreateDeployment(&store.Deployment{Name: d.name, ModelID: model.ID, NodeID: node.ID, ServedName: d.name, Engine: "vllm", State: "active", NNodes: d.nnodes, PriorityScheduling: d.prio})
		if err != nil {
			t.Fatal(err)
		}
		rig.deployments[d.name] = dep.ID
		for i, in := range d.insts {
			in.UUID = d.name + "-" + string(rune('a'+i))
			in.DeploymentID, in.State, in.APIPort, in.Ports = dep.ID, proto.InstRunning, port, "[30000]"
			if in.NodeID == 0 {
				in.NodeID = node.ID
			}
			if in.Headless {
				in.APIPort = 0
			}
			port++
			if _, err := st.CreateInstance(&in); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.OrderInstancesSleep(rig.deployments["nap"], []string{"nap-a"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(upstream.URL, "http://")
	rig.p = New(storeplane.New(st), storeNetworks(st), dialTestHub{addr: addr}, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(rig.p.Close)
	rig.acct = newAccountant(nil, 16, nil)
	rig.p.SetAccountant(rig.acct)
	rig.p.SetRequestSink(func(r uxrecord.Request) {
		rig.mu.Lock()
		rig.records = append(rig.records, r)
		rig.mu.Unlock()
	})
	rig.p.routes.rebuild()
	return rig
}

func (r *busyRig) letAll() { r.stop.Do(func() { close(r.release) }) }

// chat sends a chat for model with key, the class asked for and the
// X-GPUWorks-If-Busy value (empty: none).
func (r *busyRig) chat(key, model, class, ifBusy string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if class != "" {
		req.Header.Set(priorityClassHeader, class)
	}
	if ifBusy != "" {
		req.Header.Set(ifBusyHeader, ifBusy)
	}
	rec := httptest.NewRecorder()
	r.p.Handler()(rec, req)
	return rec
}

// hold sends n chats that the engine holds, and waits until it has them.
func (r *busyRig) hold(t *testing.T, key, model, class, ifBusy string, n int) chan *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, n)
	for range n {
		go func() { done <- r.chat(key, model, class, ifBusy) }()
	}
	r.waitEntered(t, n)
	return done
}

func (r *busyRig) waitEntered(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-r.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("a request did not reach the engine")
		}
	}
}

// forwarded sends a chat that should reach the engine and be held there.
func (r *busyRig) forwarded(t *testing.T, key, model, class, ifBusy string) chan *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- r.chat(key, model, class, ifBusy) }()
	select {
	case <-r.entered:
	case rec := <-done:
		t.Fatalf("%s with %q was answered %d %s, want it forwarded", model, ifBusy, rec.Code, rec.Body)
	case <-time.After(5 * time.Second):
		t.Fatalf("%s with %q did not reach the engine", model, ifBusy)
	}
	return done
}

func (r *busyRig) class429(depID uint) uint64 {
	r.acct.mu.Lock()
	defer r.acct.mu.Unlock()
	if c, ok := r.acct.counters[depID]; ok {
		return c.classTotals[class429]
	}
	return 0
}

// A declared request to a model whose replicas are full is answered at once
// with 429 MODEL_BUSY and Retry-After: 1, counted in the 429 class without a
// request record or a new last request time. An undeclared one, and one
// declaring any other value, are forwarded as before, and no engine sees the
// header.
func TestBusyModelRefusesADeclaredRequestAtOnce(t *testing.T) {
	rig := newBusyRig(t)
	key, err := rig.st.CreateAPIKey("busy", 0)
	if err != nil {
		t.Fatal(err)
	}
	depID := rig.deployments["two"]
	held := rig.hold(t, key, "two", "", "", 2)
	before, _ := rig.acct.LastRequestAt(depID)

	started := time.Now()
	rec := rig.chat(key, "two", "", "reject")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the refusal took %s", elapsed)
	}
	want := `{"error":{"message":"every replica of two is busy and the request asked not to wait; send it again later, elsewhere, or without X-GPUWorks-If-Busy","type":"invalid_request_error","code":"MODEL_BUSY","param":null,"params":{"model":"two"}}}`
	if rec.Code != http.StatusTooManyRequests || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("declared request = %d %s\nwant 429 %s", rec.Code, rec.Body, want)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if rec.Header().Get("X-Request-ID") == "" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("refusal headers = %v", rec.Header())
	}
	if got := rig.class429(depID); got != 1 {
		t.Fatalf("429 class = %d, want the refusal counted once", got)
	}
	if after, _ := rig.acct.LastRequestAt(depID); !after.Equal(before) {
		t.Fatalf("the refusal moved the last request time from %s to %s", before, after)
	}
	rig.mu.Lock()
	records := len(rig.records)
	rig.mu.Unlock()
	if records != 0 {
		t.Fatalf("%d request records before any answer ended, want none for the refusal", records)
	}
	if got := rig.p.balancer.counts("two-a").total; got != 2 {
		t.Fatalf("the refusal changed the replica's leases: %d", got)
	}

	// Without the declaration, or with another value, the request waits
	// as it always did.
	var more []chan *httptest.ResponseRecorder
	for _, value := range []string{"", "Reject", "yes", "wait"} {
		more = append(more, rig.forwarded(t, key, "two", "", value))
	}
	rig.letAll()
	for range 2 {
		if rec := <-held; rec.Code != http.StatusOK {
			t.Fatalf("held request = %d", rec.Code)
		}
	}
	for _, done := range more {
		if rec := <-done; rec.Code != http.StatusOK {
			t.Fatalf("forwarded request = %d %s", rec.Code, rec.Body)
		}
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.declarations) != 0 {
		t.Fatalf("the engine got X-GPUWorks-If-Busy: %q", rig.declarations)
	}
	if got := rig.class429(depID); got != 1 {
		t.Fatalf("429 class = %d after the answers, want 1", got)
	}
}

// The class a request runs at reaches the busy judgement on a replica that
// schedules by priority: two batch requests leave both slots to normal ones,
// and two normal ones both slots to high ones, but two high ones none to a
// high one.
func TestBusyJudgesAtTheRequestsClass(t *testing.T) {
	rig := newBusyRig(t)
	key := rig.key(t, store.APIKeyPriority{Class: priority.Normal, Ceiling: priority.Critical})
	accepted := []chan *httptest.ResponseRecorder{rig.hold(t, key, "prio", priority.Batch, "", 2)}
	for _, class := range []string{"", ""} {
		accepted = append(accepted, rig.forwarded(t, key, "prio", class, "reject"))
	}
	if rec := rig.chat(key, "prio", "", "reject"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a normal with two batch and two normal held = %d, want 429", rec.Code)
	}
	for _, class := range []string{priority.High, priority.High} {
		accepted = append(accepted, rig.forwarded(t, key, "prio", class, "reject"))
	}
	if rec := rig.chat(key, "prio", priority.High, "reject"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a high with two high held = %d, want 429", rec.Code)
	}
	accepted = append(accepted, rig.forwarded(t, key, "prio", priority.Critical, "reject"))
	rig.letAll()
	answers := 0
	for _, done := range accepted {
		for range cap(done) {
			if rec := <-done; rec.Code != http.StatusOK {
				t.Fatalf("accepted request = %d %s", rec.Code, rec.Body)
			}
			answers++
		}
	}
	if answers != 7 {
		t.Fatalf("%d answers, want 7", answers)
	}
}

// The deployments that never refuse: one with a replica of unknown slots,
// a sleeping one (answered as today, here without a wake to wait for), and
// a multi-node one.
func TestDeploymentsThatNeverRefuseForBusy(t *testing.T) {
	rig := newBusyRig(t)
	key, err := rig.st.CreateAPIKey("busy", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("unknown slots", func(t *testing.T) {
		rig.hold(t, key, "bare", "", "", 3)
		rig.forwarded(t, key, "bare", "", "reject")
	})
	t.Run("a capped replica full and one of unknown slots", func(t *testing.T) {
		rig.hold(t, key, "mixed", "", "", 2)
		for range 3 {
			rig.forwarded(t, key, "mixed", "", "reject")
		}
		// The declaration changes nothing on a deployment that does not
		// refuse: the requests spread as undeclared ones would.
		if got := rig.p.balancer.counts("mixed-a").total + rig.p.balancer.counts("mixed-b").total; got != 5 {
			t.Fatalf("the replicas hold %d leases, want all 5 requests", got)
		}
	})
	t.Run("asleep", func(t *testing.T) {
		rec := rig.chat(key, "nap", "", "reject")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"MODEL_ASLEEP"`) {
			t.Fatalf("declared request to a sleeping deployment = %d %s, want 503 MODEL_ASLEEP", rec.Code, rec.Body)
		}
	})
	t.Run("multi-node", func(t *testing.T) {
		rig.hold(t, key, "wide", "", "", 1)
		rig.forwarded(t, key, "wide", "", "reject")
	})
	rig.letAll()
}

// A PD deployment's decode replicas are never judged busy.
func TestPDDeploymentNeverRefusesForBusy(t *testing.T) {
	rig := newBusyRig(t)
	deps, ok := rig.p.routes.snapshot("two")
	if !ok || len(deps) != 1 || len(deps[0].candidates) != 1 {
		t.Fatalf("two is not routed: %v", deps)
	}
	rd := *deps[0]
	rd.pd = &pdRoute{}
	for range 2 {
		rig.p.balancer.take(rd.candidates[0], admission{})
	}
	if !(&routeDeployment{replicas: 1, slottedReplicas: 1}).rejectsBusy() || (&routeDeployment{pd: &pdRoute{}, replicas: 1, slottedReplicas: 1}).rejectsBusy() {
		t.Fatal("rejectsBusy does not leave PD deployments out")
	}
	tgt, ok, busy := rig.p.pickRouted(&rd, "two", "", nil, declared(0))
	if !ok || busy {
		t.Fatalf("PD pick = ok %v busy %v, want the full decode replica leased", ok, busy)
	}
	rig.p.releaseTarget(tgt)
	plain := *deps[0]
	if _, ok, busy := rig.p.pickRouted(&plain, "two", "", nil, declared(0)); ok || !busy {
		t.Fatalf("the same replica outside PD = ok %v busy %v, want busy", ok, busy)
	}
}

// /v1/key says which models refuse a declared request when busy: those
// every running replica of which states run slots, and that are neither
// multi-node nor without a running replica.
func TestKeyInfoSaysWhichModelsRefuseWhenBusy(t *testing.T) {
	rig := newBusyRig(t)
	key, err := rig.st.CreateAPIKey("busy", 0)
	if err != nil {
		t.Fatal(err)
	}
	rec := getKey(t, rig.p, http.MethodGet, key)
	var body struct {
		Features []string        `json:"features"`
		Models   json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/key = %d %s %v", rec.Code, rec.Body, err)
	}
	if !slices.Contains(body.Features, FeatureBusyReject) {
		t.Fatalf("features = %q, want busy_reject", body.Features)
	}
	got := string(body.Models)
	want := `[{"id":"bare","priority_scheduling":"none","busy_reject":false},` +
		`{"id":"idle","priority_scheduling":"none","busy_reject":false},` +
		`{"id":"mixed","priority_scheduling":"none","busy_reject":false},` +
		`{"id":"nap","priority_scheduling":"none","busy_reject":true},` +
		`{"id":"prio","priority_scheduling":"all","busy_reject":true},` +
		`{"id":"two","priority_scheduling":"none","busy_reject":true},` +
		`{"id":"wide","priority_scheduling":"none","busy_reject":false}]`
	if got != want {
		t.Fatalf("models =\n%s\nwant\n%s", got, want)
	}
}
