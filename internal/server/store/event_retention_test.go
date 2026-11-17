package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// openRetentionStore is a file-backed store: it has the read pool and the
// single-connection writer of a real installation.
func openRetentionStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addRetentionEvent(t *testing.T, st *Store, uuid, data string, at time.Time) {
	t.Helper()
	if err := st.DB().Create(&InstanceEvent{
		DeploymentID: 1, InstanceUUID: uuid, Kind: EventKindSchedule, Code: EventDeploymentUnschedulable,
		Data: data, CreatedAt: at,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

// setRetentionBatch sets the batch size for the test (the hook is per store).
func setRetentionBatch(t *testing.T, size int) {
	t.Helper()
	old := eventRetentionBatch
	eventRetentionBatch = size
	t.Cleanup(func() { eventRetentionBatch = old })
}

// eventsSize is the table's size as the byte budget counts it.
func eventsSize(t *testing.T, st *Store) int64 {
	t.Helper()
	var total int64
	if err := st.DB().Model(&InstanceEvent{}).Select("COALESCE(SUM(" + eventSizeExpr + "), 0)").Scan(&total).Error; err != nil {
		t.Fatal(err)
	}
	return total
}

// writeWithin is one control-plane write that must get the write connection
// within d: the setting write takes the same single connection as any other.
func writeWithin(st *Store, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return st.DB().WithContext(ctx).Exec("UPDATE settings SET value = value WHERE key = ?", SettingDeploymentPolicy).Error
}

// An age sweep deletes in batches, a statement each, and lets go of the write
// connection between them: a write waiting meanwhile goes through rather than
// waiting for the whole backlog. Before, one statement deleted the backlog: on
// a 1M-event table, half of it held every write for 47 s.
func TestAnAgeSweepDeletesInBatchesAndLetsWritesThroughBetween(t *testing.T) {
	st := openRetentionStore(t)
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		addRetentionEvent(t, st, "old", "x", now.Add(-72*time.Hour))
	}
	addRetentionEvent(t, st, "fresh", "x", now.Add(-time.Hour))
	addRetentionEvent(t, st, "fresh", "x", now)

	setRetentionBatch(t, 3)
	var batches []int64
	var writeErrs []error
	st.eventRetentionBatchHook = func(n int64) {
		batches = append(batches, n)
		// A batch holds the writer for milliseconds; the bound is for a
		// loaded runner, far under a whole-backlog delete.
		writeErrs = append(writeErrs, writeWithin(st, 10*time.Second))
	}
	n, err := st.DeleteEventsBefore(context.Background(), now.Add(-24*time.Hour), 0)
	if err != nil || n != 10 {
		t.Fatalf("deleted %d err=%v, want the 10 old events", n, err)
	}
	if got := len(batches); got != 4 || batches[0] != 3 || batches[3] != 1 {
		t.Fatalf("batches = %v, want 3,3,3,1", batches)
	}
	for i, err := range writeErrs {
		if err != nil {
			t.Fatalf("a write between batch %d and the next did not get the write connection: %v", i+1, err)
		}
	}
	var left []InstanceEvent
	if err := st.DB().Order("id").Find(&left).Error; err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || left[0].InstanceUUID != "fresh" || left[1].InstanceUUID != "fresh" {
		t.Fatalf("left %d events %+v, want the 2 fresh ones", len(left), left)
	}
}

// The byte estimate is a scan of the whole table, so it runs on the read
// pool: a sweep with nothing to trim finishes while another holds the write
// connection. Before, the scan ran on that connection: on a 1M-event table it
// held every write for 2.1 s each time it ran.
func TestTheByteEstimateDoesNotNeedTheWriteConnection(t *testing.T) {
	st := openRetentionStore(t)
	for i := 0; i < 20; i++ {
		addRetentionEvent(t, st, "e", strings.Repeat("x", 1000), time.Now())
	}
	tx := st.DB().Begin() // takes the writer's only connection
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()

	// A sweep that wrongly waits for the writer ends at this deadline with
	// its error, rather than running on after the test.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := st.DeleteEventsBefore(ctx, time.Time{}, 1<<40); err != nil {
		t.Fatalf("the byte estimate waited for the write connection: %v", err)
	}
}

// What a trim frees is counted the way the total is (characters of the text
// columns), so the trim stops once the table fits and not before. Before, it
// subtracted byte lengths from a character total: with text of multi-byte
// characters it stopped while the table was still over its budget. And it
// deletes only as many of the oldest as the budget needs, not a whole batch.
func TestAByteTrimStopsOnceTheTableFits(t *testing.T) {
	st := openRetentionStore(t)
	const events, chars = 20, 1000
	for i := 0; i < events; i++ {
		addRetentionEvent(t, st, "e", strings.Repeat("中", chars), time.Now())
	}
	full := eventsSize(t, st)
	one := full / events
	budget := full / 2
	setRetentionBatch(t, 4)
	var batches int
	st.eventRetentionBatchHook = func(int64) { batches++ }

	n, err := st.DeleteEventsBefore(context.Background(), time.Time{}, budget)
	if err != nil {
		t.Fatal(err)
	}
	after := eventsSize(t, st)
	if after > budget {
		t.Fatalf("after the trim the table is %d, over its budget %d (deleted %d of %d)", after, budget, n, events)
	}
	if after <= budget-one {
		t.Fatalf("after the trim the table is %d, more than one event under its budget %d (deleted %d)", after, budget, n)
	}
	if n != events/2 || batches != 3 {
		t.Fatalf("deleted %d in %d batches, want %d in 3", n, batches, events/2)
	}
	// The oldest went: the newest events are the ones kept.
	var oldest uint
	if err := st.DB().Model(&InstanceEvent{}).Select("MIN(id)").Scan(&oldest).Error; err != nil {
		t.Fatal(err)
	}
	if oldest != uint(events/2)+1 {
		t.Fatalf("oldest kept event is %d, want %d", oldest, events/2+1)
	}
}

// An event an older release wrote has NULL code and data (they came with a
// later migration). It still counts toward the byte budget by its message:
// before, LENGTH(NULL) made its whole size NULL, SUM skipped it, and such
// events were never trimmed for size.
func TestALegacyEventWithNullColumnsCountsTowardTheBudget(t *testing.T) {
	st := openRetentionStore(t)
	for i := 0; i < 10; i++ {
		if err := st.DB().Exec("INSERT INTO instance_events (instance_id, instance_uuid, deployment_id, kind, code, message, data, created_at) VALUES (0, 'legacy', 1, 'state', NULL, ?, NULL, ?)",
			strings.Repeat("m", 1000), time.Now().UTC()).Error; err != nil {
			t.Fatal(err)
		}
	}
	if got := eventsSize(t, st); got != 10*1000 {
		t.Fatalf("legacy events count %d toward the budget, want their 10000 characters of message", got)
	}
	n, err := st.DeleteEventsBefore(context.Background(), time.Time{}, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || eventsSize(t, st) != 5000 {
		t.Fatalf("deleted %d, size now %d; want 5 deleted and 5000 left", n, eventsSize(t, st))
	}
}

// A sweep stops between batches once its context ends - the server waits for
// it on shutdown - and reports what it deleted so far; the next sweep picks
// up the rest.
func TestASweepStopsBetweenBatchesWhenItsContextEnds(t *testing.T) {
	st := openRetentionStore(t)
	old := time.Now().UTC().Add(-72 * time.Hour)
	for i := 0; i < 10; i++ {
		addRetentionEvent(t, st, "old", "x", old)
	}
	setRetentionBatch(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st.eventRetentionBatchHook = func(int64) { cancel() }
	n, err := st.DeleteEventsBefore(ctx, time.Now().Add(-24*time.Hour), 0)
	if !errors.Is(err, context.Canceled) || n != 3 {
		t.Fatalf("deleted %d err=%v, want 3 deleted (one batch) and context.Canceled", n, err)
	}
	st.eventRetentionBatchHook = nil
	n, err = st.DeleteEventsBefore(context.Background(), time.Now().Add(-24*time.Hour), 0)
	if err != nil || n != 7 {
		t.Fatalf("the next sweep deleted %d err=%v, want the remaining 7", n, err)
	}

	// The byte trim stops the same way.
	for i := 0; i < 10; i++ {
		addRetentionEvent(t, st, "e", strings.Repeat("x", 1000), time.Now())
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	st.eventRetentionBatchHook = func(int64) { cancel2() }
	n, err = st.DeleteEventsBefore(ctx2, time.Time{}, 1000)
	if !errors.Is(err, context.Canceled) || n != 3 {
		t.Fatalf("byte trim deleted %d err=%v, want 3 (one batch) and context.Canceled", n, err)
	}
}
