package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// nodeCards reports a node's cards and returns them keyed by identity.
func nodeCards(t *testing.T, s *Store, uuid string, gpus []proto.GPUInfo) map[string]GPU {
	t.Helper()
	ok := true
	if err := s.UpdateNodeStatus(uuid, proto.NodeStatus{GPUProbeOK: &ok, GPUs: gpus}); err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeWithGPUs(uuid)
	if err != nil {
		t.Fatal(err)
	}
	byIdent := make(map[string]GPU, len(node.GPUs))
	for _, card := range node.GPUs {
		byIdent[card.Ident] = card
	}
	return byIdent
}

// Cards the vendor CLI does not list have no device index, so several of a
// node's cards carry -1 at once. Keying rows on the index could not store
// that; keying on identity can.
func TestCardsWithoutADeviceIndexAreStoredSideBySide(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-cards", HashToken("t"), "cards-host"); err != nil {
		t.Fatal(err)
	}
	cards := nodeCards(t, s, "uuid-cards", []proto.GPUInfo{
		{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true},
		{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
		{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1a:00.0", Ownership: accelerator.CardWithoutDriver},
		{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1c:00.0", HealthStatus: accelerator.HealthError, HealthCode: accelerator.HealthDriverInitFailed},
	})
	if len(cards) != 4 {
		t.Fatalf("stored %d cards, want 4: %+v", len(cards), cards)
	}
	if got := cards["pci:0000:1b:00.0"].Ownership; got != accelerator.CardPassedThrough {
		t.Fatalf("passed-through card ownership = %q", got)
	}
	if got := cards["pci:0000:1c:00.0"].HealthCode; got != accelerator.HealthDriverInitFailed {
		t.Fatalf("uninitialized card health = %q", got)
	}

	// Capacity counts only the node's own cards; the rest are hardware the
	// operator can see, not capacity anyone can use.
	node, err := s.NodeByUUID("uuid-cards")
	if err != nil {
		t.Fatal(err)
	}
	if node.GPUCount != 2 {
		t.Fatalf("gpu_count = %d, want 2 (the usable card and the one its driver failed)", node.GPUCount)
	}
	withGPUs, err := s.NodeWithGPUs("uuid-cards")
	if err != nil {
		t.Fatal(err)
	}
	if len(withGPUs.OwnedGPUs()) != 2 {
		t.Fatalf("owned cards = %+v, want the two bound to this node's driver", withGPUs.OwnedGPUs())
	}
}

// A card keeps its row when the CLI renumbers it, and loses it only when
// the card itself is gone.
func TestCardRowsFollowIdentityNotIndex(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-renumber", HashToken("t"), "renumber-host"); err != nil {
		t.Fatal(err)
	}
	first := nodeCards(t, s, "uuid-renumber", []proto.GPUInfo{
		{Index: 0, UUID: "GPU-1c", Vendor: "nvidia", PCIeBDF: "0000:1c:00.0", Healthy: true},
		{Index: 1, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true},
	})
	rowID := first["pci:0000:3f:00.0"].ID

	// 1c fails to initialize: 3f becomes device 0 and 1c keeps no index.
	second := nodeCards(t, s, "uuid-renumber", []proto.GPUInfo{
		{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true},
		{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1c:00.0", HealthStatus: accelerator.HealthError, HealthCode: accelerator.HealthDriverInitFailed},
	})
	if len(second) != 2 {
		t.Fatalf("cards = %+v, want both still stored", second)
	}
	if got := second["pci:0000:3f:00.0"]; got.ID != rowID || got.Index != 0 {
		t.Fatalf("the renumbered card = %+v, want row %d at index 0", got, rowID)
	}
	if got := second["pci:0000:1c:00.0"]; got.Index != -1 || got.UUID != "" {
		t.Fatalf("the card that lost its index = %+v", got)
	}

	// Physically removed: the row goes.
	third := nodeCards(t, s, "uuid-renumber", []proto.GPUInfo{
		{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true},
	})
	if len(third) != 1 || third["pci:0000:3f:00.0"].ID != rowID {
		t.Fatalf("cards = %+v, want only the one still present", third)
	}
}

// A prober that reports no identity at all behaves as it did before
// identities existed: its rows are keyed by device index.
func TestCardsWithoutAnIdentityAreKeyedByIndex(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-plain", HashToken("t"), "plain-host"); err != nil {
		t.Fatal(err)
	}
	cards := nodeCards(t, s, "uuid-plain", []proto.GPUInfo{
		{Index: 0, Vendor: "fake", Healthy: true},
		{Index: 1, Vendor: "fake", Healthy: true},
	})
	if len(cards) != 2 || cards["index:0"].Index != 0 || cards["index:1"].Index != 1 {
		t.Fatalf("cards = %+v, want them keyed by index", cards)
	}
}

func TestCardIdentityMigrationRekeysRowsAndDropsTheNodeList(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:card-identity-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Node{}, &GPU{}); err != nil {
		t.Fatal(err)
	}
	// The shape before this migration: no identity, a unique index on the
	// device index, and the node carrying the list of failed cards.
	for _, stmt := range []string{
		"DROP INDEX IF EXISTS idx_node_card",
		"ALTER TABLE gpus DROP COLUMN ident",
		"ALTER TABLE gpus DROP COLUMN ownership",
		"CREATE UNIQUE INDEX idx_node_gpu ON gpus(node_id, `index`)",
		"ALTER TABLE nodes ADD COLUMN gpu_init_failures varchar(1024)",
		"INSERT INTO nodes (uuid, name, state) VALUES ('n1','n1','online')",
		"INSERT INTO gpus (node_id, `index`, uuid, pc_ie_bdf) VALUES (1,0,'GPU-a','0000:3f:00.0'), (1,1,'ascend-1',''), (1,2,'','')",
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	m := migrationByName(t, "card_identity_and_ownership")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil || db.Migrator().HasColumn(&GPU{}, "Ident") {
		t.Fatalf("failed migration left the column (err=%v)", err)
	}

	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("migration err = %v", err)
	}
	for _, column := range []string{"Ident", "Ownership"} {
		if !db.Migrator().HasColumn(&GPU{}, column) {
			t.Fatalf("missing column %s", column)
		}
	}
	// Each card keyed by what identified it: its PCI address, else its
	// UUID, else its device index.
	want := map[int]string{0: "pci:0000:3f:00.0", 1: "uuid:ascend-1", 2: "index:2"}
	for index, ident := range want {
		var got string
		if err := db.Raw("SELECT ident FROM gpus WHERE `index` = ?", index).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got != ident {
			t.Fatalf("card at index %d = %q, want %q", index, got, ident)
		}
	}
	if db.Migrator().HasIndex(&GPU{}, "idx_node_gpu") {
		t.Fatal("the old unique index on the device index is still there; two cards without an index could not be stored")
	}
	if !db.Migrator().HasIndex(&GPU{}, "idx_node_card") {
		t.Fatal("missing the unique index on the identity")
	}
	if db.Migrator().HasColumn(&nodeGPUInitFailuresColumn{}, "gpu_init_failures") {
		t.Fatal("the node still carries its list of failed cards")
	}

	// Two cards of one node without a device index: what the old key made
	// impossible.
	if err := db.Exec("INSERT INTO gpus (node_id, ident, `index`) VALUES (1,'pci:0000:1a:00.0',-1), (1,'pci:0000:1b:00.0',-1)").Error; err != nil {
		t.Fatalf("storing two cards with no index: %v", err)
	}

	if err := db.Delete(&SchemaMigration{}, m.Version).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun err = %v", err)
	}
}

// Dropping a column must keep the table's indexes, and must actually drop
// the column. gorm's own migrator does neither on this driver: measured
// against nodes it returned no error, left the column in place, and took
// all seven named indexes with it. store.dropColumn uses SQLite's real
// DROP COLUMN instead and checks the indexes survived.
// A node's cards are listed in device order, with the ones the vendor CLI
// does not number after them. The order used to fall out of the unique
// index on the device index; when the key became the card's identity, the
// lists quietly re-sorted by PCI address, which is not the order anyone
// reads cards in.
func TestCardsAreListedInDeviceOrder(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-order", HashToken("t"), "order-host"); err != nil {
		t.Fatal(err)
	}
	// Reported out of order, and with PCI addresses whose alphabetical
	// order differs from the device order (3f is device 0, 1c is device 1).
	ok := true
	if err := s.UpdateNodeStatus("uuid-order", proto.NodeStatus{GPUProbeOK: &ok, GPUs: []proto.GPUInfo{
		{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1b:00.0", Ownership: accelerator.CardPassedThrough},
		{Index: 1, UUID: "GPU-1c", Vendor: "nvidia", PCIeBDF: "0000:1c:00.0", Healthy: true},
		{Index: -1, Vendor: "nvidia", PCIeBDF: "0000:1a:00.0", Ownership: accelerator.CardWithoutDriver},
		{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true},
	}}); err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeWithGPUs("uuid-order")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, card := range node.GPUs {
		got = append(got, card.Ident)
	}
	want := []string{
		"pci:0000:3f:00.0", // device 0
		"pci:0000:1c:00.0", // device 1
		"pci:0000:1a:00.0", // no index, by address
		"pci:0000:1b:00.0",
	}
	if len(got) != len(want) {
		t.Fatalf("cards = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("card %d = %s, want %s (full order %v)", i, got[i], want[i], got)
		}
	}
}

// A card somebody outside the platform is using takes no work: its memory
// is spoken for by something the platform cannot see or plan around, so a
// replica placed there would fail at startup against the platform's own
// accounting saying the card was free.
func TestACardInForeignUseTakesNoWork(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-foreign", HashToken("t"), "foreign-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	cards := []proto.GPUInfo{
		{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true},
		{Index: 1, UUID: "GPU-40", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", MemoryTotalBytes: 24 << 30, Healthy: true,
			ForeignProcesses: []proto.CardProcess{{PID: 4242, Name: "python3", MemoryBytes: 20 << 30}}},
	}
	if err := s.UpdateNodeStatus("uuid-foreign", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards}); err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeWithGPUs("uuid-foreign")
	if err != nil {
		t.Fatal(err)
	}
	byIdent := map[string]GPU{}
	for _, card := range node.GPUs {
		byIdent[card.Ident] = card
	}
	free, taken := byIdent["pci:0000:3f:00.0"], byIdent["pci:0000:40:00.0"]
	if !free.CanTakeWork() {
		t.Fatalf("the idle card cannot take work: %+v", free)
	}
	if taken.CanTakeWork() {
		t.Fatalf("a card held by a foreign process takes work: %+v", taken)
	}
	// It is not a fault: the card is healthy and nothing is wrong with it.
	if taken.HealthStatus != accelerator.HealthOK || taken.HealthCode != "" {
		t.Fatalf("a busy card was reported as faulty: %+v", taken)
	}
	if use := taken.ForeignUse(); len(use) != 1 || use[0].PID != 4242 || use[0].MemoryBytes != 20<<30 {
		t.Fatalf("who holds the card = %+v", use)
	}
	// The fleet query puts the same rule to the same facts.
	groups, err := s.OnlineGPUGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Cards != 1 || groups[0].Unusable != 1 {
		t.Fatalf("fleet groups = %+v, want the idle card usable and the held one not", groups)
	}
	if node.GPUCount != 2 {
		t.Fatalf("gpu_count = %d, want 2: a busy card is still the node's hardware", node.GPUCount)
	}
}

// Every status report must leave a card's normalized level and key right,
// not only the first. This was first written as a gorm BeforeSave hook,
// and gorm runs hooks on the statement's model: an update written as
// Model(&GPU{}).Updates(&row) ran it on that empty model, so the derived
// columns went out as zero values one report after a card was first seen.
func TestEveryReportLeavesTheDerivedColumnsRight(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-derived", HashToken("t"), "derived-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	card := proto.GPUInfo{Index: 0, UUID: "GPU-a", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}
	for round := 1; round <= 3; round++ {
		if err := s.UpdateNodeStatus("uuid-derived", proto.NodeStatus{GPUProbeOK: &ok, GPUs: []proto.GPUInfo{card}}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeWithGPUs("uuid-derived")
		if err != nil {
			t.Fatal(err)
		}
		stored := node.GPUs[0]
		if !stored.CanTakeWork() {
			t.Fatalf("report %d: a healthy card takes no work", round)
		}
		if stored.HealthStatus != accelerator.HealthOK {
			t.Fatalf("report %d: health_status = %q, want it normalized", round, stored.HealthStatus)
		}
		if stored.Ident == "" {
			t.Fatalf("report %d: the card lost its key", round)
		}
	}

	// What the fleet queries read.
	groups, err := s.OnlineGPUGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Cards != 1 || groups[0].Unusable != 0 {
		t.Fatalf("fleet groups = %+v, want one usable card", groups)
	}
}

// A card the vendor CLI did not list has no device index, and several such
// cards all carry -1. They must not reach the index map: an instance
// realigned onto index -1 would be pointed at no device at all.
func TestCardsWithoutADeviceIndexAreNotGivenOne(t *testing.T) {
	idents := reportedCardIdents([]proto.GPUInfo{
		{Index: 0, UUID: "GPU-a", PCIeBDF: "0000:3f:00.0"},
		{Index: -1, PCIeBDF: "0000:1a:00.0"},
		{Index: -1, PCIeBDF: "0000:1b:00.0"},
	})
	if len(idents) != 1 || idents[0] == "" {
		t.Fatalf("idents = %v, want only the card the CLI listed", idents)
	}
	if _, ok := idents[-1]; ok {
		t.Fatal("a card with no device index was given one")
	}
}

// Every field a report fills is written again when the card is already
// known. The update names its columns, and a field added to newCardRow but
// not to that list would be written once, when the card is first seen, and
// never updated afterwards — the same silent staleness the gorm hook used
// to cause, in a new place.
func TestAnUpdateWritesEveryFieldAReportFills(t *testing.T) {
	filled := reflect.ValueOf(newCardRow(7, "pci:0000:3f:00.0", proto.GPUInfo{
		Index: 3, UUID: "GPU-a", Name: "NVIDIA A100-SXM4-80GB", ComputeCapability: "8.0",
		Vendor: "nvidia", ArchID: "ga100", MemoryTotalBytes: 80 << 30, PowerLimitWatts: 400,
		PCIeBDF: "0000:3f:00.0", NUMANode: 1, MIGEnabled: true,
		Ownership: accelerator.CardPassedThrough, HealthStatus: accelerator.HealthWarning,
		Healthy: true, HealthCode: accelerator.HealthMemoryRepairPending, HealthMessage: "x: y",
		ForeignProcesses: []proto.CardProcess{{PID: 1, Name: "vllm"}},
	}))
	named := map[string]bool{}
	for _, column := range cardRowColumns {
		named[column] = true
	}
	// Not a report's to fill: the row's own key and gorm's timestamps.
	ownedByTheDatabase := map[string]bool{"ID": true, "CreatedAt": true, "UpdatedAt": true}
	for i := 0; i < filled.NumField(); i++ {
		field := filled.Type().Field(i)
		if ownedByTheDatabase[field.Name] || filled.Field(i).IsZero() {
			continue
		}
		if !named[field.Name] {
			t.Errorf("newCardRow fills %s, but an update never writes it: add it to cardRowColumns", field.Name)
		}
	}
	for column := range named {
		if _, ok := filled.Type().FieldByName(column); !ok {
			t.Errorf("cardRowColumns names %s, which GPU does not have", column)
		}
	}
}

// A model no PCI rule recognizes, that the vendor CLI nevertheless
// reports, is stored and schedulable like any other card. The PCI table
// can only add cards the CLI could not describe; it is never a gate on
// the ones it could.
//
// internal/agent/gpu pins the agent's half of this. This is the other
// end: what the server does with such a card once it arrives, which is
// what "usable on the platform" actually means.
