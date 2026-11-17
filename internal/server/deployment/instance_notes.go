package deployment

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Instance notes are non-blocking facts about a live instance that explain
// what a user may see of it - an inference slower than expected on a card
// the platform shares with load it cannot see - as codes and params
// (messages.Instance*). They are derived when the instance is read, from
// rows the read already needs: nothing is stored, nothing is kept per
// instance, so a note appears as soon as its cause is reported, goes with it,
// and leaves nothing behind when the instance does. Future notes are rules
// in instanceNoteRules.

// instanceNoteRule states the notes one fact gives an instance whose node's
// held cards (by index) are held. A rule reads nothing itself: what it
// needs joins the one read InstanceNotes makes for every instance, so a
// detail or list read never costs a query per instance.
type instanceNoteRule func(inst *store.Instance, held map[int]store.GPU) []messages.Warning

var instanceNoteRules = []instanceNoteRule{foreignCardNote}

// InstanceNotes is the notes of each live instance of insts, by UUID. It
// reads the held cards of the nodes the live instances run on once, however
// many instances there are (Rows.HeldCards).
func InstanceNotes(rows Rows, insts []store.Instance) map[string][]messages.Warning {
	out := map[string][]messages.Warning{}
	var nodeIDs []uint
	for i := range insts {
		if instanceLive(&insts[i]) && !slices.Contains(nodeIDs, insts[i].NodeID) {
			nodeIDs = append(nodeIDs, insts[i].NodeID)
		}
	}
	if len(nodeIDs) == 0 {
		return out
	}
	held := map[uint]map[int]store.GPU{}
	for nodeID, cards := range rows.HeldCards(nodeIDs) {
		held[nodeID] = make(map[int]store.GPU, len(cards))
		for _, card := range cards {
			held[nodeID][card.Index] = card
		}
	}
	for i := range insts {
		inst := &insts[i]
		if !instanceLive(inst) || len(held[inst.NodeID]) == 0 {
			continue
		}
		var notes []messages.Warning
		for _, rule := range instanceNoteRules {
			found := rule(inst, held[inst.NodeID])
			sort.SliceStable(found, func(a, b int) bool { return found[a].Code < found[b].Code })
			notes = append(notes, found...)
		}
		if len(notes) > 0 {
			out[inst.UUID] = notes
		}
	}
	return out
}

// instanceLive is an instance whose row still means it is, or is about to
// be, on its cards: every state but the finished ones
// (proto.InstanceFinishedStates). A failed attempt kept as history, or one
// awaiting removal, runs nothing; one draining or being removed still does.
func instanceLive(inst *store.Instance) bool {
	return !slices.Contains(proto.InstanceFinishedStates(), inst.State)
}

// foreignCardNote notes the instance's cards that processes outside the
// platform also use, whatever let the instance onto them: it states the
// fact, not an admission verdict. The figure is the most they hold on any of
// those cards, as the agent reports it, without the scheduler's headroom.
func foreignCardNote(inst *store.Instance, held map[int]store.GPU) []messages.Warning {
	var indexes []int
	if json.Unmarshal([]byte(inst.GPUIndexes), &indexes) != nil {
		return nil
	}
	var shared []int
	var most int64
	known := true
	for _, index := range indexes {
		card, ok := held[index]
		if !ok {
			continue
		}
		shared = append(shared, index)
		bytes, ok := card.ForeignBytes()
		known = known && ok
		most = max(most, bytes)
	}
	if len(shared) == 0 {
		return nil
	}
	if !known {
		return []messages.Warning{messages.NewWarning(messages.InstanceCardSharedOutsideUnknown, map[string]any{"gpu_indexes": shared})}
	}
	return []messages.Warning{messages.NewWarning(messages.InstanceCardSharedOutside, map[string]any{
		"gpu_indexes": shared, "foreign_bytes": most, "foreign_gib": fmt.Sprintf("%.1f", float64(most)/(1<<30)),
	})}
}
