package store

import (
	"fmt"
	"testing"

	"gorm.io/gorm"
)

func imageSourceDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := nodesOnlyDB(t, "")
	for _, ddl := range []string{kubeClustersV204,
		`INSERT INTO kube_clusters (id, uuid, name, agent_image) VALUES (1, 'c-1', 'default-image', '')`,
		`INSERT INTO kube_clusters (id, uuid, name, agent_image) VALUES (2, 'c-2', 'own-image', 'registry.example/agent:r1')`} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestAgentImageSourceMigrationKeepsEntriesOnDockerHub(t *testing.T) {
	db := imageSourceDB(t)
	m := migrationByName(t, "kube_cluster_agent_image_source")
	if m.Version != 206 {
		t.Fatalf("migration version = %d, want 206", m.Version)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID                           uint
		AgentImage, AgentImageSource string
	}
	if err := db.Raw(`SELECT id, COALESCE(agent_image,'') AS agent_image, COALESCE(agent_image_source,'') AS agent_image_source FROM kube_clusters ORDER BY id`).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].AgentImageSource != "" || rows[1].AgentImageSource != "" || rows[1].AgentImage != "registry.example/agent:r1" {
		t.Fatalf("entries after the migration: %+v", rows)
	}
	var marker struct{ Name string }
	if err := db.Raw(`SELECT name FROM schema_migrations WHERE version = 206`).Scan(&marker).Error; err != nil || marker.Name != "kube_cluster_agent_image_source" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("migrating again: %v", err)
	}
}

func TestAgentImageSourceMigrationRollsBack(t *testing.T) {
	db := imageSourceDB(t)
	m := migrationByName(t, "kube_cluster_agent_image_source")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_image_source_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("kube_clusters", "agent_image_source") {
		t.Fatal("column survived a failed migration")
	}
	if err := db.Exec(`DROP TRIGGER fail_image_source_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !db.Migrator().HasColumn("kube_clusters", "agent_image_source") {
		t.Fatal("column missing after the retry")
	}
}

func TestAnEntryKeepsItsAgentImageSource(t *testing.T) {
	st := openKubeStore(t)
	c, err := st.CreateKubeCluster(KubeCluster{Name: "ack", AgentImageSource: "swr"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.KubeClusterByUUID(c.UUID)
	if err != nil || got.AgentImageSource != "swr" {
		t.Fatalf("stored entry = %+v, %v", got, err)
	}
}

// kube_clusters as migrations 202 to 206 left it.
const kubeClustersV206 = kubeClustersV166Body + `, provider_id integer, provider_cluster_id text, agent_image_source text)`

func priorityDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := nodesOnlyDB(t, "")
	for _, ddl := range []string{kubeClustersV206,
		`INSERT INTO kube_clusters (id, uuid, name) VALUES (1, 'c-1', 'before')`} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestPriorityMigrationGivesEntriesTheDefaultAndNoPreemption(t *testing.T) {
	db := priorityDB(t)
	m := migrationByName(t, "kube_cluster_priority")
	if m.Version != 209 {
		t.Fatalf("migration version = %d, want 209", m.Version)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var row struct {
		Priority        int
		AllowPreemption bool
	}
	if err := db.Raw(`SELECT priority, allow_preemption FROM kube_clusters WHERE id = 1`).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Priority != KubeDefaultPriority || row.AllowPreemption {
		t.Fatalf("existing entry after the migration: %+v", row)
	}
	// Applying it again leaves a changed value alone.
	if err := db.Exec(`UPDATE kube_clusters SET priority = 5 WHERE id = 1`).Error; err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	if err := db.Raw(`SELECT priority, allow_preemption FROM kube_clusters WHERE id = 1`).Scan(&row).Error; err != nil || row.Priority != 5 {
		t.Fatalf("second application changed the entry: %+v %v", row, err)
	}
}

func TestPriorityMigrationRollsBack(t *testing.T) {
	db := priorityDB(t)
	m := migrationByName(t, "kube_cluster_priority")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_priority_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("kube_clusters", "priority") || db.Migrator().HasColumn("kube_clusters", "allow_preemption") {
		t.Fatal("columns survived a failed migration")
	}
	if err := db.Exec(`DROP TRIGGER fail_priority_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestDropVendorMigrationKeepsEntries(t *testing.T) {
	db := nodesOnlyDB(t, "")
	for _, ddl := range []string{kubeClustersV206,
		`ALTER TABLE kube_clusters ADD COLUMN priority integer`, `ALTER TABLE kube_clusters ADD COLUMN allow_preemption numeric`,
		`INSERT INTO kube_clusters (id, uuid, name, vendor, node_selector) VALUES (1, 'c-1', 'ascend', 'ascend', 'npu=yes')`} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	m := migrationByName(t, "kube_cluster_drop_vendor")
	if m.Version != 210 {
		t.Fatalf("migration version = %d, want 210", m.Version)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasColumn("kube_clusters", "vendor") {
		t.Fatal("vendor column survived")
	}
	var row struct{ Name, NodeSelector string }
	if err := db.Raw(`SELECT name, node_selector FROM kube_clusters WHERE id = 1`).Scan(&row).Error; err != nil || row.Name != "ascend" || row.NodeSelector != "npu=yes" {
		t.Fatalf("entry after the migration: %+v %v", row, err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
}

func TestDropVendorMigrationRollsBack(t *testing.T) {
	db := nodesOnlyDB(t, "")
	if err := db.Exec(kubeClustersV206).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "kube_cluster_drop_vendor")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_drop_vendor BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if !db.Migrator().HasColumn("kube_clusters", "vendor") {
		t.Fatal("the column was dropped by a failed migration")
	}
}
