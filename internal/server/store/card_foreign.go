package store

import (
	"encoding/json"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// ForeignBytes is what the processes outside the platform hold on this card,
// and whether that is known: zero and known when nobody holds it. It is not
// known when the list cannot be read, when the vendor tool gave a process no
// figure, or when a holder is not a process at all (a Kubernetes pod the
// kubelet gave the card to, reported without a PID): what such a holder may
// grow to is not what it holds now.
func (g GPU) ForeignBytes() (int64, bool) {
	if g.ForeignProcesses == "" {
		return 0, true
	}
	var procs []proto.CardProcess
	if err := json.Unmarshal([]byte(g.ForeignProcesses), &procs); err != nil || len(procs) == 0 {
		return 0, false
	}
	var total int64
	for _, proc := range procs {
		if proc.PID <= 0 || proc.MemoryBytes <= 0 {
			return 0, false
		}
		total += proc.MemoryBytes
	}
	return total, true
}

// SharesMemoryWithForeign is whether a memory-limited partition may share
// this card with the processes outside the platform that hold it, and what
// they hold: a card of this node's own at a level that serves, not stopped
// by an operator nor held by a card holder (card_hold.go), held, and every
// holder's memory known (ForeignBytes). It
// never makes the card take whole-card work - CanTakeWork keeps saying no -
// and whether the node's agent keeps the holders reported once a partition
// runs beside them is the planner's to ask (ADR 0068).
func (g GPU) SharesMemoryWithForeign() (int64, bool) {
	if !g.HeldOutsidePlatform() || g.SchedulingStopped || g.HeldByHolder() || !accelerator.CardHealthy(g.Ownership, g.HealthStatus) {
		return 0, false
	}
	bytes, known := g.ForeignBytes()
	if !known || bytes <= 0 {
		return 0, false
	}
	return bytes, true
}

// HeldCardsOnNodes is the cards processes outside the platform hold on these
// nodes, for what is shown beside the instances running there. It reads only
// those nodes' cards, through the node index.
func (s *Store) HeldCardsOnNodes(nodeIDs []uint) ([]GPU, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}
	var cards []GPU
	err := heldCardsQuery(s.db, nodeIDs).Find(&cards).Error
	return cards, err
}

func heldCardsQuery(db *gorm.DB, nodeIDs []uint) *gorm.DB {
	return db.Model(&GPU{}).
		Select("id", "node_id", "index", "memory_total", "ownership", "health_status", "foreign_processes").
		Where("node_id IN ? AND foreign_processes <> ''", nodeIDs)
}
