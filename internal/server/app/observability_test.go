package app

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/obsingest"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

func TestSearchLogsPipesFilterAndFullPages(t *testing.T) {
	ls, err := logstore.OpenSQLite(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ls.Close() })
	entries := make([]logstore.Entry, 0, 5)
	for i := 1; i <= 5; i++ {
		entries = append(entries, logstore.Entry{Instance: "inst-1", Node: "n", TimeMS: int64(i) * 1000, Line: "line"})
	}
	if err := ls.Append(entries); err != nil {
		t.Fatal(err)
	}
	o := &observability{obsingest.New(obsingest.Stores{Logs: ls})}

	// The store now does its own limit+1 truncation probing (logstore.Result
	// .Truncated): a page smaller than the full match set comes back exactly
	// at the requested size, with Truncated set - searchLogs just has to
	// pipe that flag through to the wire.
	page, err := o.searchLogs(api.LogsFilter{Instance: "inst-1", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("page should be exactly the requested size, got %d", len(page.Items))
	}
	if !page.Truncated {
		t.Fatal("Truncated should be true: 5 rows exist behind a 3-row page")
	}
	if page.Items[0].TimeMS != 5000 || page.Items[2].TimeMS != 3000 {
		t.Fatalf("newest-first page = %+v", page.Items)
	}

	page, err = o.searchLogs(api.LogsFilter{Instance: "inst-1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 5 {
		t.Fatalf("rows = %d, want all 5", len(page.Items))
	}
	if page.Truncated {
		t.Fatal("Truncated should be false when the page already holds every matching row")
	}
}

// fakeTSDBStore records every batch WriteBatch receives, so a test can assert
// on the samples actually persisted without a real SQLite file.
type fakeTSDBStore struct {
	mu      sync.Mutex
	written []tsdb.Sample
}

func (f *fakeTSDBStore) WriteBatch(samples []tsdb.Sample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, samples...)
	return nil
}
func (f *fakeTSDBStore) QueryRange(string, map[string]string, int64, int64, int64) ([]tsdb.Series, error) {
	return nil, nil
}
func (f *fakeTSDBStore) QueryLatest(string, map[string]string, int64, ...int64) ([]tsdb.LatestPoint, error) {
	return nil, nil
}
func (f *fakeTSDBStore) Retain(context.Context, int64) error { return nil }
func (f *fakeTSDBStore) Close() error                        { return nil }

func TestLatestMetricsPassesSnapshotBoundsToSQLite(t *testing.T) {
	db, err := tsdb.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.WriteBatch([]tsdb.Sample{
		{Metric: "util", Labels: map[string]string{"node": "n"}, TimeMS: 1000, Value: 20},
		{Metric: "util", Labels: map[string]string{"node": "n"}, TimeMS: 3000, Value: 99},
	}); err != nil {
		t.Fatal(err)
	}
	o := &observability{obsingest.New(obsingest.Stores{TSDB: db})}
	got, err := o.latestMetrics("util", map[string]string{"node": "n"}, 1000, 2000)
	if err != nil || len(got) != 1 || got[0].Value != 20 || got[0].TimeMS != 1000 || got[0].Labels["node"] != "n" {
		t.Fatalf("bounded metrics=%+v error=%v", got, err)
	}
	got, err = o.latestMetrics("util", nil, 1000)
	if err != nil || len(got) != 1 || got[0].Value != 99 {
		t.Fatalf("unbounded metrics=%+v error=%v", got, err)
	}
}
