package extmodule

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// cardRow is one card of the node as the store has it.
func cardRow(t *testing.T, e *testenv.Env, uuid, ident string) store.GPU {
	t.Helper()
	n, err := e.Store().NodeWithGPUs(uuid)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range n.GPUs {
		if g.Ident == ident {
			return g
		}
	}
	t.Fatalf("no card %s on %s", ident, uuid)
	return store.GPU{}
}

// apiCard is one card of GET /nodes/{uuid}, as the wire has it.
type apiCard struct {
	PCIeBDF           string   `json:"pcie_bdf"`
	Schedulable       bool     `json:"schedulable"`
	SchedulingStopped bool     `json:"scheduling_stopped"`
	SchedulingActions []string `json:"scheduling_actions"`
	SchedulingHeldBy  string   `json:"scheduling_held_by"`
	SchedulingHold    *struct {
		Code    string         `json:"code"`
		Params  map[string]any `json:"params"`
		Message string         `json:"message"`
	} `json:"scheduling_hold"`
}

func apiCards(t *testing.T, c *testenv.APIClient, uuid string) []apiCard {
	t.Helper()
	status, body := send(t, c, http.MethodGet, "/api/v1/nodes/"+uuid, "")
	var out struct {
		GPUs []apiCard `json:"gpus"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &out) != nil {
		t.Fatalf("GET node = %d %s", status, body)
	}
	return out.GPUs
}

// wholeCard is a request for one whole card, the scheduler choosing it.
var wholeCard = scheduler.Request{WeightBytes: 1 << 30, HeadCount: 32, GPUMemUtil: 0.9, ReserveBytes: 1 << 30}

// A card holder keeps one card out of scheduling: the card takes no work,
// the scheduler places beside it and names the hold, an operator may stop
// it but not make it schedulable, and the card is released by a pass the
// module asks for, with the operator's decision standing. The node itself
// is never cordoned.
func TestACardHolderHoldsOneCard(t *testing.T) {
	cards := &fakeCardHolder{}
	mod := &fakeModule{name: "fake", cards: cards}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{mod}}, nil)
	n, _, err := e.Store().CreateNodePending("cards")
	if err != nil {
		t.Fatal(err)
	}
	reportCards(t, e, n.UUID, 2)
	fleet, err := mod.host.CardFleet(context.Background())
	if err != nil || len(fleet) != 2 || fleet[0].NodeID != n.ID || fleet[0].NodeUUID != n.UUID || fleet[1].Index != 1 {
		t.Fatalf("CardFleet = %+v, %v", fleet, err)
	}
	held, free := fleet[1].Ident, fleet[0].Ident

	cards.hold(held)
	mod.host.ReapplySchedulingHolds()
	e.Eventually(t, 3*time.Second, func() bool { return cardRow(t, e, n.UUID, held).SchedulingHeldBy == "fakecard" })
	if g := cardRow(t, e, n.UUID, held); g.CanTakeWork() {
		t.Fatal("a held card takes work")
	}
	if g := cardRow(t, e, n.UUID, free); !g.CanTakeWork() || g.HeldByHolder() {
		t.Fatal("holding one card held the other")
	}
	if s, by := schedulingOf(t, e, n.UUID); s != store.NodeSchedulable || by != store.SchedulingBySelf {
		t.Fatalf("a card hold cordoned the node: %s by %q", s, by)
	}

	// The scheduler leaves the held card alone and says why.
	node, err := e.Store().NodeWithGPUs(n.UUID)
	if err != nil {
		t.Fatal(err)
	}
	snap := planning.NodeSnapshot(e.Store(), node, true, "")
	items := scheduler.GPUCandidates(wholeCard, snap)
	if len(items) != 2 || !items[0].Available || items[1].Available || items[1].Reason == nil || items[1].Reason.Code != scheduler.ReasonGPUsHeld {
		t.Fatalf("candidates = %+v", items)
	}
	if res := scheduler.Schedule(wholeCard, snap); !res.OK || !reflect.DeepEqual(res.Plan.GPUIndexes, []int{0}) {
		t.Fatalf("schedule = ok %v %+v plan %+v, want GPU 0", res.OK, res.Message, res.Plan)
	}

	// The API shows the hold and offers only to stop the card.
	api := e.API(t)
	got := apiCards(t, api, n.UUID)
	if len(got) != 2 || got[1].Schedulable || got[1].SchedulingHeldBy != "fakecard" || got[1].SchedulingHold == nil ||
		got[1].SchedulingHold.Code != "FAKE_CARD_HOLD" || got[1].SchedulingHold.Params["reason"] != "fake" ||
		!reflect.DeepEqual(got[1].SchedulingActions, []string{"stopped"}) || got[0].SchedulingHold != nil || !got[0].Schedulable {
		t.Fatalf("cards on the wire = %+v", got)
	}
	bdf := got[1].PCIeBDF
	status, body := send(t, api, http.MethodPut, "/api/v1/nodes/"+n.UUID+"/gpus/scheduling", `{"pcie_bdf":"`+bdf+`","scheduling":"schedulable"}`)
	if code, params := productError(body); status != http.StatusConflict || code != "FAKE_CARD_HOLD" || params["reason"] != "fake" {
		t.Fatalf("enable a held card = %d %s", status, body)
	}
	if status, body := send(t, api, http.MethodPut, "/api/v1/nodes/"+n.UUID+"/gpus/scheduling", `{"pcie_bdf":"`+bdf+`","scheduling":"stopped"}`); status != http.StatusOK {
		t.Fatalf("stop a held card = %d %s", status, body)
	}

	// Released on request for this node alone, without a report.
	cards.release()
	mod.host.ReapplySchedulingHoldsFor([]uint{n.ID})
	e.Eventually(t, 5*time.Second, func() bool { return !cardRow(t, e, n.UUID, held).HeldByHolder() })
	if g := cardRow(t, e, n.UUID, held); g.CanTakeWork() || !g.SchedulingStopped {
		t.Fatal("the operator's stop did not stand once the hold ended")
	}
	if status, body := send(t, api, http.MethodPut, "/api/v1/nodes/"+n.UUID+"/gpus/scheduling", `{"pcie_bdf":"`+bdf+`","scheduling":"schedulable"}`); status != http.StatusOK {
		t.Fatalf("enable a released card = %d %s", status, body)
	}
	if g := cardRow(t, e, n.UUID, held); !g.CanTakeWork() {
		t.Fatal("a released card the operator enabled takes no work")
	}
}

// A report asks the card holders too: a card held while the node reports
// is held by that report, and a new card a report adds is announced on the
// fleet topic.
func TestAReportAsksTheCardHolders(t *testing.T) {
	cards := &fakeCardHolder{}
	mod := &fakeModule{name: "fake", cards: cards}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{mod}}, nil)
	sub := mod.host.Bus().Subscribe(64, store.NodeFleetTopic)
	defer sub.Close()
	n, _, err := e.Store().CreateNodePending("reported")
	if err != nil {
		t.Fatal(err)
	}
	reportCards(t, e, n.UUID, 1)
	fleet, err := mod.host.CardFleet(context.Background())
	if err != nil || len(fleet) != 1 {
		t.Fatalf("CardFleet = %+v, %v", fleet, err)
	}
	cards.hold(fleet[0].Ident)
	reportCards(t, e, n.UUID, 1)
	if g := cardRow(t, e, n.UUID, fleet[0].Ident); g.SchedulingHeldBy != "fakecard" || g.CanTakeWork() {
		t.Fatalf("after a report: %+v", g)
	}
	for len(sub.C()) > 0 {
		<-sub.C()
	}
	reportCards(t, e, n.UUID, 2) // the same count of cards could not say this
	select {
	case ev := <-sub.C():
		if c, ok := store.NodeFleetChangeOf(ev); !ok || c != (store.NodeFleetChange{NodeID: n.ID, Kind: store.NodeFactsChanged}) {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a new card was not announced")
	}
}

// The core asks the card holders inside the writer's transaction once per
// card per report: asking about a card nobody holds allocates nothing, and
// neither does a holder that refuses with a hold it built once.
func TestAskingTheCardHoldersAllocatesNothing(t *testing.T) {
	cards := &fakeCardHolder{}
	mod := &fakeModule{name: "fake", cards: cards}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{mod}}, nil)
	facts := store.CardHoldFacts{NodeID: 1, NodeUUID: "n", Ident: "pci:0000:3f:00.0", Vendor: "nvidia"}
	cards.hold("pci:0000:40:00.0")
	if n := testing.AllocsPerRun(1000, func() { _, _ = e.Store().CardHoldFor(facts) }); n != 0 {
		t.Fatalf("a card nobody holds: %v allocations per ask", n)
	}
	cards.hold(facts.Ident)
	if src, hold := e.Store().CardHoldFor(facts); src != "fakecard" || hold.Code != "FAKE_CARD_HOLD" {
		t.Fatalf("held = %q %+v", src, hold)
	}
	if n := testing.AllocsPerRun(1000, func() { _, _ = e.Store().CardHoldFor(facts) }); n != 0 {
		t.Fatalf("a held card: %v allocations per ask", n)
	}
}
