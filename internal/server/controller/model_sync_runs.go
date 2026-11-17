package controller

import (
	"context"
	"sync"
	"sync/atomic"
)

type modelSyncRun struct {
	transferID string
	cancel     context.CancelFunc
	// automatic marks a copy whose source the controller chose itself. Its
	// failure is no one's recorded choice, so the model falls back to the
	// public download rather than waiting on an operator's retry.
	automatic bool
	// canceled is set by an operator's cancel before it cancels the copy: the
	// copy's own error is then that cancel, not a failure to fall back from,
	// and cancelModelSync records the outcome.
	canceled atomic.Bool
}

// modelSyncRuns are the peer model syncs this controller runs, by model file
// key. byKey is read and written only under mu; some steps read it, write the
// store and update it as one, so they hold mu across the store write. Its
// zero value is ready to use.
type modelSyncRuns struct {
	mu    sync.Mutex
	byKey map[string]*modelSyncRun
}

// put registers run under key. The caller holds mu.
func (r *modelSyncRuns) put(key string, run *modelSyncRun) {
	if r.byKey == nil {
		r.byKey = map[string]*modelSyncRun{}
	}
	r.byKey[key] = run
}

// cancelAll cancels every run; their own cleanup removes them.
func (r *modelSyncRuns) cancelAll() {
	r.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(r.byKey))
	for _, run := range r.byKey {
		cancels = append(cancels, run.cancel)
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
