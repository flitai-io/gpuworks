package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Borrowing cards through ACK GPU sharing (ACK Pro's scheduler-side sharing
// of one card by several pods, "cgpu").
//
// The node's cards are counted in GiB of memory (aliyun.com/gpu-mem, with
// aliyun.com/gpu-count cards behind it) and the nvidia.com/gpu plugin gives
// out nothing. The hold pod asks for a whole card's memory through the
// default scheduler; no way of naming the card is known, so which one the
// pod got is read from the allocation the scheduler writes on it,
// scheduler.framework.gpushare.allocation = {"<container>":{"<card>":GiB}},
// and the card number is mapped to a UUID through the node's own probe. As
// with a plain device plugin, borrowing takes every card still free, keeps
// the wanted ones and returns the rest.

const ackAllocationAnnotation = "scheduler.framework.gpushare.allocation"

// ackNode is what ACK's sharing says about the node's cards.
type ackNode struct {
	cardGiB int  // memory of one card
	cards   int  // how many cards
	core    bool // the node also hands out compute shares (core_mem policy)
}

// isACKSharing says the node is under ACK's GPU sharing: it advertises GiB of
// card memory, or is labelled with a sharing policy.
func isACKSharing(node kubeapi.Node, capacity map[string]string) bool {
	if n, _ := strconv.Atoi(capacity[resACKMem]); n > 0 {
		return true
	}
	switch node.Metadata.Labels[ackScheduleLabel] {
	case "share", "cgpu", "core_mem":
		return true
	}
	return false
}

// classifyACK works out the memory of one card and checks the cards are what
// whole-card borrowing needs: all of one size, as many as the node's probe
// finds.
func (p *cardPool) classifyACK(ctx context.Context, node kubeapi.Node, capacity map[string]string, v cardView) (cardView, error) {
	refuse := func(format string, args ...any) (cardView, error) {
		v.mode, v.shared = modeFractional, fmt.Sprintf(format, args...)+"; the platform borrows whole cards only"
		return v, nil
	}
	mem, _ := strconv.Atoi(capacity[resACKMem])
	count, _ := strconv.Atoi(capacity[resACKCount])
	if mem <= 0 || count <= 0 {
		return refuse("ACK GPU sharing is on for this node (%s=%s) but it advertises %s=%q and %s=%q, which the platform needs to ask for a whole card",
			ackScheduleLabel, node.Metadata.Labels[ackScheduleLabel], resACKMem, capacity[resACKMem], resACKCount, capacity[resACKCount])
	}
	if mem%count != 0 {
		return refuse("ACK GPU sharing counts %d GiB on %d cards, which is not a whole number of GiB per card, so the cards are not of one size", mem, count)
	}
	a := &ackNode{cardGiB: mem / count, cards: count}
	if label := node.Metadata.Labels[ackMemLabel]; label != "" {
		mib, err := strconv.Atoi(strings.TrimSuffix(label, "MiB"))
		if err != nil || mib/1024 != a.cardGiB {
			return refuse("ACK GPU sharing counts %d GiB per card but the node's %s label says %q", a.cardGiB, ackMemLabel, label)
		}
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return cardView{}, err
	}
	if len(byIndex) > 0 && len(byIndex) != count {
		return refuse("ACK GPU sharing counts %d cards on this node but the node's probe finds %d", count, len(byIndex))
	}
	if n, _ := strconv.Atoi(capacity[resACKCore]); n > 0 {
		a.core = true
	}
	v.mode, v.resource, v.ack = modeACK, resACKMem, a
	return v, nil
}

// ackHoldPod is the hold pod for one whole card: the default scheduler picks
// the node (by hostname) and the card.
func (p *cardPool) ackHoldPod(v cardView, name string) kubeapi.Pod {
	pod := p.baseHoldPod(name)
	pod.Spec.NodeSelector = map[string]string{labelHostname: v.hostname}
	limits := map[string]string{resACKMem: strconv.Itoa(v.ack.cardGiB)}
	if v.ack.core {
		limits[resACKCore] = "100"
	}
	pod.Spec.Containers[0].Resources.Limits = limits
	return pod
}

// ackAllocation is the memory, in GiB, the scheduler gave a pod on each card
// of the node, by card number.
func ackAllocation(pod kubeapi.Pod) (map[int]int, error) {
	raw := pod.Metadata.Annotations[ackAllocationAnnotation]
	if raw == "" || pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
		return nil, nil
	}
	var byContainer map[string]map[string]int
	if err := json.Unmarshal([]byte(raw), &byContainer); err != nil {
		return nil, fmt.Errorf("%s: %w", ackAllocationAnnotation, err)
	}
	out := map[int]int{}
	for _, cards := range byContainer {
		for idx, gib := range cards {
			n, err := strconv.Atoi(idx)
			if err != nil {
				return nil, fmt.Errorf("%s: card %q is not a number", ackAllocationAnnotation, idx)
			}
			out[n] += gib
		}
	}
	return out, nil
}

// holdCard says which card a hold pod got and checks it is a whole one.
func (a *ackNode) holdCard(pod kubeapi.Pod, byIndex map[int]string) (string, error) {
	alloc, err := ackAllocation(pod)
	if err != nil {
		return "", err
	}
	if len(alloc) != 1 {
		return "", fmt.Errorf("the scheduler gave the hold pod %d cards instead of one", len(alloc))
	}
	for idx, gib := range alloc {
		uuid := byIndex[idx]
		switch {
		case uuid == "":
			return "", fmt.Errorf("the scheduler gave the hold pod card %d, which the node's probe does not know", idx)
		case gib != a.cardGiB:
			return "", fmt.Errorf("the scheduler gave the hold pod only %d of card %d's %d GiB", gib, idx, a.cardGiB)
		default:
			return uuid, nil
		}
	}
	return "", nil
}

// borrowACK takes every card no pod uses, keeps the wanted ones and returns
// the rest. The caller holds borrowMu and not mu.
func (p *cardPool) borrowACK(ctx context.Context, v cardView, want []string) error {
	byIndex, err := p.cards(ctx)
	if err != nil {
		return err
	}
	holders, err := p.externalHolders(ctx, v, true)
	if err != nil {
		return err
	}
	p.mu.Lock()
	var free int
	for _, uuid := range byIndex {
		if len(holders[uuid]) == 0 && p.held[uuid] == "" {
			free++
		}
	}
	p.mu.Unlock()
	if free == 0 {
		return p.notGiven(ctx, v, p.unheld(want), nil, true)
	}
	var names []string
	for i := 0; i < free; i++ {
		name := holdPrefix + randomSuffix()
		if err := p.api.Create(ctx, kubeapi.PodsPath(p.ns), p.ackHoldPod(v, name), nil); err != nil {
			p.deletePods(ctx, names)
			return fmt.Errorf("create hold pod: %w", err)
		}
		names = append(names, name)
	}
	outcomes := p.waitScheduled(ctx, names)
	why := map[string]string{}
	var drop []string
	var reasons []string
	for _, name := range names {
		if o := outcomes[name]; !o.running {
			drop = append(drop, name)
			reasons = append(reasons, o.why)
		}
	}
	p.deletePods(ctx, drop)
	if err := p.syncHeld(ctx, v); err != nil {
		return err
	}
	still := p.unheld(want)
	keep := map[string]bool{}
	for _, card := range want {
		keep[card] = true
	}
	p.returnUnused(ctx, keep)
	if len(still) == 0 {
		return nil
	}
	sort.Strings(reasons)
	for _, card := range still {
		if len(reasons) > 0 {
			why[card] = reasons[0]
		}
	}
	return p.notGiven(ctx, v, still, why, false)
}

// ackHolders are the pods other than the platform's hold pods the scheduler
// gave memory of a card of this node to, keyed by card and then by pod.
func (p *cardPool) ackHolders(pods []kubeapi.Pod, byIndex map[int]string, into map[string]map[string]proto.CardProcess) {
	for _, pod := range pods {
		if p.isHoldPod(pod) {
			continue
		}
		alloc, err := ackAllocation(pod)
		if err != nil {
			p.log.Warn("a pod's ACK GPU sharing allocation cannot be read", "pod", pod.Metadata.Namespace+"/"+pod.Metadata.Name, "err", err)
			continue
		}
		for idx, gib := range alloc {
			if uuid := byIndex[idx]; uuid != "" {
				put(into, uuid, pod, proto.CardProcess{
					Name: "Pod " + pod.Metadata.Namespace + "/" + pod.Metadata.Name + " (" + strconv.Itoa(gib) + " GiB)", MemoryBytes: int64(gib) << 30,
				})
			}
		}
	}
}
