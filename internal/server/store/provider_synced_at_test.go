package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// providersBeforeSyncedAt is providers before SyncedAt.
const providersBeforeSyncedAt = `CREATE TABLE providers (id integer PRIMARY KEY, name text, kind text, creds_cipher text, state text,
	created_at datetime, updated_at datetime)`

func providersDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := nodesOnlyDB(t, "")
	for _, ddl := range []string{providersBeforeSyncedAt,
		`INSERT INTO providers (id, name, kind, state) VALUES (1, 'ali', 'aliyun', 'valid')`} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestProviderSyncedAtMigrationKeepsAccountsUnsynced(t *testing.T) {
	db := providersDB(t)
	m := migrationByName(t, "provider_synced_at")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var p struct {
		Name     string
		SyncedAt *time.Time
	}
	if err := db.Raw(`SELECT name, synced_at FROM providers WHERE id = 1`).Scan(&p).Error; err != nil || p.Name != "ali" || p.SyncedAt != nil {
		t.Fatalf("existing account after the migration: %+v, %v", p, err)
	}
	var marker int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&marker).Error; err != nil || marker != 1 {
		t.Fatalf("marker = %d, %v", marker, err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
}

func TestProviderSyncedAtMigrationRollsBack(t *testing.T) {
	db := providersDB(t)
	m := migrationByName(t, "provider_synced_at")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_synced_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("providers", "synced_at") {
		t.Fatal("the column survived a failed migration")
	}
	if err := db.Exec(`DROP TRIGGER fail_synced_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !db.Migrator().HasColumn("providers", "synced_at") {
		t.Fatal("retry did not add the column")
	}
}

// A successful listing records its time on the account, an edit does not, and
// a sync that fails to write leaves the time as it was.
func TestASyncRecordsWhenTheAccountWasListed(t *testing.T) {
	st := openKubeStore(t)
	p, err := st.CreateProvider(&Provider{Name: "ali", Kind: "aliyun", CredsCipher: "c", State: "valid"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetProvider(p.ID); got.SyncedAt != nil {
		t.Fatalf("a new account has a sync time: %v", got.SyncedAt)
	}
	before := time.Now().UTC().Add(-time.Second)
	if err := st.SyncProviderInstances(p.ID, []ProviderInstance{{ExternalID: "i-1", Name: "a"}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetProvider(p.ID)
	if err != nil || got.SyncedAt == nil || got.SyncedAt.Before(before) {
		t.Fatalf("after a sync: %+v, %v", got.SyncedAt, err)
	}
	synced := *got.SyncedAt
	// An empty listing is a sync too: every instance is gone.
	time.Sleep(10 * time.Millisecond)
	if err := st.SyncProviderInstances(p.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetProvider(p.ID); got.SyncedAt == nil || !got.SyncedAt.After(synced) {
		t.Fatalf("an empty listing did not record its time: %v (was %v)", got.SyncedAt, synced)
	}
	synced = *got.SyncedAt
	if _, err := st.UpdateProvider(p.ID, "renamed", "c2", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetProvider(p.ID); got.SyncedAt == nil || !got.SyncedAt.Equal(synced) {
		t.Fatalf("an edit changed the sync time: %v (was %v)", got.SyncedAt, synced)
	}
	// A listing that cannot be written rolls the time back with it.
	if err := st.db.Exec(`CREATE TRIGGER fail_instance BEFORE INSERT ON provider_instances BEGIN SELECT RAISE(ABORT, 'injected'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.SyncProviderInstances(p.ID, []ProviderInstance{{ExternalID: "i-2", Name: "b"}}); err == nil {
		t.Fatal("the injected failure did not fail the sync")
	}
	if got, _ = st.GetProvider(p.ID); got.SyncedAt == nil || !got.SyncedAt.Equal(synced) {
		t.Fatalf("a failed sync changed the time: %v (was %v)", got.SyncedAt, synced)
	}
}

// The platform list counts every account's own instances, and those carrying a
// node, in one grouped query; a managed cluster's nodes are not instances.
func TestAccountInstancesLeaveOutManagedClusterNodes(t *testing.T) {
	st := openKubeStore(t)
	a, _ := st.CreateProvider(&Provider{Name: "a", Kind: "aliyun", CredsCipher: "c", State: "valid"})
	b, _ := st.CreateProvider(&Provider{Name: "b", Kind: "runpod", CredsCipher: "c", State: "valid"})
	empty, _ := st.CreateProvider(&Provider{Name: "empty", Kind: "aliyun", CredsCipher: "c", State: "valid"})
	for _, pi := range []ProviderInstance{
		{ProviderID: a.ID, ExternalID: "i-1", NodeID: 7},
		{ProviderID: a.ID, ExternalID: "i-2"},
		{ProviderID: a.ID, ExternalID: "i-3", ManagedCluster: "c-1", NodeID: 8},
		{ProviderID: b.ID, ExternalID: "p-1", NodeID: 9},
		{ProviderID: empty.ID, ExternalID: "n-1", ManagedCluster: "c-2"},
	} {
		pi := pi
		if err := st.UpsertProviderInstance(&pi); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.AccountInstanceCounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[a.ID] != 2 || got[b.ID] != 1 {
		t.Fatalf("counts = %v, want a:2 b:1 (an account of cluster nodes only is absent)", got)
	}
	// The list an account shows is the one it counts.
	for _, want := range []struct {
		id  uint
		ids []string
	}{{a.ID, []string{"i-1", "i-2"}}, {b.ID, []string{"p-1"}}, {empty.ID, nil}} {
		list, err := st.AccountInstances(want.id)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, pi := range list {
			ids = append(ids, pi.ExternalID)
		}
		if strings.Join(ids, ",") != strings.Join(want.ids, ",") || len(list) != got[want.id] {
			t.Fatalf("instances of %d = %v, want %v counted %d", want.id, ids, want.ids, got[want.id])
		}
	}
}

func TestNodeCountsByProviderCountWhatTheSourceFilterLists(t *testing.T) {
	st := openKubeStore(t)
	a, _ := st.CreateProvider(&Provider{Name: "a", Kind: "aliyun", CredsCipher: "c", State: "valid"})
	b, _ := st.CreateProvider(&Provider{Name: "b", Kind: "runpod", CredsCipher: "c", State: "valid"})
	cloud, err := st.CreateKubeCluster(KubeCluster{Name: "ack", Namespace: "gpuworks-system", ProviderID: &a.ID, ProviderClusterID: "c-1"})
	if err != nil {
		t.Fatal(err)
	}
	lab, err := st.CreateKubeCluster(KubeCluster{Name: "lab", Namespace: "gpuworks-lab"})
	if err != nil {
		t.Fatal(err)
	}
	vm, err := st.RegisterNode("vm-uuid", "hash", "vm")
	if err != nil {
		t.Fatal(err)
	}
	pod, err := st.RegisterNode("pod-uuid", "hash", "pod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("hand-uuid", "hash", "by-hand"); err != nil {
		t.Fatal(err)
	}
	ack1, _, err := st.EnrollKubeNode(cloud.Token, "uid-ack", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnrollKubeNode(cloud.Token, "uid-ack", "gpu-2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnrollKubeNode(lab.Token, "uid-lab", "gpu-1"); err != nil {
		t.Fatal(err)
	}
	for _, pi := range []ProviderInstance{
		{ProviderID: a.ID, ExternalID: "i-1", NodeID: vm.ID},
		// The cloud also lists the cluster's node as an instance of the
		// account: it counts once, under its cluster.
		{ProviderID: a.ID, ExternalID: "i-ack", ManagedCluster: "c-1", NodeID: ack1.ID},
		{ProviderID: b.ID, ExternalID: "p-1", NodeID: pod.ID},
	} {
		pi := pi
		if err := st.UpsertProviderInstance(&pi); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.NodeCountsByProvider()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[a.ID] != 3 || got[b.ID] != 1 {
		t.Fatalf("node counts = %v, want a:3 (one instance, two cluster nodes) b:1; the lab cluster and the node added by hand belong to no account", got)
	}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	sources, err := st.FleetNodeSources(nodes)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[uint]int{}
	for _, source := range sources {
		if source.ProviderID != 0 {
			listed[source.ProviderID]++
		}
	}
	if listed[a.ID] != got[a.ID] || listed[b.ID] != got[b.ID] {
		t.Fatalf("source filter lists %v, counts say %v", listed, got)
	}
}
