package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// card is one probed NVIDIA card at a device index.
func card(index int, bdf string) proto.GPUInfo {
	return proto.GPUInfo{
		Index: index, UUID: "GPU-" + bdf, Vendor: "nvidia", Name: "RTX 3090",
		PCIeBDF: bdf, MemoryTotalBytes: 24 << 30, Healthy: true,
	}
}

func instanceRow(t *testing.T, s *Store, uuid string, nodeID uint, indexes []int, idents []string) {
	t.Helper()
	inst := &Instance{UUID: uuid, NodeID: nodeID, State: proto.InstRunning, GPUIndexes: jsonOf(t, indexes)}
	if len(idents) > 0 {
		inst.GPUIdents = jsonOf(t, idents)
	}
	spec := proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", GPUIndexes: indexes, GPUIdents: idents}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	inst.DesiredStateJSON = string(raw)
	if err := s.db.Create(inst).Error; err != nil {
		t.Fatal(err)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func instanceByUUID(t *testing.T, s *Store, uuid string) Instance {
	t.Helper()
	var inst Instance
	if err := s.db.Where("uuid = ?", uuid).First(&inst).Error; err != nil {
		t.Fatal(err)
	}
	return inst
}

// A card that fails to initialize disappears from the vendor CLI and every
// later card moves down one index. An instance must follow its own card, in
// the column and in the desired state the agent is replayed, instead of
// staying on an index that now names a different card.
func TestInstancesFollowTheirCardsWhenAProbeRenumbersThem(t *testing.T) {
	s := newTestStore(t)
	node, err := s.RegisterNode("uuid-drift", HashToken("t"), "drift-host")
	if err != nil {
		t.Fatal(err)
	}
	ok := true
	all := []proto.GPUInfo{card(0, "0000:1a:00.0"), card(1, "0000:1c:00.0"), card(2, "0000:3f:00.0")}
	if err := s.UpdateNodeStatus("uuid-drift", proto.NodeStatus{GPUProbeOK: &ok, GPUs: all}); err != nil {
		t.Fatal(err)
	}
	instanceRow(t, s, "inst-on-3f", node.ID, []int{2}, []string{"pci:0000:3f:00.0"})
	instanceRow(t, s, "inst-unbound", node.ID, []int{1}, nil)

	// The card at index 0 fails to initialize: the CLI now lists two cards.
	survivors := []proto.GPUInfo{card(0, "0000:1c:00.0"), card(1, "0000:3f:00.0")}
	if err := s.UpdateNodeStatus("uuid-drift", proto.NodeStatus{GPUProbeOK: &ok, GPUs: survivors}); err != nil {
		t.Fatal(err)
	}

	moved := instanceByUUID(t, s, "inst-on-3f")
	if moved.GPUIndexes != "[1]" {
		t.Fatalf("bound instance column = %s, want [1] (its card moved from device 2 to 1)", moved.GPUIndexes)
	}
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(moved.DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.GPUIndexes) != 1 || spec.GPUIndexes[0] != 1 {
		t.Fatalf("replayed desired state = %v, want the new index; it must agree with the column", spec.GPUIndexes)
	}
	if len(spec.GPUIdents) != 1 || spec.GPUIdents[0] != "pci:0000:3f:00.0" {
		t.Fatalf("rewriting the indexes dropped the binding: %v", spec.GPUIdents)
	}
	if unbound := instanceByUUID(t, s, "inst-unbound"); unbound.GPUIndexes != "[1]" {
		t.Fatalf("instance without a binding = %s, want its index untouched", unbound.GPUIndexes)
	}
}

// A card that is simply missing from this probe may come back. Pointing the
// instance somewhere else, or at nothing, is worse than leaving it where its
// card last was.
func TestInstanceKeepsItsIndexesWhenItsCardIsMissing(t *testing.T) {
	s := newTestStore(t)
	node, err := s.RegisterNode("uuid-missing", HashToken("t"), "missing-host")
	if err != nil {
		t.Fatal(err)
	}
	ok := true
	both := []proto.GPUInfo{card(0, "0000:1a:00.0"), card(1, "0000:1c:00.0")}
	if err := s.UpdateNodeStatus("uuid-missing", proto.NodeStatus{GPUProbeOK: &ok, GPUs: both}); err != nil {
		t.Fatal(err)
	}
	instanceRow(t, s, "inst-gone", node.ID, []int{1}, []string{"pci:0000:1c:00.0"})

	// 1a is gone and 1c keeps index 1 only because nothing else moved; make
	// the remaining card renumber so realignment actually runs.
	if err := s.UpdateNodeStatus("uuid-missing", proto.NodeStatus{GPUProbeOK: &ok, GPUs: []proto.GPUInfo{card(0, "0000:1a:00.0"), card(1, "0000:5e:00.0"), card(2, "0000:1a:00.0")}}); err != nil {
		t.Fatal(err)
	}
	if got := instanceByUUID(t, s, "inst-gone"); got.GPUIndexes != "[1]" {
		t.Fatalf("instance whose card is absent = %s, want [1] unchanged", got.GPUIndexes)
	}
}

func TestCardIdentsForNeedsEveryCardIdentified(t *testing.T) {
	node := &Node{GPUs: []GPU{
		{Index: 0, UUID: "GPU-a", PCIeBDF: "0000:1a:00.0"},
		{Index: 3, UUID: "ascend-3"},
		{Index: 4},
	}}
	if got := CardIdentsFor(node, []int{0, 3}); len(got) != 2 || got[0] != "pci:0000:1a:00.0" || got[1] != "uuid:ascend-3" {
		t.Fatalf("idents = %v", got)
	}
	if got := CardIdentsFor(node, []int{0, 4}); got != nil {
		t.Fatalf("a selection with an unidentified card = %v, want nil so the caller keeps indexes", got)
	}
	if got := CardIdentsFor(node, []int{0, 9}); got != nil {
		t.Fatalf("a selection naming a card the node lacks = %v, want nil", got)
	}
	if got := CardIdentsFor(nil, []int{0}); got != nil {
		t.Fatalf("idents without a node = %v", got)
	}
}

func TestInstanceGPUIdentsMigrationBackfillsFromTheCurrentCards(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:instance-gpu-idents-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Node{}, &GPU{}, &Instance{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE instances DROP COLUMN gpu_idents").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO gpus (node_id, `index`, uuid, pc_ie_bdf) VALUES (7,0,'GPU-a','0000:1a:00.0'), (7,2,'GPU-b','0000:3f:00.0'), (7,5,'ascend-5','')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO instances (uuid, node_id, state, gp_uindexes) VALUES ('i-two',7,'running','[0,2]'), ('i-synthetic',7,'running','[5]'), ('i-unknown-card',7,'running','[9]'), ('i-none',7,'running','')").Error; err != nil {
		t.Fatal(err)
	}

	m := migrationByName(t, "instance_gpu_idents")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil || db.Migrator().HasColumn(&Instance{}, "GPUIdents") {
		t.Fatalf("failed migration left the column (err=%v)", err)
	}
	var markers int64
	db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers)
	if markers != 0 {
		t.Fatal("failed migration left its marker")
	}

	if err := migrateDB(db, []migration{m}); err != nil || !db.Migrator().HasColumn(&Instance{}, "GPUIdents") {
		t.Fatalf("migration err = %v", err)
	}
	want := map[string]string{
		"i-two":          `["pci:0000:1a:00.0","pci:0000:3f:00.0"]`,
		"i-synthetic":    `["uuid:ascend-5"]`,
		"i-unknown-card": "",
		"i-none":         "",
	}
	for uuid, expect := range want {
		var got string
		if err := db.Raw("SELECT COALESCE(gpu_idents, '') FROM instances WHERE uuid = ?", uuid).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got != expect {
			t.Fatalf("%s gpu_idents = %q, want %q", uuid, got, expect)
		}
	}
	if err := db.Delete(&SchemaMigration{}, m.Version).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun err = %v", err)
	}
}
