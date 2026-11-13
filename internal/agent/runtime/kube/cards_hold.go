package kube

import (
	"context"
	"fmt"
)

// holdForCheck keeps a card the platform holds from being returned to the
// kubelet while something (the per-card runtime check) uses it outside an
// instance, and says why it cannot when the card is not the platform's: a card
// the kubelet may give to another pod at any moment is not one to put a context
// on. release must be called when the use ends.
//
// A node whose cards no device plugin hands out has none of that: they are the
// platform's, and nothing is held.
func (p *cardPool) holdForCheck(ctx context.Context, card string) (release func(), why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return nil, fmt.Sprintf("which cards the platform holds on this node cannot be read: %v", err)
	}
	if p.view.shared != "" {
		return nil, p.view.shared
	}
	if p.view.resource == "" {
		return func() {}, ""
	}
	if p.isStale(card) {
		return nil, p.staleReason(card)
	}
	if p.held[card] == "" && !p.settledUser(card) {
		return nil, fmt.Sprintf("Kubernetes allocates card %s: the platform holds it only while an instance runs on it or it is kept for one, and the runtime check never asks a card Kubernetes may give to another pod", card)
	}
	if p.pins == nil {
		p.pins = map[string]int{}
	}
	p.pins[card]++
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.pins[card]--; p.pins[card] <= 0 {
			delete(p.pins, card)
		}
	}, ""
}

// settledUser says whether an instance whose pod exists uses the card.
func (p *cardPool) settledUser(card string) bool {
	for instance := range p.users[card] {
		if !p.starting[instance] {
			return true
		}
	}
	return false
}

// HoldCardForCheck keeps the card the platform holds from being returned to
// Kubernetes until release is called. A card the platform does not hold comes
// back with why, and nothing may touch it.
func (d *Driver) HoldCardForCheck(ctx context.Context, card string) (release func(), why string) {
	if d.o.Kubelet == nil {
		return func() {}, ""
	}
	return d.cards.holdForCheck(ctx, card)
}
