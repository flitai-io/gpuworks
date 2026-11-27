package store

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// cardHoldRow is a card and the facts of its node, as a pass over the card
// holds reads them: one query joining each card to its node.
type cardHoldRow struct {
	GPUID            uint             `gorm:"column:gpu_id"`
	NodeID           uint             `gorm:"column:node_id"`
	NodeUUID         string           `gorm:"column:node_uuid"`
	NodeCreatedAt    time.Time        `gorm:"column:node_created_at"`
	Ident            string           `gorm:"column:ident"`
	Index            int              `gorm:"column:card_index"`
	Vendor           string           `gorm:"column:vendor"`
	Ownership        string           `gorm:"column:ownership"`
	SchedulingHeldBy SchedulingSource `gorm:"column:scheduling_held_by"`
}

func (r *cardHoldRow) facts() CardHoldFacts {
	return CardHoldFacts{NodeID: r.NodeID, NodeUUID: r.NodeUUID, NodeCreatedAt: r.NodeCreatedAt,
		Ident: r.Ident, Index: r.Index, Vendor: r.Vendor, Ownership: r.Ownership}
}

// cardHoldRows reads the cards of the nodes in ids, or of every node when ids
// is nil, in node and identity order; only those a card holder's source
// marks when heldOnly.
func cardHoldRows(tx *gorm.DB, ids []uint, heldOnly bool) ([]cardHoldRow, error) {
	q := tx.Table("gpus AS g").Joins("JOIN nodes AS n ON n.id = g.node_id").
		Select("g.id AS gpu_id, n.id AS node_id, n.uuid AS node_uuid, n.created_at AS node_created_at, " +
			"g.ident AS ident, g.`index` AS card_index, g.vendor AS vendor, g.ownership AS ownership, g.scheduling_held_by AS scheduling_held_by")
	if ids != nil {
		q = q.Where("g.node_id IN ?", ids)
	}
	if heldOnly {
		q = q.Where("g.scheduling_held_by <> ''")
	}
	var rows []cardHoldRow
	err := q.Order("n.id, g.ident").Scan(&rows).Error
	return rows, err
}

// applyCardHoldsTx asks the card holders about the cards of the nodes in ids
// (every node for nil) and writes each card whose answer differs from its
// row. It returns the UUIDs of the nodes with a card that changed, once
// each. With no card holder registered it reads nothing: the passes node
// holders ask for cost what they did, and marks a former build left are
// released at boot (ReleaseUnownedHolds).
func (s *Store) applyCardHoldsTx(tx *gorm.DB, ids []uint) ([]string, error) {
	if len(s.admission.cardSchedulingHolders()) == 0 {
		return nil, nil
	}
	rows, err := cardHoldRows(tx, ids, false)
	if err != nil {
		return nil, err
	}
	var changed []string
	for i := range rows {
		r := &rows[i]
		src, _ := s.admission.cardHoldOf(r.facts())
		if src == r.SchedulingHeldBy {
			continue
		}
		if err := tx.Model(&GPU{}).Where("id = ?", r.GPUID).Update("scheduling_held_by", src).Error; err != nil {
			return nil, err
		}
		if len(changed) == 0 || changed[len(changed)-1] != r.NodeUUID {
			changed = append(changed, r.NodeUUID)
		}
	}
	return changed, nil
}

// releaseUnownedCardHoldsTx releases every card marked by a source that is
// neither a registered card holder's nor one of known, as
// ReleaseUnownedHolds does for nodes. It returns the UUIDs of the nodes
// with a card it released, once each.
func (s *Store) releaseUnownedCardHoldsTx(tx *gorm.DB, known []SchedulingSource) ([]string, error) {
	rows, err := cardHoldRows(tx, nil, true)
	if err != nil {
		return nil, err
	}
	var released []string
	for i := range rows {
		r := &rows[i]
		if s.admission.registeredCardSource(r.SchedulingHeldBy) || sourceIn(r.SchedulingHeldBy, known) {
			continue
		}
		if err := tx.Model(&GPU{}).Where("id = ?", r.GPUID).Update("scheduling_held_by", SchedulingBySelf).Error; err != nil {
			return nil, err
		}
		if len(released) == 0 || released[len(released)-1] != r.NodeUUID {
			released = append(released, r.NodeUUID)
		}
	}
	return released, nil
}

// CardFleet is every card's hold facts, ordered by node id (enrollment
// order) and then by card identity, in one query on the read pool. It is
// for a card holder that derives its answers from the whole fleet's cards:
// at boot, and for a recompute after NodeFleetTopic events (a report that
// adds, removes or changes a card's hold facts publishes NodeFactsChanged),
// coalesced; never per status report or per event, and never from inside a
// hook (CardSchedulingHolder.HoldCard).
func (s *Store) CardFleet(ctx context.Context) ([]CardHoldFacts, error) {
	rows, err := cardHoldRows(s.reader().WithContext(ctx), nil, false)
	if err != nil {
		return nil, err
	}
	out := make([]CardHoldFacts, len(rows))
	for i := range rows {
		out[i] = rows[i].facts()
	}
	return out, nil
}
