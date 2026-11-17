// Package statsdb keeps the long-term service quality, GPU utilization and
// card time of usage analytics in stats.db, a SQLite file of its own
// beside metrics.db and ux.db (ADR 0055): an hour table and a day table per
// statistic, one row per object, typed columns. Hours are added to as
// rollup's deltas arrive; days are rolled up from hours in the statistics
// time zone; both are retained by age.
//
// The token usage is not here: it is what customers are billed by, so it
// lives in the main database (store.UsageHourly).
package statsdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	// Registers the pure-Go "sqlite" driver.
	_ "github.com/glebarez/go-sqlite"

	"gpuworks.ai/gpuworks/internal/sqliteutil"
)

// schemaVersion is stored in PRAGMA user_version. A file written by a newer
// schema is refused rather than misread.
const schemaVersion = 4

// schemaSteps take a file from one version to the next: step i makes
// version i+1.
var schemaSteps = [][]string{schemaV1, schemaV2, schemaV3, schemaV4}

// Store is stats.db.
type Store struct {
	path   string
	writer *sql.DB // the one connection SQLite lets write
	reader *sql.DB
}

// Open opens or creates stats.db at path.
func Open(path string) (result *Store, err error) {
	defer func() { err = sqliteutil.WrapOpenError(path, err) }()
	writer, err := openPooled(path, 1)
	if err != nil {
		return nil, err
	}
	reader, err := openPooled(path, 4)
	if err != nil {
		writer.Close()
		return nil, err
	}
	s := &Store{path: path, writer: writer, reader: reader}
	if err := s.initSchema(); err != nil {
		writer.Close()
		reader.Close()
		return nil, err
	}
	return s, nil
}

func openPooled(path string, conns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open stats db: %w", err)
	}
	db.SetMaxOpenConns(conns)
	// WAL keeps readers and the writer from blocking each other; NORMAL may
	// lose the last transactions on an OS crash, at most a flush of
	// statistics.
	for _, pragma := range []string{"PRAGMA journal_mode=WAL;", "PRAGMA busy_timeout=5000;", "PRAGMA synchronous=NORMAL;"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	return db, nil
}

// ErrNewerSchema is a stats.db written by a newer release.
var ErrNewerSchema = errors.New("stats.db was written by a newer release")

func (s *Store) initSchema() error {
	var version int
	if err := s.writer.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read stats db version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("%w (schema %d, this release reads %d)", ErrNewerSchema, version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}
	tx, err := s.writer.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for step := version; step < schemaVersion; step++ {
		for _, stmt := range schemaSteps[step] {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("stats db schema %d: %w", step+1, err)
			}
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the file.
func (s *Store) Close() error {
	return errors.Join(s.writer.Close(), s.reader.Close())
}

// SizeBytes is the file's size on disk, its write-ahead log included.
func (s *Store) SizeBytes() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

// Kind is one statistic stats.db keeps.
type Kind string

const (
	KindQuality Kind = "quality"
	KindGPU     Kind = "gpu"
	KindGPUTime Kind = "gpu_time"
	// KindScheduling is the scheduling activity (scheduling.go).
	KindScheduling Kind = "scheduling"
)

// Kinds lists them.
func Kinds() []Kind { return []Kind{KindQuality, KindGPU, KindGPUTime, KindScheduling} }

// tables are the hour and day tables of a kind.
func (k Kind) tables() (hourly, daily []string) {
	switch k {
	case KindQuality:
		return []string{"quality_hourly"}, []string{"quality_daily"}
	case KindGPU:
		return []string{"gpu_hourly", "gpu_node_hourly", "gpu_model_hourly"}, []string{"gpu_daily", "gpu_node_daily", "gpu_model_daily"}
	case KindGPUTime:
		return []string{"gpu_time_hourly", "gpu_capacity_hourly"}, []string{"gpu_time_daily", "gpu_capacity_daily"}
	case KindScheduling:
		return []string{"sched_reason_hourly", "sched_card_hourly", "sched_deployment_hourly"},
			[]string{"sched_reason_daily", "sched_card_daily", "sched_deployment_daily"}
	}
	return nil, nil
}

// RowCount is how many hour and day rows a kind holds.
func (s *Store) RowCount(ctx context.Context, kind Kind) (hourly, daily int64, err error) {
	hours, days := kind.tables()
	for _, t := range hours {
		var n int64
		if err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&n); err != nil {
			return 0, 0, err
		}
		hourly += n
	}
	for _, t := range days {
		var n int64
		if err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&n); err != nil {
			return 0, 0, err
		}
		daily += n
	}
	return hourly, daily, nil
}

// schemaV1: every hour table keys on hour_ms (UTC hour start, Unix ms), every
// day table on day (YYYYMMDD in the statistics time zone).
var schemaV1 = []string{
	// gpu_model is the card's model as the inventory last named it while it
	// reported: kept with its samples so a card since removed still has one,
	// and so a fleet's cards group by model in SQL.
	//
	// ttft_hist and e2e_hist are rollup.Histogram's encoding, read and
	// written whole, never queried into.
	`CREATE TABLE quality_hourly (
		hour_ms INTEGER NOT NULL, deployment_id INTEGER NOT NULL,
		requests INTEGER NOT NULL, completed INTEGER NOT NULL, length INTEGER NOT NULL,
		client_closed INTEGER NOT NULL, interrupted INTEGER NOT NULL, error INTEGER NOT NULL,
		ttft_hist BLOB NOT NULL, e2e_hist BLOB NOT NULL,
		output_tokens INTEGER NOT NULL, decode_ms INTEGER NOT NULL,
		PRIMARY KEY (hour_ms, deployment_id))`,
	`CREATE TABLE quality_daily (
		day INTEGER NOT NULL, deployment_id INTEGER NOT NULL,
		requests INTEGER NOT NULL, completed INTEGER NOT NULL, length INTEGER NOT NULL,
		client_closed INTEGER NOT NULL, interrupted INTEGER NOT NULL, error INTEGER NOT NULL,
		ttft_hist BLOB NOT NULL, e2e_hist BLOB NOT NULL,
		output_tokens INTEGER NOT NULL, decode_ms INTEGER NOT NULL,
		PRIMARY KEY (day, deployment_id))`,
	`CREATE TABLE gpu_hourly (
		hour_ms INTEGER NOT NULL, node_uuid TEXT NOT NULL, gpu_index INTEGER NOT NULL, gpu_uuid TEXT NOT NULL, gpu_model TEXT NOT NULL,
		util_sum REAL NOT NULL, util_n INTEGER NOT NULL, util_max REAL NOT NULL,
		mem_used_sum REAL NOT NULL, mem_used_n INTEGER NOT NULL, mem_used_max REAL NOT NULL, mem_total REAL NOT NULL,
		power_sum REAL NOT NULL, power_n INTEGER NOT NULL,
		temp_sum REAL NOT NULL, temp_n INTEGER NOT NULL, temp_max REAL NOT NULL,
		PRIMARY KEY (hour_ms, node_uuid, gpu_index, gpu_uuid))`,
	`CREATE TABLE gpu_daily (
		day INTEGER NOT NULL, node_uuid TEXT NOT NULL, gpu_index INTEGER NOT NULL, gpu_uuid TEXT NOT NULL, gpu_model TEXT NOT NULL,
		util_sum REAL NOT NULL, util_n INTEGER NOT NULL, util_max REAL NOT NULL,
		mem_used_sum REAL NOT NULL, mem_used_n INTEGER NOT NULL, mem_used_max REAL NOT NULL, mem_total REAL NOT NULL,
		power_sum REAL NOT NULL, power_n INTEGER NOT NULL,
		temp_sum REAL NOT NULL, temp_n INTEGER NOT NULL, temp_max REAL NOT NULL,
		PRIMARY KEY (day, node_uuid, gpu_index, gpu_uuid))`,
	`CREATE TABLE gpu_time_hourly (
		hour_ms INTEGER NOT NULL, deployment_id INTEGER NOT NULL, gpu_model TEXT NOT NULL,
		gpu_seconds REAL NOT NULL, whole_gpu_seconds REAL NOT NULL,
		instance_seconds REAL NOT NULL, max_instances INTEGER NOT NULL,
		PRIMARY KEY (hour_ms, deployment_id, gpu_model))`,
	`CREATE TABLE gpu_time_daily (
		day INTEGER NOT NULL, deployment_id INTEGER NOT NULL, gpu_model TEXT NOT NULL,
		gpu_seconds REAL NOT NULL, whole_gpu_seconds REAL NOT NULL,
		instance_seconds REAL NOT NULL, max_instances INTEGER NOT NULL,
		PRIMARY KEY (day, deployment_id, gpu_model))`,
	`CREATE TABLE gpu_capacity_hourly (
		hour_ms INTEGER NOT NULL, gpu_model TEXT NOT NULL,
		total_gpu_seconds REAL NOT NULL, schedulable_gpu_seconds REAL NOT NULL,
		PRIMARY KEY (hour_ms, gpu_model))`,
	`CREATE TABLE gpu_capacity_daily (
		day INTEGER NOT NULL, gpu_model TEXT NOT NULL,
		total_gpu_seconds REAL NOT NULL, schedulable_gpu_seconds REAL NOT NULL,
		PRIMARY KEY (day, gpu_model))`,
	// rolled_days are the days whose day rows are rolled up; a day missing
	// here (the server was down at its end) is rolled on start.
	`CREATE TABLE rolled_days (day INTEGER PRIMARY KEY)`,
	// backfill is how far the one-off fill from ux.db and metrics.db got,
	// per kind: it fills hours before through_ms until done.
	`CREATE TABLE backfill (kind TEXT PRIMARY KEY, through_ms INTEGER NOT NULL, done INTEGER NOT NULL)`,
}

// schemaV2 adds the GPU statistics summed per node and card model, written
// with the cards' rows: a fleet's pages read them instead of every card's
// rows (ADR 0055). A node's rows are split by card model, so a node of mixed
// cards can be filtered by model. Their figures are the cards' summed;
// mem_total is the largest card's. The step fills them from the cards' rows
// a file already holds.
var schemaV2 = []string{
	`CREATE TABLE gpu_node_hourly (
		hour_ms INTEGER NOT NULL, node_uuid TEXT NOT NULL, gpu_model TEXT NOT NULL,
		` + gpuColumns + `,
		PRIMARY KEY (hour_ms, node_uuid, gpu_model))`,
	`CREATE TABLE gpu_node_daily (
		day INTEGER NOT NULL, node_uuid TEXT NOT NULL, gpu_model TEXT NOT NULL,
		` + gpuColumns + `,
		PRIMARY KEY (day, node_uuid, gpu_model))`,
	`CREATE TABLE gpu_model_hourly (
		hour_ms INTEGER NOT NULL, gpu_model TEXT NOT NULL,
		` + gpuColumns + `,
		PRIMARY KEY (hour_ms, gpu_model))`,
	`CREATE TABLE gpu_model_daily (
		day INTEGER NOT NULL, gpu_model TEXT NOT NULL,
		` + gpuColumns + `,
		PRIMARY KEY (day, gpu_model))`,
	`INSERT INTO gpu_node_hourly SELECT hour_ms, node_uuid, gpu_model, ` + gpuSums + ` FROM gpu_hourly GROUP BY hour_ms, node_uuid, gpu_model`,
	`INSERT INTO gpu_node_daily SELECT day, node_uuid, gpu_model, ` + gpuSums + ` FROM gpu_daily GROUP BY day, node_uuid, gpu_model`,
	`INSERT INTO gpu_model_hourly SELECT hour_ms, gpu_model, ` + gpuSums + ` FROM gpu_hourly GROUP BY hour_ms, gpu_model`,
	`INSERT INTO gpu_model_daily SELECT day, gpu_model, ` + gpuSums + ` FROM gpu_daily GROUP BY day, gpu_model`,
}

// gpuColumns are the figure columns every GPU table has.
const gpuColumns = `util_sum REAL NOT NULL, util_n INTEGER NOT NULL, util_max REAL NOT NULL,
		mem_used_sum REAL NOT NULL, mem_used_n INTEGER NOT NULL, mem_used_max REAL NOT NULL, mem_total REAL NOT NULL,
		power_sum REAL NOT NULL, power_n INTEGER NOT NULL,
		temp_sum REAL NOT NULL, temp_n INTEGER NOT NULL, temp_max REAL NOT NULL`
