package deployment

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// noteRows answers the reads InstanceNotes makes and counts them.
type noteRows struct {
	Rows
	nodes     map[uint]*store.Node
	held      map[uint][]store.GPU
	heldCalls int
	nodeReads int
	asked     [][]uint
}

func (r *noteRows) Node(id uint) *store.Node {
	r.nodeReads++
	return r.nodes[id]
}

func (r *noteRows) HeldCards(nodeIDs []uint) map[uint][]store.GPU {
	r.heldCalls++
	r.asked = append(r.asked, append([]uint(nil), nodeIDs...))
	out := map[uint][]store.GPU{}
	for _, id := range nodeIDs {
		if cards := r.held[id]; len(cards) > 0 {
			out[id] = cards
		}
	}
	return out
}

const gib62 = `[{"pid":41235,"name":"python3","memory_bytes":67089334272}]`

func heldCard(nodeID uint, index int, processes string) store.GPU {
	return store.GPU{NodeID: nodeID, Index: index, MemoryTotal: 143771 << 20, ForeignProcesses: processes}
}

func TestInstanceNotesFollowWhatSharesTheCards(t *testing.T) {
	rows := &noteRows{
		nodes: map[uint]*store.Node{1: {ID: 1, UUID: "n1"}, 2: {ID: 2, UUID: "n2"}},
		held: map[uint][]store.GPU{
			1: {heldCard(1, 0, gib62), heldCard(1, 2, `[{"pid":7,"name":"x"}]`)},
		},
	}
	insts := []store.Instance{
		{UUID: "known", NodeID: 1, State: proto.InstRunning, GPUIndexes: "[0]"},
		{UUID: "unknown", NodeID: 1, State: proto.InstRunning, GPUIndexes: "[2]"},
		{UUID: "tp-two", NodeID: 1, State: proto.InstRunning, GPUIndexes: "[0,1]"},
		{UUID: "clean", NodeID: 1, State: proto.InstRunning, GPUIndexes: "[1]"},
		{UUID: "other-node", NodeID: 2, State: proto.InstRunning, GPUIndexes: "[0]"},
		{UUID: "stopped", NodeID: 1, State: proto.InstStopped, GPUIndexes: "[0]"},
	}
	notes := InstanceNotes(rows, insts)
	known := []messages.Warning{{Code: messages.InstanceCardSharedOutside, Params: map[string]any{
		"gpu_indexes": []int{0}, "foreign_bytes": int64(67089334272), "foreign_gib": "62.5"}}}
	if !reflect.DeepEqual(notes["known"], known) {
		t.Fatalf("known = %+v", notes["known"])
	}
	if !reflect.DeepEqual(notes["tp-two"], known) {
		t.Fatalf("a TP group with one shared card = %+v", notes["tp-two"])
	}
	unknown := []messages.Warning{{Code: messages.InstanceCardSharedOutsideUnknown, Params: map[string]any{"gpu_indexes": []int{2}}}}
	if !reflect.DeepEqual(notes["unknown"], unknown) {
		t.Fatalf("unknown = %+v", notes["unknown"])
	}
	for _, uuid := range []string{"clean", "other-node", "stopped"} {
		if len(notes[uuid]) != 0 {
			t.Fatalf("%s has notes %+v", uuid, notes[uuid])
		}
	}
	// One read for every node the live instances run on, whatever their count.
	if rows.heldCalls != 1 || !reflect.DeepEqual(rows.asked[0], []uint{1, 2}) {
		t.Fatalf("held card reads = %d %v", rows.heldCalls, rows.asked)
	}
}

// Notes are derived on read: they go when the processes go, and an
// instance that is gone has none left to show.
func TestInstanceNotesClearWithTheirCause(t *testing.T) {
	rows := &noteRows{nodes: map[uint]*store.Node{1: {ID: 1}}, held: map[uint][]store.GPU{1: {heldCard(1, 0, gib62)}}}
	inst := store.Instance{UUID: "beside", NodeID: 1, State: proto.InstRunning, GPUIndexes: "[0]"}
	if notes := InstanceNotes(rows, []store.Instance{inst}); len(notes["beside"]) != 1 {
		t.Fatalf("notes = %+v", notes)
	}
	rows.held = map[uint][]store.GPU{}
	if notes := InstanceNotes(rows, []store.Instance{inst}); len(notes["beside"]) != 0 {
		t.Fatalf("after the processes left = %+v", notes)
	}
	rows.held = map[uint][]store.GPU{1: {heldCard(1, 0, gib62)}}
	if notes := InstanceNotes(rows, nil); len(notes) != 0 || rows.heldCalls != 2 {
		t.Fatalf("no instances = %+v, reads %d", notes, rows.heldCalls)
	}
}

// The note states a fact, whatever let the instance onto the card: a
// whole-card instance an operator allowed onto a held card, or an agent too
// old to share, shows it as well.
func TestInstanceNotesAreFactsNotAdmissionVerdicts(t *testing.T) {
	rows := &noteRows{
		nodes: map[uint]*store.Node{1: {ID: 1, PartitionSafetyProtocol: 0}},
		held:  map[uint][]store.GPU{1: {heldCard(1, 0, gib62)}},
	}
	whole := store.Instance{UUID: "whole", NodeID: 1, State: proto.InstRunning, GPUIndexes: "[0]"}
	if notes := InstanceNotes(rows, []store.Instance{whole}); len(notes["whole"]) != 1 || notes["whole"][0].Code != messages.InstanceCardSharedOutside {
		t.Fatalf("notes = %+v", notes)
	}
}

// Only an instance whose row still means it is on its cards gets a note: a
// failed attempt kept as history, or one awaiting removal, holds no card and
// runs nothing, so a process outside the platform there explains nothing
// about it. One draining or being removed is still on its cards.
func TestInstanceNotesSkipFinishedInstances(t *testing.T) {
	rows := &noteRows{held: map[uint][]store.GPU{1: {heldCard(1, 0, gib62)}}}
	var insts []store.Instance
	for _, state := range []string{proto.InstRunning, proto.InstDraining, proto.InstRemoving, proto.InstStopped, proto.InstFailed, proto.InstPendingRemoval} {
		insts = append(insts, store.Instance{UUID: state, NodeID: 1, State: state, GPUIndexes: "[0]"})
	}
	notes := InstanceNotes(rows, insts)
	for _, state := range []string{proto.InstRunning, proto.InstDraining, proto.InstRemoving} {
		if len(notes[state]) != 1 {
			t.Fatalf("%s notes = %+v, want one", state, notes[state])
		}
	}
	for _, state := range []string{proto.InstStopped, proto.InstFailed, proto.InstPendingRemoval} {
		if len(notes[state]) != 0 {
			t.Fatalf("%s notes = %+v, want none", state, notes[state])
		}
	}
}

// The notes read the held cards once and nothing per instance: a pool
// deployment's detail with a replica on each of many held nodes costs one
// query, not one node read per replica.
func TestInstanceNotesReadNothingPerInstance(t *testing.T) {
	rows := &noteRows{held: map[uint][]store.GPU{}}
	var insts []store.Instance
	for id := uint(1); id <= 5; id++ {
		rows.held[id] = []store.GPU{heldCard(id, 0, gib62)}
		insts = append(insts, store.Instance{UUID: fmt.Sprint(id), NodeID: id, State: proto.InstRunning, GPUIndexes: "[0]"})
	}
	if notes := InstanceNotes(rows, insts); len(notes) != 5 {
		t.Fatalf("notes = %+v", notes)
	}
	if rows.heldCalls != 1 || rows.nodeReads != 0 {
		t.Fatalf("held card reads %d, node reads %d; want 1 and 0", rows.heldCalls, rows.nodeReads)
	}
}

// StoreRows answers the held cards of many nodes with one indexed query,
// whatever the number of nodes, and only the held cards.
func TestStoreRowsHeldCardsIsOneQuery(t *testing.T) {
	st, err := store.Open("file:deployment_held_cards_query?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var ids []uint
	for i := range 4 {
		name := fmt.Sprintf("held-%d", i)
		node, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, node.ID)
		for index := range 2 {
			card := &store.GPU{NodeID: node.ID, Index: index, UUID: fmt.Sprintf("%s-%d", name, index), MemoryTotal: 80 << 30}
			if index == 0 {
				card.ForeignProcesses = gib62
			}
			if err := st.DB().Create(card).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	var queries atomic.Int64
	if err := st.DB().Callback().Query().After("gorm:query").Register("count_held_card_queries", func(*gorm.DB) { queries.Add(1) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove("count_held_card_queries") })
	held := NewStoreRows(st, nil).HeldCards(ids)
	if got := queries.Load(); got != 1 {
		t.Fatalf("queries = %d, want 1", got)
	}
	if len(held) != len(ids) {
		t.Fatalf("held = %+v", held)
	}
	for _, id := range ids {
		if cards := held[id]; len(cards) != 1 || cards[0].Index != 0 {
			t.Fatalf("node %d held = %+v", id, cards)
		}
	}
}
