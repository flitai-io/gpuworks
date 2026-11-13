package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// Unit-level tests for classification and the Accountant itself. HTTP/proxy
// end-to-end accounting tests live in accounting_e2e_test.go.

// --- classification -------------------------------------------------------

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		asleep bool
		want   reqClass
		wantOK bool
	}{
		{status: 200, want: class2xx, wantOK: true},
		{status: 201, want: class2xx, wantOK: true},
		{status: 299, want: class2xx, wantOK: true},
		{status: 400, want: class4xx, wantOK: true},
		{status: 404, want: class4xx, wantOK: true},
		{status: 429, want: class429, wantOK: true},
		{status: 500, want: class5xx, wantOK: true},
		{status: 502, want: class5xx, wantOK: true},
		// A non-asleep 503 (e.g. "still starting") is a plain 5xx.
		{status: 503, want: class5xx, wantOK: true},
		// asleep always wins regardless of the numeric status passed in.
		{status: 503, asleep: true, want: classAsleep, wantOK: true},
		{status: 200, asleep: true, want: classAsleep, wantOK: true},
		// A successful WebSocket/h2c upgrade handshake folds into 2xx.
		{status: 101, want: class2xx, wantOK: true},
		// Statuses the contract has no bucket for: counted for duration, not class.
		{status: 100, wantOK: false},
		{status: 304, wantOK: false},
		{status: 0, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status=%d/asleep=%v", tc.status, tc.asleep), func(t *testing.T) {
			got, ok := classifyStatus(tc.status, tc.asleep)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("classifyStatus(%d,%v) = (%q,%v), want (%q,%v)", tc.status, tc.asleep, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestSelectDeploymentInstanceDistributedAsleepFlag guards the distributed
// asleep sentinel hook: a distributed deployment whose replica is asleep must
// surface asleep=true, not just a message containing "asleep". The leader
// reports the level; a headless follower sleeps with the leader's engine and
// reports none of its own.
func TestSelectDeploymentInstanceDistributedAsleepFlag(t *testing.T) {
	st, err := store.Open("file:proxy_acct_dist_asleep?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/dist", Revision: "main", Name: "dist"})
	if err != nil {
		t.Fatal(err)
	}
	nodeA, _ := st.RegisterNode("dist-a", store.HashToken("a"), "dist-a")
	nodeB, _ := st.RegisterNode("dist-b", store.HashToken("b"), "dist-b")
	nodeIDsJSON := fmt.Sprintf("[%d,%d]", nodeA.ID, nodeB.ID)
	dep, err := st.CreateDeployment(&store.Deployment{
		ModelID: model.ID, NodeID: nodeA.ID, NodeIDs: nodeIDsJSON, ServedName: "dist",
		Engine: "vllm", State: "active", NNodes: 2, DistGroup: "group-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "dist-leader", DeploymentID: dep.ID, NodeID: nodeA.ID, State: proto.InstRunning,
		APIPort: 30000, SleepLevel: 1, DistGroup: "group-x", DistRole: "leader", NNodes: 2, NodeRank: 0, DistAttempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{
		UUID: "dist-follower", DeploymentID: dep.ID, NodeID: nodeB.ID, State: proto.InstRunning,
		DistGroup: "group-x", DistRole: "follower", NNodes: 2, NodeRank: 1,
		Headless: true, DistAttempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"dist-a": true, "dist-b": true}}, nil, 0)
	t.Cleanup(p.Close)
	selected, _, status, message, asleep := p.selectDeploymentInstance("dist")
	if status != http.StatusServiceUnavailable || !strings.Contains(message, "asleep") || !asleep {
		t.Fatalf("status=%d message=%q asleep=%v, want 503 asleep=true", status, message, asleep)
	}
	if selected == nil || selected.ID != dep.ID {
		t.Fatalf("attempted deployment = %+v, want %d", selected, dep.ID)
	}
}

// --- Accountant unit behavior ----------------------------------------------

func TestAccountantDispatchFinishRecordsClassesDurationAndLastRequestAt(t *testing.T) {
	a := newAccountant(nil, 4, nil)
	fixedNow := time.Unix(1700000000, 0)
	a.now = func() time.Time { return fixedNow }

	a.Dispatch(7)
	a.Finish(7, 200, 150*time.Millisecond)
	a.Dispatch(7)
	a.Finish(7, 404, 5*time.Millisecond)
	a.Dispatch(7)
	a.Finish(7, 429, 5*time.Millisecond)
	a.Dispatch(7)
	a.Finish(7, 500, 5*time.Millisecond)
	// RecordImmediate never dispatched, so it must not touch duration_sum/count.
	a.RecordImmediate(7, http.StatusServiceUnavailable, true)
	// A request that never resolved a deployment at all is deployment 0.
	a.RecordImmediate(0, http.StatusNotFound, false)

	a.mu.Lock()
	c7 := a.counters[7]
	c0 := a.counters[0]
	a.mu.Unlock()

	if c7 == nil {
		t.Fatal("deployment 7 has no counters")
	}
	if c7.inflight != 0 {
		t.Fatalf("inflight = %d, want 0 (every Dispatch was matched by a Finish)", c7.inflight)
	}
	wantClasses := map[reqClass]uint64{class2xx: 1, class4xx: 1, class429: 1, class5xx: 1, classAsleep: 1}
	for class, want := range wantClasses {
		if got := c7.classTotals[class]; got != want {
			t.Fatalf("class %s = %d, want %d (all: %v)", class, got, want, c7.classTotals)
		}
	}
	if c7.durationCount != 4 {
		t.Fatalf("durationCount = %d, want 4 (only the 4 dispatched requests)", c7.durationCount)
	}
	if want := 150.0 + 5 + 5 + 5; c7.durationSumMs != want {
		t.Fatalf("durationSumMs = %v, want %v (RecordImmediate never contributes)", c7.durationSumMs, want)
	}
	if c0 == nil || c0.classTotals[class4xx] != 1 || c0.durationCount != 0 {
		t.Fatalf("deployment 0 = %+v, want class4xx=1 durationCount=0", c0)
	}

	if got, ok := a.LastRequestAt(7); !ok || !got.Equal(fixedNow) {
		t.Fatalf("LastRequestAt(7) = %v,%v want %v,true", got, ok, fixedNow)
	}
	if _, ok := a.LastRequestAt(99); ok {
		t.Fatal("LastRequestAt for a deployment with no traffic should report not-ok")
	}
}

// TestAccountantDispatchAdvancesLastRequestAtBeforeFinish covers item 2: a
// long-lived request must keep idle detection from firing for as long as it
// is in flight, not only once it completes.
func TestAccountantDispatchAdvancesLastRequestAtBeforeFinish(t *testing.T) {
	a := newAccountant(nil, 4, nil)
	t0 := time.Unix(1700000000, 0)
	a.now = func() time.Time { return t0 }

	if _, ok := a.LastRequestAt(5); ok {
		t.Fatal("LastRequestAt before any traffic should report not-ok")
	}
	a.Dispatch(5)
	got, ok := a.LastRequestAt(5)
	if !ok || !got.Equal(t0) {
		t.Fatalf("LastRequestAt right after Dispatch = %v,%v, want %v,true", got, ok, t0)
	}

	t1 := t0.Add(5 * time.Second)
	a.now = func() time.Time { return t1 }
	a.Finish(5, 200, time.Second)
	got, ok = a.LastRequestAt(5)
	if !ok || !got.Equal(t1) {
		t.Fatalf("LastRequestAt after Finish = %v,%v, want %v,true", got, ok, t1)
	}
}

// TestAccountantNilReceiverIsSafe documents the "unwired Proxy" contract: an
// Accountant field left nil (no SetAccountant call) must not panic.
func TestAccountantNilReceiverIsSafe(t *testing.T) {
	var a *Accountant
	a.Dispatch(1)
	a.Finish(1, 200, time.Millisecond)
	a.RecordImmediate(0, 404, false)
	if _, ok := a.LastRequestAt(1); ok {
		t.Fatal("nil Accountant.LastRequestAt should report not-ok")
	}
	if got := a.Dropped(); got != 0 {
		t.Fatalf("nil Accountant.Dropped() = %d, want 0", got)
	}
	a.Run(context.Background()) // must return immediately, not panic or hang
}

func assertSample(t *testing.T, samples []tsdb.Sample, metric string, labels map[string]string, want float64) {
	t.Helper()
	for _, s := range samples {
		if s.Metric != metric || !labelsEqual(s.Labels, labels) {
			continue
		}
		if s.Value != want {
			t.Fatalf("sample %s%v = %v, want %v", metric, labels, s.Value, want)
		}
		return
	}
	t.Fatalf("sample %s%v not found in %+v", metric, labels, samples)
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// countLogMsg counts JSON log lines in buf whose "msg" field equals msg
// (hasLogMsg, defined in copyobs_test.go, only reports presence).
func countLogMsg(buf *syncBuffer, msg string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		if strings.Contains(line, `"msg":"`+msg+`"`) {
			n++
		}
	}
	return n
}

func TestAccountantFlushProducesExpectedSamplesMonotonicAcrossFlushes(t *testing.T) {
	a := newAccountant(nil, 4, nil)
	t1 := time.Unix(1700000000, 0)
	a.now = func() time.Time { return t1 }

	a.Dispatch(3)
	a.Finish(3, 200, 10*time.Millisecond)

	a.flush()
	var batch1 []tsdb.Sample
	select {
	case batch1 = <-a.queue:
	default:
		t.Fatal("flush with activity should have enqueued a batch")
	}
	depLabel := map[string]string{"deployment": "3", "node": "server"}
	classLabel := map[string]string{"deployment": "3", "class": "2xx", "node": "server"}
	assertSample(t, batch1, metricRequestsTotal, classLabel, 1)
	assertSample(t, batch1, metricDurationSum, depLabel, 10)
	assertSample(t, batch1, metricDurationCount, depLabel, 1)
	assertSample(t, batch1, metricInflight, depLabel, 0)

	// No activity since the last flush and no in-flight requests: nothing
	// should be enqueued.
	a.flush()
	select {
	case batch := <-a.queue:
		t.Fatalf("flush with no new activity enqueued a batch: %+v", batch)
	default:
	}

	// Counters are cumulative: a second request pushes the total to 2, not
	// resets it to 1.
	t2 := t1.Add(20 * time.Second)
	a.now = func() time.Time { return t2 }
	a.Dispatch(3)
	a.Finish(3, 200, 30*time.Millisecond)
	a.flush()
	var batch2 []tsdb.Sample
	select {
	case batch2 = <-a.queue:
	default:
		t.Fatal("second flush with activity should have enqueued a batch")
	}
	assertSample(t, batch2, metricRequestsTotal, classLabel, 2)
	assertSample(t, batch2, metricDurationSum, depLabel, 40)
	assertSample(t, batch2, metricDurationCount, depLabel, 2)
}

// TestAccountantInFlightGaugeStaysVisibleAcrossMultipleFlushes covers item 2:
// a single long-lived in-flight request must keep showing up in the
// in-flight gauge on every flush it spans, not just the first one.
func TestAccountantInFlightGaugeStaysVisibleAcrossMultipleFlushes(t *testing.T) {
	a := newAccountant(nil, 8, nil)
	t0 := time.Unix(1700000000, 0)
	a.now = func() time.Time { return t0 }
	a.Dispatch(5)

	const ticks = 6
	for i := 0; i < ticks; i++ {
		tick := t0.Add(time.Duration(i+1) * flushInterval)
		a.now = func() time.Time { return tick }
		a.flush()
		var batch []tsdb.Sample
		select {
		case batch = <-a.queue:
		default:
			t.Fatalf("flush %d: an in-flight request should keep the deployment reported", i)
		}
		assertSample(t, batch, metricInflight, map[string]string{"deployment": "5", "node": "server"}, 1)
	}

	a.Finish(5, 200, 10*time.Millisecond)
	a.flush()
	batch := <-a.queue
	assertSample(t, batch, metricInflight, map[string]string{"deployment": "5", "node": "server"}, 0)
}

// TestAccountantFlushRetriesDroppedCountsOnNextFlush covers item 3: a batch
// dropped under backpressure must not lose its counts — the affected
// deployment stays dirty and is re-emitted the next time flush succeeds.
func TestAccountantFlushRetriesDroppedCountsOnNextFlush(t *testing.T) {
	a := newAccountant(nil, 1, nil) // capacity 1, nothing draining it yet
	a.now = func() time.Time { return time.Unix(1700000000, 0) }

	a.Dispatch(1)
	a.Finish(1, 200, time.Millisecond)
	a.flush() // occupies the only queue slot with deployment 1's batch

	a.Dispatch(2)
	a.Finish(2, 200, time.Millisecond)
	a.flush() // queue is full: dropped, deployment 2 must stay dirty
	if got := a.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}

	first := <-a.queue // drain deployment 1's batch, freeing a slot
	assertSample(t, first, metricRequestsTotal, map[string]string{"deployment": "1", "class": "2xx", "node": "server"}, 1)

	a.flush() // deployment 2's dropped counts should be retried now
	select {
	case second := <-a.queue:
		assertSample(t, second, metricRequestsTotal, map[string]string{"deployment": "2", "class": "2xx", "node": "server"}, 1)
		assertSample(t, second, metricAccountingDroppedTotal, map[string]string{"node": "server"}, 1)
	default:
		t.Fatal("deployment 2's dropped counts were not retried on the next flush")
	}
}

func TestAccountantFlushDropsUnderBackpressureWithoutBlocking(t *testing.T) {
	a := newAccountant(nil, 1, nil) // capacity 1, nothing draining it
	a.now = func() time.Time { return time.Unix(1700000000, 0) }

	a.Dispatch(1)
	a.Finish(1, 200, time.Millisecond)
	a.flush() // occupies the only queue slot

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Dispatch(1)
		a.Finish(1, 200, time.Millisecond)
		a.flush() // queue is still full: must drop, never block
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("flush blocked instead of dropping under sink backpressure")
	}
	if got := a.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
}

// TestAccountantFlushDropWarningsAreRateLimited covers item 4: the first drop
// always warns, then only every 100th does.
func TestAccountantFlushDropWarningsAreRateLimited(t *testing.T) {
	buf := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := newAccountant(nil, 1, log)
	a.now = func() time.Time { return time.Unix(1700000000, 0) }

	a.Dispatch(1)
	a.Finish(1, 200, time.Millisecond)
	a.flush() // fills the only slot; never drained below, so every later flush drops

	const drops = 250
	for i := 0; i < drops; i++ {
		a.Dispatch(1)
		a.Finish(1, 200, time.Millisecond)
		a.flush()
	}
	if got := a.Dropped(); got != drops {
		t.Fatalf("Dropped() = %d, want %d", got, drops)
	}
	const wantWarnings = 3 // drop #1, #100, #200
	if got := countLogMsg(buf, "proxy accounting flush dropped under sink backpressure"); got != wantWarnings {
		t.Fatalf("drop warning count = %d, want %d", got, wantWarnings)
	}
}

// TestAccountantFlushDoesNotLoseActivityInterleavedAfterSnapshot is the N1
// regression test. flush() used to compute the snapshot and clear dirty as
// two separately-locked steps; a Dispatch/Finish landing in the gap between
// them would set dirty back to true correctly, only for the clear step —
// unaware of the fresh update — to stomp it back to false, silently hiding
// that update from every later flush. snapshotAndClearDirty now does both
// under one lock, so the gap cannot exist; this test uses a hook that fires
// at exactly the point the old gap used to be to prove nothing is lost.
func TestAccountantFlushDoesNotLoseActivityInterleavedAfterSnapshot(t *testing.T) {
	a := newAccountant(nil, 4, nil)
	a.now = func() time.Time { return time.Unix(1700000000, 0) }
	a.Dispatch(1)
	a.Finish(1, 200, time.Millisecond)

	a.afterSnapshotAndClearForTest = func() {
		// A request completing "between snapshot and clear" -- i.e. right
		// after this flush already captured (and, pre-fix, would have wrongly
		// cleared) dirty for deployment 1.
		a.Dispatch(1)
		a.Finish(1, 200, time.Millisecond)
	}
	a.flush() // first flush: sends the original Finish's counts
	a.afterSnapshotAndClearForTest = nil

	batch1 := <-a.queue
	assertSample(t, batch1, metricRequestsTotal, map[string]string{"deployment": "1", "class": "2xx", "node": "server"}, 1)

	// The interleaved Finish must not have been swallowed: it must still
	// surface on the very next flush.
	a.flush()
	select {
	case batch2 := <-a.queue:
		assertSample(t, batch2, metricRequestsTotal, map[string]string{"deployment": "1", "class": "2xx", "node": "server"}, 2)
	default:
		t.Fatal("the interleaved Finish's activity was lost: the next flush had nothing to emit")
	}
}

// TestAccountantFlushEmitsDroppedTotalOnlyOnce is the N2 regression test:
// once a drop has occurred, an otherwise-idle accountant must not keep
// re-emitting gpuworks_proxy_accounting_dropped_total on every later flush --
// only the first successful flush after the count changes carries it.
func TestAccountantFlushEmitsDroppedTotalOnlyOnce(t *testing.T) {
	a := newAccountant(nil, 1, nil) // capacity 1, nothing draining it
	a.now = func() time.Time { return time.Unix(1700000000, 0) }

	a.Dispatch(1)
	a.Finish(1, 200, time.Millisecond)
	a.flush() // occupies the only queue slot

	a.Dispatch(1)
	a.Finish(1, 200, time.Millisecond)
	a.flush() // queue full: dropped
	if got := a.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}

	first := <-a.queue // drain, freeing a slot; no dropped-total yet (dropped == lastEmittedDropped's baseline)
	assertSample(t, first, metricRequestsTotal, map[string]string{"deployment": "1", "class": "2xx", "node": "server"}, 1)

	a.flush() // the retried counts go out, now WITH the dropped-total (count changed: 0 -> 1)
	second := <-a.queue
	assertSample(t, second, metricAccountingDroppedTotal, map[string]string{"node": "server"}, 1)

	// The accountant is now idle (no dirty deployments, no new drops): the
	// next flush must emit nothing at all, not a repeat of dropped-total.
	a.flush()
	select {
	case batch := <-a.queue:
		t.Fatalf("idle accountant with an already-reported drop emitted a batch: %+v", batch)
	default:
	}
}

func TestAccountantRoutingSeriesHaveBoundedCardinality(t *testing.T) {
	a := newAccountant(nil, 1, nil)
	a.now = func() time.Time { return time.Unix(1700000000, 0) }
	for i := 0; i < maxAccountingSeries+200; i++ {
		a.RecordRouted(1, fmt.Sprintf("instance-%d", i))
		a.RecordRetry(1, fmt.Sprintf("reason-%d", i))
	}
	a.snapshotAndClearDirty()
	a.mu.Lock()
	routed, retries := len(a.routed), len(a.retries)
	a.mu.Unlock()
	if routed > maxAccountingSeries || retries > maxAccountingSeries {
		t.Fatalf("routing series grew without bound: routed=%d retries=%d", routed, retries)
	}
}
