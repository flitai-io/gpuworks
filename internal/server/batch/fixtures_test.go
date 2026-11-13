package batch

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a clock the test moves: After channels fire when Advance
// carries the time to their deadline.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	delays []time.Duration
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.delays = append(c.delays, d)
	if d <= 0 {
		t.ch <- c.now
		return t.ch
	}
	c.timers = append(c.timers, t)
	return t.ch
}

// Advance moves the time on and fires the timers it reaches.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var keep []*fakeTimer
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
		} else {
			keep = append(keep, t)
		}
	}
	c.timers = keep
	c.mu.Unlock()
}

// waiting is how many timers are armed.
func (c *fakeClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// delaysSeen are the durations of every After call so far.
func (c *fakeClock) delaysSeen() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.delays)
}

// memRepo keeps files and batches in memory and counts its writes.
type memRepo struct {
	mu       sync.Mutex
	files    map[string]File
	batches  map[string]Batch
	order    []string
	progress int
	saves    map[string][]Progress
	created  int
	// createErr fails CreateBatch, as a database that refuses the row.
	createErr error
}

func newMemRepo() *memRepo {
	return &memRepo{files: map[string]File{}, batches: map[string]Batch{}, saves: map[string][]Progress{}}
}

func (m *memRepo) CreateFile(f File) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[f.ID] = f
	return nil
}

func (m *memRepo) GetFile(id string) (File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[id]
	if !ok {
		return File{}, ErrNotFound
	}
	return f, nil
}

func (m *memRepo) ListFiles(owner Owner, purpose, after string, limit int) ([]File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []File
	for _, f := range m.files {
		if owner.Sees(f.UserID) && (purpose == "" || f.Purpose == purpose) {
			all = append(all, f)
		}
	}
	slices.SortFunc(all, func(a, b File) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	if after != "" {
		if i := slices.IndexFunc(all, func(f File) bool { return f.ID == after }); i >= 0 {
			all = all[i+1:]
		}
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (m *memRepo) DeleteFile(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[id]; !ok {
		return ErrNotFound
	}
	for _, b := range m.batches {
		if b.InputFileID == id && !Terminal(b.Status) {
			return ErrFileInUse
		}
	}
	delete(m.files, id)
	return nil
}

func (m *memRepo) CreateBatch(b Batch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return m.createErr
	}
	m.batches[b.ID] = b
	m.order = append(m.order, b.ID)
	m.created++
	return nil
}

func (m *memRepo) GetBatch(id string) (Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.batches[id]
	if !ok {
		return Batch{}, ErrNotFound
	}
	return b, nil
}

func (m *memRepo) sortedBatches(owner Owner, statuses []string) []Batch {
	var all []Batch
	for _, b := range m.batches {
		if owner.Sees(b.UserID) && (len(statuses) == 0 || slices.Contains(statuses, b.Status)) {
			all = append(all, b)
		}
	}
	slices.SortFunc(all, func(a, b Batch) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	return all
}

func (m *memRepo) ListBatchesAfter(owner Owner, after string, limit int) ([]Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.sortedBatches(owner, nil)
	if after != "" {
		if i := slices.IndexFunc(all, func(b Batch) bool { return b.ID == after }); i >= 0 {
			all = all[i+1:]
		}
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (m *memRepo) ListBatchesPage(owner Owner, statuses []string, offset, limit int) ([]Batch, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.sortedBatches(owner, statuses)
	total := len(all)
	if offset > len(all) {
		offset = len(all)
	}
	all = all[offset:]
	if len(all) > limit {
		all = all[:limit]
	}
	return all, total, nil
}

func (m *memRepo) CountByStatus(owner Owner, statuses []string) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, b := range m.sortedBatches(owner, statuses) {
		out[b.Status]++
	}
	return out, nil
}

func (m *memRepo) Unfinished() ([]Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Batch
	for _, id := range m.order {
		if b := m.batches[id]; !Terminal(b.Status) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *memRepo) update(id string, f func(*Batch) bool) (Batch, bool) {
	b, ok := m.batches[id]
	if !ok || !f(&b) {
		return b, false
	}
	m.batches[id] = b
	return b, true
}

func (m *memRepo) Start(id string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.update(id, func(b *Batch) bool {
		if b.Status != StatusValidating {
			return false
		}
		b.Status, b.InProgressAt = StatusInProgress, &at
		return true
	})
	return ok, nil
}

func (m *memRepo) SaveProgress(id string, p Progress) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.progress++
	m.saves[id] = append(m.saves[id], p)
	m.update(id, func(b *Batch) bool {
		if Terminal(b.Status) {
			return false
		}
		b.Completed, b.Failed, b.CheckpointOffset = p.Completed, p.Failed, p.Offset
		return true
	})
	return nil
}

func (m *memRepo) RequestCancel(id string, at time.Time) (Batch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.batches[id]
	if !ok {
		return Batch{}, ErrNotFound
	}
	if Terminal(b.Status) {
		return b, ErrTerminal
	}
	if Cancellable(b.Status) {
		b.Status, b.CancellingAt = StatusCancelling, &at
		m.batches[id] = b
	}
	return b, nil
}

func (m *memRepo) BeginFinalize(id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.update(id, func(b *Batch) bool {
		if b.Status != StatusInProgress {
			return false
		}
		b.Status, b.FinalizingAt = StatusFinalizing, &at
		return true
	})
	return nil
}

func (m *memRepo) Finish(id string, f Finish) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.batches[id]
	if !ok {
		return ErrNotFound
	}
	if Terminal(b.Status) {
		return ErrTerminal
	}
	at := f.At
	b.Status, b.Completed, b.Failed, b.ErrorCode, b.ErrorMessage = f.Status, f.Completed, f.Failed, f.ErrorCode, f.ErrorMessage
	switch f.Status {
	case StatusCompleted:
		b.CompletedAt = &at
	case StatusFailed:
		b.FailedAt = &at
	case StatusExpired:
		b.ExpiredAt = &at
	case StatusCancelled:
		b.CancelledAt = &at
	}
	for _, file := range []*File{f.Output, f.Error} {
		if file != nil {
			m.files[file.ID] = *file
		}
	}
	if f.Output != nil {
		b.OutputFileID = f.Output.ID
	}
	if f.Error != nil {
		b.ErrorFileID = f.Error.ID
	}
	m.batches[id] = b
	return nil
}

func (m *memRepo) DeleteOutputsBefore(cutoff time.Time) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, f := range m.files {
		if f.Purpose == PurposeOutput && f.CreatedAt.Before(cutoff) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		delete(m.files, id)
	}
	return ids, nil
}

func (m *memRepo) saveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.progress
}

// fakeSender answers requests from a function and measures concurrency.
type fakeSender struct {
	mu      sync.Mutex
	handle  func(ctx context.Context, owner Owner, path string, body []byte) Reply
	current int
	peak    int
	calls   map[string]int
	total   int
}

func (f *fakeSender) Send(ctx context.Context, owner Owner, path string, body []byte) Reply {
	f.mu.Lock()
	f.current++
	f.peak = max(f.peak, f.current)
	f.total++
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[string(body)]++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.current--
		f.mu.Unlock()
	}()
	return f.handle(ctx, owner, path, body)
}

func (f *fakeSender) peakInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func (f *fakeSender) callsFor(body string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[body]
}

func okReply(body string) Reply {
	return Reply{Status: 200, RequestID: "req-1", Body: []byte(body)}
}

// inputLines is a JSONL file of n chat requests with custom ids c0..c(n-1).
func inputLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"custom_id":"c%d","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[{"role":"user","content":"q%d"}]}}`+"\n", i, i)
	}
	return b.String()
}

// rig is a service over a memory repo, a fake clock and a fake sender, with
// quick backoffs and no jitter.
type rig struct {
	t     *testing.T
	repo  *memRepo
	clock *fakeClock
	send  *fakeSender
	svc   *Service
	cfg   Config
}

func newRig(t *testing.T, cfg Config, handle func(ctx context.Context, owner Owner, path string, body []byte) Reply) *rig {
	t.Helper()
	r := &rig{t: t, repo: newMemRepo(), clock: newFakeClock(), send: &fakeSender{handle: handle}}
	cfg.Dir = t.TempDir()
	cfg.Clock = r.clock
	cfg.Jitter = func(d time.Duration) time.Duration { return d }
	r.cfg = cfg
	r.svc = New(r.repo, r.send, cfg)
	return r
}

// restart builds a new service over the same repo, directory and clock, as a
// server restart does.
func (r *rig) restart() *Service {
	r.svc = New(r.repo, r.send, r.cfg)
	return r.svc
}

// start runs the service until the test ends or stop is called.
func (r *rig) start() (stop func()) {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.svc.Start(ctx); err != nil {
		r.t.Fatal(err)
	}
	svc := r.svc
	done := false
	stop = func() {
		if !done {
			done = true
			cancel()
			svc.Wait()
		}
	}
	r.t.Cleanup(stop)
	return stop
}

var testOwner = Owner{UserID: 7, APIKeyID: 70}

// batchOf uploads a file of n requests and creates a batch over it.
func (r *rig) batchOf(n int) Batch {
	r.t.Helper()
	return r.batchFrom(inputLines(n))
}

func (r *rig) batchFrom(content string) Batch {
	r.t.Helper()
	f, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(content))
	if err != nil {
		r.t.Fatalf("upload: %v", err)
	}
	b, err := r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "24h"})
	if err != nil {
		r.t.Fatalf("create: %v", err)
	}
	return b
}

// await polls until the batch has the status.
func (r *rig) await(id, status string) Batch {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := r.repo.GetBatch(id)
		if b.Status == status {
			return b
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("batch %s is %q, want %q", id, b.Status, status)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// eventually polls cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *fakeSender) sent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

func (f *fakeSender) distinct() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSender) callCounts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for k, v := range f.calls {
		out[k] = v
	}
	return out
}

func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}
