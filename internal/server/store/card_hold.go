package store

import (
	"errors"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// A card holder is a scheduling holder for single cards: while it holds a
// card, the card takes no new work (GPU.CanTakeWork is false) and its row
// records the holder's source (GPU.SchedulingHeldBy); what runs there keeps
// running. It is the per-card counterpart of SchedulingHolder, with the
// same rules: a hold outranks the platform's verdict and an operator's
// enable, and ranks below an operator's stop, so an operator may still stop
// a held card but cannot make it schedulable while it is held
// (ErrCardHeld).
//
// A hold is not stored: it is the holder's answer for the card's current
// facts, asked again on every status report that writes the node's cards
// and on every ApplySchedulingHolds, so a card whose hold ends is released
// by the next one. Only its effect, the source on the card's row, is: the
// readers that count cards in SQL (OnlineGPUGroups) read it from there.

// CardHoldFacts is what a card holder decides on: the node the card is in
// and the card's own identity.
type CardHoldFacts struct {
	NodeID        uint
	NodeUUID      string
	NodeCreatedAt time.Time
	// Ident is the card's identity, the key its row is stored under
	// (accelerator.CardIdent: its hardware identity, or "index:<n>" for a
	// prober that reports none). It does not change while the card stays
	// in the node.
	Ident string
	// Index is the vendor CLI's device index, -1 when it lists none; it
	// is renumbered when the card set changes, so it is shown, not keyed.
	Index  int
	Vendor string
	// Ownership is empty for a card this node may use, otherwise why not
	// (accelerator.Card*). A card that is not the node's own takes no work
	// whatever a holder answers.
	Ownership string
}

// CardHold is a holder's answer for one card. The zero value does not hold
// it. Like NodeHold it is shown to an operator as a product message (ADR
// 0067); an empty code is shown as CARD_SCHEDULING_HELD.
type CardHold struct {
	Code     string
	Fallback string
	Params   map[string]any
}

// Held reports whether h holds the card.
func (h CardHold) Held() bool { return h.Code != "" || h.Fallback != "" || len(h.Params) > 0 }

// ProductError is the hold as an operator is told it.
func (h CardHold) ProductError() *messages.ProductError {
	code, fallback := h.Code, h.Fallback
	if code == "" {
		code = messages.ErrorCardSchedulingHeld
	}
	if fallback == "" {
		fallback = "the card is held out of scheduling"
	}
	return messages.NewProductError(code, fallback, h.Params)
}

// CardSchedulingHolder holds single cards out of scheduling.
type CardSchedulingHolder interface {
	// Source names the holder; it is what GPU.SchedulingHeldBy records
	// for a card it holds. The same pattern as SchedulingHolder.Source;
	// a module that holds nodes and cards may use one source for both.
	Source() SchedulingSource
	// HoldCard answers for one card. It runs inside the writer's
	// transactions (every status report of every node, once per card),
	// which is a single connection: it must be pure, O(1) and
	// allocation-free when it does not hold, and never call the Store.
	HoldCard(f CardHoldFacts) CardHold
}

// ErrCardHeld is wrapped, with the hold's *messages.ProductError, by
// SetCardScheduling asked to make a held card schedulable.
var ErrCardHeld = errors.New("card is held out of scheduling")

// AddCardSchedulingHolder registers h. Card holders are asked in
// registration order and the first that holds a card owns it. A second card
// holder with the same source is refused with ErrHolderConflict.
func (s *Store) AddCardSchedulingHolder(h CardSchedulingHolder) error {
	if h == nil {
		return errors.New("card scheduling holder is nil")
	}
	src := h.Source()
	if !holderSourcePattern.MatchString(string(src)) || src == SchedulingByOperator || src == SchedulingByPlatform {
		return fmt.Errorf("card scheduling holder source %q: want 1-16 of [a-z0-9_] starting with a letter, other than operator and platform", src)
	}
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	current := s.admission.cardSchedulingHolders()
	for _, other := range current {
		if other.Source() == src {
			return fmt.Errorf("%w: %s", ErrHolderConflict, src)
		}
	}
	next := append(append([]CardSchedulingHolder(nil), current...), h)
	s.admission.cardHolders.Store(&next)
	return nil
}

func (a *nodeAdmission) cardSchedulingHolders() []CardSchedulingHolder {
	if p := a.cardHolders.Load(); p != nil {
		return *p
	}
	return nil
}

// cardHoldOf is the first registered card holder's hold on the card, and
// its source; "" when none holds it.
func (a *nodeAdmission) cardHoldOf(f CardHoldFacts) (SchedulingSource, CardHold) {
	for _, h := range a.cardSchedulingHolders() {
		if hold := h.HoldCard(f); hold.Held() {
			return h.Source(), hold
		}
	}
	return SchedulingBySelf, CardHold{}
}

// registeredCardSource reports whether src is a registered card holder's.
func (a *nodeAdmission) registeredCardSource(src SchedulingSource) bool {
	for _, h := range a.cardSchedulingHolders() {
		if h.Source() == src {
			return true
		}
	}
	return false
}

// cardHoldSource is the source that holds the card now, or "" when none
// does: what a write of the card stores in GPU.SchedulingHeldBy.
func (a *nodeAdmission) cardHoldSource(node NodeHoldFacts, g *GPU) SchedulingSource {
	src, _ := a.cardHoldOf(cardHoldFacts(node, g))
	return src
}

func cardHoldFacts(node NodeHoldFacts, g *GPU) CardHoldFacts {
	return CardHoldFacts{NodeID: node.ID, NodeUUID: node.UUID, NodeCreatedAt: node.CreatedAt,
		Ident: g.Ident, Index: g.Index, Vendor: g.Vendor, Ownership: g.Ownership}
}

// CardHoldFor is the registered card holders' answer for a card with these
// facts, and the source of the holder that holds it ("" when none does):
// what a status report or a hold pass asks inside the writer's
// transaction. It reads nothing.
func (s *Store) CardHoldFor(f CardHoldFacts) (SchedulingSource, CardHold) {
	return s.admission.cardHoldOf(f)
}

// CardHoldOf is why a card holder keeps g out of scheduling, for showing
// it: the hold its holder answers now, or the generic hold while its row
// says it is held and a pass has not yet released it; nil when the row
// says nobody holds it. It reads nothing.
func (s *Store) CardHoldOf(n *Node, g GPU) *messages.ProductError {
	if !g.HeldByHolder() {
		return nil
	}
	for _, h := range s.admission.cardSchedulingHolders() {
		if h.Source() != g.SchedulingHeldBy {
			continue
		}
		if hold := h.HoldCard(cardHoldFacts(nodeHoldFacts(n), &g)); hold.Held() {
			return hold.ProductError()
		}
	}
	return CardHold{}.ProductError()
}

// HeldByHolder is whether a card holder keeps the card out of scheduling,
// as its row records.
func (g GPU) HeldByHolder() bool { return g.SchedulingHeldBy != SchedulingBySelf }
