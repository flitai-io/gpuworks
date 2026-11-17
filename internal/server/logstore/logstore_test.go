package logstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

// TestFTS5Available is the load-bearing check: the whole store depends on the
// modernc/glebarez pure-Go SQLite build including the FTS5 extension — and on
// the table-valued-function form the partition queries use.
func TestFTS5Available(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE m(id INTEGER PRIMARY KEY, line TEXT)`); err != nil {
		t.Fatalf("meta table: %v", err)
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(line, content='m', content_rowid='id')`); err != nil {
		t.Fatalf("FTS5 not available (CREATE VIRTUAL TABLE failed): %v", err)
	}
	if _, err := db.Exec(`INSERT INTO m(line) VALUES ('CUDA out of memory')`); err != nil {
		t.Fatalf("insert meta: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t(rowid, line) VALUES (1, 'CUDA out of memory')`); err != nil {
		t.Fatalf("insert fts: %v", err)
	}
	var got string
	if err := db.QueryRow(`SELECT m.line FROM t(?) f JOIN m ON m.id = f.rowid`, "memory").Scan(&got); err != nil {
		t.Fatalf("FTS5 table-valued-function query failed: %v", err)
	}
	if got != "CUDA out of memory" {
		t.Fatalf("unexpected MATCH result: %q", got)
	}
}

func newStore(t *testing.T) Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logs.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seed spans two UTC days so cross-partition behavior is exercised by the
// shared fixtures, not only by dedicated tests. Levels are stamped like the
// agent would.
func seed(t *testing.T, s Store) {
	t.Helper()
	day2 := time.Now().UTC().Add(24 * time.Hour).Format("20060102")
	at := func(h, m int) int64 {
		t2, err := time.ParseInLocation("20060102-15:04", day2+"-15:00", time.UTC)
		if err != nil {
			t.Fatalf("parse fixture day: %v", err)
		}
		return t2.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute).UnixMilli()
	}
	entries := []Entry{
		{Instance: "inst-a", Node: "node-1", TimeMS: at(0, 10), Level: LevelInfo, Source: SourceInstance, Line: "starting container"},
		{Instance: "inst-a", Node: "node-1", TimeMS: at(0, 20), Level: LevelInfo, Source: SourceInstance, Line: "loading model weights"},
		{Instance: "inst-a", Node: "node-1", TimeMS: at(0, 30), Level: LevelError, Source: SourceInstance, Line: "CUDA out of memory"},
		{Instance: "inst-b", Node: "node-2", TimeMS: at(0, 15), Level: LevelInfo, Source: SourceInstance, Line: "healthy and serving"},
		{Instance: "inst-b", Node: "node-2", TimeMS: at(0, 25), Level: LevelWarn, Source: SourceInstance, Line: "CUDA driver initialized with warnings"},
	}
	if err := s.Append(entries); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func TestSearchFindsPhrase(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	res, err := s.Search(Filter{Query: `CUDA out of memory`, Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, e := range res.Entries {
		if e.Line == "CUDA out of memory" && e.Instance == "inst-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected to find OOM line, got %+v", res.Entries)
	}
}

func TestSearchInstancePrefixFilter(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	// The UI shows 8-char short IDs, so a short prefix must find the instance.
	res, err := s.Search(Filter{Query: "CUDA", Instance: "inst-b", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) == 0 {
		t.Fatalf("expected matches for inst-b")
	}
	for _, e := range res.Entries {
		if e.Instance != "inst-b" {
			t.Fatalf("instance filter leaked entry: %+v", e)
		}
	}
	// Newest-first ordering.
	for i := 1; i < len(res.Entries); i++ {
		if res.Entries[i-1].TimeMS < res.Entries[i].TimeMS {
			t.Fatalf("Search not newest-first: %+v", res.Entries)
		}
	}
}

func TestSearchSpecialCharsNoSyntaxError(t *testing.T) {
	s := newStore(t)
	seed(t, s)
	// A raw string with FTS5-special punctuation must not error out.
	if _, err := s.Search(Filter{Query: "memory: (out)", Limit: 10}); err != nil {
		t.Fatalf("Search with special chars errored: %v", err)
	}
}

func TestSearchEmptyQueryFallsBackToTail(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	res, err := s.Search(Filter{Instance: "inst-a", Limit: 10})
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("expected 3 inst-a entries, got %d", len(res.Entries))
	}
	for i := 1; i < len(res.Entries); i++ {
		if res.Entries[i-1].TimeMS < res.Entries[i].TimeMS {
			t.Fatalf("tail not newest-first: %+v", res.Entries)
		}
	}
}

// TestSearchPagesByArrivalAcrossPartitions pins the ordering contract: pages
// follow insertion order (rowid) within each partition and newest-day-first
// across partitions, so pagination streams from the tail instead of sorting
// the match set by wall-clock ts. The fixture inserts a10..a30, b15, b25 in
// that order, so arrival order is id 5,4,3,2,1 — which interleaves the two
// instances differently than their ts order.
func TestSearchPagesByArrivalAcrossPartitions(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	res, err := s.Search(Filter{Limit: 100})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 5 {
		t.Fatalf("expected all 5 entries across partitions, got %d", len(res.Entries))
	}
	want := []int64{5, 4, 3, 2, 1}
	for i, e := range res.Entries {
		if e.ID != want[i] {
			t.Fatalf("arrival order broken at %d: got id %d want %d (%+v)", i, e.ID, want[i], res.Entries)
		}
	}
}

func TestSearchLevelFilter(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	res, err := s.Search(Filter{Level: LevelWarn, Limit: 100})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("level>=warn should match 2 lines, got %d: %+v", len(res.Entries), res.Entries)
	}
	for _, e := range res.Entries {
		if e.Level != LevelWarn && e.Level != LevelError {
			t.Fatalf("level filter leaked %q line: %+v", e.Level, e)
		}
	}

	res, err = s.Search(Filter{Level: LevelError, Limit: 100})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Line != "CUDA out of memory" {
		t.Fatalf("level>=error should match exactly the OOM line, got %+v", res.Entries)
	}
}

func TestSearchWithTotal(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	res, err := s.Search(Filter{Query: "CUDA", Limit: 1, WithTotal: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 2 {
		t.Fatalf("Total = %d, want 2 matches for CUDA", res.Total)
	}
	if res.TotalCapped {
		t.Fatal("TotalCapped set on a tiny corpus")
	}
	if len(res.Entries) != 1 {
		t.Fatalf("page should respect small limit, got %d", len(res.Entries))
	}
}

func TestSearchCursorPaginates(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	seen := map[string]bool{}
	cursorTS, cursorID := int64(0), int64(0)
	pages := 0
	for {
		res, err := s.Search(Filter{Limit: 2, CursorTS: cursorTS, CursorID: cursorID})
		if err != nil {
			t.Fatalf("Search page %d: %v", pages, err)
		}
		if len(res.Entries) == 0 {
			break
		}
		pages++
		for _, e := range res.Entries {
			key := fmt.Sprintf("%s:%d:%d", e.Instance, e.TimeMS, e.ID)
			if seen[key] {
				t.Fatalf("cursor pagination revisited %s", key)
			}
			seen[key] = true
			cursorTS, cursorID = e.TimeMS, e.ID
		}
		if pages > 10 {
			t.Fatal("cursor pagination did not exhaust the corpus")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("cursor pagination saw %d rows, want 5", len(seen))
	}
}

// TestContextWindow builds a symmetric window around the middle line of an
// instance and checks order, match position, and same-instance containment.
func TestContextWindow(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	res, err := s.Search(Filter{Query: `"loading model weights"`, Limit: 1})
	if err != nil {
		t.Fatalf("Search anchor: %v", err)
	}
	anchor := res.Entries[0]

	rows, idx, err := s.Context(anchor.Instance, "node-1", anchor.TimeMS, anchor.ID, 1, 1)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if len(rows) != 3 || idx != 1 {
		t.Fatalf("Context = %d rows, matchIndex %d; want 3 rows around index 1", len(rows), idx)
	}
	if rows[0].Line != "starting container" ||
		rows[1].Line != "loading model weights" ||
		rows[2].Line != "CUDA out of memory" {
		t.Fatalf("window content wrong: %+v", rows)
	}
	// A wider window cannot leak the other instance's lines.
	rows, _, err = s.Context(anchor.Instance, "node-1", anchor.TimeMS, anchor.ID, 5, 5)
	if err != nil {
		t.Fatalf("Context wide: %v", err)
	}
	for _, e := range rows {
		if e.Instance != anchor.Instance {
			t.Fatalf("context leaked another instance: %+v", e)
		}
	}
}

// TestContextMissingAnchorMatchIndex pins the contract for an anchor id that
// does not exist (GWKS-147): matchIndex must be -1, never an index into the
// returned slice. The reporter measured items=[672,674] with match_index=2 -
// two real rows but an index one past the end - so a caller indexing
// items[match_index] panicked or highlighted the wrong row. Both shapes are
// covered: neighbours present, and an empty window.
func TestContextMissingAnchorMatchIndex(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	res, err := s.Search(Filter{Query: `"CUDA out of memory"`, Limit: 1})
	if err != nil {
		t.Fatalf("Search anchor: %v", err)
	}
	real := res.Entries[0]
	// A nonexistent id between real neighbours: the case that returned a
	// non-empty window with an out-of-range index.
	const missingID = 1 << 40
	rows, idx, err := s.Context(real.Instance, "node-1", real.TimeMS, missingID, 2, 2)
	if err != nil {
		t.Fatalf("Context missing anchor: %v", err)
	}
	if idx != -1 {
		t.Fatalf("matchIndex = %d for a missing anchor with %d rows; want -1", idx, len(rows))
	}
	if len(rows) == 0 {
		t.Fatal("expected the neighbouring rows to still be returned around a missing anchor")
	}

	// Empty window: an instance with no rows at all returns no neighbours
	// either, and the old code reported matchIndex == len(rows) == 0.
	rows, idx, err = s.Context("inst-absent", "", real.TimeMS, missingID, 2, 2)
	if err != nil {
		t.Fatalf("Context missing anchor, empty window: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 for an instance with no rows", len(rows))
	}
	if idx != -1 {
		t.Fatalf("matchIndex = %d for an empty window; want -1", idx)
	}

	// A present anchor still reports its own position in the same window.
	rows, idx, err = s.Context(real.Instance, "node-1", real.TimeMS, real.ID, 2, 2)
	if err != nil {
		t.Fatalf("Context present anchor: %v", err)
	}
	if idx < 0 || idx >= len(rows) {
		t.Fatalf("matchIndex = %d is outside a %d-row window", idx, len(rows))
	}
	if rows[idx].ID != real.ID {
		t.Fatalf("rows[%d] = %+v, want the anchor id %d", idx, rows[idx], real.ID)
	}
}

func TestHistogram(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	buckets, err := s.Histogram(Filter{}, 60_000)
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}
	total := int64(0)
	for _, b := range buckets {
		for _, n := range b.Counts {
			total += n
		}
	}
	if total != 5 {
		t.Fatalf("histogram total = %d, want 5", total)
	}
	// Buckets are newest-first like every other store read.
	for i := 1; i < len(buckets); i++ {
		if buckets[i-1].StartMS < buckets[i].StartMS {
			t.Fatalf("histogram bucket order broken: %+v", buckets)
		}
	}
}

func TestRetainDeletesOld(t *testing.T) {
	s := newStore(t)
	seed(t, s)

	// Delete everything older than 15:22 of the fixture day: keeps the
	// 15:25 (warn) and 15:30 (error) rows, drops the other three.
	day2 := time.Now().UTC().Add(24 * time.Hour).Format("20060102")
	t2, _ := time.ParseInLocation("20060102-15:04", day2+"-15:00", time.UTC)
	cutoff := t2.Add(22 * time.Minute).UnixMilli()
	if err := s.Retain(context.Background(), cutoff); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	res, err := s.Search(Filter{Limit: 100})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("expected the 2 newest entries after retain, got %d: %+v", len(res.Entries), res.Entries)
	}
	for _, e := range res.Entries {
		if e.TimeMS < cutoff {
			t.Fatalf("Retain left stale entry: %+v", e)
		}
	}
}

// TestRetainDropsWholePartitions checks UTC boundaries and both halves of
// each expired partition, including the FTS shadow tables.
func TestRetainDropsWholePartitions(t *testing.T) {
	for _, offset := range []time.Duration{0, 12 * time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			s := newStore(t).(*sqliteStore)
			midnight := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
			cutoff := midnight.Add(offset).UnixMilli()
			expired := midnight.Add(-time.Millisecond).UnixMilli()
			entries := []Entry{
				{TimeMS: expired, Line: "expired previous day"},
				{TimeMS: cutoff - 1, Line: "expired boundary"},
				{TimeMS: cutoff, Line: "surviving boundary"},
				{TimeMS: midnight.Add(24 * time.Hour).UnixMilli(), Line: "surviving future"},
			}
			if err := s.Append(entries); err != nil {
				t.Fatal(err)
			}
			if err := s.Retain(context.Background(), cutoff); err != nil {
				t.Fatal(err)
			}
			var count int
			name := partitionName(expired)
			if err := s.reader.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ? OR name GLOB ?`, name, name+"_*").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("expired partition and FTS objects remain: %d", count)
			}
			for _, query := range []string{"", "expired", "surviving"} {
				result, err := s.Search(Filter{Query: query, FromMS: expired, Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				want := 2
				if query == "expired" {
					want = 0
				}
				if len(result.Entries) != want {
					t.Fatalf("query %q = %+v, want %d surviving rows", query, result.Entries, want)
				}
			}
			if err := s.Retain(context.Background(), cutoff); err != nil {
				t.Fatalf("repeat retention: %v", err)
			}
		})
	}
}

// TestLegacyV1Migration opens a hand-built v1 store (single plain FTS5 table)
// and verifies the atomic migration: rows preserved and searchable, legacy
// table gone, defaults stamped, and reopening is a no-op.
func TestLegacyV1Migration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v1: %v", err)
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE logs USING fts5(line, instance UNINDEXED, node UNINDEXED, ts UNINDEXED)`); err != nil {
		t.Fatalf("create v1: %v", err)
	}
	v1lines := []struct {
		line, inst, node string
		ts               int64
	}{
		{"starting container", "inst-old", "node-1", 1700000000000},
		{"CUDA out of memory", "inst-old", "node-1", 1700000001000},
		{"recent line", "inst-new", "node-2", time.Now().UnixMilli()},
	}
	for _, l := range v1lines {
		if _, err := db.Exec(`INSERT INTO logs(line, instance, node, ts) VALUES (?,?,?,?)`,
			l.line, l.inst, l.node, l.ts); err != nil {
			t.Fatalf("seed v1: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v1: %v", err)
	}

	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite (migration): %v", err)
	}
	defer s.Close()

	res, err := s.Search(Filter{Limit: 100})
	if err != nil {
		t.Fatalf("Search after migration: %v", err)
	}
	if len(res.Entries) != len(v1lines) {
		t.Fatalf("migrated %d rows, want %d: %+v", len(res.Entries), len(v1lines), res.Entries)
	}
	for _, e := range res.Entries {
		if e.Source != SourceInstance || e.Level != "" {
			t.Fatalf("migrated row should default source=instance, level=unknown: %+v", e)
		}
	}
	// The store keeps working after migration: appends and searches mix.
	if err := s.Append([]Entry{{Instance: "inst-new", TimeMS: time.Now().UnixMilli(), Level: LevelInfo, Source: SourceInstance, Line: "post-migration line"}}); err != nil {
		t.Fatalf("Append after migration: %v", err)
	}
	res, err = s.Search(Filter{Query: "post-migration", Limit: 10})
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("search after append: %v %+v", err, res.Entries)
	}

	// Reopen: migration must be a no-op (legacy table is gone).
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	res, err = s2.Search(Filter{Limit: 100})
	if err != nil || len(res.Entries) != len(v1lines)+1 {
		t.Fatalf("reopen changed the corpus: %v %d", err, len(res.Entries))
	}
}

// TestCheckpointAdvancesTheWAL pins the hygiene contract: after writes with
// no reader pinning frames, a passive checkpoint reports progress and the
// store keeps serving reads.
func TestCheckpointAdvancesTheWAL(t *testing.T) {
	s := newStore(t).(*sqliteStore)
	now := time.Now().UnixMilli()
	var rows []Entry
	for i := 0; i < 200; i++ {
		rows = append(rows, Entry{Instance: "inst-cp", TimeMS: now, Level: LevelInfo, Source: SourceInstance, Line: "checkpoint probe line"})
	}
	if err := s.Append(rows); err != nil {
		t.Fatal(err)
	}
	busy, logPages, checkpointed, err := s.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if busy {
		t.Fatal("checkpoint reported busy with no readers open")
	}
	if logPages == 0 || checkpointed == 0 {
		t.Fatalf("checkpoint made no progress: log=%d checkpointed=%d", logPages, checkpointed)
	}
	res, err := s.Search(Filter{Instance: "inst-cp", Limit: 5})
	if err != nil || len(res.Entries) != 5 {
		t.Fatalf("store unreadable after checkpoint: %v %+v", err, res.Entries)
	}
}

// TestPartitionMemoShortCircuitsDDL documents the memo's contract: after a
// day is ensured once, later appends skip the DDL entirely — observable by
// dropping the partition behind the memo's back and seeing the insert fail
// instead of silently recreating the table. Retain is the only sanctioned
// dropper, and it invalidates the memo (next test).
func TestPartitionMemoShortCircuitsDDL(t *testing.T) {
	s := newStore(t).(*sqliteStore)
	day := time.Now().UTC().Add(-48 * time.Hour) // an old day, not today's traffic
	if err := s.Append([]Entry{{Instance: "inst-memo", TimeMS: day.UnixMilli(), Line: "first", Source: SourceInstance}}); err != nil {
		t.Fatal(err)
	}
	// Drop the meta partition directly, behind the memo.
	name := partitionPrefix + partitionDay(day.UnixMilli())
	if _, err := s.writer.Exec(`DROP TABLE ` + name); err != nil {
		t.Fatal(err)
	}
	err := s.Append([]Entry{{Instance: "inst-memo", TimeMS: day.Add(time.Second).UnixMilli(), Line: "second", Source: SourceInstance}})
	if err == nil {
		t.Fatal("append succeeded against a dropped partition: the memo did not short-circuit the DDL")
	}
}

// TestRetainInvalidatesPartitionMemo: retention drops expired days, so a late
// row for a dropped day must recreate the partition rather than fail.
func TestRetainInvalidatesPartitionMemo(t *testing.T) {
	s := newStore(t).(*sqliteStore)
	old := time.Now().UTC().Add(-72 * time.Hour)
	if err := s.Append([]Entry{{Instance: "inst-late", TimeMS: old.UnixMilli(), Line: "before retain", Source: SourceInstance}}); err != nil {
		t.Fatal(err)
	}
	// Retain everything older than now-1h: drops the -72h partition.
	if err := s.Retain(context.Background(), time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// A late-arriving row for the dropped day recreates its partition.
	if err := s.Append([]Entry{{Instance: "inst-late", TimeMS: old.Add(time.Minute).UnixMilli(), Line: "after retain", Source: SourceInstance}}); err != nil {
		t.Fatalf("late row for a retained-away day must succeed: %v", err)
	}
	res, err := s.Search(Filter{Instance: "inst-late", FromMS: old.Add(-time.Minute).UnixMilli(), Limit: 10})
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("late row not searchable: %v %+v", err, res.Entries)
	}
}

// The FTS5 errors come from the real SQLite driver, so the classification is
// pinned against what it actually says for each invalid input.
func TestFTSQueryErrorClassifiesTheDriversAnswer(t *testing.T) {
	db, err := openPooled(filepath.Join(t.TempDir(), "fts.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(line)`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`memory: (out)`, `nosuchcol:word`, `AND`} {
		_, qerr := db.Exec(`SELECT * FROM t WHERE t MATCH ?`, query)
		if qerr == nil {
			t.Fatalf("SQLite accepted %q", query)
		}
		if got := ftsQueryError(qerr); !errors.Is(fmt.Errorf("search: %w", got), errFTSSyntax) || got.Error() == "" {
			t.Fatalf("%q: %v not classified as errFTSSyntax", query, qerr)
		}
	}
	_, missing := db.Exec(`SELECT * FROM no_such_partition`)
	if missing == nil || errors.Is(ftsQueryError(missing), errFTSSyntax) {
		t.Fatalf("missing table classified as FTS syntax: %v", missing)
	}
	if ftsQueryError(nil) != nil || errors.Is(ftsQueryError(errors.New("disk I/O error")), errFTSSyntax) {
		t.Fatal("unrelated errors must pass through")
	}
}
