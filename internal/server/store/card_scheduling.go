package store

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// An operator's say over one card, the way NodeScheduling is over a node.
//
// Two decisions, both the operator's and both above the platform's verdict
// (docs/adr/0060-card-health-from-hardware-facts.md):
//
//   - Stopping a card's scheduling holds until the operator says otherwise.
//     Nothing the platform sees lifts it.
//   - Putting a card the platform judged unusable (a fault, or a process
//     outside the platform) back into scheduling lets work onto it until the
//     next report at least cardAllowanceWindow later, which judges the card
//     afresh. That is long enough to place a deployment by hand and see
//     whether the card really cannot serve; a card still faulty is stopped
//     again by that report, a card that recovered simply stays schedulable.

// cardAllowanceWindow is the least time an operator's allowance lasts. A
// report every minute would otherwise leave anything from a second to a
// minute, depending on when the operator clicked.
const cardAllowanceWindow = time.Minute

// ErrInvalidCardScheduling and ErrCardNotOwned are the refusals of
// SetCardScheduling.
var (
	ErrInvalidCardScheduling = errors.New("card scheduling is not one of stopped, schedulable")
	ErrCardNotOwned          = errors.New("the card is not this node's to use")
)

// CardScheduling is what an operator sets a card to.
type CardScheduling string

const (
	CardSchedulingStopped     CardScheduling = "stopped"
	CardSchedulingSchedulable CardScheduling = "schedulable"
)

// Valid reports whether s is one of the card scheduling states.
func (s CardScheduling) Valid() bool {
	return s == CardSchedulingStopped || s == CardSchedulingSchedulable
}

// SetCardScheduling records an operator's decision about one card of a node,
// named by its identity (accelerator.CardIdent). Making it schedulable lifts
// an operator's stop, and lets work onto a card the platform judged unusable
// until a report re-judges it; a card the platform already considers usable
// needs no allowance. A card that is not this node's to use (lent to a
// virtual machine, left without a driver) cannot be allowed.
func (s *Store) SetCardScheduling(nodeUUID, ident string, to CardScheduling) (*GPU, error) {
	if !to.Valid() {
		return nil, ErrInvalidCardScheduling
	}
	node, err := s.NodeByUUID(nodeUUID)
	if err != nil {
		return nil, err
	}
	var card GPU
	if err := s.db.Where("node_id = ? AND ident = ?", node.ID, ident).First(&card).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	updates := map[string]any{"scheduling_stopped": to == CardSchedulingStopped, "scheduling_allowed_at": nil}
	if to == CardSchedulingSchedulable {
		if !accelerator.CardUsable(card.Ownership) {
			return nil, ErrCardNotOwned
		}
		if !accelerator.CardSchedulable(card.Ownership, card.HealthStatus, card.HeldOutsidePlatform()) {
			updates["scheduling_allowed_at"] = time.Now().UTC()
		}
	}
	if err := s.db.Model(&GPU{}).Where("id = ?", card.ID).Updates(updates).Error; err != nil {
		return nil, err
	}
	// Placing work reads the cards through the node: every deployment
	// waiting for room is reconciled, as when a node is uncordoned.
	s.publishNode(nodeUUID, NodeChangeScheduling)
	if err := s.db.First(&card, card.ID).Error; err != nil {
		return nil, err
	}
	return &card, nil
}

// expireCardAllowances clears the allowances a report outlives: the report
// judges those cards afresh. It costs a write only on a node an operator
// recently allowed a card on.
func expireCardAllowances(tx *gorm.DB, cards []GPU, now time.Time) (expired bool, err error) {
	var ids []uint
	for _, c := range cards {
		if c.SchedulingAllowedAt != nil && now.Sub(*c.SchedulingAllowedAt) >= cardAllowanceWindow {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return false, nil
	}
	return true, tx.Model(&GPU{}).Where("id IN ?", ids).Update("scheduling_allowed_at", nil).Error
}

// SchedulingActions are what an operator may do to the card now: stop it
// (unless they already have), or make it schedulable (when they stopped it,
// or the platform's verdict keeps work off it). The server answers this so
// a client never works it out (frontend makes no business decisions).
func (g GPU) SchedulingActions() []string {
	if !accelerator.CardUsable(g.Ownership) {
		return nil
	}
	var actions []string
	if !g.SchedulingStopped {
		actions = append(actions, string(CardSchedulingStopped))
	}
	if g.SchedulingStopped || !g.CanTakeWork() {
		actions = append(actions, string(CardSchedulingSchedulable))
	}
	return actions
}
