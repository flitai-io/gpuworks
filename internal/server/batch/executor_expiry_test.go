package batch

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestResumeAtDeadlineKeepsCheckpointWithoutStartingExpiryTimer(t *testing.T) {
	r := newRig(t, Config{PerBatch: 1}, func(ctx context.Context, owner Owner, path string, body []byte) Reply {
		if strings.Contains(string(body), `"q0"`) {
			return echo(ctx, owner, path, body)
		}
		<-ctx.Done()
		return Reply{Err: ctx.Err()}
	})
	stop := r.start()
	b := r.batchOf(3)
	eventually(t, "first result saved and second request in flight", func() bool { return r.send.sent() == 2 })
	stop()
	sentBeforeRestart := r.send.sent()
	r.clock.Advance(b.ExpiresAt.Sub(r.clock.Now()))
	r.send.handle = echo
	r.restart()
	r.start()

	done := r.await(b.ID, StatusExpired)
	if sent := r.send.sent() - sentBeforeRestart; sent != 0 {
		t.Fatalf("sent %d requests on expired resume", sent)
	}
	if done.Completed != 1 || done.Failed != 2 || done.ExpiredAt == nil {
		t.Fatalf("expired batch = %+v", done)
	}
	if got := customIDs(r.resultLines(done.OutputFileID)); !slices.Equal(got, []string{"c0"}) {
		t.Fatalf("checkpointed results = %v", got)
	}
	expired := r.resultLines(done.ErrorFileID)
	if got := customIDs(expired); !slices.Equal(got, []string{"c1", "c2"}) {
		t.Fatalf("expired requests = %v", got)
	}
	for _, line := range expired {
		if line.Error == nil || line.Error.Code != "batch_expired" {
			t.Fatalf("error for %s = %+v", line.CustomID, line.Error)
		}
	}
	if hasDelay(r.clock, 0) {
		t.Fatal("an already expired batch started a deadline timer")
	}
}

func TestRunningBatchExpiryAbortsInFlightAndLeavesQueuedRequestUnsent(t *testing.T) {
	r := newRig(t, Config{PerBatch: 1}, func(ctx context.Context, _ Owner, _ string, _ []byte) Reply {
		<-ctx.Done()
		return Reply{Err: ctx.Err()}
	})
	r.start()
	b := r.batchOf(2)
	remaining := b.ExpiresAt.Sub(r.clock.Now())
	eventually(t, "one request in flight with expiry armed", func() bool {
		return r.send.sent() == 1 && hasDelay(r.clock, remaining)
	})
	r.clock.Advance(remaining)

	done := r.await(b.ID, StatusExpired)
	if sent := r.send.sent(); sent != 1 || done.Completed != 0 || done.Failed != b.Total {
		t.Fatalf("sent %d requests, completed %d, failed %d", sent, done.Completed, done.Failed)
	}
}

func TestResumeCancellingBatchPastDeadlineFinishesCancelled(t *testing.T) {
	r := newRig(t, Config{}, echo)
	b := r.batchOf(2)
	if _, err := r.svc.Cancel(testOwner, b.ID); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(b.ExpiresAt.Sub(r.clock.Now()))
	r.restart()
	r.start()

	done := r.await(b.ID, StatusCancelled)
	if r.send.sent() != 0 || done.CancelledAt == nil || done.ExpiredAt != nil || done.Failed != 0 {
		t.Fatalf("sent %d requests, batch = %+v", r.send.sent(), done)
	}
}
