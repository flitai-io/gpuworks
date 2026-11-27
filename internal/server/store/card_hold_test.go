package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// setCardHolder holds the cards it is told to, by identity, from its own
// in-memory state only.
type setCardHolder struct {
	source SchedulingSource
	code   string
	mu     sync.Mutex
	held   map[string]bool
}

func newSetCardHolder(source SchedulingSource, code string) *setCardHolder {
	return &setCardHolder{source: source, code: code, held: map[string]bool{}}
}

func (h *setCardHolder) Source() SchedulingSource { return h.source }

func (h *setCardHolder) HoldCard(f CardHoldFacts) CardHold {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.held[f.Ident] {
		return CardHold{}
	}
	return CardHold{Code: h.code, Fallback: "held by " + string(h.source), Params: map[string]any{"card": f.Index}}
}

func (h *setCardHolder) set(ident string, held bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held[ident] = held
}

var card3f = accelerator.CardIdent("GPU-3f", "0000:3f:00.0")

func TestAddCardSchedulingHolderValidatesTheSource(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []SchedulingSource{"", "operator", "platform", "Cap", "1cap", "a-b", "seventeen_chars_x"} {
		if err := s.AddCardSchedulingHolder(newSetCardHolder(bad, "")); err == nil {
			t.Errorf("source %q was accepted", bad)
		}
	}
	if err := s.AddCardSchedulingHolder(nil); err == nil {
		t.Error("a nil card holder was accepted")
	}
	if err := s.AddCardSchedulingHolder(newSetCardHolder("cap", "")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddCardSchedulingHolder(newSetCardHolder("cap", "")); !errors.Is(err, ErrHolderConflict) {
		t.Fatalf("duplicate source: err = %v, want ErrHolderConflict", err)
	}
	// One module may hold nodes and cards under one source.
	if err := s.AddSchedulingHolder(newSetHolder("cap", "")); err != nil {
		t.Fatalf("a node holder sharing a card holder's source was refused: %v", err)
	}
}

// A held card takes no work from the next report on, and only that card:
// its row records the holder, the fleet counts it unusable, and an operator
// is offered only to stop it.
func TestAReportHoldsOneCard(t *testing.T) {
	s := newTestStore(t)
	h := newSetCardHolder("cap", "CAP_CARD")
	if err := s.AddCardSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	report := cardNode(t, s, "n-held")
	report("", false)
	if c := cardOf(t, s, "n-held", "0000:40:00.0"); !c.CanTakeWork() || c.HeldByHolder() {
		t.Fatalf("before the hold: %+v", c)
	}

	h.set(card40, true)
	sub := s.Bus.Subscribe(16, "node")
	t.Cleanup(sub.Close)
	report("", false)
	if !drainedReason(sub.C(), NodeChangeScheduling) {
		t.Fatal("the card hold was not announced as a scheduling change")
	}
	c := cardOf(t, s, "n-held", "0000:40:00.0")
	if c.CanTakeWork() || c.SchedulingHeldBy != "cap" || fmt.Sprint(c.SchedulingActions()) != "[stopped]" {
		t.Fatalf("held card = can=%v by=%q actions=%v", c.CanTakeWork(), c.SchedulingHeldBy, c.SchedulingActions())
	}
	if other := cardOf(t, s, "n-held", "0000:3f:00.0"); !other.CanTakeWork() || other.HeldByHolder() {
		t.Fatal("holding one card held another")
	}
	node, err := s.NodeWithGPUs("n-held")
	if err != nil {
		t.Fatal(err)
	}
	if pe := s.CardHoldOf(node, c); pe == nil || pe.Code() != "CAP_CARD" || pe.Params()["card"] != 1 {
		t.Fatalf("CardHoldOf = %+v", pe)
	}
	if pe := s.CardHoldOf(node, cardOf(t, s, "n-held", "0000:3f:00.0")); pe != nil {
		t.Fatalf("CardHoldOf a free card = %+v", pe)
	}
	if n, _ := s.NodeByUUID("n-held"); n.Scheduling != NodeSchedulable {
		t.Fatalf("a card hold cordoned the node: %s", n.Scheduling)
	}
	groups, err := s.OnlineGPUGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Cards != 1 || groups[0].Unusable != 1 {
		t.Fatalf("fleet counts = %+v", groups)
	}
	// Unchanged reports keep the hold and do not rewrite the row.
	before := c.UpdatedAt
	report("", false)
	if c := cardOf(t, s, "n-held", "0000:40:00.0"); !c.HeldByHolder() || !c.UpdatedAt.Equal(before) {
		t.Fatalf("a second report: held=%v rewritten=%v", c.HeldByHolder(), !c.UpdatedAt.Equal(before))
	}
	// The hold ends: the next report releases it.
	h.set(card40, false)
	report("", false)
	if c := cardOf(t, s, "n-held", "0000:40:00.0"); !c.CanTakeWork() || c.HeldByHolder() {
		t.Fatalf("after the hold ended: %+v", c)
	}
}

// An operator cannot make a held card schedulable (the refusal carries the
// holder's message) but may stop it; a card they enabled before the hold is
// held all the same, and their decisions stand once the hold ends.
func TestAnOperatorUnderACardHold(t *testing.T) {
	s := newTestStore(t)
	h := newSetCardHolder("cap", "CAP_CARD")
	if err := s.AddCardSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	report := cardNode(t, s, "n-op")
	report(accelerator.HealthRuntimeUnusable, false)
	// The faulty card enabled by the operator, then held.
	if _, err := s.SetCardScheduling("n-op", card40, CardSchedulingSchedulable); err != nil {
		t.Fatal(err)
	}
	h.set(card40, true)
	h.set(card3f, true)
	report(accelerator.HealthRuntimeUnusable, false)
	if c := cardOf(t, s, "n-op", "0000:40:00.0"); c.CanTakeWork() || c.SchedulingAllowedAt == nil {
		t.Fatalf("an enabled card under a hold: can=%v allowed=%v", c.CanTakeWork(), c.SchedulingAllowedAt != nil)
	}
	_, err := s.SetCardScheduling("n-op", card3f, CardSchedulingSchedulable)
	var pe *messages.ProductError
	if !errors.Is(err, ErrCardHeld) || !errors.As(err, &pe) || pe.Code() != "CAP_CARD" {
		t.Fatalf("enable a held card = %v", err)
	}
	if _, err := s.SetCardScheduling("n-op", card3f, CardSchedulingStopped); err != nil {
		t.Fatalf("stopping a held card was refused: %v", err)
	}
	if c := cardOf(t, s, "n-op", "0000:3f:00.0"); c.SchedulingActions() != nil {
		t.Fatalf("a stopped, held card offers %v", c.SchedulingActions())
	}
	h.set(card40, false)
	h.set(card3f, false)
	report(accelerator.HealthRuntimeUnusable, false)
	if c := cardOf(t, s, "n-op", "0000:40:00.0"); !c.CanTakeWork() {
		t.Fatal("the operator's enable did not stand after the hold ended")
	}
	if c := cardOf(t, s, "n-op", "0000:3f:00.0"); c.CanTakeWork() || !c.SchedulingStopped {
		t.Fatal("the operator's stop did not stand after the hold ended")
	}
}

// A card holder whose answers changed has them applied without a report,
// over the fleet or over the named nodes only; holds left by a source no
// holder registers are released at boot.
func TestApplySchedulingHoldsHoldsAndReleasesCards(t *testing.T) {
	s := newTestStore(t)
	h := newSetCardHolder("cap", "CAP_CARD")
	if err := s.AddCardSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	cardNode(t, s, "n-a")("", false)
	cardNode(t, s, "n-b")("", false)
	held := func(uuid string) bool { return cardOf(t, s, uuid, "0000:40:00.0").HeldByHolder() }

	h.set(card40, true) // the same identity on both nodes
	sub := s.Bus.Subscribe(16, "node")
	t.Cleanup(sub.Close)
	nodeB, err := s.NodeByUUID("n-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplySchedulingHoldsFor([]uint{nodeB.ID}); err != nil {
		t.Fatal(err)
	}
	if held("n-a") || !held("n-b") {
		t.Fatalf("a scoped pass: a=%v b=%v", held("n-a"), held("n-b"))
	}
	if !drainedReason(sub.C(), NodeChangeScheduling) {
		t.Fatal("the scoped pass did not announce the card's node")
	}
	if nodes, released, err := s.ApplySchedulingHolds(); err != nil || len(nodes)+len(released) != 0 {
		t.Fatalf("a card pass counted nodes: held=%v released=%v err=%v", nodes, released, err)
	}
	if !held("n-a") || !held("n-b") {
		t.Fatal("the fleet pass did not hold both cards")
	}
	h.set(card40, false)
	if _, _, err := s.ApplySchedulingHolds(); err != nil {
		t.Fatal(err)
	}
	if held("n-a") || held("n-b") {
		t.Fatal("the fleet pass did not release the cards")
	}

	// A build without that holder releases what it left.
	h.set(card40, true)
	if _, _, err := s.ApplySchedulingHolds(); err != nil {
		t.Fatal(err)
	}
	other := newTestStore(t)
	other.db = s.db
	if _, err := other.ReleaseUnownedHolds("cap"); err != nil || !held("n-a") {
		t.Fatalf("a known source was released: %v", err)
	}
	if _, err := other.ReleaseUnownedHolds(); err != nil {
		t.Fatal(err)
	}
	if held("n-a") || held("n-b") {
		t.Fatal("holds of a source no holder registers survived the boot release")
	}
}

// A report that adds, removes or renumbers a card announces the node's
// facts on the fleet topic, and CardFleet lists every card's facts.
func TestCardFactsAreAnnouncedAndListed(t *testing.T) {
	s := newTestStore(t)
	n := newHoldNode(t, s, "fleet")
	one := proto.GPUInfo{Index: 0, UUID: "GPU-3f", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}
	two := proto.GPUInfo{Index: 1, UUID: "GPU-40", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}
	n.report(dockerOK, one, two)
	sub := s.Bus.Subscribe(16, NodeFleetTopic)
	t.Cleanup(sub.Close)

	n.report(dockerOK, one, two)
	if drainedReason(sub.C(), string(NodeFactsChanged)) {
		t.Fatal("an unchanged report was announced")
	}
	// Same count, another card: the node's own facts are unchanged.
	swapped := two
	swapped.UUID, swapped.PCIeBDF = "GPU-41", "0000:41:00.0"
	n.report(dockerOK, one, swapped)
	if !drainedReason(sub.C(), string(NodeFactsChanged)) {
		t.Fatal("a swapped card was not announced")
	}
	fleet, err := s.CardFleet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	row := n.row()
	want := []CardHoldFacts{
		{NodeID: row.ID, NodeUUID: "fleet", NodeCreatedAt: row.CreatedAt, Ident: card3f, Index: 0, Vendor: "nvidia"},
		{NodeID: row.ID, NodeUUID: "fleet", NodeCreatedAt: row.CreatedAt, Ident: accelerator.CardIdent("GPU-41", "0000:41:00.0"), Index: 1, Vendor: "nvidia"},
	}
	if len(fleet) != len(want) {
		t.Fatalf("CardFleet = %+v", fleet)
	}
	for i := range want {
		if fleet[i].Ident != want[i].Ident || fleet[i].NodeID != want[i].NodeID || fleet[i].NodeUUID != want[i].NodeUUID ||
			!fleet[i].NodeCreatedAt.Equal(want[i].NodeCreatedAt) || fleet[i].Index != want[i].Index || fleet[i].Vendor != want[i].Vendor {
			t.Fatalf("CardFleet[%d] = %+v, want %+v", i, fleet[i], want[i])
		}
	}
}

// A held card is not shared with processes outside the platform either.
func TestAHeldCardIsNotSharedWithForeignProcesses(t *testing.T) {
	g := GPU{HealthStatus: accelerator.HealthOK, ForeignProcesses: `[{"pid":1,"memory_bytes":1073741824}]`}
	if _, ok := g.SharesMemoryWithForeign(); !ok {
		t.Fatal("fixture: the card should be shareable")
	}
	g.SchedulingHeldBy = "cap"
	if _, ok := g.SharesMemoryWithForeign(); ok {
		t.Fatal("a held card is shareable")
	}
}

// Asking the card holders about a card they do not hold allocates nothing:
// it runs once per card on every status report of every node.
func TestCardHoldOfAllocatesNothingWhenNotHeld(t *testing.T) {
	s := newTestStore(t)
	h := newSetCardHolder("cap", "CAP_CARD")
	h.set("pci:0000:99:00.0", true)
	if err := s.AddCardSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	node := NodeHoldFacts{ID: 1, UUID: "free"}
	card := &GPU{Ident: card3f, Vendor: "nvidia"}
	if allocs := testing.AllocsPerRun(100, func() { _ = s.admission.cardHoldSource(node, card) }); allocs != 0 {
		t.Fatalf("cardHoldSource allocated %v times per call", allocs)
	}
}

func TestGPUSchedulingHeldByMigration(t *testing.T) {
	s := newTestStore(t)
	db := s.db
	m := migrationByName(t, "gpu_scheduling_held_by")
	if !db.Migrator().HasColumn(&GPU{}, "SchedulingHeldBy") {
		t.Fatal("scheduling_held_by missing after migrations")
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}
	t.Run("from an older schema", func(t *testing.T) {
		old := nodesOnlyDB(t, "")
		if err := old.Exec(`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, ident TEXT)`).Error; err != nil {
			t.Fatal(err)
		}
		if err := old.Exec(`INSERT INTO gpus (node_id, ident) VALUES (1, 'pci:0000:3f:00.0')`).Error; err != nil {
			t.Fatal(err)
		}
		if err := migrateDB(old, []migration{m}); err != nil {
			t.Fatal(err)
		}
		var by string
		if err := old.Raw(`SELECT scheduling_held_by FROM gpus`).Scan(&by).Error; err != nil || by != "" {
			t.Fatalf("existing card held by %q, %v", by, err)
		}
		// Re-opening applies nothing again.
		if err := migrateDB(old, []migration{m}); err != nil {
			t.Fatalf("re-open: %v", err)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		failing := nodesOnlyDB(t, "")
		if err := failing.Exec(`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, ident TEXT)`).Error; err != nil {
			t.Fatal(err)
		}
		if err := failing.Exec(fmt.Sprintf(`CREATE TRIGGER fail_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
			t.Fatal(err)
		}
		if err := migrateDB(failing, []migration{m}); err == nil {
			t.Fatal("injected marker failure did not fail the migration")
		}
		if failing.Migrator().HasColumn("gpus", "scheduling_held_by") {
			t.Fatal("the column survived a failed migration")
		}
		var count int64
		if err := failing.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("marker after a failed migration: %d, %v", count, err)
		}
	})
}

// A hold only keeps new work off the card: an instance running on it is not
// touched, by the report that holds it nor by a pass.
func TestACardHoldLeavesRunningWorkAlone(t *testing.T) {
	s := newTestStore(t)
	h := newSetCardHolder("cap", "CAP_CARD")
	if err := s.AddCardSchedulingHolder(h); err != nil {
		t.Fatal(err)
	}
	report := cardNode(t, s, "n-run")
	report("", false)
	node, err := s.NodeByUUID("n-run")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := s.CreateDeployment(&Deployment{Name: "on-held", ServedName: "on-held"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := s.CreateInstance(&Instance{UUID: "runs-on-held", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[1]"})
	if err != nil {
		t.Fatal(err)
	}
	h.set(card40, true)
	report("", false)
	if _, _, err := s.ApplySchedulingHolds(); err != nil {
		t.Fatal(err)
	}
	if c := cardOf(t, s, "n-run", "0000:40:00.0"); !c.HeldByHolder() {
		t.Fatal("fixture: the card is not held")
	}
	got, err := s.InstanceByUUID(inst.UUID)
	if err != nil || got.State != proto.InstRunning || got.GPUIndexes != "[1]" || got.NodeID != node.ID {
		t.Fatalf("instance after the hold = %+v, %v", got, err)
	}
}
