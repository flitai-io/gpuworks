package tsdb

import (
	"fmt"
	"testing"
	"time"
)

// An open range scan holds a read transaction on the read pool for as long as
// rows are being streamed. Writes must not queue behind it: before the pools
// were split, the scan held the store's only connection and WriteBatch waited
// until it finished.
func TestWriteBatchDoesNotWaitForInFlightRangeScan(t *testing.T) {
	s := openTemp(t).(*sqliteStore)
	if err := s.WriteBatch([]Sample{sample("0", 1000, 10), sample("0", 2000, 20)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, err := s.reader.Query(fmt.Sprintf(queryRangeJoinSQL, ""), 0, 10_000, metricUtil)
	if err != nil {
		t.Fatalf("open scan: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("scan returned no rows: %v", rows.Err())
	}

	done := make(chan error, 1)
	go func() { done <- s.WriteBatch([]Sample{sample("0", 3000, 30)}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write during scan: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WriteBatch blocked behind an in-flight read")
	}

	// A second query runs on another reader connection and sees the commit.
	got, err := s.QueryRange(metricUtil, map[string]string{"gpu": "0"}, 0, 10_000, 1000)
	if err != nil {
		t.Fatalf("query during scan: %v", err)
	}
	if len(got) != 1 || len(got[0].Points) != 3 {
		t.Fatalf("query during scan = %+v, want one series with 3 points", got)
	}
}

func TestReadPoolRejectsWrites(t *testing.T) {
	s := openTemp(t).(*sqliteStore)
	if s.reader == s.db {
		t.Fatal("file-backed store shares the writer connection for reads")
	}
	if _, err := s.reader.Exec(`INSERT INTO series(metric, labels_key, labels_json) VALUES('m', 'k', '{}')`); err == nil {
		t.Fatal("read pool accepted a write")
	}
}

func TestInMemoryStoreReadsItsOwnWrites(t *testing.T) {
	st, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.WriteBatch([]Sample{sample("0", 1000, 10)}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := st.QueryLatest(metricUtil, nil, 0)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(got) != 1 || got[0].Value != 10 {
		t.Fatalf("latest = %+v, want one point with value 10", got)
	}
}
