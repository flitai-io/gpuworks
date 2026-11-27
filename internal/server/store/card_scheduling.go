package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// An operator's say over one card, the way NodeScheduling is over a node.
//
// Both enabling and disabling scheduling persist until the operator changes
// their decision. Reports keep health and outside-use facts current without
// revoking that decision. Ownership remains a hard boundary.

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
// named by its identity (accelerator.CardIdent). Enabling it records explicit
// intent even when it is healthy now, so later reports cannot revoke it.
// A card that is not this node's to use cannot be enabled, nor can one a
// card holder holds (ErrCardHeld, wrapping the hold's product error); a
// held card may still be stopped.
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
		if src, hold := s.admission.cardHoldOf(cardHoldFacts(nodeHoldFacts(node), &card)); src != SchedulingBySelf {
			return nil, fmt.Errorf("%w: %w", ErrCardHeld, hold.ProductError())
		}
		updates["scheduling_allowed_at"] = time.Now().UTC()
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

// SchedulingActions are what an operator may do to the card now: stop it
// (unless they already have), or make it schedulable (when they stopped it,
// or the platform's verdict keeps work off it) unless a card holder holds
// it. The server answers this so a client never works it out (frontend
// makes no business decisions).
func (g GPU) SchedulingActions() []string {
	if !accelerator.CardUsable(g.Ownership) {
		return nil
	}
	var actions []string
	if !g.SchedulingStopped {
		actions = append(actions, string(CardSchedulingStopped))
	}
	if !g.HeldByHolder() && (g.SchedulingStopped || !g.CanTakeWork()) {
		actions = append(actions, string(CardSchedulingSchedulable))
	}
	return actions
}
