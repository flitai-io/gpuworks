package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestKubeClustersMigrationAddsTableAndNodeColumns(t *testing.T) {
	db := nodesOnlyDB(t, "install_dir text")
	if err := db.Exec(`INSERT INTO nodes (id, uuid) VALUES (1, 'n-1')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "kube_clusters")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("kube_clusters") || !db.Migrator().HasColumn("nodes", "kube_cluster_id") ||
		!db.Migrator().HasColumn("nodes", "kube_node_name") || !db.Migrator().HasIndex(&Node{}, "idx_node_kube") {
		t.Fatal("kube_clusters schema incomplete after the migration")
	}
	var row struct {
		KubeClusterID int
		KubeNodeName  string
	}
	if err := db.Raw(`SELECT COALESCE(kube_cluster_id,0) AS kube_cluster_id, COALESCE(kube_node_name,'') AS kube_node_name FROM nodes WHERE id = 1`).Scan(&row).Error; err != nil || row.KubeClusterID != 0 || row.KubeNodeName != "" {
		t.Fatalf("existing node after the migration: %+v, %v", row, err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
}

func TestKubeClustersMigrationRollsBackAndCanRetry(t *testing.T) {
	db := nodesOnlyDB(t, "")
	m := migrationByName(t, "kube_clusters")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_kube_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasTable("kube_clusters") || db.Migrator().HasColumn("nodes", "kube_cluster_id") {
		t.Fatal("schema survived a failed migration")
	}
	if err := db.Exec(`DROP TRIGGER fail_kube_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func openKubeStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestEnrollKubeNodeIsIdempotentAndBindsTheCluster(t *testing.T) {
	st := openKubeStore(t)
	c, err := st.CreateKubeCluster(KubeCluster{Name: "prod", Namespace: "gpuworks-system", IdleReturnSeconds: KubeDefaultIdleReturnSeconds})
	if err != nil {
		t.Fatal(err)
	}
	if c.Token == "" || c.TokenHash != HashToken(c.Token) || c.UUID == "" {
		t.Fatalf("created cluster = %+v", c)
	}
	n1, got, err := st.EnrollKubeNode(c.Token, "uid-a", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ClusterUID != "uid-a" || n1.KubeClusterID != c.ID || n1.KubeNodeName != "gpu-1" || n1.Name != "gpu-1" || n1.State != NodeStateConnecting {
		t.Fatalf("first enrollment: node=%+v cluster=%+v", n1, got)
	}
	again, _, err := st.EnrollKubeNode(c.Token, "uid-a", "gpu-1")
	if err != nil || again.UUID != n1.UUID || again.Token != n1.Token {
		t.Fatalf("re-enrollment = %+v, %v; want the same node and token", again, err)
	}
	n2, _, err := st.EnrollKubeNode(c.Token, "uid-a", "gpu-2")
	if err != nil || n2.UUID == n1.UUID {
		t.Fatalf("second node = %+v, %v", n2, err)
	}
	if _, _, err := st.EnrollKubeNode(c.Token, "uid-b", "gpu-3"); !errors.Is(err, ErrKubeClusterMismatch) {
		t.Fatalf("other cluster: err=%v, want ErrKubeClusterMismatch", err)
	}
	if _, _, err := st.EnrollKubeNode("wrong", "uid-a", "gpu-3"); !errors.Is(err, ErrKubeTokenUnknown) {
		t.Fatalf("wrong token: err=%v", err)
	}
	if _, _, err := st.EnrollKubeNode(c.Token, "", "gpu-3"); err == nil {
		t.Fatal("empty cluster uid accepted")
	}
	if err := st.DB().Model(&Node{}).Where("kube_cluster_id = ? AND kube_node_name = ?", c.ID, "gpu-1").Update("gpu_vendor", "nvidia").Error; err != nil {
		t.Fatal(err)
	}
	byVendor, err := st.KubeClusterNodeVendors()
	if err != nil || byVendor[c.ID]["nvidia"] != 1 || byVendor[c.ID][""] != 1 || len(byVendor[c.ID]) != 2 {
		t.Fatalf("nodes by vendor = %v, %v", byVendor, err)
	}
	if err := st.DeleteKubeCluster(c.UUID); !errors.Is(err, ErrKubeClusterHasNodes) {
		t.Fatalf("delete with nodes: %v", err)
	}
	if err := st.RevokeKubeCluster(c.UUID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnrollKubeNode(c.Token, "uid-a", "gpu-1"); !errors.Is(err, ErrKubeTokenUnknown) {
		t.Fatalf("revoked token: err=%v", err)
	}
	if err := st.RevokeKubeCluster("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke missing: %v", err)
	}
}

func TestDeleteKubeClusterWithoutNodes(t *testing.T) {
	st := openKubeStore(t)
	c, err := st.CreateKubeCluster(KubeCluster{Name: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteKubeCluster(c.UUID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.KubeClusterByUUID(c.UUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := st.DeleteKubeCluster(c.UUID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

// A node cordoned in Kubernetes is cordoned here too, and comes back when
// Kubernetes lets it; a node the operator cordoned for another reason is
// not touched by a Kubernetes node that was never cordoned.
func TestAKubernetesCordonIsFollowed(t *testing.T) {
	s := openKubeStore(t)
	if _, err := s.RegisterNode("uuid-kube", HashToken("t"), "kube-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	report := func(unschedulable bool) Node {
		t.Helper()
		if err := s.UpdateNodeStatus("uuid-kube", proto.NodeStatus{GPUProbeOK: &ok,
			Sys: proto.SysInfo{RuntimeDriver: "kube", KubeUnschedulable: unschedulable}}); err != nil {
			t.Fatal(err)
		}
		n, err := s.NodeByUUID("uuid-kube")
		if err != nil {
			t.Fatal(err)
		}
		return *n
	}
	if n := report(false); n.Scheduling != NodeSchedulable || n.KubeUnschedulable {
		t.Fatalf("uncordoned node = %q %v", n.Scheduling, n.KubeUnschedulable)
	}
	if n := report(true); n.Scheduling != NodeCordoned || !n.KubeUnschedulable || n.SchedulingBy != SchedulingByPlatform || n.CordonReason() != CordonKubeUnschedulable {
		t.Fatalf("after kubectl cordon = %q %v by %q for %q", n.Scheduling, n.KubeUnschedulable, n.SchedulingBy, n.CordonReason())
	}
	if n := report(true); n.Scheduling != NodeCordoned {
		t.Fatalf("still cordoned = %q", n.Scheduling)
	}
	if n := report(false); n.Scheduling != NodeSchedulable || n.KubeUnschedulable {
		t.Fatalf("after kubectl uncordon = %q %v", n.Scheduling, n.KubeUnschedulable)
	}
	// An operator's own cordon stays, also when Kubernetes cordons and
	// uncordons the node under it.
	if err := s.DB().Model(&Node{}).Where("uuid = ?", "uuid-kube").Updates(map[string]any{
		"scheduling": NodeCordoned, "scheduling_by": SchedulingByOperator}).Error; err != nil {
		t.Fatal(err)
	}
	if n := report(false); n.Scheduling != NodeCordoned {
		t.Fatalf("operator cordon lifted = %q", n.Scheduling)
	}
	report(true)
	if n := report(false); n.Scheduling != NodeCordoned || n.SchedulingBy != SchedulingByOperator {
		t.Fatalf("operator cordon after a Kubernetes cordon and uncordon = %q by %q", n.Scheduling, n.SchedulingBy)
	}
}

// kubeClustersV165 is the kube_clusters table as migration 165 created it,
// before the image pull secret.
const kubeClustersV165 = kubeClustersV165Body + ")"

const kubeClustersV165Body = `CREATE TABLE kube_clusters (id integer PRIMARY KEY, uuid text, name text, token text, token_hash text,
	cluster_uid text, namespace text, server_url text, agent_image text, vendor text, node_selector text, runtime_class text,
	containerd_socket text, state_dir text, api_server text, engine_ports text, idle_return_seconds integer,
	revoked_at datetime, created_at datetime, updated_at datetime`

func TestKubeClusterImagePullSecretMigrationKeepsRows(t *testing.T) {
	db := nodesOnlyDB(t, "")
	if err := db.Exec(kubeClustersV165).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO kube_clusters (id, uuid, name, runtime_class) VALUES (1, 'c-1', 'old', 'nvidia')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "kube_cluster_image_pull_secret")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var row struct {
		Name, RuntimeClass, ImagePullSecret string
	}
	if err := db.Raw(`SELECT name, runtime_class, COALESCE(image_pull_secret,'') AS image_pull_secret FROM kube_clusters WHERE id = 1`).Scan(&row).Error; err != nil ||
		row.Name != "old" || row.RuntimeClass != "nvidia" || row.ImagePullSecret != "" {
		t.Fatalf("existing cluster after the migration: %+v, %v", row, err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
}

func TestKubeClusterImagePullSecretMigrationRollsBack(t *testing.T) {
	db := nodesOnlyDB(t, "")
	if err := db.Exec(kubeClustersV165).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByName(t, "kube_cluster_image_pull_secret")
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_pull_secret_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err == nil {
		t.Fatal("injected marker failure did not fail the migration")
	}
	if db.Migrator().HasColumn("kube_clusters", "image_pull_secret") {
		t.Fatal("column survived a failed migration")
	}
	if err := db.Exec(`DROP TRIGGER fail_pull_secret_marker`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil || !db.Migrator().HasColumn("kube_clusters", "image_pull_secret") {
		t.Fatalf("retry: %v", err)
	}
}

func TestKubeClusterRefsReadsOnlyTheRequestedIdentities(t *testing.T) {
	st := openKubeStore(t)
	a, err := st.CreateKubeCluster(KubeCluster{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateKubeCluster(KubeCluster{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.KubeClusterRefs([]uint{a.ID, 9999})
	if err != nil || len(got) != 1 || got[a.ID].UUID != a.UUID || got[a.ID].Name != "a" {
		t.Fatalf("refs = %+v, %v; want cluster a alone (a missing id is absent, b not asked for)", got, err)
	}
	if got[a.ID].Token != "" || got[a.ID].TokenHash != "" {
		t.Fatalf("refs loaded the enrollment token: %+v", got[a.ID])
	}
	if _, ok := got[b.ID]; ok {
		t.Fatal("cluster b returned unasked")
	}
	if got, err := st.KubeClusterRefs(nil); err != nil || len(got) != 0 {
		t.Fatalf("refs of no ids = %+v, %v", got, err)
	}
}

// A Kubernetes node another entry of the same cluster has, online or seen
// lately, is refused to a second entry (both agents would share its state
// directory); once its holder has been gone long enough it can move. Another
// cluster's node of the same name is a different node.
func TestAKubeNodeHeldByAnotherEntryIsRefused(t *testing.T) {
	st := openKubeStore(t)
	a, err := st.CreateKubeCluster(KubeCluster{Name: "a", Namespace: "gw-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateKubeCluster(KubeCluster{Name: "b", Namespace: "gw-b"})
	if err != nil {
		t.Fatal(err)
	}
	held, _, err := st.EnrollKubeNode(a.Token, "uid-1", "gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.EnrollKubeNode(b.Token, "uid-1", "gpu-1")
	var taken *KubeNodeTakenError
	if !errors.As(err, &taken) || taken.Entry != "a" || !errors.Is(err, ErrKubeNodeTaken) {
		t.Fatalf("second entry enrolling a held node: %v", err)
	}
	// Another node of b's is fine.
	if _, _, err := st.EnrollKubeNode(b.Token, "uid-1", "gpu-2"); err != nil {
		t.Fatalf("a free node: %v", err)
	}
	// The holder went offline a while ago: the node can move.
	old := time.Now().Add(-KubeNodeTakenFor - time.Minute)
	if err := st.DB().Model(&Node{}).Where("id = ?", held.ID).Updates(map[string]any{"state": NodeStateOffline, "last_seen_at": old}).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnrollKubeNode(b.Token, "uid-1", "gpu-1"); err != nil {
		t.Fatalf("a node whose holder is long gone: %v", err)
	}
	// Now b holds it: a's stale row for the node does not let a take it back.
	if _, _, err := st.EnrollKubeNode(a.Token, "uid-1", "gpu-1"); !errors.Is(err, ErrKubeNodeTaken) {
		t.Fatalf("the former holder re-enrolling a node another entry holds: %v", err)
	}
	// Recently seen though offline still holds.
	c, err := st.CreateKubeCluster(KubeCluster{Name: "c", Namespace: "gw-c"})
	if err != nil {
		t.Fatal(err)
	}
	recent := time.Now().Add(-time.Minute)
	if err := st.DB().Model(&Node{}).Where("kube_cluster_id = ? AND kube_node_name = ?", b.ID, "gpu-2").Updates(map[string]any{"state": NodeStateOffline, "last_seen_at": recent}).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EnrollKubeNode(c.Token, "uid-1", "gpu-2"); !errors.Is(err, ErrKubeNodeTaken) {
		t.Fatalf("a node seen a minute ago: %v", err)
	}
	// Another cluster's node of the same name is a different node.
	if _, _, err := st.EnrollKubeNode(c.Token, "uid-2", "gpu-1"); !errors.Is(err, ErrKubeClusterMismatch) && err != nil {
		t.Fatalf("other cluster: %v", err)
	}
}
