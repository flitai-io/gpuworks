package store

import (
	"fmt"
	"testing"
)

// The runtime a node passed over and why arrive as two columns, empty for
// every existing node until its next report. Applying the change again
// changes nothing.
func TestRuntimeFallbackMigrationAddsItsColumns(t *testing.T) {
	db := nodesOnlyDB(t, "runtime_gpu_blocked text")
	if err := db.Exec(`INSERT INTO nodes (id, uuid, runtime_gpu_blocked) VALUES (1, 'n-1', 'gpu_plugin_missing')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "node_runtime_fallback")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"runtime_fallback_from", "runtime_fallback_reason"} {
		if !db.Migrator().HasColumn("nodes", column) {
			t.Fatalf("%s is missing after the migration", column)
		}
	}
	var row struct{ RuntimeFallbackFrom, RuntimeFallbackReason, RuntimeGPUBlocked string }
	if err := db.Raw(`SELECT COALESCE(runtime_fallback_from, '') AS runtime_fallback_from, COALESCE(runtime_fallback_reason, '') AS runtime_fallback_reason, runtime_gpu_blocked FROM nodes WHERE id = 1`).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.RuntimeFallbackFrom != "" || row.RuntimeFallbackReason != "" || row.RuntimeGPUBlocked != "gpu_plugin_missing" {
		t.Fatalf("existing node after the migration: %+v", row)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", m.Version, m.Name).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d, error=%v", markers, err)
	}
}

// Schema and marker are one transaction: a failure leaves neither, and the
// retry completes.
func TestRuntimeFallbackMigrationRollsBackAndCanRetry(t *testing.T) {
	db := nodesOnlyDB(t, "")
	m := migrationByName(t, "node_runtime_fallback")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_fallback_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("nodes", "runtime_fallback_from") || db.Migrator().HasColumn("nodes", "runtime_fallback_reason") {
		t.Fatal("a column survived a failed migration")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration markers=%d, error=%v", markers, err)
	}
	if err := db.Exec(`DROP TRIGGER fail_fallback_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry after the injected failure: %v", err)
	}
	if !db.Migrator().HasColumn("nodes", "runtime_fallback_from") || !db.Migrator().HasColumn("nodes", "runtime_fallback_reason") {
		t.Fatal("the retry did not add the columns")
	}
}

var runtimeFallbackSocketColumns = []string{
	"runtime_fallback_socket", "runtime_fallback_socket_group", "runtime_fallback_socket_gid",
	"runtime_fallback_socket_group_held",
	"runtime_fallback_socket_account", "runtime_fallback_socket_same_name_gid",
	"runtime_fallback_socket_restart_pending", "runtime_fallback_socket_supervisor",
	"runtime_fallback_socket_state_dir",
}

// The socket facts arrive as typed columns, empty for every existing node
// (which reads back as no socket) until its next report. Applying the
// change again changes nothing.
func TestRuntimeFallbackSocketMigrationAddsItsColumns(t *testing.T) {
	db := nodesOnlyDB(t, "runtime_fallback_from text, runtime_fallback_reason text")
	if err := db.Exec(`INSERT INTO nodes (id, uuid, runtime_fallback_from, runtime_fallback_reason) VALUES (1, 'n-1', 'docker', 'daemon_not_ready')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "node_runtime_fallback_socket")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	for _, column := range runtimeFallbackSocketColumns {
		if !db.Migrator().HasColumn("nodes", column) {
			t.Fatalf("%s is missing after the migration", column)
		}
	}
	var row struct {
		RuntimeFallbackReason, Socket string
		GID                           int
		Pending                       bool
	}
	if err := db.Raw(`SELECT runtime_fallback_reason, COALESCE(runtime_fallback_socket, '') AS socket, COALESCE(runtime_fallback_socket_gid, 0) AS gid, COALESCE(runtime_fallback_socket_restart_pending, false) AS pending FROM nodes WHERE id = 1`).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.RuntimeFallbackReason != "daemon_not_ready" || row.Socket != "" || row.GID != 0 || row.Pending {
		t.Fatalf("existing node after the migration: %+v", row)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ? AND name = ?", m.Version, m.Name).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d, error=%v", markers, err)
	}
}

// Schema and marker are one transaction: a failure leaves no column, and
// the retry completes.
func TestRuntimeFallbackSocketMigrationRollsBackAndCanRetry(t *testing.T) {
	db := nodesOnlyDB(t, "")
	m := migrationByName(t, "node_runtime_fallback_socket")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_socket_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	for _, column := range runtimeFallbackSocketColumns {
		if db.Migrator().HasColumn("nodes", column) {
			t.Fatalf("%s survived a failed migration", column)
		}
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("failed migration markers=%d, error=%v", markers, err)
	}
	if err := db.Exec(`DROP TRIGGER fail_socket_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry after the injected failure: %v", err)
	}
	for _, column := range runtimeFallbackSocketColumns {
		if !db.Migrator().HasColumn("nodes", column) {
			t.Fatalf("the retry did not add %s", column)
		}
	}
}
