package store

import (
	"encoding/json"
	"log/slog"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// An instance's cards are bound by hardware identity (Instance.GPUIdents),
// but everything that counts them — the ledger, wake admission, the free-GPU
// views, the agent's device injection — works in the device indexes the
// driver gives those cards. The two are kept in step here: whenever a probe
// renumbers a node's cards, every instance on it is rewritten to the indexes
// its own cards now carry, so the counting code keeps its index language and
// still lands on the right card.

// cardIdentsByIndex is the hardware identity of each card, keyed by the
// device index it currently carries; card(i) reads the i-th of n cards. A
// card the vendor CLI did not list carries no index (-1) and is left out:
// it has no device to name, several such cards would collide on the one
// key, and nothing can run on them.
//
// It is the one place that rule lives, for the cards of a report, of a
// stored node, and of the migration that first bound the instances.
func cardIdentsByIndex(n int, card func(i int) (index int, uuid, bdf string)) map[int]string {
	idents := make(map[int]string, n)
	for i := 0; i < n; i++ {
		index, uuid, bdf := card(i)
		if index < 0 {
			continue
		}
		if ident := accelerator.CardIdent(uuid, bdf); ident != "" {
			idents[index] = ident
		}
	}
	return idents
}

// reportedCardIdents is cardIdentsByIndex of a report's cards.
func reportedCardIdents(gpus []proto.GPUInfo) map[int]string {
	return cardIdentsByIndex(len(gpus), func(i int) (int, string, string) {
		return gpus[i].Index, gpus[i].UUID, gpus[i].PCIeBDF
	})
}

// storedCardIdents is cardIdentsByIndex of stored cards.
func storedCardIdents(cards []GPU) map[int]string {
	return cardIdentsByIndex(len(cards), func(i int) (int, string, string) {
		return cards[i].Index, cards[i].UUID, cards[i].PCIeBDF
	})
}

// identsForIndexes is the identity of each index, in order. The result is
// empty unless every index has one: a partly identified selection would bind
// some cards and silently leave the others on a drifting index.
func identsForIndexes(indexes []int, byIndex map[int]string) []string {
	idents := make([]string, 0, len(indexes))
	for _, index := range indexes {
		ident, ok := byIndex[index]
		if !ok {
			return nil
		}
		idents = append(idents, ident)
	}
	return idents
}

// CardIdentsFor is the identity of each of a node's cards named by indexes,
// in the same order, or nil when the node's prober reports no identity for
// one of them (test fixtures and probers that know only indexes then keep
// today's index-only behavior).
func CardIdentsFor(node *Node, indexes []int) []string {
	if node == nil || len(indexes) == 0 {
		return nil
	}
	return identsForIndexes(indexes, storedCardIdents(node.GPUs))
}

// realignInstanceGPUs rewrites the device indexes of the node's instances to
// the ones their bound cards now carry. It runs only when a probe actually
// renumbered a card, so a fleet whose cards never move pays nothing for it.
// An instance whose card is missing from this probe is left alone: the card
// may come back, and an instance pointed at nothing is worse than one
// pointed at where its card last was.
func realignInstanceGPUs(tx *gorm.DB, nodeID uint, gpus []proto.GPUInfo) error {
	indexOfIdent := map[string]int{}
	for index, ident := range reportedCardIdents(gpus) {
		indexOfIdent[ident] = index
	}
	if len(indexOfIdent) == 0 {
		return nil
	}
	var instances []Instance
	if err := tx.Select("ID", "UUID", "GPUIndexes", "GPUIdents", "DesiredStateJSON").
		Where("node_id = ? AND gpu_idents <> ''", nodeID).Find(&instances).Error; err != nil {
		return err
	}
	for _, inst := range instances {
		var idents []string
		if err := json.Unmarshal([]byte(inst.GPUIdents), &idents); err != nil || len(idents) == 0 {
			continue
		}
		moved := make([]int, 0, len(idents))
		for _, ident := range idents {
			index, ok := indexOfIdent[ident]
			if !ok {
				moved = nil
				break
			}
			moved = append(moved, index)
		}
		if len(moved) == 0 {
			slog.Warn("instance keeps its device indexes: a card it is bound to is missing from this probe",
				"instance", inst.UUID, "idents", inst.GPUIdents)
			continue
		}
		raw, err := json.Marshal(moved)
		if err != nil || string(raw) == inst.GPUIndexes {
			continue
		}
		// Named by field, not by column: gorm's spelling of GPUIndexes is
		// not the one anybody writes by hand.
		row := Instance{GPUIndexes: string(raw)}
		fields := []string{"GPUIndexes"}
		if spec := respecGPUIndexes(inst.DesiredStateJSON, moved); spec != "" {
			row.DesiredStateJSON = spec
			fields = append(fields, "DesiredStateJSON")
		}
		if err := tx.Model(&Instance{}).Where("id = ?", inst.ID).Select(fields).Updates(row).Error; err != nil {
			return err
		}
		slog.Info("instance follows its cards to their new device indexes",
			"instance", inst.UUID, "from", inst.GPUIndexes, "to", string(raw))
	}
	return nil
}

// respecGPUIndexes returns the desired state with its device indexes
// replaced, or "" when there is nothing to rewrite. The agent is replayed
// this document on every reconnect, so it has to agree with the column.
func respecGPUIndexes(specJSON string, indexes []int) string {
	if specJSON == "" {
		return ""
	}
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		return ""
	}
	spec.GPUIndexes = indexes
	raw, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return string(raw)
}

// backfillInstanceGPUIdents gives instances placed before Instance.GPUIdents
// existed the identity of the cards their device indexes name right now.
// Afterwards only realignInstanceGPUs maintains the binding.
//
// The queries go through the models rather than raw column names: GORM
// spells these fields in ways the hand cannot guess (gpus.pc_ie_bdf,
// instances.gp_uindexes), and a typo here would silently back-fill nothing.
func backfillInstanceGPUIdents(db *gorm.DB) error {
	var cards []GPU
	if err := db.Select("NodeID", "Index", "UUID", "PCIeBDF").Find(&cards).Error; err != nil {
		return err
	}
	cardsOf := map[uint][]GPU{}
	for _, c := range cards {
		cardsOf[c.NodeID] = append(cardsOf[c.NodeID], c)
	}
	byNode := make(map[uint]map[int]string, len(cardsOf))
	for node, nodeCards := range cardsOf {
		byNode[node] = storedCardIdents(nodeCards)
	}
	if len(byNode) == 0 {
		return nil
	}
	var instances []Instance
	if err := db.Select("ID", "NodeID", "GPUIndexes").Where("gp_uindexes <> ''").Find(&instances).Error; err != nil {
		return err
	}
	for _, inst := range instances {
		var indexes []int
		if err := json.Unmarshal([]byte(inst.GPUIndexes), &indexes); err != nil || len(indexes) == 0 {
			continue
		}
		idents := identsForIndexes(indexes, byNode[inst.NodeID])
		if len(idents) == 0 {
			continue
		}
		raw, err := json.Marshal(idents)
		if err != nil {
			continue
		}
		if err := db.Model(&Instance{}).Where("id = ?", inst.ID).Update("GPUIdents", string(raw)).Error; err != nil {
			return err
		}
	}
	return nil
}
