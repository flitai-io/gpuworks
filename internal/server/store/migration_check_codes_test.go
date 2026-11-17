package store

import (
	"slices"
	"testing"
	"time"
)

// legacyCheckCodes is a store whose card rows and failure records were written
// before migration 217, exactly as an older server stored them: lower_snake
// codes beside rows already in UPPER_SNAKE and rows with none.
func legacyCheckCodes(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	node, err := s.RegisterNode("n-codes", HashToken("n-codes"), "n-codes")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM schema_migrations WHERE version = 217`, nil},
		{`INSERT INTO gpus (node_id, ident, "index", uuid, health_status, health_code) VALUES (?, 'uuid:GPU-0', 0, 'GPU-0', 'Error', 'runtime_unusable')`, []any{node.ID}},
		{`INSERT INTO gpus (node_id, ident, "index", uuid, health_status, health_code) VALUES (?, 'uuid:GPU-1', 1, 'GPU-1', 'Warning', 'PCIE_LINK_DEGRADED')`, []any{node.ID}},
		{`INSERT INTO gpus (node_id, ident, "index", uuid, health_status, health_code) VALUES (?, 'uuid:GPU-2', 2, 'GPU-2', 'OK', '')`, []any{node.ID}},
		{`INSERT INTO card_health_episodes (node_id, card_ident, check_id, level, reason, started_at, last_seen_at, count, flapping, first_evidence, latest_evidence)
			VALUES (?, 'uuid:GPU-0', 'runtime', 'fail', 'runtime_unusable', ?, ?, 3, false, '', '')`, []any{node.ID, started, started}},
		{`INSERT INTO card_health_episodes (node_id, card_ident, check_id, level, reason, started_at, last_seen_at, count, flapping, first_evidence, latest_evidence)
			VALUES (?, 'uuid:GPU-1', 'occupancy', 'warn', 'foreign_processes', ?, ?, 1, false, '', '')`, []any{node.ID, started, started}},
	} {
		if err := s.db.Exec(stmt.sql, stmt.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func storedCheckCodes(t *testing.T, s *Store) (cards, records []string) {
	t.Helper()
	if err := s.db.Raw(`SELECT health_code FROM gpus WHERE uuid LIKE 'GPU-%' ORDER BY "index"`).Scan(&cards).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Raw(`SELECT reason FROM card_health_episodes ORDER BY card_ident`).Scan(&records).Error; err != nil {
		t.Fatal(err)
	}
	return cards, records
}

func TestCheckCodesMigrationUpperCasesStoredCodesOnceAndIdempotently(t *testing.T) {
	s := legacyCheckCodes(t)
	m := migrationByVersion(t, 217)
	if err := migrateDB(s.db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	cards, records := storedCheckCodes(t, s)
	if want := []string{"RUNTIME_UNUSABLE", "PCIE_LINK_DEGRADED", ""}; !slices.Equal(cards, want) {
		t.Fatalf("gpus.health_code = %q, want %q", cards, want)
	}
	if want := []string{"RUNTIME_UNUSABLE", "FOREIGN_PROCESSES"}; !slices.Equal(records, want) {
		t.Fatalf("card_health_episodes.reason = %q, want %q", records, want)
	}
	// The readers return what is stored: no reader normalizes.
	node, err := s.NodeWithGPUs("n-codes")
	if err != nil || node.GPUs[0].HealthCode != "RUNTIME_UNUSABLE" {
		t.Fatalf("card = %+v, %v", node.GPUs, err)
	}
	open, err := s.OpenCardHealthEpisodes()
	if err != nil || len(open) != 2 || open[0].Reason != "RUNTIME_UNUSABLE" || open[1].Reason != "FOREIGN_PROCESSES" {
		t.Fatalf("open = %+v, %v", open, err)
	}
	var marker SchemaMigration
	if err := s.db.First(&marker, 217).Error; err != nil || marker.Name != "check_codes_upper_snake" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	// Run again: nothing changes.
	if err := s.db.Exec(`DELETE FROM schema_migrations WHERE version = 217`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(s.db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(s.db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	again, againRecords := storedCheckCodes(t, s)
	if !slices.Equal(again, cards) || !slices.Equal(againRecords, records) {
		t.Fatalf("rerun changed codes: %q %q", again, againRecords)
	}
}

// A failure partway through, after the cards were rewritten or at the marker,
// rolls back the codes and the marker together; the migration then recovers.
func TestCheckCodesMigrationRollsBackCodesAndMarkerTogether(t *testing.T) {
	for _, stage := range []string{"records", "marker"} {
		t.Run(stage, func(t *testing.T) {
			s := legacyCheckCodes(t)
			trigger := `CREATE TRIGGER fail_check_codes BEFORE UPDATE ON card_health_episodes BEGIN SELECT RAISE(ABORT, 'injected record failure'); END`
			if stage == "marker" {
				trigger = `CREATE TRIGGER fail_check_codes BEFORE INSERT ON schema_migrations WHEN NEW.version = 217 BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`
			}
			if err := s.db.Exec(trigger).Error; err != nil {
				t.Fatal(err)
			}
			m := migrationByVersion(t, 217)
			if err := migrateDB(s.db, []migration{m}); err == nil {
				t.Fatal("injected failure did not abort the migration")
			}
			cards, records := storedCheckCodes(t, s)
			if !slices.Equal(cards, []string{"runtime_unusable", "PCIE_LINK_DEGRADED", ""}) || !slices.Equal(records, []string{"runtime_unusable", "foreign_processes"}) {
				t.Fatalf("partial rewrite committed: %q %q", cards, records)
			}
			var count int64
			if err := s.db.Model(&SchemaMigration{}).Where("version = 217").Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("marker of a failed migration = %d, %v", count, err)
			}
			if err := s.db.Exec(`DROP TRIGGER fail_check_codes`).Error; err != nil {
				t.Fatal(err)
			}
			if err := migrateDB(s.db, []migration{m}); err != nil {
				t.Fatalf("migration cannot recover: %v", err)
			}
			if cards, _ := storedCheckCodes(t, s); cards[0] != "RUNTIME_UNUSABLE" {
				t.Fatalf("after recovery = %q", cards)
			}
		})
	}
}
