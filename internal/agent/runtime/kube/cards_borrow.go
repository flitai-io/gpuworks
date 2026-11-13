package kube

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// The slow half of borrowing: creating hold pods, waiting for the kubelet
// to admit them, giving back the ones that are not needed and waiting for
// the kubelet to free their cards. None of it runs under cardPool.mu, which
// guards the pool's bookkeeping and is taken by status reporting; borrows
// are serialized by borrowMu instead.

const (
	// releaseWait bounds how long returning cards waits for the kubelet to
	// free them: a deleted pod's devices stay allocated until its containers
	// are gone, and a borrow that began before then would find no free card.
	defaultReleaseWait = 30 * time.Second
	// borrowRetry bounds how long acquire keeps borrowing when the kubelet
	// did not give a wanted card (one still being freed by an earlier
	// return or an idle sweep).
	defaultBorrowRetry = 15 * time.Second
)

// errCardsNotGiven is a borrow that ended with some wanted cards not held.
type errCardsNotGiven struct {
	cards   []string
	refused string // the kubelet's reason when it refused hold pods; "" when another pod won the race
	// detail replaces the default account of why, where a scheduler gave the
	// reason (HAMi, ACK GPU sharing).
	detail string
	// final: trying again will not help (another pod holds the card, or the
	// scheduler gave something else than asked).
	final bool
}

func (e *errCardsNotGiven) Error() string {
	why := "another pod took them first"
	switch {
	case e.detail != "":
		why = e.detail
	case e.refused != "":
		why = "the kubelet refused the hold pods: " + e.refused
	}
	return fmt.Sprintf("Kubernetes did not give this node's cards %s to the platform: %s", strings.Join(e.cards, ","), why)
}

// retryable: a shortage that is not a refusal may be a card the kubelet or
// scheduler had not yet freed.
func (e *errCardsNotGiven) retryable() bool { return e.refused == "" && !e.final }

// borrowWithRetry borrows the cards in want. A card the kubelet had not yet
// freed shows up on a later attempt, so a shortage that is not a refusal is
// tried again for a bounded time.
func (p *cardPool) borrowWithRetry(ctx context.Context, want []string) error {
	p.borrowMu.Lock()
	defer p.borrowMu.Unlock()
	limit := p.borrowRetry
	if limit <= 0 {
		limit = defaultBorrowRetry
	}
	deadline := time.Now().Add(limit)
	for {
		err := p.borrow(ctx, want)
		var short *errCardsNotGiven
		if err == nil || !errors.As(err, &short) || !short.retryable() || !time.Now().Before(deadline) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(p.poll):
		}
	}
}

// borrow takes the wanted cards that no hold pod holds, the way the node's
// mode allows: through a plain device plugin every free card is taken, the
// wanted ones kept and the rest returned; through HAMi the wanted cards are
// named; through ACK GPU sharing the free cards are taken like a plugin's.
// The caller holds borrowMu and not mu.
func (p *cardPool) borrow(ctx context.Context, want []string) error {
	if len(p.unheld(want)) == 0 {
		return nil
	}
	v, err := p.refresh(ctx)
	if err != nil {
		return err
	}
	if v.shared != "" {
		return errors.New(v.shared)
	}
	if err := p.checkStale(want); err != nil {
		return err
	}
	switch v.mode {
	case modeHAMi:
		return p.borrowHAMi(ctx, v, want)
	case modeACK:
		return p.borrowACK(ctx, v, want)
	}
	if err := p.checkHolders(ctx, want); err != nil {
		return err
	}
	_, refused, err := p.takeAllFree(ctx)
	if err != nil {
		return err
	}
	still := p.unheld(want)
	keep := map[string]bool{}
	for _, card := range want {
		keep[card] = true
	}
	p.returnUnused(ctx, keep)
	if len(still) > 0 {
		return &errCardsNotGiven{cards: still, refused: refused}
	}
	return nil
}

// checkStale fails when a wanted card is held for another mode than the
// node's now: the node changed under it, and borrowing it again as if it were
// the platform's is what must not happen.
func (p *cardPool) checkStale(want []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, card := range want {
		if p.isStale(card) {
			return errors.New("card " + card + " cannot be borrowed: " + p.staleReason(card))
		}
	}
	return nil
}

// unheld are the cards in want the platform does not hold.
func (p *cardPool) unheld(want []string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, card := range want {
		if p.held[card] == "" {
			out = append(out, card)
		}
	}
	return out
}

// currentResource is the device plugin resource the node advertises.
func (p *cardPool) currentResource() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.view.resource
}

// checkHolders fails fast when a wanted card is allocated to a pod that is
// not the platform's.
func (p *cardPool) checkHolders(ctx context.Context, want []string) error {
	holders, err := p.externalHolders(ctx, p.snapshot(), true)
	if err != nil {
		return err
	}
	for _, card := range want {
		if h := holders[card]; len(h) > 0 {
			return fmt.Errorf("card %s is allocated to %s by Kubernetes", card, h[0].Name)
		}
	}
	return nil
}

// takeAllFree creates one hold pod per free card and waits for the kubelet
// to admit them; it returns how many it got and, when some were refused,
// the kubelet's reason (node pressure, a failed allocation).
func (p *cardPool) takeAllFree(ctx context.Context) (int, string, error) {
	resource := p.currentResource()
	free, err := p.freeCount(ctx, resource)
	if err != nil || free <= 0 {
		return 0, "", err
	}
	names := make([]string, 0, free)
	for i := 0; i < free; i++ {
		name := holdPrefix + randomSuffix()
		if err := p.api.Create(ctx, kubeapi.PodsPath(p.ns), p.holdPod(name, resource), nil); err != nil {
			p.deletePods(ctx, names)
			return 0, "", fmt.Errorf("create hold pod: %w", err)
		}
		names = append(names, name)
	}
	admitted, refused := p.waitAdmitted(ctx, names)
	var failed []string
	for _, n := range names {
		if !admitted[n] {
			failed = append(failed, n)
		}
	}
	p.deletePods(ctx, failed)
	held, err := p.listHeld(ctx, resource)
	if err != nil {
		return 0, "", err
	}
	p.mu.Lock()
	p.setHeld(held)
	p.mu.Unlock()
	return len(names) - len(failed), refused, nil
}

// freeCount is how many of the advertised cards no pod holds.
func (p *cardPool) freeCount(ctx context.Context, resource string) (int, error) {
	var node kubeapi.Node
	if err := p.api.Get(ctx, kubeapi.NodePath(p.node), &node); err != nil {
		return 0, err
	}
	total, _ := strconv.Atoi(node.Status.Allocatable[resource])
	allocs, err := p.kubelet.List(ctx)
	if err != nil {
		return 0, err
	}
	used := map[string]bool{}
	for _, a := range allocs {
		if a.Resource == resource {
			used[a.DeviceID] = true
		}
	}
	return total - len(used), nil
}

func (p *cardPool) holdPod(name, resource string) kubeapi.Pod {
	pod := p.baseHoldPod(name)
	pod.Spec.NodeName = p.node
	c := &pod.Spec.Containers[0]
	c.Resources.Requests[resource] = "1"
	c.Resources.Limits = map[string]string{resource: "1"}
	return pod
}

// waitAdmitted waits until each hold pod runs or the kubelet refused it, and
// returns the first refusal's reason.
func (p *cardPool) waitAdmitted(ctx context.Context, names []string) (map[string]bool, string) {
	out, refused := map[string]bool{}, ""
	pending := map[string]bool{}
	for _, n := range names {
		pending[n] = true
	}
	deadline := p.now().Add(60 * time.Second)
	for len(pending) > 0 && p.now().Before(deadline) && ctx.Err() == nil {
		for n := range pending {
			var pod kubeapi.Pod
			err := p.api.Get(ctx, kubeapi.PodPath(p.ns, n), &pod)
			switch {
			case err != nil && kubeapi.IsNotFound(err):
				delete(pending, n)
			case err != nil:
			case pod.Status.Phase == "Running":
				out[n] = true
				delete(pending, n)
			case pod.Status.Phase == "Failed":
				// A failed device allocation is the race borrow already
				// describes; anything else (eviction under node pressure)
				// is the reason worth reporting.
				if refused == "" && pod.Status.Reason != "UnexpectedAdmissionError" {
					refused = strings.TrimSpace(pod.Status.Reason + ": " + pod.Status.Message)
				}
				delete(pending, n)
			}
		}
		if len(pending) > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(p.poll):
			}
		}
	}
	return out, refused
}

func (p *cardPool) deletePods(ctx context.Context, names []string) {
	zero := int64(0)
	for _, n := range names {
		if err := p.api.Delete(ctx, kubeapi.PodPath(p.ns, n), &zero); err != nil {
			p.log.Warn("delete hold pod failed", "pod", n, "err", err)
		}
	}
}

// returnUnused deletes the hold pods of borrowed cards nobody uses, keeping
// the ones in keep, and waits until the kubelet has freed their cards: the
// next borrow counts a card free only once no pod is allocated it.
func (p *cardPool) returnUnused(ctx context.Context, keep map[string]bool) {
	p.mu.Lock()
	var pods []string
	for card, pod := range p.held {
		if keep[card] || len(p.users[card]) > 0 || p.pins[card] > 0 {
			continue
		}
		if _, idle := p.idleSince[card]; idle {
			continue // borrowed earlier and still in its grace period
		}
		pods = append(pods, pod)
		p.returning[pod] = true
		delete(p.held, card)
	}
	p.mu.Unlock()
	p.deletePods(ctx, pods)
	p.waitReleased(ctx, pods)
}

// waitReleased waits, for a bounded time, until the kubelet no longer lists
// an allocation to any of the deleted hold pods.
func (p *cardPool) waitReleased(ctx context.Context, pods []string) {
	if len(pods) == 0 {
		return
	}
	if v := p.snapshot(); v.mode == modeHAMi || v.mode == modeACK {
		p.waitGone(ctx, pods) // their schedulers count a pod until it is gone
		return
	}
	gone := map[string]bool{}
	for _, n := range pods {
		gone[n] = true
	}
	limit := p.releaseWait
	if limit <= 0 {
		limit = defaultReleaseWait
	}
	deadline := time.Now().Add(limit)
	for {
		allocs, err := p.kubelet.List(ctx)
		if err == nil {
			holding := false
			for _, a := range allocs {
				if a.Namespace == p.ns && gone[a.Pod] {
					holding = true
					break
				}
			}
			if !holding {
				return
			}
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			p.log.Warn("the kubelet has not freed the returned cards yet", "pods", pods, "err", err)
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(p.poll):
		}
	}
}
