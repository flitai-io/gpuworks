package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// resultLines reads an output or error file of a finished batch.
func (r *rig) resultLines(fileID string) []resultLine {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.cfg.Dir, fileID))
	if err != nil {
		r.t.Fatalf("read %s: %v", fileID, err)
	}
	var out []resultLine
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var l resultLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			r.t.Fatalf("result line %q: %v", line, err)
		}
		out = append(out, l)
	}
	return out
}

func customIDs(lines []resultLine) []string {
	var ids []string
	for _, l := range lines {
		ids = append(ids, l.CustomID)
	}
	return ids
}

func echo(_ context.Context, _ Owner, _ string, body []byte) Reply {
	return okReply(`{"object":"chat.completion","choices":[{"index":0,"finish_reason":"stop"}],"echo":` + string(body) + `}`)
}

func TestExecutorCompletesABatchAndWritesTheOutputFile(t *testing.T) {
	r := newRig(t, Config{}, echo)
	r.start()
	b := r.batchOf(20)
	done := r.await(b.ID, StatusCompleted)

	if done.Total != 20 || done.Completed != 20 || done.Failed != 0 {
		t.Fatalf("counts = %+v", done)
	}
	if done.OutputFileID == "" || done.ErrorFileID != "" || done.CompletedAt == nil || done.InProgressAt == nil || done.FinalizingAt == nil {
		t.Fatalf("batch = %+v", done)
	}
	lines := r.resultLines(done.OutputFileID)
	if len(lines) != 20 {
		t.Fatalf("output lines = %d", len(lines))
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if seen[l.CustomID] || l.Error != nil || l.Response == nil || l.Response.StatusCode != 200 || l.Response.RequestID != "req-1" || !strings.HasPrefix(l.ID, "batch_req_") {
			t.Fatalf("bad line %+v", l)
		}
		seen[l.CustomID] = true
		var body struct{ Echo struct{ Model string } }
		if json.Unmarshal(l.Response.Body, &body) != nil || body.Echo.Model != "m" {
			t.Fatalf("body = %s", l.Response.Body)
		}
	}
	file, err := r.repo.GetFile(done.OutputFileID)
	if err != nil || file.Purpose != PurposeOutput || file.UserID != testOwner.UserID || file.Bytes == 0 {
		t.Fatalf("output row = %+v err=%v", file, err)
	}
	// The partial results go once the finish is recorded, a moment after the
	// batch reads completed.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat(r.svc.partialPath(b.ID))
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the partial file is still there: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The request goes to the gateway as the batch owner's, to the line's url,
// with the line's body untouched.
func TestExecutorSendsTheLinesBodyAsTheOwner(t *testing.T) {
	var mu sync.Mutex
	var got []string
	r := newRig(t, Config{}, func(_ context.Context, owner Owner, path string, body []byte) Reply {
		mu.Lock()
		got = append(got, fmt.Sprintf("%d/%d %s %s", owner.UserID, owner.APIKeyID, path, body))
		mu.Unlock()
		return okReply(`{}`)
	})
	r.start()
	b := r.batchOf(1)
	r.await(b.ID, StatusCompleted)
	want := `7/70 /v1/chat/completions {"model":"m","messages":[{"role":"user","content":"q0"}]}`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
}

func TestExecutorRunsAtMostPerBatchRequestsAtOnce(t *testing.T) {
	gate := make(chan struct{})
	r := newRig(t, Config{PerBatch: 8}, func(ctx context.Context, _ Owner, _ string, _ []byte) Reply {
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return okReply(`{}`)
	})
	r.start()
	b := r.batchOf(40)
	eventually(t, "8 requests in flight", func() bool { return r.send.peakInFlight() == 8 })
	time.Sleep(50 * time.Millisecond)
	if p := r.send.peakInFlight(); p != 8 {
		t.Fatalf("peak = %d, want exactly the per-batch cap of 8", p)
	}
	close(gate)
	r.await(b.ID, StatusCompleted)
	if p := r.send.peakInFlight(); p != 8 {
		t.Fatalf("peak = %d after the run", p)
	}
}

// Two batches share the global cap, and a batch beyond the executor slots
// waits validating until one ends.
func TestExecutorGlobalCapAcrossBatchesAndExecutorSlots(t *testing.T) {
	gate := make(chan struct{})
	r := newRig(t, Config{PerBatch: 8, GlobalCap: 5, MaxRunning: 2}, func(ctx context.Context, _ Owner, _ string, _ []byte) Reply {
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return okReply(`{}`)
	})
	r.start()
	a, b, c := r.batchOf(20), r.batchOf(20), r.batchOf(3)
	eventually(t, "the global cap reached", func() bool { return r.send.peakInFlight() == 5 })
	time.Sleep(50 * time.Millisecond)
	if p := r.send.peakInFlight(); p != 5 {
		t.Fatalf("peak = %d, want the global cap of 5", p)
	}
	statuses := map[string]int{}
	for _, id := range []string{a.ID, b.ID, c.ID} {
		got, _ := r.repo.GetBatch(id)
		statuses[got.Status]++
	}
	if statuses[StatusInProgress] != 2 || statuses[StatusValidating] != 1 {
		t.Fatalf("statuses = %v, want two executing and one waiting validating", statuses)
	}
	close(gate)
	for _, id := range []string{a.ID, b.ID, c.ID} {
		r.await(id, StatusCompleted)
	}
	if p := r.send.peakInFlight(); p > 5 {
		t.Fatalf("peak = %d, over the cap", p)
	}
}

func TestRetryClassificationAndErrorFile(t *testing.T) {
	abort := `{"choices":[{"index":0,"finish_reason":"abort","message":{"content":"par"}}]}`
	stop := `{"choices":[{"index":0,"finish_reason":"stop","message":{"content":"done"}}]}`
	for _, tc := range []struct {
		name    string
		first   Reply
		retried bool
	}{
		{"preempted 200 with finish_reason abort", okReply(abort), true},
		{"one of two choices aborted", okReply(`{"choices":[{"finish_reason":"stop"},{"finish_reason":"abort"}]}`), true},
		{"503 asleep", Reply{Status: 503, Body: []byte(`{"error":{"message":"asleep"}}`)}, true},
		{"429", Reply{Status: 429, Body: []byte(`{"error":{"message":"slow down"}}`)}, true},
		{"500", Reply{Status: 500, Body: []byte(`oops`)}, true},
		{"502 with a retry-after", Reply{Status: 502, RetryAfter: 3 * time.Second}, true},
		{"transport error", Reply{Err: fmt.Errorf("connection reset")}, true},
		{"abort text in a normal answer", okReply(`{"choices":[{"finish_reason":"stop","message":{"content":"abort the mission"}}]}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var mu sync.Mutex
			r := newRig(t, Config{}, func(context.Context, Owner, string, []byte) Reply {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					return tc.first
				}
				return okReply(stop)
			})
			r.start()
			b := r.batchOf(1)
			if tc.retried {
				eventually(t, "the retry's backoff", func() bool { return retryDelays(r.clock) > 0 })
				if st, _ := r.repo.GetBatch(b.ID); st.Failed != 0 || st.Status != StatusInProgress {
					t.Fatalf("a retried request counted: %+v", st)
				}
				r.clock.Advance(time.Minute)
			}
			done := r.await(b.ID, StatusCompleted)
			if want := map[bool]int{true: 2, false: 1}[tc.retried]; calls != want {
				t.Fatalf("sent %d times, want %d", calls, want)
			}
			if done.Completed != 1 || done.Failed != 0 || done.ErrorFileID != "" {
				t.Fatalf("batch = %+v", done)
			}
		})
	}
}

func TestClientErrorsAreFinalAndGoToTheErrorFile(t *testing.T) {
	r := newRig(t, Config{}, func(_ context.Context, _ Owner, _ string, body []byte) Reply {
		switch {
		case strings.Contains(string(body), `"q0"`):
			return Reply{Status: 400, RequestID: "r400", Body: []byte(`{"error":{"message":"max_tokens is too large","type":"invalid_request_error","code":"context_length_exceeded"}}`)}
		case strings.Contains(string(body), `"q1"`):
			return Reply{Status: 404, RequestID: "r404", Body: []byte(`{"error":{"message":"model m not found"}}`)}
		case strings.Contains(string(body), `"q2"`):
			return Reply{Status: 401, Body: []byte(`<html>nope</html>`)}
		case strings.Contains(string(body), `"q3"`):
			return Reply{Status: 200, TooLarge: true}
		}
		return okReply(`{"ok":true}`)
	})
	r.start()
	b := r.batchOf(6)
	done := r.await(b.ID, StatusCompleted)
	if done.Completed != 2 || done.Failed != 4 || done.OutputFileID == "" || done.ErrorFileID == "" {
		t.Fatalf("batch = %+v", done)
	}
	if r.send.distinct() != 6 {
		t.Fatalf("%d distinct requests sent", len(r.send.calls))
	}
	for body, n := range r.send.callCounts() {
		if n != 1 {
			t.Fatalf("%s sent %d times; a client error is not retried", body, n)
		}
	}
	errs := map[string]resultLine{}
	for _, l := range r.resultLines(done.ErrorFileID) {
		errs[l.CustomID] = l
	}
	c0, c1, c2, c3 := errs["c0"], errs["c1"], errs["c2"], errs["c3"]
	if c0.Response == nil || c0.Response.StatusCode != 400 || c0.Response.RequestID != "r400" || c0.Error == nil || c0.Error.Code != "context_length_exceeded" || c0.Error.Message != "max_tokens is too large" {
		t.Fatalf("c0 = %+v", c0)
	}
	if c1.Error == nil || c1.Error.Code != "http_404" || c1.Error.Message != "model m not found" {
		t.Fatalf("c1 = %+v %+v", c1, c1.Error)
	}
	if c2.Response == nil || string(c2.Response.Body) != "\"\\u003chtml\\u003enope\\u003c/html\\u003e\"" || c2.Error == nil || c2.Error.Code != "http_401" {
		t.Fatalf("c2 = %+v body=%s err=%+v", c2, c2.Response.Body, c2.Error)
	}
	if c3.Response == nil || c3.Error == nil || c3.Error.Code != "response_too_large" {
		t.Fatalf("c3 = %+v", c3)
	}
	if got := customIDs(r.resultLines(done.OutputFileID)); len(got) != 2 {
		t.Fatalf("output = %v", got)
	}
}

func TestRetryBackoffGrowsAndIsCapped(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	r := newRig(t, Config{RetryBase: 2 * time.Second, RetryCap: 60 * time.Second}, func(context.Context, Owner, string, []byte) Reply {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 9 {
			return Reply{Status: 503}
		}
		return okReply(`{}`)
	})
	r.start()
	b := r.batchOf(1)
	for i := 0; i < 9; i++ {
		want := i + 1
		eventually(t, "the next backoff", func() bool { return r.clock.waiting() >= 1 && retryDelays(r.clock) >= want })
		r.clock.Advance(time.Minute)
	}
	r.await(b.ID, StatusCompleted)
	var got []time.Duration
	for _, d := range r.clock.delaysSeen() {
		if d != 5*time.Second && d != 24*time.Hour && d != time.Hour {
			got = append(got, d)
		}
	}
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60, 60}
	if len(got) < len(want) {
		t.Fatalf("delays = %v", got)
	}
	for i, w := range want {
		if got[i] != w*time.Second {
			t.Fatalf("delays = %v, want 2s doubling to a 60s cap", got)
		}
	}
}

// retryDelays counts the backoff waits armed so far (not the checkpoint,
// sweep or expiry timers).
func hasDelay(c *fakeClock, want time.Duration) bool {
	for _, d := range c.delaysSeen() {
		if d == want {
			return true
		}
	}
	return false
}

func retryDelays(c *fakeClock) int {
	n := 0
	for _, d := range c.delaysSeen() {
		if d != 5*time.Second && d != time.Hour && d < 23*time.Hour {
			n++
		}
	}
	return n
}

func TestBackoffRespectsRetryAfterWithinTheCap(t *testing.T) {
	for _, tc := range []struct {
		attempt     int
		asked, want time.Duration
	}{
		{0, 0, 2 * time.Second},
		{3, 0, 16 * time.Second},
		{10, 0, 60 * time.Second},
		{0, 30 * time.Second, 30 * time.Second},
		{0, 10 * time.Minute, 60 * time.Second},
	} {
		if got := backoff(tc.attempt, 2*time.Second, 60*time.Second, tc.asked); got != tc.want {
			t.Errorf("backoff(%d, asked %v) = %v, want %v", tc.attempt, tc.asked, got, tc.want)
		}
	}
}

// Progress is one row update per checkpoint, however many requests finish.
func TestProgressWritesAreThrottled(t *testing.T) {
	r := newRig(t, Config{CheckpointEvery: 5 * time.Second}, echo)
	r.start()
	b := r.batchOf(300)
	done := r.await(b.ID, StatusCompleted)
	if done.Completed != 300 {
		t.Fatalf("batch = %+v", done)
	}
	if saves := r.repo.saveCount(); saves > 2 {
		t.Fatalf("%d progress writes for 300 requests finished inside one checkpoint period", saves)
	}
}

func TestCheckpointIsWrittenEveryPeriodWhileResultsArrive(t *testing.T) {
	gate := make(chan struct{}, 1000)
	r := newRig(t, Config{PerBatch: 4, CheckpointEvery: 5 * time.Second}, func(ctx context.Context, _ Owner, _ string, _ []byte) Reply {
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return okReply(`{}`)
	})
	r.start()
	b := r.batchOf(12)
	eventually(t, "the checkpoint timer", func() bool { return r.clock.waiting() > 0 })
	for i := 0; i < 4; i++ {
		gate <- struct{}{}
	}
	eventually(t, "four results", func() bool { return r.progressSeen(b.ID, 4) })
}

func (r *rig) progressSeen(id string, completed int) bool {
	for i := 0; i < 3; i++ {
		r.clock.Advance(5 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	b, _ := r.repo.GetBatch(id)
	return b.Completed >= completed
}

// A restart resumes from the partial file: requests with a result are not
// sent again, the torn tail after the checkpoint is cut, the rest run.
func TestRestartResumesFromTheCheckpoint(t *testing.T) {
	var mu sync.Mutex
	block := true
	r := newRig(t, Config{PerBatch: 1, CheckpointEvery: 5 * time.Second}, func(ctx context.Context, _ Owner, _ string, body []byte) Reply {
		mu.Lock()
		blocked := block && !strings.Contains(string(body), `"q0"`) && !strings.Contains(string(body), `"q1"`) && !strings.Contains(string(body), `"q2"`)
		mu.Unlock()
		if blocked {
			<-ctx.Done()
			return Reply{Err: ctx.Err()}
		}
		return okReply(`{"ok":1}`)
	})
	stop := r.start()
	b := r.batchOf(10)
	eventually(t, "three results", func() bool {
		r.clock.Advance(5 * time.Second)
		time.Sleep(5 * time.Millisecond)
		got, _ := r.repo.GetBatch(b.ID)
		return got.Completed == 3
	})
	stop() // the server goes down
	saved, _ := r.repo.GetBatch(b.ID)
	if saved.Status != StatusInProgress || saved.Completed != 3 || saved.CheckpointOffset == 0 {
		t.Fatalf("checkpoint = %+v", saved)
	}
	// The crash left a torn tail after the checkpoint.
	f, err := os.OpenFile(r.svc.partialPath(b.ID), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"id":"batch_req_torn","custom_id":"c9","resp`)
	f.Close()

	mu.Lock()
	block = false
	mu.Unlock()
	r.send.mu.Lock()
	r.send.calls = nil
	r.send.mu.Unlock()
	r.restart()
	r.start()
	done := r.await(b.ID, StatusCompleted)
	if done.Completed != 10 || done.Failed != 0 {
		t.Fatalf("batch = %+v", done)
	}
	for _, id := range []string{"q0", "q1", "q2"} {
		if n := r.send.callsFor(fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"%s"}]}`, id)); n != 0 {
			t.Fatalf("%s was sent again after the restart (%d times)", id, n)
		}
	}
	ids := customIDs(r.resultLines(done.OutputFileID))
	if len(ids) != 10 {
		t.Fatalf("output has %d lines: %v", len(ids), ids)
	}
	uniq := map[string]bool{}
	for _, id := range ids {
		uniq[id] = true
	}
	if len(uniq) != 10 {
		t.Fatalf("custom_ids repeat: %v", ids)
	}
}

// A result written after the last checkpoint survives a restart when the
// partial file still has it up to the checkpoint offset only; the rest of
// the lines re-run, and the output still has one line per custom_id.
func TestRestartWithoutACheckpointRerunsEverythingAndStaysUnique(t *testing.T) {
	r := newRig(t, Config{PerBatch: 2}, func(ctx context.Context, _ Owner, _ string, body []byte) Reply {
		if strings.Contains(string(body), `"q0"`) {
			return okReply(`{"ok":1}`)
		}
		<-ctx.Done()
		return Reply{Err: ctx.Err()}
	})
	stop := r.start()
	b := r.batchOf(6)
	eventually(t, "one result in the buffer", func() bool {
		r.svc.mu.Lock()
		defer r.svc.mu.Unlock()
		return len(r.svc.runs) == 1
	})
	time.Sleep(50 * time.Millisecond)
	stop() // shutdown takes a final checkpoint
	saved, _ := r.repo.GetBatch(b.ID)
	if saved.Completed != 1 {
		t.Fatalf("final checkpoint = %+v", saved)
	}
	// The row says nothing was checkpointed: a restart cannot trust the file.
	r.repo.mu.Lock()
	row := r.repo.batches[b.ID]
	row.CheckpointOffset, row.Completed = 0, 0
	r.repo.batches[b.ID] = row
	r.repo.mu.Unlock()
	r.send.handle = echo
	r.restart()
	r.start()
	done := r.await(b.ID, StatusCompleted)
	ids := customIDs(r.resultLines(done.OutputFileID))
	if done.Completed != 6 || len(ids) != 6 {
		t.Fatalf("batch = %+v, output = %v", done, ids)
	}
}

func TestCancelStopsDispatchLetsInFlightFinishAndKeepsTheResults(t *testing.T) {
	release := make(chan struct{})
	r := newRig(t, Config{PerBatch: 2}, func(ctx context.Context, _ Owner, _ string, _ []byte) Reply {
		select {
		case <-release:
		case <-ctx.Done():
			return Reply{Err: ctx.Err()}
		}
		return okReply(`{"ok":1}`)
	})
	r.start()
	b := r.batchOf(10)
	eventually(t, "two in flight", func() bool { return r.send.peakInFlight() == 2 })
	got, err := r.svc.Cancel(testOwner, b.ID)
	if err != nil || got.Status != StatusCancelling {
		t.Fatalf("cancel = %+v %v", got, err)
	}
	time.Sleep(30 * time.Millisecond)
	if st, _ := r.repo.GetBatch(b.ID); st.Status != StatusCancelling {
		t.Fatalf("status = %s while requests are in flight", st.Status)
	}
	close(release) // the two in flight finish
	done := r.await(b.ID, StatusCancelled)
	if done.CancelledAt == nil || done.CancellingAt == nil || done.Completed != 2 || done.Failed != 0 || done.OutputFileID == "" {
		t.Fatalf("cancelled batch = %+v", done)
	}
	if n := len(r.resultLines(done.OutputFileID)); n != 2 {
		t.Fatalf("output has %d lines, want the 2 that were in flight", n)
	}
	if r.send.sent() != 2 {
		t.Fatalf("%d requests were sent; none may start after the cancel", r.send.sent())
	}
	if _, err := r.svc.Cancel(testOwner, b.ID); err == nil {
		t.Fatal("a finished batch was cancelled again")
	} else if e, ok := err.(*Error); !ok || e.Status != 409 || e.Code != "batch_not_cancellable" {
		t.Fatalf("second cancel = %v", err)
	}
}

func TestCancelGraceAbortsRequestsThatDoNotFinish(t *testing.T) {
	r := newRig(t, Config{PerBatch: 2, CancelGrace: 2 * time.Minute}, func(ctx context.Context, _ Owner, _ string, _ []byte) Reply {
		<-ctx.Done()
		return Reply{Err: ctx.Err()}
	})
	r.start()
	b := r.batchOf(5)
	eventually(t, "two in flight", func() bool { return r.send.peakInFlight() == 2 })
	if _, err := r.svc.Cancel(testOwner, b.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the grace timer", func() bool { return hasDelay(r.clock, 2*time.Minute) })
	r.clock.Advance(3 * time.Minute)
	done := r.await(b.ID, StatusCancelled)
	if done.Completed != 0 || done.Failed != 0 || done.OutputFileID != "" || done.ErrorFileID != "" {
		t.Fatalf("batch = %+v", done)
	}
}

func TestCancelOfAWaitingBatchFinalizesWithoutRunning(t *testing.T) {
	gate := make(chan struct{})
	r := newRig(t, Config{MaxRunning: 1}, func(ctx context.Context, _ Owner, _ string, _ []byte) Reply {
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return okReply(`{}`)
	})
	r.start()
	first := r.batchOf(2)
	r.await(first.ID, StatusInProgress)
	second := r.batchOf(2)
	if _, err := r.svc.Cancel(testOwner, second.ID); err != nil {
		t.Fatal(err)
	}
	done := r.await(second.ID, StatusCancelled)
	if done.Completed != 0 || r.send.sent() > 2 {
		t.Fatalf("batch = %+v sent=%d", done, r.send.sent())
	}
	close(gate)
	r.await(first.ID, StatusCompleted)
}

func TestExpiryEndsTheBatchAndMovesTheRestToTheErrorFile(t *testing.T) {
	r := newRig(t, Config{PerBatch: 4}, func(_ context.Context, _ Owner, _ string, body []byte) Reply {
		if strings.Contains(string(body), `"q0"`) {
			return okReply(`{"ok":1}`)
		}
		return Reply{Status: 503, Body: []byte(`{"error":{"message":"asleep"}}`)}
	})
	r.start()
	b := r.batchOf(6)
	eventually(t, "retries waiting", func() bool { return retryDelays(r.clock) >= 4 })
	r.clock.Advance(24*time.Hour + time.Minute)
	done := r.await(b.ID, StatusExpired)
	if done.ExpiredAt == nil || done.Completed != 1 || done.Failed != 5 || done.OutputFileID == "" || done.ErrorFileID == "" {
		t.Fatalf("expired batch = %+v", done)
	}
	for _, l := range r.resultLines(done.ErrorFileID) {
		if l.Error == nil || l.Error.Code != "batch_expired" || l.Response != nil {
			t.Fatalf("error line = %+v", l)
		}
	}
	if n := len(r.resultLines(done.ErrorFileID)); n != 5 {
		t.Fatalf("error file has %d lines", n)
	}
}

// A batch that was in flight at its expiry ends at a restart after it, and
// sends nothing when it is resumed: its requests are recorded as expired without a single
// inference. The expiry used to arrive only through a zero-length timer in
// another goroutine, so the dispatch could win the race and send first; the
// loop gives that scheduling race many chances to show.
func TestExpiredBatchResumedAfterItsDeadlineSendsNothing(t *testing.T) {
	for round := 0; round < 40; round++ {
		var resumed atomic.Bool
		var sentAfterResume atomic.Int32
		r := newRig(t, Config{PerBatch: 2}, func(ctx context.Context, _ Owner, _ string, _ []byte) Reply {
			if resumed.Load() {
				sentAfterResume.Add(1)
				return okReply(`{"ok":1}`)
			}
			<-ctx.Done()
			return Reply{Err: ctx.Err()}
		})
		stop := r.start()
		b := r.batchOf(3)
		eventually(t, "in flight", func() bool { return r.send.peakInFlight() == 2 })
		stop()
		r.clock.Advance(25 * time.Hour)
		resumed.Store(true)
		r.restart()
		r.start()
		done := r.await(b.ID, StatusExpired)
		if n := sentAfterResume.Load(); n != 0 {
			t.Fatalf("round %d: the expired batch sent %d inference requests after it was resumed", round, n)
		}
		if done.Failed != 3 || done.Completed != 0 {
			t.Fatalf("round %d: batch = %+v", round, done)
		}
		for _, l := range r.resultLines(done.ErrorFileID) {
			if l.Error == nil || l.Error.Code != "batch_expired" {
				t.Fatalf("round %d: error line = %+v", round, l)
			}
		}
	}
}

// A batch accepted but not yet started when its deadline passes (it waited
// for an executor, or the server was down) goes from validating straight to
// expired without a request.
func TestBatchStartedAfterItsDeadlineSendsNothing(t *testing.T) {
	for round := 0; round < 40; round++ {
		r := newRig(t, Config{PerBatch: 2}, func(context.Context, Owner, string, []byte) Reply {
			return okReply(`{"ok":1}`)
		})
		b := r.batchOf(3)
		r.clock.Advance(25 * time.Hour)
		r.start()
		done := r.await(b.ID, StatusExpired)
		r.send.mu.Lock()
		sent := r.send.total
		r.send.mu.Unlock()
		if sent != 0 || done.Failed != 3 || done.Completed != 0 {
			t.Fatalf("round %d: sent %d, batch = %+v", round, sent, done)
		}
	}
}

func TestMissingInputFileFailsTheBatch(t *testing.T) {
	r := newRig(t, Config{}, echo)
	r.start()
	f, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(inputLines(2)))
	if err != nil {
		t.Fatal(err)
	}
	path, _ := r.svc.filePath(f.ID)
	os.Remove(path)
	b, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	done := r.await(b.ID, StatusFailed)
	if done.ErrorCode != "input_file_unreadable" || done.FailedAt == nil {
		t.Fatalf("batch = %+v", done)
	}
	w := toWireBatch(done)
	if w.Errors == nil || len(w.Errors.Data) != 1 || w.Errors.Data[0].Code != "input_file_unreadable" {
		t.Fatalf("wire errors = %+v", w.Errors)
	}
}

// The output of a partial file with a custom_id twice keeps the last line,
// and routes it by that line's outcome.
func TestAssembleKeepsTheLastLinePerCustomID(t *testing.T) {
	dir := t.TempDir()
	partialPath := filepath.Join(dir, "p")
	lines := []string{
		string(encodeFailure("b", "a", "boom", "first try")),
		string(encodeResult("b", "b", okReply(`{"v":1}`), nil)),
		string(encodeResult("b", "a", okReply(`{"v":2}`), nil)),
		string(encodeResult("b", "b", Reply{Status: 400, Body: []byte(`{}`)}, &resultError{Code: "bad", Message: "bad"})),
		string(encodeResult("b", "c", okReply(`{"v":3}`), nil)),
	}
	if err := os.WriteFile(partialPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := assemble(partialPath, filepath.Join(dir, "out"), filepath.Join(dir, "err"))
	if err != nil || res.completed != 2 || res.failed != 1 {
		t.Fatalf("assembled = %+v err=%v", res, err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "out"))
	errs, _ := os.ReadFile(filepath.Join(dir, "err"))
	if strings.Count(string(out), "\n") != 2 || !strings.Contains(string(out), `"custom_id":"a"`) || !strings.Contains(string(out), `{"v":2}`) || strings.Contains(string(out), `"custom_id":"b"`) {
		t.Fatalf("output = %s", out)
	}
	if strings.Count(string(errs), "\n") != 1 || !strings.Contains(string(errs), `"custom_id":"b"`) || int64(len(out)) != res.outBytes {
		t.Fatalf("errors = %s", errs)
	}
}

func TestOpenPartialDropsATornTailAndAShortFileStartsOver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p")
	good := string(encodeResult("b", "a", okReply(`{}`), nil)) + "\n" + string(encodeFailure("b", "z", "x", "y")) + "\n"
	os.WriteFile(path, []byte(good+`{"torn`), 0o600)
	p, err := openPartial(path, int64(len(good)))
	if err != nil {
		t.Fatal(err)
	}
	if c, f := p.counts(); c != 1 || f != 1 || p.offset != int64(len(good)) || !p.has("a") || !p.has("z") || p.has("torn") {
		t.Fatalf("resume state: %d/%d offset %d", c, f, p.offset)
	}
	p.close()
	if st, _ := os.Stat(path); st.Size() != int64(len(good)) {
		t.Fatalf("size = %d, the tail was kept", st.Size())
	}
	os.WriteFile(path, []byte("short"), 0o600)
	p, err = openPartial(path, 999)
	if err != nil || p.offset != 0 || p.has("a") {
		t.Fatalf("short file: %+v err=%v", p, err)
	}
	p.close()
	os.WriteFile(path, []byte("not json\n"), 0o600)
	if _, err := openPartial(path, 9); err == nil {
		t.Fatal("a corrupt partial file was accepted")
	}
}

// A batch whose results are being assembled cannot be cancelled any more.
func TestCancelOfAFinalizingBatchIsRefused(t *testing.T) {
	r := newRig(t, Config{}, echo)
	f, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(inputLines(1)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	r.repo.Start(b.ID, r.clock.Now())
	r.repo.BeginFinalize(b.ID, r.clock.Now())
	_, err = r.svc.Cancel(testOwner, b.ID)
	if e, ok := err.(*Error); !ok || e.Status != 409 || e.Code != "batch_not_cancellable" || !strings.Contains(e.Message, "finalizing") {
		t.Fatalf("cancel = %v", err)
	}
}
