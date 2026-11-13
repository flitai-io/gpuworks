package tsdb

// Schema versioning for the tsdb SQLite file. tsdb does not use GORM (it talks
// to database/sql directly, see tsdb.go), so this mirrors the main store's
// schema_migrations pattern (internal/server/store/migrations.go) by hand: a
// small marker table plus an ordered list of {version, apply} steps, each
// applied inside one transaction so a failing statement rolls back both the
// schema change and the marker insert together.

import (
	"database/sql"
	"fmt"
	"time"
)

// schemaMigration is one applied-once schema step.
type schemaMigration struct {
	Version int
	Name    string
	Apply   func(*sql.Tx) error
}

// baselineSchema creates the series/samples tables as they existed before
// tsdb had any schema versioning. Kept as CREATE TABLE/INDEX IF NOT EXISTS so
// it is a no-op against a pre-existing on-disk file (the common case: every
// tsdb file created before this migration framework existed already has these
// tables, and this step's only job for those files is to record version 1).
const baselineSchema = `
CREATE TABLE IF NOT EXISTS series (
	id          INTEGER PRIMARY KEY,
	metric      TEXT NOT NULL,
	labels_key  TEXT NOT NULL UNIQUE,
	labels_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_series_metric ON series(metric);
CREATE TABLE IF NOT EXISTS samples (
	series_id INTEGER NOT NULL,
	ts_ms     INTEGER NOT NULL,
	value     REAL NOT NULL,
	PRIMARY KEY (series_id, ts_ms)
);
CREATE INDEX IF NOT EXISTS idx_samples_ts ON samples(ts_ms);`

// schemaMigrations is the full ordered list applied by migrateSchema. Adding a
// migration means appending a new entry with the next version number; it must
// never be edited in place once released; a mid-migration failure test lives
// in schema_test.go.
var schemaMigrations = []schemaMigration{
	{
		Version: 1,
		Name:    "baseline",
		Apply: func(tx *sql.Tx) error {
			_, err := tx.Exec(baselineSchema)
			return err
		},
	},
	{
		// series_instance_deployment_columns adds the instance/deployment
		// columns QueryRange/QueryLatest push matchers into (tsdb.go) so a
		// monitor lookup by instance or deployment does not have to load and
		// JSON-decode every series row for a metric. Both columns are
		// nullable: most metrics (GPU/host samples) carry neither label.
		Version: 2,
		Name:    "series_instance_deployment_columns",
		Apply: func(tx *sql.Tx) error {
			if _, err := tx.Exec(`ALTER TABLE series ADD COLUMN instance TEXT`); err != nil {
				return fmt.Errorf("add instance column: %w", err)
			}
			if _, err := tx.Exec(`ALTER TABLE series ADD COLUMN deployment TEXT`); err != nil {
				return fmt.Errorf("add deployment column: %w", err)
			}
			// Backfill once from the existing labels_json so rows written
			// before this migration are queryable the same way as new ones.
			if _, err := tx.Exec(`UPDATE series SET
				instance = json_extract(labels_json, '$.instance'),
				deployment = json_extract(labels_json, '$.deployment')`); err != nil {
				return fmt.Errorf("backfill instance/deployment: %w", err)
			}
			if _, err := tx.Exec(`CREATE INDEX idx_series_metric_instance ON series(metric, instance)`); err != nil {
				return fmt.Errorf("create idx_series_metric_instance: %w", err)
			}
			if _, err := tx.Exec(`CREATE INDEX idx_series_metric_deployment ON series(metric, deployment)`); err != nil {
				return fmt.Errorf("create idx_series_metric_deployment: %w", err)
			}
			return nil
		},
	},
	{
		// series_node_column adds the node column a node-matched query (a
		// node's charts, a node's RDMA counters) pushes into SQL, the same
		// way migration 2 did for instance and deployment. Without it such a
		// query read and decoded every series of the metric in the fleet.
		Version: 3,
		Name:    "series_node_column",
		Apply: func(tx *sql.Tx) error {
			if _, err := tx.Exec(`ALTER TABLE series ADD COLUMN node TEXT`); err != nil {
				return fmt.Errorf("add node column: %w", err)
			}
			if _, err := tx.Exec(`UPDATE series SET node = json_extract(labels_json, '$.node')`); err != nil {
				return fmt.Errorf("backfill node: %w", err)
			}
			if _, err := tx.Exec(`CREATE INDEX idx_series_metric_node ON series(metric, node)`); err != nil {
				return fmt.Errorf("create idx_series_metric_node: %w", err)
			}
			return nil
		},
	},
}

// migrateSchema applies every not-yet-applied migration in list, in order.
// Called on every open (see OpenSQLite), so it must be cheap and idempotent
// when everything is already applied: one SELECT per migration, no writes.
func migrateSchema(db *sql.DB, list []schemaMigration) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	for _, m := range list {
		applied, err := schemaVersionApplied(db, m.Version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := applySchemaMigration(db, m); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

func schemaVersionApplied(db *sql.DB, version int) (bool, error) {
	var exists int
	err := db.QueryRow(`SELECT 1 FROM schema_version WHERE version = ?`, version).Scan(&exists)
	switch {
	case err == nil:
		return true, nil
	case err == sql.ErrNoRows:
		return false, nil
	default:
		return false, err
	}
}

// applySchemaMigration runs the schema change and its marker insert in one
// transaction, so a failing statement anywhere in Apply leaves neither behind.
func applySchemaMigration(db *sql.DB, m schemaMigration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	if err := m.Apply(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_version(version, name, applied_at) VALUES(?, ?, ?)`,
		m.Version, m.Name, time.Now().UTC().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
