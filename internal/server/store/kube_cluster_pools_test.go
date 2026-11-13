package store

import (
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"
)

// kubeClustersV204 is kube_clusters as migrations 202 to 204 left it.
const kubeClustersV204 = kubeClustersV166Body + `, provider_id integer, provider_cluster_id text)`

const kubeClustersV166Body = kubeClustersV165Body + `, image_pull_secret text`

func poolsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := nodesOnlyDB(t, "")
	stmts := []string{
		kubeClustersV204,
		`INSERT INTO kube_clusters (id, uuid, name, node_selector) VALUES (1, 'c-1', 'by-hand', 'gpu=a')`,
		`INSERT INTO kube_clusters (id, uuid, name, node_selector, provider_id, provider_cluster_id) VALUES (2, 'c-2', 'ack-one', 'alibabacloud.com/nodepool-id=np-1', 3, 'ack-1')`,
		`INSERT INTO kube_clusters (id, uuid, name, node_selector, provider_id, provider_cluster_id) VALUES (3, 'c-3', 'ack-edited', 'alibabacloud.com/nodepool-id=np-1,gpu=l20', 3, 'ack-2')`,
		`INSERT INTO kube_clusters (id, uuid, name, node_selector, provider_id, provider_cluster_id) VALUES (4, 'c-4', 'ack-all', '', 3, 'ack-3')`,
		`INSERT INTO kube_clusters (id, uuid, name, node_selector, provider_id, provider_cluster_id) VALUES (5, 'c-5', 'doks', 'doks.digitalocean.com/node-pool-id=3f8e-aa', 4, 'doks-1')`,
		// An entry whose cloud account was deleted keeps its selector: nothing says it names a pool.
		`INSERT INTO kube_clusters (id, uuid, name, node_selector, provider_id, provider_cluster_id) VALUES (6, 'c-6', 'orphan', 'alibabacloud.com/nodepool-id=np-9', NULL, '')`,
	}
	for _, ddl := range stmts {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func selectorOf(t *testing.T, db *gorm.DB, id uint) string {
	t.Helper()
	var s struct{ NodeSelector string }
	if err := db.Raw(`SELECT COALESCE(node_selector,'') AS node_selector FROM kube_clusters WHERE id = ?`, id).Scan(&s).Error; err != nil {
		t.Fatal(err)
	}
	return s.NodeSelector
}

func poolRows(t *testing.T, db *gorm.DB) map[uint][]string {
	t.Helper()
	var rows []KubeClusterPool
	if err := db.Order("kube_cluster_id, pool_id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := map[uint][]string{}
	for _, r := range rows {
		out[r.KubeClusterID] = append(out[r.KubeClusterID], r.LabelKey+"="+r.PoolID)
	}
	return out
}

func TestKubeClusterPoolsMigrationMovesSinglePoolSelectorsAndKeepsTheRest(t *testing.T) {
	db := poolsDB(t)
	m := migrationByName(t, "kube_cluster_pools")
	if m.Version != 205 {
		t.Fatalf("migration version = %d; kube's own migrations are 202 to 205 above master's 201", m.Version)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("kube_cluster_pools") || !db.Migrator().HasIndex(&KubeClusterPool{}, "idx_kube_cluster_pool") {
		t.Fatal("pool table or its unique index is missing")
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 205).First(&marker).Error; err != nil || marker.Name != "kube_cluster_pools" {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
	wantPools := map[uint][]string{
		2: {"alibabacloud.com/nodepool-id=np-1"},
		5: {"doks.digitalocean.com/node-pool-id=3f8e-aa"},
	}
	if got := poolRows(t, db); fmt.Sprint(got) != fmt.Sprint(wantPools) {
		t.Fatalf("pool rows = %v, want %v", got, wantPools)
	}
	for id, want := range map[uint]string{
		1: "gpu=a",                                     // by hand: its free-form selector
		2: "",                                          // moved into its pool row
		3: "alibabacloud.com/nodepool-id=np-1,gpu=l20", // more than one pair: not a pool name, left alone
		4: "",                                          // selected every node
		5: "",                                          // moved
		6: "alibabacloud.com/nodepool-id=np-9",         // no account any more
	} {
		if got := selectorOf(t, db, id); got != want {
			t.Errorf("entry %d selector = %q, want %q", id, got, want)
		}
	}

	// Applying it again changes nothing.
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	if got := poolRows(t, db); fmt.Sprint(got) != fmt.Sprint(wantPools) {
		t.Fatalf("pool rows after a second application = %v", got)
	}
	// And a retried one that finds the rows already made does not duplicate them.
	if err := db.Exec(`UPDATE kube_clusters SET node_selector = 'alibabacloud.com/nodepool-id=np-1' WHERE id = 2`).Error; err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("application over existing rows: %v", err)
	}
	if got := poolRows(t, db); fmt.Sprint(got) != fmt.Sprint(wantPools) || selectorOf(t, db, 2) != "" {
		t.Fatalf("pool rows after re-running over a restored selector = %v, selector %q", got, selectorOf(t, db, 2))
	}
}

func TestKubeClusterPoolsMigrationRollsBackWithItsMarker(t *testing.T) {
	db := poolsDB(t)
	m := migrationByName(t, "kube_cluster_pools")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_pools_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasTable("kube_cluster_pools") {
		t.Fatal("the pool table survived a failed migration")
	}
	if got := selectorOf(t, db, 2); got != "alibabacloud.com/nodepool-id=np-1" {
		t.Fatalf("a selector was moved although the migration failed: %q", got)
	}
	if err := db.Exec(`DROP TRIGGER fail_pools_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := poolRows(t, db); len(got) != 2 || selectorOf(t, db, 2) != "" {
		t.Fatalf("after the retry: pools %v, selector %q", got, selectorOf(t, db, 2))
	}
}

// A failure after some rows were moved leaves none of them moved: the data
// change and the schema change are one transaction.
func TestKubeClusterPoolsMigrationFailingMidwayLeavesTheDataAsItWas(t *testing.T) {
	db := poolsDB(t)
	m := migrationByName(t, "kube_cluster_pools")
	wrapped := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected after the data was moved")
	}}
	if err := migrateDB(db, []migration{wrapped}); err == nil {
		t.Fatal("the injected failure did not fail the migration")
	}
	if db.Migrator().HasTable("kube_cluster_pools") {
		t.Fatal("the pool table survived")
	}
	for id, want := range map[uint]string{2: "alibabacloud.com/nodepool-id=np-1", 5: "doks.digitalocean.com/node-pool-id=3f8e-aa"} {
		if got := selectorOf(t, db, id); got != want {
			t.Errorf("entry %d selector = %q after a failed migration, want %q", id, got, want)
		}
	}
}

func TestKubeClusterPoolsAreOwnedByTheirEntry(t *testing.T) {
	st := openKubeStore(t)
	const label = "alibabacloud.com/nodepool-id"
	c, err := st.CreateKubeCluster(KubeCluster{Name: "ack", Namespace: "gpuworks-system"},
		KubeClusterPool{PoolID: "np-b", LabelKey: label}, KubeClusterPool{PoolID: "np-a", LabelKey: label}, KubeClusterPool{PoolID: "np-a", LabelKey: label})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateKubeCluster(KubeCluster{Name: "other", Namespace: "gpuworks-system"}, KubeClusterPool{PoolID: "np-a", LabelKey: label})
	if err != nil {
		t.Fatalf("the same pool id in another entry: %v", err)
	}
	bare, err := st.CreateKubeCluster(KubeCluster{Name: "by-hand", Namespace: "gpuworks-system", NodeSelector: "gpu=a"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.KubeClusterPools(c.ID, other.ID, bare.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ids := poolIDs(got[c.ID]); fmt.Sprint(ids) != "[np-a np-b]" {
		t.Fatalf("pools of the entry = %v; ordered by id, once each", ids)
	}
	if len(got[other.ID]) != 1 || len(got[bare.ID]) != 0 {
		t.Fatalf("pools = %v", got)
	}
	if empty, err := st.KubeClusterPools(); err != nil || len(empty) != 0 {
		t.Fatalf("no ids = %v, %v", empty, err)
	}

	// Replacing the set clears the legacy selector, leaves other entries alone.
	if err := st.db.Model(&KubeCluster{}).Where("id = ?", c.ID).Update("node_selector", "gpu=legacy").Error; err != nil {
		t.Fatal(err)
	}
	updated, err := st.SetKubeClusterPools(c.UUID, []KubeClusterPool{{PoolID: "np-c", LabelKey: label}, {PoolID: "np-a", LabelKey: label}})
	if err != nil || updated.NodeSelector != "" {
		t.Fatalf("SetKubeClusterPools = %+v, %v", updated, err)
	}
	got, _ = st.KubeClusterPools(c.ID, other.ID)
	if fmt.Sprint(poolIDs(got[c.ID])) != "[np-a np-c]" || fmt.Sprint(poolIDs(got[other.ID])) != "[np-a]" {
		t.Fatalf("pools after replacing = %v", got)
	}
	reloaded, _ := st.KubeClusterByUUID(c.UUID)
	if reloaded.NodeSelector != "" {
		t.Fatalf("the legacy selector survived: %q", reloaded.NodeSelector)
	}
	if _, err := st.SetKubeClusterPools("nope", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown entry: %v", err)
	}

	// Pools of different clouds never share an entry; nothing is written.
	if _, err := st.SetKubeClusterPools(c.UUID, []KubeClusterPool{{PoolID: "x", LabelKey: label}, {PoolID: "y", LabelKey: "doks.digitalocean.com/node-pool-id"}}); !errors.Is(err, ErrKubeClusterPoolLabels) {
		t.Fatalf("mixed label keys: %v", err)
	}
	if _, err := st.CreateKubeCluster(KubeCluster{Name: "mixed"}, KubeClusterPool{PoolID: "x", LabelKey: "a/b"}, KubeClusterPool{PoolID: "y", LabelKey: "c/d"}); !errors.Is(err, ErrKubeClusterPoolLabels) {
		t.Fatalf("mixed label keys on create: %v", err)
	}
	if rows, _ := st.KubeClusters(); len(rows) != 3 {
		t.Fatalf("a refused create left %d entries", len(rows))
	}
	got, _ = st.KubeClusterPools(c.ID)
	if fmt.Sprint(poolIDs(got[c.ID])) != "[np-a np-c]" {
		t.Fatalf("a refused replacement changed the pools: %v", got)
	}

	// Deleting the entry takes its pools; the other entry's stay.
	if err := st.DeleteKubeCluster(c.UUID); err != nil {
		t.Fatal(err)
	}
	var left int64
	st.db.Model(&KubeClusterPool{}).Count(&left)
	if left != 1 {
		t.Fatalf("pool rows after deleting an entry = %d, want the other entry's one", left)
	}
}

func poolIDs(pools []KubeClusterPool) []string {
	out := make([]string, 0, len(pools))
	for _, p := range pools {
		out = append(out, p.PoolID)
	}
	return out
}
