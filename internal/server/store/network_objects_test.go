package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestNetworkCRUDAndAttachments(t *testing.T) {
	s := newTestStore(t)
	node, err := s.RegisterNode("uuid-net", HashToken("tok"), "h20-01")
	if err != nil {
		t.Fatal(err)
	}
	ib, err := s.CreateNetwork(&Network{Name: "ib-a", Type: "rdma", Roles: EncodeStringList([]string{"compute"}), CIDR: "10.20.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNetwork(&Network{Name: "IB-A", Type: "ethernet"}); !errors.Is(err, ErrNetworkNameTaken) {
		t.Fatalf("duplicate name (case-insensitive) err = %v", err)
	}
	other, err := s.CreateNetwork(&Network{Name: "storage", Type: "ethernet", Roles: "[]"})
	if err != nil {
		t.Fatal(err)
	}
	other.Name = "ib-a"
	if _, err := s.UpdateNetwork(other); !errors.Is(err, ErrNetworkNameTaken) {
		t.Fatalf("rename onto existing err = %v", err)
	}
	ib.Description, ib.CIDR = "fabric A", "10.20.0.0/16"
	updated, err := s.UpdateNetwork(ib)
	if err != nil || updated.Description != "fabric A" || updated.CIDR != "10.20.0.0/16" || updated.Name != "ib-a" {
		t.Fatalf("update = %#v, %v", updated, err)
	}
	if _, err := s.UpdateNetwork(&Network{ID: 999, Name: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing err = %v", err)
	}

	byLabel, err := s.CreateNetworkAttachment(&NetworkAttachment{NetworkID: ib.ID, LabelKey: "pool", LabelValue: "h20-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNetworkAttachment(&NetworkAttachment{NetworkID: ib.ID, LabelKey: "pool", LabelValue: "h20-a"}); !errors.Is(err, ErrNetworkAttachmentExists) {
		t.Fatalf("duplicate label attachment err = %v", err)
	}
	byNode, err := s.CreateNetworkAttachment(&NetworkAttachment{NetworkID: ib.ID, NodeID: node.ID, HCAs: "[]"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateNetworkAttachment(&NetworkAttachment{NetworkID: ib.ID, NodeID: node.ID}); !errors.Is(err, ErrNetworkAttachmentExists) {
		t.Fatalf("duplicate node attachment err = %v", err)
	}
	if _, err := s.CreateNetworkAttachment(&NetworkAttachment{NetworkID: 999, NodeID: node.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("attach to missing network err = %v", err)
	}
	if _, err := s.CreateNetworkAttachment(&NetworkAttachment{NetworkID: other.ID, NodeID: node.ID}); err != nil {
		t.Fatal(err)
	}

	got, err := s.UpdateNetworkAttachmentOverrides(ib.ID, byNode.ID, "bond0", "10.20.1.11", []string{"mlx5_0", "mlx5_1"})
	if err != nil || got.Interface != "bond0" || got.IP != "10.20.1.11" || got.HCAs != `["mlx5_0","mlx5_1"]` {
		t.Fatalf("override = %#v, %v", got, err)
	}
	if _, err := s.UpdateNetworkAttachmentOverrides(other.ID, byNode.ID, "", "", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("override through the wrong network err = %v", err)
	}
	if err := s.DeleteNetworkAttachment(other.ID, byLabel.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("detach through the wrong network err = %v", err)
	}
	if err := s.DeleteNetworkAttachment(ib.ID, byLabel.ID); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListNetworkAttachments()
	if err != nil || len(all) != 2 || all[0].ID != byNode.ID {
		t.Fatalf("attachments = %#v, %v", all, err)
	}

	if err := s.DeleteNetwork(ib.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNetwork(ib.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	all, _ = s.ListNetworkAttachments()
	if len(all) != 1 || all[0].NetworkID != other.ID {
		t.Fatalf("attachments after network delete = %#v", all)
	}
	if err := s.DeleteNode(node.UUID); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ListNetworkAttachments()
	if len(all) != 0 {
		t.Fatalf("attachments after node delete = %#v", all)
	}
	networks, _ := s.ListNetworks()
	if len(networks) != 1 || networks[0].Name != "storage" {
		t.Fatalf("networks = %#v", networks)
	}
}

func TestUpdateNodeStatusKeepsInventoryFromOlderAgents(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-inv", HashToken("tok"), "h"); err != nil {
		t.Fatal(err)
	}
	inv := proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "eth0", State: "up", Addrs: []string{"10.0.0.2/24"}}},
		RDMAPorts:  []proto.RDMAPort{{Device: "mlx5_0", Port: 1, State: "ACTIVE", LinkLayer: "InfiniBand", RateGbps: 400}},
	}
	if err := s.UpdateNodeStatus("uuid-inv", proto.NodeStatus{Network: &inv}); err != nil {
		t.Fatal(err)
	}
	read := func() proto.NetInventory {
		t.Helper()
		n, err := s.NodeByUUID("uuid-inv")
		if err != nil {
			t.Fatal(err)
		}
		var got proto.NetInventory
		if err := json.Unmarshal([]byte(n.NetworkInventory), &got); err != nil {
			t.Fatalf("inventory %q: %v", n.NetworkInventory, err)
		}
		return got
	}
	if got := read(); len(got.Interfaces) != 1 || got.RDMAPorts[0].RateGbps != 400 {
		t.Fatalf("inventory = %#v", got)
	}
	// An agent without the field (nil) must not erase the last inventory.
	if err := s.UpdateNodeStatus("uuid-inv", proto.NodeStatus{}); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got.Interfaces) != 1 {
		t.Fatalf("inventory erased by old agent: %#v", got)
	}
	empty := proto.NetInventory{Interfaces: []proto.NetInterface{}, RDMAPorts: []proto.RDMAPort{}}
	if err := s.UpdateNodeStatus("uuid-inv", proto.NodeStatus{Network: &empty}); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got.Interfaces) != 0 || got.RDMAPorts == nil {
		t.Fatalf("empty inventory not stored: %#v", got)
	}
}

func TestNetworkObjectsMigrationAddsSchemaAndIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:network-objects-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Node{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE nodes DROP COLUMN network_inventory").Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 96)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Node{}, "NetworkInventory") || !db.Migrator().HasTable(&Network{}) || !db.Migrator().HasTable(&NetworkAttachment{}) {
		t.Fatal("network schema missing after migration")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 96).Error; err != nil || marker.Name != "network_objects" {
		t.Fatalf("migration marker = %+v, err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
}

func TestNetworkObjectsMigrationRollsBackWithItsMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:network-objects-migration-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 96)
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil {
		t.Fatal("migration succeeded despite injected failure")
	}
	if db.Migrator().HasTable(&Network{}) || db.Migrator().HasTable(&NetworkAttachment{}) {
		t.Fatal("network tables survived a rolled-back migration")
	}
	var count int64
	db.Model(&SchemaMigration{}).Where("version = ?", 96).Count(&count)
	if count != 0 {
		t.Fatal("marker recorded for a failed migration")
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
}

func TestServingForwardStatus(t *testing.T) {
	s := newTestStore(t)
	with, err := s.RegisterNode("with-token", HashToken("t1"), "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := NodeServingForward(""); ok {
		t.Fatal("empty status decoded")
	}
	first := ServingForwardStatus{Mode: ServingForwardUnreachable, Reason: "dial timeout", At: time.Unix(100, 0).UTC()}
	if err := s.SetNodeServingForward(with.UUID, first); err != nil {
		t.Fatal(err)
	}
	// Same mode and reason: the first timestamp stays.
	if err := s.SetNodeServingForward(with.UUID, ServingForwardStatus{Mode: ServingForwardUnreachable, Reason: "dial timeout", At: time.Unix(200, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	node, _ := s.NodeByUUID(with.UUID)
	if got, ok := NodeServingForward(node.ServingForward); !ok || !got.At.Equal(first.At) {
		t.Fatalf("status = %#v", got)
	}
	if err := s.SetNodeServingForward(with.UUID, ServingForwardStatus{Mode: ServingForwardDirect, At: time.Unix(300, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	node, _ = s.NodeByUUID(with.UUID)
	if got, _ := NodeServingForward(node.ServingForward); got.Mode != ServingForwardDirect || got.Reason != "" {
		t.Fatalf("status = %#v", got)
	}
	if err := s.SetNodeServingForward("missing", first); err == nil {
		t.Fatal("status for a missing node accepted")
	}
}

func TestServingForwardStatusMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:serving-forward-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Node{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE nodes DROP COLUMN serving_forward").Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 98)
	failing := migration{Version: 98, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil || db.Migrator().HasColumn(&Node{}, "ServingForward") {
		t.Fatalf("failed migration left the column (err=%v)", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil || !db.Migrator().HasColumn(&Node{}, "ServingForward") {
		t.Fatalf("migration err = %v", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
}

func TestNetworkTypeRDMAMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:network-type-rdma-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Network{}); err != nil {
		t.Fatal(err)
	}
	for i, typ := range []string{"roce", "ib", "ethernet"} {
		db.Create(&Network{Name: typ, Type: typ, Roles: "[]", ID: uint(i + 1)})
	}
	m := migrationByVersion(t, 99)
	failing := migration{Version: 99, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil {
		t.Fatal("injected failure did not fail the migration")
	}
	var unchanged int64
	db.Model(&Network{}).Where("type IN ?", []string{"roce", "ib"}).Count(&unchanged)
	if unchanged != 2 {
		t.Fatalf("rolled-back migration changed %d rows", 2-unchanged)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var types []string
	db.Model(&Network{}).Order("id").Pluck("type", &types)
	if !reflect.DeepEqual(types, []string{"rdma", "rdma", "ethernet"}) {
		t.Fatalf("types = %v", types)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
}

func TestPlaintextTokenIgnoresThePlaceholder(t *testing.T) {
	s := newTestStore(t)
	legacy, err := s.RegisterNode("legacy-node", HashToken("x"), "h")
	if err != nil {
		t.Fatal(err)
	}
	if token, ok := legacy.PlaintextToken(); ok || token != "" {
		t.Fatalf("placeholder read as a token: %q", token)
	}
	pending, plain, err := s.CreateNodePending("p")
	if err != nil {
		t.Fatal(err)
	}
	if token, ok := pending.PlaintextToken(); !ok || token != plain {
		t.Fatalf("token = %q, %v", token, ok)
	}
	if _, ok := (Node{}).PlaintextToken(); ok {
		t.Fatal("empty token accepted")
	}
}
