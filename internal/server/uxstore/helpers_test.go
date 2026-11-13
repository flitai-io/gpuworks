package uxstore

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// clock is a settable store clock, safe for the writer goroutine to read.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// day0 is a fixed UTC noon the tests count days from.
var day0 = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// testOpts never flushes on its own: tests drive flushes with sync.
func testOpts(c *clock) Options {
	return Options{FlushEvery: time.Hour, Now: c.Now}
}

func openAt(t *testing.T, path string, opts Options) *Store {
	t.Helper()
	s, err := Open(path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func openTest(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ux.db")
	return openAt(t, path, opts), path
}

func mustSync(t *testing.T, s *Store, retain bool) {
	t.Helper()
	if err := s.sync(retain); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// okReq is a normal, fast, successful streamed request with usage.
func okReq(id string, dep uint, ts int64) uxrecord.Request {
	return uxrecord.Request{
		ID: id, ReceivedMS: ts, DeploymentID: dep, InstanceUUID: "inst-1", Path: "/v1/chat/completions",
		Stream: true, DispatchMS: 1, FirstByteMS: 50, FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: 80,
		LastChunkMS: 900, EndMS: 901, Events: 20, MaxGapMS: 60, Status: 200, Outcome: uxrecord.OutcomeOK,
		FinishReason: "stop", Usage: true, PromptTokens: 1000, CompletionTokens: 100, CachedTokens: 600,
		EngineQueueMS: uxrecord.Unknown, EnginePrefillMS: uxrecord.Unknown, EngineDecodeMS: uxrecord.Unknown,
	}
}

// allRequests pages through every request of a query.
func allRequests(t *testing.T, s *Store, q RequestQuery) []uxrecord.Request {
	t.Helper()
	var out []uxrecord.Request
	for range 10000 {
		page, next, err := s.Requests(context.Background(), q)
		if err != nil {
			t.Fatalf("Requests: %v", err)
		}
		out = append(out, page...)
		if next == "" {
			return out
		}
		q.Cursor = next
	}
	t.Fatal("pagination did not end")
	return nil
}

func tableNames(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	rows, err := s.reader.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	return out
}

func ms(t time.Time) int64 { return t.UnixMilli() }
