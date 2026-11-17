package batch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Config tunes the executor. A zero field takes its default.
type Config struct {
	// Dir holds the files' bytes: uploads, outputs and partial results.
	Dir string
	// PerBatch is how many requests of one batch run at once (8).
	PerBatch int
	// GlobalCap bounds the requests of all batches in flight together (64).
	GlobalCap int
	// MaxRunning bounds the batches executed at once (32); further batches
	// stay validating until one ends.
	MaxRunning int
	// CheckpointEvery spaces the writes of a batch's progress (5s).
	CheckpointEvery time.Duration
	// RetryBase and RetryCap bound the backoff between attempts (2s, 60s).
	RetryBase, RetryCap time.Duration
	// CancelGrace is how long a cancelled batch lets requests in flight
	// finish before it aborts them (2m).
	CancelGrace time.Duration
	// AttemptTimeout bounds one attempt (1h): longer than the longest wake
	// hold plus a generation.
	AttemptTimeout time.Duration
	// OutputRetention is how long output and error files are kept (30 days).
	OutputRetention time.Duration
	// SweepEvery spaces the removals of expired output files (1h).
	SweepEvery time.Duration
	Clock      Clock
	// Models checks the model a batch is created with; nil accepts any name.
	Models Models
	// PriorityClasses checks the request priority class a batch is created
	// with; nil accepts any name.
	PriorityClasses PriorityClasses
	// Jitter spreads a backoff; the default is ±20% at random.
	Jitter func(time.Duration) time.Duration
	Log    *slog.Logger
}

func (c Config) withDefaults() Config {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defD := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.PerBatch, 8)
	def(&c.GlobalCap, 64)
	def(&c.MaxRunning, 32)
	defD(&c.CheckpointEvery, 5*time.Second)
	defD(&c.RetryBase, 2*time.Second)
	defD(&c.RetryCap, 60*time.Second)
	defD(&c.CancelGrace, 2*time.Minute)
	defD(&c.AttemptTimeout, time.Hour)
	defD(&c.OutputRetention, 30*24*time.Hour)
	defD(&c.SweepEvery, time.Hour)
	if c.Clock == nil {
		c.Clock = realClock{}
	}
	if c.Jitter == nil {
		c.Jitter = func(d time.Duration) time.Duration {
			return time.Duration(float64(d) * (0.8 + 0.4*mrand.Float64()))
		}
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	return c
}

// Service runs batches and serves their files.
type Service struct {
	repo Repo
	send Sender
	cfg  Config
	// global bounds the requests in flight across batches, slots the batches
	// executing.
	global chan struct{}
	slots  chan struct{}

	mu   sync.Mutex
	ctx  context.Context
	runs map[string]*handle
	wg   sync.WaitGroup

	// stopped caches whether a model is served only by stopped deployments
	// (modelStopped).
	stoppedMu sync.Mutex
	stopped   map[string]stoppedLookup
}

// handle is a launched batch: closing cancel tells its executor to stop;
// waits is why its requests are being retried.
type handle struct {
	cancel chan struct{}
	once   sync.Once
	waits  waitBoard
}

func (h *handle) stop() { h.once.Do(func() { close(h.cancel) }) }

// New builds a Service. Start begins executing.
func New(repo Repo, send Sender, cfg Config) *Service {
	cfg = cfg.withDefaults()
	return &Service{
		repo: repo, send: send, cfg: cfg,
		global: make(chan struct{}, cfg.GlobalCap),
		slots:  make(chan struct{}, cfg.MaxRunning),
		runs:   map[string]*handle{},
	}
}

// Start resumes every batch that had not finished and starts the removal of
// expired output files. It returns at once; the work stops with ctx, and Wait
// returns when it has.
func (s *Service) Start(ctx context.Context) error {
	if err := os.MkdirAll(s.cfg.Dir, 0o750); err != nil {
		return fmt.Errorf("batch file area: %w", err)
	}
	removeStaleUploads(s.cfg.Dir, s.cfg.Clock.Now().Add(-24*time.Hour))
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	pending, err := s.repo.Unfinished()
	if err != nil {
		return err
	}
	for _, b := range pending {
		s.launch(b)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.sweepLoop(ctx)
	}()
	return nil
}

// Wait blocks until every executor and the sweep have returned.
func (s *Service) Wait() { s.wg.Wait() }

// launch starts the executor of a batch unless one runs already or the
// service has not started.
func (s *Service) launch(b Batch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil {
		return
	}
	if _, running := s.runs[b.ID]; running {
		return
	}
	h := &handle{cancel: make(chan struct{})}
	s.runs[b.ID] = h
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.runs, b.ID)
			s.mu.Unlock()
		}()
		s.execute(s.ctx, b, h)
	}()
}

// signalCancel tells the executor of a cancelled batch, launching one when
// none runs (the batch finalizes as cancelled).
func (s *Service) signalCancel(b Batch) {
	s.mu.Lock()
	h := s.runs[b.ID]
	s.mu.Unlock()
	if h != nil {
		h.stop()
		return
	}
	s.launch(b)
}

// sweepLoop removes output and error files past their retention.
func (s *Service) sweepLoop(ctx context.Context) {
	for {
		s.sweep()
		select {
		case <-ctx.Done():
			return
		case <-s.cfg.Clock.After(s.cfg.SweepEvery):
		}
	}
}

func (s *Service) sweep() {
	ids, err := s.repo.DeleteOutputsBefore(s.cfg.Clock.Now().Add(-s.cfg.OutputRetention))
	if err != nil {
		s.cfg.Log.Warn("batch: remove expired output files", "err", err)
		return
	}
	for _, id := range ids {
		if p, err := s.filePath(id); err == nil {
			_ = os.Remove(p)
		}
	}
}

// removeStaleUploads deletes uploads an earlier run left half-written.
func removeStaleUploads(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "upload-") && !strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(before) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

var idPattern = regexp.MustCompile(`^(file-|batch_)[0-9a-f]{24}$`)

// filePath is where the bytes of a file are; an id that is not one of ours
// never reaches the disk.
func (s *Service) filePath(id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", ErrNotFound
	}
	return filepath.Join(s.cfg.Dir, id), nil
}

func (s *Service) partialPath(batchID string) string {
	return filepath.Join(s.cfg.Dir, batchID+".results")
}

func randomID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

// outputFileID is the id of a batch's output (kind "output") or error file:
// derived from the batch, so finishing a batch twice writes the same files.
func outputFileID(batchID, kind string) string {
	sum := sha256.Sum256([]byte(batchID + "/" + kind))
	return "file-" + hex.EncodeToString(sum[:12])
}
