package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Hold only the first run read, after SQL returned, so the competing operation
// can commit deterministically before the stale caller tries to write.
func pauseBenchmarkRunRead(t *testing.T, st *Store) (<-chan struct{}, func()) {
	t.Helper()
	read, release := make(chan struct{}), make(chan struct{})
	var held atomic.Bool
	if err := st.db.Callback().Query().After("gorm:query").Register("test:pause_benchmark_run", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "BenchmarkRun" && held.CompareAndSwap(false, true) {
			close(read)
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	return read, func() { close(release) }
}

func waitBenchmarkRunRead(t *testing.T, read <-chan struct{}) {
	t.Helper()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("status did not reach the read barrier")
	}
}

func TestBenchmarkConcurrentCancelPreservesTerminalStateAndPoint(t *testing.T) {
	for _, tc := range []struct {
		name, envelope, existing, incoming, expected string
		pointEvent                                   bool
	}{
		{"ignore late progress result", "running", "running", "done", "running", false},
		{"ignore late completion result", "done", "running", "done", "running", false},
		{"ignore late failure result", "failed", "running", "failed", "running", false},
		{"retain canceled partial result", "running", "running", "canceled", "canceled", true},
		{"keep prior complete evidence", "running", "done", "canceled", "done", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := bus.New()
			st, err := Open("file:cancel_race_"+tc.name+"?mode=memory&cache=shared", events)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			run, err := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", State: "running", Mode: "grid"})
			if err != nil {
				t.Fatal(err)
			}
			original := BenchmarkPoint{RunID: run.ID, Seq: 1, State: tc.existing, Success: 13, Curve: `{"version":1}`}
			if err := st.UpsertBenchmarkPoint(&original); err != nil {
				t.Fatal(err)
			}
			sub := events.Subscribe(8, "benchmark")
			defer sub.Close()
			read, release := pauseBenchmarkRunRead(t, st)
			done := make(chan error, 1)
			point := json.RawMessage(fmt.Sprintf(`{"seq":1,"state":%q,"success":1,"errors":1,"long_tail":{"Evidence":{"version":1,"all":{"attempted":2,"canceled":1}}}}`, tc.incoming))
			go func() {
				done <- st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: tc.envelope, Point: point})
			}()
			waitBenchmarkRunRead(t, read)
			if err := st.CancelBenchmarkRun(run.ID, "operator canceled"); err != nil {
				release()
				t.Fatal(err)
			}
			before, err := st.GetBenchmarkRun(run.ID)
			if err != nil {
				release()
				t.Fatal(err)
			}
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			after, err := st.GetBenchmarkRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != "canceled" || after.Error != before.Error || after.Progress != before.Progress || !after.EndedAt.Equal(*before.EndedAt) {
				t.Fatalf("stale report changed canceled run: before=%+v after=%+v", before, after)
			}
			saved, err := st.GetBenchmarkPoint(run.ID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if saved.State != tc.expected || saved.Curve != original.Curve || saved.ID != original.ID {
				t.Fatalf("point identity/evidence changed: before=%+v after=%+v", original, saved)
			}
			expectedSuccess := 13
			if tc.pointEvent {
				expectedSuccess = 1
			}
			if saved.Success != expectedSuccess {
				t.Fatalf("point success=%d, want %d", saved.Success, expectedSuccess)
			}
			reasons := []string{}
			for len(sub.C()) > 0 {
				reasons = append(reasons, (<-sub.C()).Reason)
			}
			want := []string{"canceled"}
			if tc.pointEvent {
				want = append(want, "point")
			}
			if fmt.Sprint(reasons) != fmt.Sprint(want) {
				t.Fatalf("events=%v want=%v", reasons, want)
			}
		})
	}
}

func TestBenchmarkConcurrentCompletionWinsStaleCancel(t *testing.T) {
	for _, terminal := range []string{"done", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			events := bus.New()
			st, err := Open("file:complete_cancel_race_"+terminal+"?mode=memory&cache=shared", events)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			run, err := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", State: "running", Mode: "grid", Search: `{"concurrency":[1]}`})
			if err != nil {
				t.Fatal(err)
			}
			sub := events.Subscribe(8, "benchmark")
			defer sub.Close()
			read, release := pauseBenchmarkRunRead(t, st)
			done := make(chan error, 1)
			go func() { done <- st.CancelBenchmarkRun(run.ID, "stale cancellation") }()
			waitBenchmarkRunRead(t, read)
			err = st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: terminal, Message: "original terminal reason", StopReason: proto.BenchmarkStopGridExhausted, Point: json.RawMessage(`{"seq":1,"state":"done","success":1,"output_tput":2}`)})
			if err != nil {
				release()
				t.Fatal(err)
			}
			before, err := st.GetBenchmarkRun(run.ID)
			if err != nil {
				release()
				t.Fatal(err)
			}
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			after, err := st.GetBenchmarkRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != terminal || after.Error != before.Error || after.Verdict != before.Verdict || !after.EndedAt.Equal(*before.EndedAt) {
				t.Fatalf("stale cancel changed terminal run: before=%+v after=%+v", before, after)
			}
			if len(sub.C()) != 1 || (<-sub.C()).Reason != "status" {
				t.Fatal("losing cancel published a state change")
			}
		})
	}
}

func TestBenchmarkPointFailureRollsBackStatusWithoutPublishing(t *testing.T) {
	events := bus.New()
	st, err := Open("file:benchmark_point_status_rollback?mode=memory&cache=shared", events)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", State: "running", Mode: "grid"})
	if err != nil {
		t.Fatal(err)
	}
	sub := events.Subscribe(8, "benchmark")
	defer sub.Close()
	if err := st.db.Callback().Create().Before("gorm:create").Register("test:point_failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "BenchmarkPoint" {
			tx.AddError(errors.New("injected point write failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "done", Point: json.RawMessage(`{"seq":1,"state":"done"}`)}); err == nil {
		t.Fatal("point error was hidden")
	}
	after, err := st.GetBenchmarkRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "running" || after.EndedAt != nil || len(sub.C()) != 0 {
		t.Fatalf("failed point leaked terminal state/event: %+v", after)
	}
}

func TestBenchmarkDeletedCanceledRunCannotAcquireLatePoint(t *testing.T) {
	events := bus.New()
	st, err := Open("file:benchmark_deleted_partial_point?mode=memory&cache=shared", events)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", State: "canceled", Mode: "grid"})
	if err != nil {
		t.Fatal(err)
	}
	sub := events.Subscribe(8, "benchmark")
	defer sub.Close()
	read, release := pauseBenchmarkRunRead(t, st)
	done := make(chan error, 1)
	go func() {
		done <- st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "running", Point: json.RawMessage(`{"seq":1,"state":"canceled","errors":1}`)})
	}()
	waitBenchmarkRunRead(t, read)
	if err := st.DeleteBenchmarkRun(run.ID); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	points, err := st.ListBenchmarkPoints(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 0 {
		t.Fatalf("late cancellation inserted orphan points: %+v", points)
	}
	if len(sub.C()) != 1 || (<-sub.C()).Reason != "deleted" {
		t.Fatal("late orphan point published a change")
	}
}
