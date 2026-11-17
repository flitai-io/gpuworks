package uxstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func TestRoundTripEveryField(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c))
	full := uxrecord.Request{
		ID: "req-full", ReceivedMS: ms(day0) - 5000, DeploymentID: 7, InstanceUUID: "0f3c-uuid",
		APIKeyID: 11, UserID: 13, Source: uxrecord.SourcePlayground, Path: "/v1/completions",
		Stream: true, BodyBytes: 123456, Images: 3, DispatchMS: 2, FirstByteMS: 40, FirstThinkMS: 55,
		FirstAnswerMS: 1500, LastChunkMS: 4000, EndMS: 4001, Events: 321, ThinkEvents: 77, MaxGapMS: 420,
		Status: 200, Outcome: uxrecord.OutcomeTruncated, FinishReason: "length", Usage: true,
		PromptTokens: 9000, CompletionTokens: 2048, CachedTokens: 8192, ReasoningTokens: 600,
		EngineQueueMS: 12, EnginePrefillMS: 340, EngineDecodeMS: 2500, PriorityClass: "high",
	}
	unknown := uxrecord.Request{
		ID: "req-unknown", ReceivedMS: ms(day0) - 4000, DeploymentID: 7, Source: uxrecord.SourceAPI,
		Path: "/v1/chat/completions", DispatchMS: uxrecord.Unknown, FirstByteMS: uxrecord.Unknown,
		FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: uxrecord.Unknown, LastChunkMS: uxrecord.Unknown,
		EndMS: 30, Status: 502, Outcome: uxrecord.OutcomeUpstreamError, CachedTokens: uxrecord.Unknown,
		EngineQueueMS: uxrecord.Unknown, EnginePrefillMS: uxrecord.Unknown, EngineDecodeMS: uxrecord.Unknown,
	}
	s.Observe(full)
	s.Observe(unknown)
	mustSync(t, s, false)

	got, next, err := s.Requests(context.Background(), RequestQuery{DeploymentID: 7})
	if err != nil || next != "" {
		t.Fatalf("Requests: err=%v next=%q", err, next)
	}
	full.Weight, unknown.Weight = 1, 1
	want := []uxrecord.Request{unknown, full} // newest first
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}

	// The columns are typed integers, Unknown included.
	var typ string
	var cached int64
	if err := s.reader.QueryRow(`SELECT typeof(cached_tokens), cached_tokens FROM req_20261012 WHERE id='req-unknown'`).
		Scan(&typ, &cached); err != nil {
		t.Fatal(err)
	}
	if typ != "integer" || cached != -1 {
		t.Fatalf("cached_tokens stored as %s %d, want integer -1", typ, cached)
	}

	// The priority class narrows the log.
	got, _, err = s.Requests(context.Background(), RequestQuery{DeploymentID: 7, PriorityClass: "high"})
	if err != nil || len(got) != 1 || got[0].ID != "req-full" {
		t.Fatalf("by class = %+v err=%v", got, err)
	}
}

func TestPartitionPerUTCDay(t *testing.T) {
	c := newClock(day0)
	s, _ := openTest(t, testOpts(c))
	lastMS := time.Date(2026, 10, 11, 23, 59, 59, 999e6, time.UTC)
	s.Observe(okReq("a", 1, ms(lastMS)))
	s.Observe(okReq("b", 1, ms(lastMS)+1))
	mustSync(t, s, false)

	tables := tableNames(t, s)
	for _, name := range []string{"req_20261011", "grid_20261011", "cnt_20261011", "req_20261012", "grid_20261012", "cnt_20261012"} {
		if !tables[name] {
			t.Errorf("missing table %s (have %v)", name, tables)
		}
	}
	for table, wantID := range map[string]string{"req_20261011": "a", "req_20261012": "b"} {
		var id string
		if err := s.reader.QueryRow(`SELECT id FROM ` + table).Scan(&id); err != nil || id != wantID {
			t.Errorf("%s holds %q (%v), want %q", table, id, err, wantID)
		}
	}
	// The 23:55 bucket of the first day stays in the first day's grid.
	var bucket int64
	if err := s.reader.QueryRow(`SELECT bucket_ms FROM grid_20261011`).Scan(&bucket); err != nil {
		t.Fatal(err)
	}
	if want := ms(time.Date(2026, 10, 11, 23, 55, 0, 0, time.UTC)); bucket != want {
		t.Fatalf("bucket = %d, want %d", bucket, want)
	}
	if st := s.Stats(); st.Partitions != 2 || st.RowsWritten != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestCloseFlushesPendingAndReopenIsIdempotent(t *testing.T) {
	c := newClock(day0)
	path := filepath.Join(t.TempDir(), "ux.db")
	s, err := Open(path, testOpts(c))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 25 {
		s.Observe(okReq(fmt.Sprintf("r%d", i), 1, ms(day0)+int64(i)))
	}
	if err := s.Close(); err != nil { // no sync: Close must flush
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	s.Observe(okReq("late", 1, ms(day0))) // after Close: dropped, not a panic
	if s.Dropped() != 1 {
		t.Fatalf("Dropped after close = %d, want 1", s.Dropped())
	}

	s2 := openAt(t, path, testOpts(c))
	if got := s2.Stats().Partitions; got != 1 {
		t.Fatalf("partitions after reopen = %d, want 1", got)
	}
	s2.Observe(okReq("r25", 1, ms(day0)+25)) // same day: memoized, no DDL needed
	mustSync(t, s2, false)
	all := allRequests(t, s2, RequestQuery{DeploymentID: 1})
	if len(all) != 26 || all[0].ID != "r25" || all[25].ID != "r0" {
		t.Fatalf("after reopen got %d rows, first %q last %q", len(all), all[0].ID, all[len(all)-1].ID)
	}
	cov, err := s2.Coverage(context.Background(), 1, 0, 0)
	if err != nil || cov.Total != 26 {
		t.Fatalf("coverage after reopen = %+v, %v", cov, err)
	}
	var v int
	if err := s2.reader.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("user_version = %d (%v), want %d", v, err, schemaVersion)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ux.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if s, err := Open(path, Options{}); err == nil || !strings.Contains(err.Error(), "newer") {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open of a newer schema: err = %v", err)
	}
}

func TestQueueFullDropsAndCounts(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.Queue = 2
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	opts.beforeRecord = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	s, path := openTest(t, opts)

	s.Observe(okReq("held", 1, ms(day0))) // the writer takes it and blocks
	<-entered
	for i := range 9 { // two fit in the queue, seven are dropped
		s.Observe(okReq(fmt.Sprintf("q%d", i), 1, ms(day0)+int64(i)+1))
	}
	if got := s.Dropped(); got != 7 {
		t.Fatalf("Dropped = %d, want 7", got)
	}
	close(release)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Observed != 10 || st.Dropped != 7 || st.RowsWritten != 3 {
		t.Fatalf("stats = %+v", st)
	}
	s2 := openAt(t, path, testOpts(c))
	if got := len(allRequests(t, s2, RequestQuery{DeploymentID: 1})); got != 3 {
		t.Fatalf("stored %d rows, want 3", got)
	}
}

// A failed transaction loses its batch, is counted, and does not leave the
// day marked ready: the next flush creates the tables again.
func TestFlushFailureIsCountedAndPartitionRetried(t *testing.T) {
	c := newClock(day0)
	s, path := openTest(t, testOpts(c))
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// A view in the table's place: CREATE TABLE IF NOT EXISTS is a no-op and
	// the insert fails.
	if _, err := other.Exec(`CREATE VIEW req_20261012 AS SELECT 1 AS x`); err != nil {
		t.Fatal(err)
	}
	s.Observe(okReq("lost", 1, ms(day0)))
	if err := s.sync(false); err == nil {
		t.Fatal("flush into a view succeeded")
	}
	if st := s.Stats(); st.FlushErrors != 1 || st.RowsWritten != 0 || st.Partitions != 0 {
		t.Fatalf("stats after failure = %+v", st)
	}
	if _, err := other.Exec(`DROP VIEW req_20261012`); err != nil {
		t.Fatal(err)
	}
	s.Observe(okReq("kept", 1, ms(day0)+1))
	mustSync(t, s, false)
	got := allRequests(t, s, RequestQuery{DeploymentID: 1})
	if len(got) != 1 || got[0].ID != "kept" || s.Stats().Partitions != 1 {
		t.Fatalf("after retry: rows %+v stats %+v", got, s.Stats())
	}
}

func TestConcurrentObserve(t *testing.T) {
	c := newClock(day0)
	opts := testOpts(c)
	opts.FlushEvery = 5 * time.Millisecond
	opts.RowsPerSecond = 1 << 20 // no sampling: every row is checked below
	s, path := openTest(t, opts)
	const workers, each = 16, 300
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				s.Observe(okReq(fmt.Sprintf("w%d-%d", w, i), uint(1+w%2), ms(day0)+int64(i)))
			}
		}()
	}
	readCtx, stop := context.WithCancel(context.Background())
	readDone := make(chan struct{})
	go func() { // readers run against the writer
		defer close(readDone)
		for readCtx.Err() == nil {
			if _, _, err := s.Requests(readCtx, RequestQuery{DeploymentID: 1, Limit: 20}); err != nil && readCtx.Err() == nil {
				t.Errorf("concurrent Requests: %v", err)
				return
			}
			if _, err := s.Grid(readCtx, GridQuery{Kind: uxrecord.GridIO}); err != nil && readCtx.Err() == nil {
				t.Errorf("concurrent Grid: %v", err)
				return
			}
		}
	}()
	wg.Wait()
	stop()
	<-readDone
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Dropped() != 0 {
		t.Fatalf("dropped %d with a large queue", s.Dropped())
	}
	s2 := openAt(t, path, testOpts(c))
	n := len(allRequests(t, s2, RequestQuery{DeploymentID: 1, Limit: MaxRequestLimit})) +
		len(allRequests(t, s2, RequestQuery{DeploymentID: 2, Limit: MaxRequestLimit}))
	cov, err := s2.Coverage(context.Background(), 0, 0, 0)
	if err != nil || n != workers*each || cov.Total != workers*each {
		t.Fatalf("rows %d coverage %+v err %v, want %d", n, cov, err, workers*each)
	}
}
