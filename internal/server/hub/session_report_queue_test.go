package hub

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// queueFixture is a current session over a file-backed store with instances
// already pulling, and a way to hold the store's writer so the session's
// worker stalls on its first write.
type queueFixture struct {
	st   *store.Store
	h    *Hub
	s    *Session
	node store.Node
}

func newQueueFixture(t *testing.T, instances ...string) *queueFixture {
	t.Helper()
	st, err := store.Open(t.TempDir()+"/queue.db", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node := store.Node{UUID: "queue-node", Name: "queue-node"}
	if err := st.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	for _, uuid := range instances {
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, NodeID: node.ID, State: proto.InstPulling, Phase: proto.PhaseImagePull}); err != nil {
			t.Fatal(err)
		}
	}
	h := New(st, quietLog())
	s := reportTestSession(h, &node)
	h.sessions[node.UUID] = s
	return &queueFixture{st: st, h: h, s: s, node: node}
}

// holdWriter keeps the store's single writer busy until the returned release
// is called, so whatever the worker writes next waits.
func (f *queueFixture) holdWriter(t *testing.T) (release func()) {
	t.Helper()
	held, done := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- f.st.DB().Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("UPDATE nodes SET name = name WHERE id = ?", f.node.ID).Error; err != nil {
				return err
			}
			close(held)
			<-done
			return nil
		})
	}()
	<-held
	return func() {
		close(done)
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
}

func (f *queueFixture) report(t *testing.T, st proto.InstanceStatus) {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	within(t, "status.instance receipt", time.Second, func() { f.s.handle(proto.Envelope{T: proto.TStatusInstance, D: raw}) })
}

func (f *queueFixture) pending() int {
	q := f.s.reportQ.Load()
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// waitTaken returns once the worker has taken everything queued so far and is
// stuck writing it.
func (f *queueFixture) waitTaken(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.pending() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("worker never took the first report")
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *queueFixture) row(t *testing.T, uuid string) *store.Instance {
	t.Helper()
	row, err := f.st.InstanceByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// events counts the instance's timeline rows (creation writes the first).
func (f *queueFixture) events(t *testing.T, uuid string) int64 {
	t.Helper()
	var n int64
	if err := f.st.DB().Model(&store.InstanceEvent{}).Where("instance_uuid = ?", uuid).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func pullProgress(uuid string, done int64) proto.InstanceStatus {
	return proto.InstanceStatus{UUID: uuid, State: proto.InstPulling, Phase: proto.PhaseImagePull, Message: "pulling", BytesDone: done, BytesTotal: 100}
}

// While the store is slow, an instance's progress reports take at most two
// slots: the first of the state and phase (the transition, never replaced) and
// the newest after it, so a storm's backlog is bounded by the node's
// instances, not by how long the store is behind. The newest progress is what
// lands.
func TestProgressReportsCoalescePerInstance(t *testing.T) {
	f := newQueueFixture(t, "a", "b")
	release := f.holdWriter(t)
	f.report(t, pullProgress("a", 1))
	f.waitTaken(t)
	for done := int64(2); done <= 50; done++ {
		f.report(t, pullProgress("a", done))
		f.report(t, pullProgress("b", done))
	}
	// a's first report was already taken, so a keeps one slot; b keeps its
	// first report and its newest.
	if n := f.pending(); n != 3 {
		t.Fatalf("pending = %d, want a's newest, and b's first and newest", n)
	}
	release()
	f.s.drainReports()
	for _, uuid := range []string{"a", "b"} {
		if got := f.row(t, uuid).BytesDone; got != 50 {
			t.Fatalf("%s bytes_done = %d, want the newest progress (50)", uuid, got)
		}
	}
}

// Coalescing never merges away a transition or a report that carries more than
// progress: a new phase, or anything else for the instance, starts a new slot,
// and the reports apply in receipt order.
func TestProgressCoalescingStopsAtAnythingButProgress(t *testing.T) {
	f := newQueueFixture(t, "a")
	created := f.events(t, "a")
	release := f.holdWriter(t)
	f.report(t, pullProgress("a", 1))
	f.waitTaken(t)

	f.report(t, pullProgress("a", 2))
	extract := pullProgress("a", 3)
	extract.Phase = proto.PhaseImageExtract
	f.report(t, extract) // a new phase: a milestone
	withPorts := pullProgress("a", 4)
	withPorts.Phase, withPorts.Ports = proto.PhaseImageExtract, []int{8000}
	f.report(t, withPorts) // more than progress
	f.report(t, func() proto.InstanceStatus { st := pullProgress("a", 5); st.Phase = proto.PhaseImageExtract; return st }())
	if n := f.pending(); n != 4 {
		t.Fatalf("pending = %d, want 4: progress, new phase, ports, later progress", n)
	}
	release()
	f.s.drainReports()
	row := f.row(t, "a")
	if row.Phase != proto.PhaseImageExtract || row.BytesDone != 5 || row.Ports != "[8000]" {
		t.Fatalf("row phase=%q bytes_done=%d ports=%q, want every report applied in order", row.Phase, row.BytesDone, row.Ports)
	}
	if got := f.events(t, "a") - created; got != 1 {
		t.Fatalf("timeline events = %d, want the one phase change recorded and no progress", got)
	}
}

// A batched progress report the store does not take as progress — the row
// moved to another phase meanwhile — goes through the full path, which owns
// the transition and its event.
func TestBatchedProgressFallsBackToFullPath(t *testing.T) {
	f := newQueueFixture(t, "a")
	created := f.events(t, "a")
	if err := f.st.DB().Model(&store.Instance{}).Where("uuid = ?", "a").Update("phase", proto.PhaseImageVerify).Error; err != nil {
		t.Fatal(err)
	}
	f.report(t, pullProgress("a", 7))
	f.s.drainReports()
	row := f.row(t, "a")
	if row.Phase != proto.PhaseImagePull || row.BytesDone != 7 {
		t.Fatalf("row phase=%q bytes_done=%d, want the report applied by the full path", row.Phase, row.BytesDone)
	}
	if got := f.events(t, "a") - created; got != 1 {
		t.Fatalf("timeline events = %d, want the phase change recorded once", got)
	}
}

// Batching keeps receipt order across report kinds: a report received after
// progress sees that progress persisted (or a newer one that took its slot),
// never an older one.
func TestBatchedProgressKeepsOrderWithOtherReports(t *testing.T) {
	f := newQueueFixture(t, "a")
	release := f.holdWriter(t)
	f.report(t, pullProgress("a", 1))
	f.waitTaken(t)
	f.report(t, pullProgress("a", 2))
	var seen int64 = -1
	f.s.submitReport(func() { seen = f.row(t, "a").BytesDone })
	f.report(t, pullProgress("a", 3))
	release()
	f.s.drainReports()
	if seen < 2 {
		t.Fatalf("a report received after progress 2 saw bytes_done=%d", seen)
	}
	if got := f.row(t, "a").BytesDone; got != 3 {
		t.Fatalf("bytes_done = %d, want the last progress", got)
	}
}

// A replaced session's batched progress never lands.
func TestBatchedProgressIsFenced(t *testing.T) {
	f := newQueueFixture(t, "a")
	release := f.holdWriter(t)
	f.report(t, pullProgress("a", 1))
	f.waitTaken(t)
	f.report(t, pullProgress("a", 2))
	f.h.mu.Lock()
	f.h.sessions[f.node.UUID] = reportTestSession(f.h, &f.node)
	f.h.mu.Unlock()
	release()
	f.s.drainReports()
	if got := f.row(t, "a").BytesDone; got > 1 {
		t.Fatalf("bytes_done = %d: a replaced session's queued progress persisted", got)
	}
}

// Reports that cannot coalesce still back-pressure only their own read loop,
// and a read loop waiting on a full queue gives up once its session closes.
func TestFullReportQueueReleasesOnClose(t *testing.T) {
	f := newQueueFixture(t)
	release := f.holdWriter(t)
	defer f.s.drainReports()
	defer release()
	f.s.submitReport(func() { _ = f.st.DB().Exec("UPDATE nodes SET name = name").Error })
	f.waitTaken(t)
	for i := 0; i < sessionReportDepth; i++ {
		f.s.submitReport(func() {})
	}
	blocked := make(chan struct{})
	go func() { f.s.submitReport(func() {}); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("a full queue accepted another report")
	case <-time.After(50 * time.Millisecond):
	}
	f.s.markClosed()
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("closing the session left its read loop waiting on the full queue")
	}
}

// A batch the store fails as a whole is retried report by report on the full
// path, so a failed transaction loses no progress.
func TestFailedProgressBatchFallsBackToFullPath(t *testing.T) {
	f := newQueueFixture(t, "a", "b")
	injected := errors.New("injected batch failure")
	failed := false // only the first instance write: the batch's
	if err := f.st.DB().Callback().Update().Before("gorm:update").Register("test:fail_batch", func(db *gorm.DB) {
		if updates, ok := db.Statement.Dest.(map[string]any); ok && updates["bytes_done"] == int64(9) && !failed {
			failed = true
			_ = db.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	f.report(t, pullProgress("a", 9))
	f.report(t, pullProgress("b", 9))
	f.s.drainReports()
	if !failed {
		t.Fatal("the batch never wrote, so its failure was not exercised")
	}
	for _, uuid := range []string{"a", "b"} {
		if got := f.row(t, uuid).BytesDone; got != 9 {
			t.Fatalf("%s bytes_done = %d, want the report applied by the full path after the batch failed", uuid, got)
		}
	}
}

// The first report of a state and phase is the transition: the store records
// it as a timeline event carrying its message, the way an agent reports a
// preparation note just before the phase's own progress. A later progress
// report of the same phase must not replace it.
func TestFirstReportOfAPhaseIsNeverReplaced(t *testing.T) {
	f := newQueueFixture(t, "a")
	release := f.holdWriter(t)
	f.report(t, pullProgress("a", 1))
	f.waitTaken(t)
	note := proto.InstanceStatus{UUID: "a", State: proto.InstStarting, Phase: proto.PhaseContainerPrepare, Message: "device-share enabled: physical card 0"}
	f.report(t, note)
	preparing := note
	preparing.Message = "preparing container"
	f.report(t, preparing)
	release()
	f.s.drainReports()
	events, err := f.st.InstanceEvents(f.row(t, "a").ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(event.Message+event.Data, "device-share enabled") {
			if got := f.row(t, "a").Message; got != "preparing container" {
				t.Fatalf("message = %q, want the later report applied too", got)
			}
			return
		}
	}
	t.Fatalf("the phase's first report was replaced: no timeline event carries its message (events %+v)", events)
}
