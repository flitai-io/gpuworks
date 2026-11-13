package kube

import (
	"context"
	"fmt"
	"net/url"
	"sort"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/podresources"
)

// What the hold pods on the node say about who holds which card, and what
// happens when the node changes how it allocates cards while the platform
// holds some.
//
// A hold pod is only a claim in the books of whatever allocated it. Switch a
// node from a plain device plugin to ACK GPU sharing, say, and the plugin's
// books are gone: the sharing scheduler counts GiB, has never heard of
// nvidia.com/gpu, and gives the card a hold pod still holds to another pod.
// The platform cannot undo that, so it must not go on as if the card were
// its own: the cards of hold pods created for another mode are stale, they
// are reported as used outside the platform with that reason, nothing is
// borrowed in the old mode any more, and the hold pods are deleted as soon
// as no instance uses their card.

// staleHold is a card held by a hold pod created for another mode than the
// node's now.
type staleHold struct {
	pod string
	sig string // the mode it was created for (cardView.signature)
}

// holdScan is the hold pods of the node sorted by what they hold.
type holdScan struct {
	held    map[string]string    // card -> hold pod, created for the node's mode
	stale   map[string]staleHold // card -> hold pod created for another mode
	orphans []string             // hold pods of the node's mode holding no whole card
}

// holdPods lists the hold pods of this node. It reads the API server's
// current state, not its watch cache: the pods it asks about were created
// moments ago.
func (p *cardPool) holdPods(ctx context.Context) ([]kubeapi.Pod, error) {
	var list kubeapi.PodList
	q := "?labelSelector=" + url.QueryEscape(LabelRole+"="+RoleHold+","+LabelNode+"="+p.node)
	if err := p.api.Get(ctx, kubeapi.PodsPath(p.ns)+q, &list); err != nil {
		return nil, fmt.Errorf("list hold pods: %w", err)
	}
	return list.Items, nil
}

// scan sorts the node's hold pods by whether they were created for the node's
// mode, and reads which card each holds: from the kubelet's allocations for a
// device plugin's pods, from the annotations HAMi or ACK's scheduler wrote
// for theirs.
func (p *cardPool) scan(ctx context.Context, v cardView) (holdScan, error) {
	out := holdScan{held: map[string]string{}, stale: map[string]staleHold{}}
	pods, err := p.holdPods(ctx)
	if err != nil {
		return out, err
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return out, err
	}
	var allocs []podresources.Allocation
	loaded := false
	sig := v.signature()
	for _, pod := range pods {
		name := pod.Metadata.Name
		if pod.Metadata.DeletionTimestamp != nil {
			continue // being returned
		}
		psig := holdSignature(pod)
		var cards []string
		switch kind := signatureKind(psig); kind {
		case "hami":
			devs, err := hamiAllocations(pod)
			if err != nil {
				p.log.Warn("a hold pod's HAMi allocation cannot be read", "pod", name, "err", err)
			}
			for _, d := range devs {
				cards = append(cards, d.uuid)
			}
		case "ack":
			alloc, err := ackAllocation(pod)
			if err != nil {
				p.log.Warn("a hold pod's ACK GPU sharing allocation cannot be read", "pod", name, "err", err)
			}
			for idx := range alloc {
				if uuid := byIndex[idx]; uuid != "" {
					cards = append(cards, uuid)
				}
			}
		default:
			if p.kubelet != nil && !loaded {
				if allocs, err = p.kubelet.List(ctx); err != nil {
					return out, err
				}
				loaded = true
			}
			seen := map[string]bool{}
			for _, a := range allocs {
				if a.Namespace == pod.Metadata.Namespace && a.Pod == name {
					if card := p.cardOf(ctx, a.DeviceID, byIndex); card != "" && !seen[card] {
						seen[card] = true
						cards = append(cards, card)
					}
				}
			}
		}
		sort.Strings(cards)
		if psig != sig {
			for _, card := range cards {
				out.stale[card] = staleHold{pod: name, sig: psig}
			}
			continue
		}
		whole := len(cards) > 0
		switch {
		case v.mode == modeHAMi && whole:
			if len(cards) != 1 || v.hami.verifyWhole(pod, cards[0]) != nil {
				whole = false
				if len(cards) == 1 {
					p.log.Warn("HAMi did not give a hold pod a whole card", "pod", name, "err", v.hami.verifyWhole(pod, cards[0]))
				}
			}
		case v.mode == modeACK && whole:
			card, err := v.ack.holdCard(pod, byIndex)
			cards, whole = []string{card}, err == nil
			if err != nil {
				p.log.Warn("ACK GPU sharing did not give a hold pod a whole card", "pod", name, "err", err)
			}
		}
		if !whole {
			if v.mode == modeHAMi || v.mode == modeACK {
				out.orphans = append(out.orphans, name)
			}
			continue
		}
		for _, card := range cards {
			out.held[card] = name
		}
	}
	return out, nil
}

// signatureKind is the family of a hold pod's signature.
func signatureKind(sig string) string {
	switch {
	case sig == "ack-gpushare":
		return "ack"
	case len(sig) > 5 && sig[:5] == "hami:":
		return "hami"
	}
	return "plugin"
}

// syncHeld rereads the hold pods after a borrow and records the cards they
// hold.
func (p *cardPool) syncHeld(ctx context.Context, v cardView) error {
	s, err := p.scan(ctx, v)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setHeld(s.held)
	p.stale = s.stale
	return nil
}

// resync rebuilds which cards the platform holds, and which hold pods are
// stale, from the hold pods on the node. The first time, hold pods that hold
// nothing are leftovers of an agent that stopped mid-borrow and are deleted
// (no borrow is running then).
func (p *cardPool) resync(ctx context.Context, first bool) error {
	s, err := p.scan(ctx, p.view)
	if err != nil {
		return err
	}
	if first {
		p.deletePods(ctx, s.orphans)
	}
	p.setHeld(s.held)
	for card, st := range s.stale {
		if p.stale[card].pod != st.pod {
			p.log.Warn("the node's cards are allocated differently than when the platform borrowed this one; Kubernetes may give it to another pod",
				"card", card, "hold_pod", st.pod, "was", describeSignature(st.sig), "now", describeSignature(p.view.signature()))
		}
	}
	p.stale = s.stale
	return nil
}

// isStale says the card is held for another mode than the node's now and
// the platform holds it in no way that is good now: when a hold pod of the
// node's mode holds it too (the node's scheduler counted the card to a new
// hold pod), it is the platform's again.
func (p *cardPool) isStale(card string) bool {
	_, ok := p.stale[card]
	return ok && p.held[card] == ""
}

// staleReason is why a card held for another mode is reported as used.
func (p *cardPool) staleReason(card string) string {
	st, ok := p.stale[card]
	if !ok {
		return ""
	}
	return fmt.Sprintf("the node's cards were allocated by %s when the platform borrowed this one and are by %s now; Kubernetes may give the card to another pod while the platform's hold pod %s still names it. The platform stops using it once no instance runs on it",
		describeSignature(st.sig), describeSignature(p.view.signature()), st.pod)
}
