package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

// Existing nodes gain the placement policy column switched off - the
// behaviour before the switch existed - and the migration is idempotent;
// schema and marker roll back together, and a database that rolled back
// starts again and migrates.
func TestNodePlacementPolicyMigration(t *testing.T) {
	s := newTestStore(t)
	if err := s.db.Create(&Node{UUID: "legacy"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := dropColumns(s.db, "nodes", "placement_share_with_foreign"); err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "node_placement_policy")
	if err := s.db.Exec("DELETE FROM schema_migrations WHERE version = ?", m.Version).Error; err != nil {
		t.Fatal(err)
	}

	boom := errors.New("after column")
	failed := m
	failed.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}
	if err := applyMigration(s.db, failed); !errors.Is(err, boom) {
		t.Fatalf("rollback = %v", err)
	}
	if s.db.Migrator().HasColumn("nodes", "placement_share_with_foreign") {
		t.Fatal("the failed transaction kept the column")
	}
	var count int64
	if err := s.db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("the failed transaction kept the marker: %d %v", count, err)
	}

	for range 2 {
		if err := migrateDB(s.db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	if !s.db.Migrator().HasColumn("nodes", "placement_share_with_foreign") {
		t.Fatal("nodes.placement_share_with_foreign missing")
	}
	var marker SchemaMigration
	if err := s.db.First(&marker, m.Version).Error; err != nil || marker.Name != "node_placement_policy" {
		t.Fatalf("marker = %+v %v", marker, err)
	}
	node, err := s.NodeByUUID("legacy")
	if err != nil || node.PlacementPolicy.ShareWithForeign {
		t.Fatalf("legacy node policy = %+v (%v), want off", node.PlacementPolicy, err)
	}
	var notNull int
	if err := s.db.Raw(`SELECT "notnull" FROM pragma_table_info('nodes') WHERE name = 'placement_share_with_foreign'`).Scan(&notNull).Error; err != nil || notNull != 1 {
		t.Fatalf("placement_share_with_foreign notnull = %d (%v), want NOT NULL", notNull, err)
	}
}
