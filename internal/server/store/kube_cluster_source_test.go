package store

import (
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"
)

// kubeClustersV166 is kube_clusters as migrations 165 and 166 left it.
const kubeClustersV166 = kubeClustersV165Body + `, image_pull_secret text)`

// providerInstancesV166 is provider_instances before ManagedCluster.
const providerInstancesV166 = `CREATE TABLE provider_instances (id integer PRIMARY KEY, provider_id integer, external_id text, name text,
	gpu_model text, gpu_count integer, power text, price_per_hr real, region text, public_ip text, ssh_port integer, ssh_user text,
	ssh_connections text, kind text, node_id integer, created_at datetime, updated_at datetime)`

func sourcesDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := nodesOnlyDB(t, "")
	for _, ddl := range []string{kubeClustersV166, providerInstancesV166,
		`INSERT INTO kube_clusters (id, uuid, name, image_pull_secret) VALUES (1, 'c-1', 'by-hand', 'regcred')`,
		`INSERT INTO provider_instances (id, provider_id, external_id, name) VALUES (1, 3, 'i-1', 'vm')`} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestCloudKubernetesSourcesMigrationKeepsRows(t *testing.T) {
	db := sourcesDB(t)
	m := migrationByName(t, "cloud_kubernetes_sources")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var c struct {
		Name, ImagePullSecret, ProviderClusterID string
		ProviderID                               *uint
	}
	if err := db.Raw(`SELECT name, image_pull_secret, COALESCE(provider_cluster_id,'') AS provider_cluster_id, provider_id FROM kube_clusters WHERE id = 1`).Scan(&c).Error; err != nil ||
		c.Name != "by-hand" || c.ImagePullSecret != "regcred" || c.ProviderID != nil || c.ProviderClusterID != "" {
		t.Fatalf("existing cluster after the migration: %+v, %v", c, err)
	}
	var pi struct{ Name, ManagedCluster string }
	if err := db.Raw(`SELECT name, COALESCE(managed_cluster,'') AS managed_cluster FROM provider_instances WHERE id = 1`).Scan(&pi).Error; err != nil || pi.Name != "vm" || pi.ManagedCluster != "" {
		t.Fatalf("existing instance after the migration: %+v, %v", pi, err)
	}
	if !db.Migrator().HasIndex(&KubeCluster{}, "idx_kube_cluster_source") {
		t.Fatal("source index missing")
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
}

func TestCloudKubernetesSourcesMigrationRollsBack(t *testing.T) {
	db := sourcesDB(t)
	m := migrationByName(t, "cloud_kubernetes_sources")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_sources_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("kube_clusters", "provider_id") || db.Migrator().HasColumn("provider_instances", "managed_cluster") {
		t.Fatal("columns survived a failed migration")
	}
	if err := db.Exec(`DROP TRIGGER fail_sources_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestAClusterIsMadeOnceFromItsSourceAndOutlivesTheAccount(t *testing.T) {
	st := openKubeStore(t)
	p := Provider{Name: "ali", Kind: "aliyun", State: "valid"}
	if err := st.db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	src := KubeCluster{Name: "ack", Namespace: "gpuworks-system", ProviderID: &p.ID, ProviderClusterID: "c123"}
	if _, err := st.CreateKubeCluster(src); err != nil {
		t.Fatal(err)
	}
	again := src
	again.Name = "ack-2"
	if _, err := st.CreateKubeCluster(again); !errors.Is(err, ErrKubeClusterSourceTaken) {
		t.Fatalf("second entry from the same cluster: %v", err)
	}
	other := src
	other.Name, other.ProviderClusterID = "ack-other", "c456"
	if _, err := st.CreateKubeCluster(other); err != nil {
		t.Fatalf("another cluster of the account: %v", err)
	}
	if _, err := st.CreateKubeCluster(KubeCluster{Name: "by-hand", Namespace: "gpuworks-system"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateKubeCluster(KubeCluster{Name: "by-hand-2", Namespace: "gpuworks-system"}); err != nil {
		t.Fatalf("entries without a source never collide: %v", err)
	}
	from, err := st.KubeClustersFromProvider(p.ID)
	if err != nil || len(from) != 2 || from["c123"].Name != "ack" || from["c456"].Name != "ack-other" {
		t.Fatalf("KubeClustersFromProvider = %v, %v", from, err)
	}
	if err := st.DeleteProvider(p.ID); err != nil {
		t.Fatal(err)
	}
	rows, _ := st.KubeClusters()
	if len(rows) != 4 {
		t.Fatalf("clusters after deleting the account: %d", len(rows))
	}
	for _, c := range rows {
		if c.ProviderID != nil || c.ProviderClusterID != "" {
			t.Fatalf("cluster %s still names the deleted account: %+v", c.Name, c)
		}
	}
}
