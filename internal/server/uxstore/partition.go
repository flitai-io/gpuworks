package uxstore

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A partition is one UTC day: req_, grid_, cnt_ and lat_ tables named after
// the day (req_20261012), so name order is day order. They are created
// together and dropped together; a day written before lat_ existed gets it
// when the store opens (initWriter).

const (
	reqPrefix  = "req_"
	gridPrefix = "grid_"
	cntPrefix  = "cnt_"
	latPrefix  = "lat_"
)

const dayMS = 24 * int64(time.Hour/time.Millisecond)

// bucketMS is the width of a grid and coverage bucket: 5 minutes. It divides
// a day, so a bucket never straddles two partitions.
const bucketMS = 5 * int64(time.Minute/time.Millisecond)

// floorBucket is the start of the 5-minute bucket holding ms.
func floorBucket(ms int64) int64 {
	b := ms - ms%bucketMS
	if ms < 0 && ms%bucketMS != 0 {
		b -= bucketMS
	}
	return b
}

// dayOf is the UTC day a timestamp belongs to.
func dayOf(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("20060102")
}

// dayStartMS is the first millisecond of a partition's UTC day.
func dayStartMS(day string) (int64, error) {
	t, err := time.ParseInLocation("20060102", day, time.UTC)
	if err != nil {
		return 0, fmt.Errorf("parse partition day %q: %w", day, err)
	}
	return t.UnixMilli(), nil
}

// execer is the subset of database/sql handles DDL runs against.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// partitionDDL creates one day's tables. Every statement is IF NOT EXISTS, so
// it is safe on an existing file.
func partitionDDL(day string) []string {
	req, grid, cnt, lat := reqPrefix+day, gridPrefix+day, cntPrefix+day, latPrefix+day
	return []string{
		// seq is the insert sequence: an INTEGER PRIMARY KEY alias of the
		// rowid, so VACUUM cannot renumber it under a pagination cursor.
		// trace_id is the request's W3C trace id (16 bytes; empty for rows
		// written before it existed), not indexed: a random key would make
		// every insert dirty its own index page, so a lookup goes through
		// the _dep index over a time window instead (lookup.go, ADR 0046).
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			seq INTEGER PRIMARY KEY,
			id TEXT NOT NULL,
			ts INTEGER NOT NULL,
			deployment_id INTEGER NOT NULL,
			instance_uuid TEXT NOT NULL,
			api_key_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			source INTEGER NOT NULL,
			path TEXT NOT NULL,
			stream INTEGER NOT NULL,
			body_bytes INTEGER NOT NULL,
			images INTEGER NOT NULL,
			dispatch_ms INTEGER NOT NULL,
			first_byte_ms INTEGER NOT NULL,
			first_think_ms INTEGER NOT NULL,
			first_answer_ms INTEGER NOT NULL,
			last_chunk_ms INTEGER NOT NULL,
			end_ms INTEGER NOT NULL,
			events INTEGER NOT NULL,
			think_events INTEGER NOT NULL,
			max_gap_ms INTEGER NOT NULL,
			status INTEGER NOT NULL,
			outcome INTEGER NOT NULL,
			finish_reason TEXT NOT NULL,
			usage INTEGER NOT NULL,
			prompt_tokens INTEGER NOT NULL,
			completion_tokens INTEGER NOT NULL,
			cached_tokens INTEGER NOT NULL,
			reasoning_tokens INTEGER NOT NULL,
			engine_queue_ms INTEGER NOT NULL,
			engine_prefill_ms INTEGER NOT NULL,
			engine_decode_ms INTEGER NOT NULL,
			weight INTEGER NOT NULL,
			trace_id BLOB NOT NULL DEFAULT x'',
			pd_prefill_ms INTEGER NOT NULL DEFAULT -1,
			pd_prefill_queue_ms INTEGER NOT NULL DEFAULT -1,
			pd_prefill_compute_ms INTEGER NOT NULL DEFAULT -1,
			pd_prefill_instance TEXT NOT NULL DEFAULT '',
			pd_fallback TEXT NOT NULL DEFAULT '',
			priority_class TEXT NOT NULL DEFAULT ''
		)`, req),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %[1]s_dep ON %[1]s(deployment_id, ts, seq)`, req),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %[1]s_key ON %[1]s(api_key_id, ts, seq)`, req),
		// The primary key leads with the bucket so the all-deployments query
		// is a range scan of the time window; the _dep index serves the
		// per-deployment one.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			bucket_ms INTEGER NOT NULL,
			deployment_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			x INTEGER NOT NULL,
			y INTEGER NOT NULL,
			n INTEGER NOT NULL,
			PRIMARY KEY (bucket_ms, deployment_id, kind, x, y)
		) WITHOUT ROWID`, grid),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %[1]s_dep ON %[1]s(deployment_id, kind, bucket_ms)`, grid),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			bucket_ms INTEGER NOT NULL,
			deployment_id INTEGER NOT NULL,
			total INTEGER NOT NULL,
			with_usage INTEGER NOT NULL,
			with_cache INTEGER NOT NULL,
			PRIMARY KEY (deployment_id, bucket_ms)
		) WITHOUT ROWID`, cnt),
		// The latency breakdown's sums (uxrecord.LatencySums): whole replies,
		// their end-to-end and per-segment milliseconds, and the tokens and
		// milliseconds of each speed.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			bucket_ms INTEGER NOT NULL,
			deployment_id INTEGER NOT NULL,
			requests INTEGER NOT NULL,
			e2e_ms REAL NOT NULL,
			gateway_ms REAL NOT NULL,
			preprocess_ms REAL NOT NULL,
			queue_ms REAL NOT NULL,
			prefill_ms REAL NOT NULL,
			waiting_ms REAL NOT NULL,
			thinking_ms REAL NOT NULL,
			answering_ms REAL NOT NULL,
			tail_ms REAL NOT NULL,
			prefill_speed_tokens REAL NOT NULL,
			prefill_speed_ms REAL NOT NULL,
			thinking_speed_tokens REAL NOT NULL,
			thinking_speed_ms REAL NOT NULL,
			answering_speed_tokens REAL NOT NULL,
			answering_speed_ms REAL NOT NULL,
			decode_speed_tokens REAL NOT NULL,
			decode_speed_ms REAL NOT NULL,
			pd_prefill_ms REAL NOT NULL DEFAULT 0,
			PRIMARY KEY (deployment_id, bucket_ms)
		) WITHOUT ROWID`, lat),
	}
}

// addedColumns are the columns added to a day's tables after the table's
// first release, per table prefix, with the DDL that adds each. Every one has
// a default, so a release that predates it still writes the table (it names
// its columns) and user_version need not move: rolling back keeps the file.
var addedColumns = []struct {
	prefix string
	cols   []struct{ name, ddl string }
}{
	{reqPrefix, []struct{ name, ddl string }{
		{"trace_id", `ALTER TABLE %s ADD COLUMN trace_id BLOB NOT NULL DEFAULT x''`},
		// A PD request's prefill stage (ADR 0047); -1 / '' for none.
		{"pd_prefill_ms", `ALTER TABLE %s ADD COLUMN pd_prefill_ms INTEGER NOT NULL DEFAULT -1`},
		{"pd_prefill_queue_ms", `ALTER TABLE %s ADD COLUMN pd_prefill_queue_ms INTEGER NOT NULL DEFAULT -1`},
		{"pd_prefill_compute_ms", `ALTER TABLE %s ADD COLUMN pd_prefill_compute_ms INTEGER NOT NULL DEFAULT -1`},
		{"pd_prefill_instance", `ALTER TABLE %s ADD COLUMN pd_prefill_instance TEXT NOT NULL DEFAULT ''`},
		{"pd_fallback", `ALTER TABLE %s ADD COLUMN pd_fallback TEXT NOT NULL DEFAULT ''`},
		// The priority class the gateway ran the request at; '' before.
		{"priority_class", `ALTER TABLE %s ADD COLUMN priority_class TEXT NOT NULL DEFAULT ''`},
	}},
	{latPrefix, []struct{ name, ddl string }{
		{"pd_prefill_ms", `ALTER TABLE %s ADD COLUMN pd_prefill_ms REAL NOT NULL DEFAULT 0`},
	}},
}

// completePartition adds the columns of addedColumns a day's tables lack: a
// day written by an older release, before or after an upgrade (a rollback
// may write new days in the old shape).
func completePartition(db interface {
	execer
	queryer
}, day string) error {
	for _, added := range addedColumns {
		if err := completeTable(db, added.prefix+day, added.cols); err != nil {
			return err
		}
	}
	return nil
}

func completeTable(db interface {
	execer
	queryer
}, table string, cols []struct{ name, ddl string }) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(have) == 0 { // no such table that day
		return err
	}
	for _, col := range cols {
		if have[col.name] {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(col.ddl, table)); err != nil {
			return fmt.Errorf("add %s.%s: %w", table, col.name, err)
		}
	}
	return nil
}

// ensurePartition creates a day's tables unless the writer already knows
// them. created reports whether DDL ran; the caller marks the day ready only
// after its transaction commits, so a rolled-back create is retried.
func (s *Store) ensurePartition(e execer, day string) (created bool, err error) {
	if _, ok := s.w.ready[day]; ok {
		return false, nil
	}
	for _, stmt := range partitionDDL(day) {
		if _, err := e.Exec(stmt); err != nil {
			return false, fmt.Errorf("ensure partition %s: %w", day, err)
		}
	}
	return true, nil
}

// dropPartition removes a day's tables (indexes go with them).
func dropPartition(e execer, day string) error {
	for _, prefix := range []string{reqPrefix, gridPrefix, cntPrefix, latPrefix} {
		if _, err := e.Exec(`DROP TABLE IF EXISTS ` + prefix + day); err != nil {
			return fmt.Errorf("drop partition %s%s: %w", prefix, day, err)
		}
	}
	return nil
}

// queryer is the subset of database/sql handles catalog reads run against.
type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// listDays returns the days that have any partition table, newest first.
func listDays(db queryer) ([]string, error) {
	const glob = `[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]`
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND
		(name GLOB 'req_` + glob + `' OR name GLOB 'grid_` + glob + `' OR name GLOB 'cnt_` + glob + `')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		day := name[strings.IndexByte(name, '_')+1:]
		if _, ok := seen[day]; !ok {
			seen[day] = struct{}{}
			out = append(out, day)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(b, a) })
	return out, nil
}

// daysInRange narrows a newest-first day list to [fromMS, toMS]; a bound <= 0
// is open.
func daysInRange(days []string, fromMS, toMS int64) []string {
	fromDay, toDay := "", ""
	if fromMS > 0 {
		fromDay = dayOf(fromMS)
	}
	if toMS > 0 {
		toDay = dayOf(toMS)
	}
	out := make([]string, 0, len(days))
	for _, day := range days {
		if (toDay != "" && day > toDay) || (fromDay != "" && day < fromDay) {
			continue
		}
		out = append(out, day)
	}
	return out
}
