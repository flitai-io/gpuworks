package kube

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
)

// How the platform borrows a node's cards depends on what the cluster runs on
// that node, and it is read from the node on every load, so a plugin,
// scheduler or sharing mode installed after the agent started is noticed.
//
//   - A plain device plugin (nvidia.com/gpu, huawei.com/Ascend910, ...) hands
//     out whole cards, and the hold pod asks it for one (cards_borrow.go).
//   - HAMi (cards_hami.go) registers a card as several shares and gives a pod
//     only what its own scheduler assigned, so the hold pod goes through that
//     scheduler and names the card it wants.
//   - ACK GPU sharing (cards_ack.go) counts GiB of card memory; the hold pod
//     asks for a whole card's worth, and which card it gets is read back.
//   - Any other way of handing out parts of cards (MIG, time-slicing, vNPU
//     templates without HAMi) leaves no unit the platform could own, and the
//     node's cards are reported as used outside it.
//   - Where nothing advertises the cards, the platform simply uses them.

// cardMode is how a node's cards are allocated.
type cardMode int

const (
	// modeNone: no device plugin advertises cards.
	modeNone cardMode = iota
	// modePlugin: a device plugin hands out whole cards.
	modePlugin
	// modeHAMi: HAMi's scheduler assigns cards, and parts of them.
	modeHAMi
	// modeACK: ACK's GPU sharing scheduler assigns GiB of card memory.
	modeACK
	// modeFractional: parts of cards are handed out some other way.
	modeFractional
)

const (
	labelHostname = "kubernetes.io/hostname"
	// ackScheduleLabel is ACK's per-node GPU scheduling policy.
	ackScheduleLabel = "ack.node.gpu.schedule"
	ackMemLabel      = "aliyun.accelerator/nvidia_mem"
	resACKMem        = "aliyun.com/gpu-mem"
	resACKCount      = "aliyun.com/gpu-count"
	resACKCore       = "aliyun.com/gpu-core.percentage"
)

// cardView is what the last classification of the node found. It is
// replaced as a whole and never changed afterwards, so a copy taken under
// cardPool.mu can be read without it.
type cardView struct {
	mode cardMode
	// resource is what hold pods request: the device plugin's resource, HAMi's
	// card resource, or ACK's GiB resource; "" = none advertised.
	resource string
	// shared says why the node's cards cannot be borrowed (the mode is
	// modeFractional), "" = they can.
	shared string
	// hostname is the node's kubernetes.io/hostname label: what hold pods
	// that go through a scheduler select the node by.
	hostname string
	hami     *hamiNode
	ack      *ackNode
}

// signature names the mode and resource hold pods are created for. A hold pod
// of another signature than the node's now is one the node changed its mode
// under (see cards_switch.go).
func (v cardView) signature() string {
	switch v.mode {
	case modePlugin:
		return "plugin:" + v.resource
	case modeHAMi:
		return "hami:" + v.resource
	case modeACK:
		return "ack-gpushare"
	case modeFractional:
		return "fractional"
	}
	return "none"
}

// describeSignature is the mode as a person names it, for reasons.
func describeSignature(sig string) string {
	switch {
	case sig == "ack-gpushare":
		return "ACK GPU sharing (" + resACKMem + ")"
	case strings.HasPrefix(sig, "hami:"):
		return "HAMi (" + strings.TrimPrefix(sig, "hami:") + ")"
	case strings.HasPrefix(sig, "plugin:"):
		return "the device plugin (" + strings.TrimPrefix(sig, "plugin:") + ")"
	case sig == "fractional":
		return "fractional allocation"
	}
	return "no device plugin"
}

// holdSignature is the signature a hold pod was created for, read from its
// spec: the resource it requests and whether it goes through a scheduler.
func holdSignature(pod kubeapi.Pod) string {
	var limits map[string]string
	if len(pod.Spec.Containers) > 0 && pod.Spec.Containers[0].Resources != nil {
		limits = pod.Spec.Containers[0].Resources.Limits
	}
	if _, ok := limits[resACKMem]; ok {
		return "ack-gpushare"
	}
	resources := make([]string, 0, len(limits))
	for name := range limits {
		if kind, _ := accelerator.ClassifyKubeResource(name); kind == accelerator.KubeResourceCard {
			resources = append(resources, name)
		}
	}
	sort.Strings(resources)
	if len(resources) == 0 {
		return "none"
	}
	if pod.Spec.SchedulerName != "" {
		return "hami:" + resources[0]
	}
	return "plugin:" + resources[0]
}

// classify reads which device resource the node advertises and how it
// allocates the cards behind it.
func (p *cardPool) classify(ctx context.Context) (cardView, error) {
	var node kubeapi.Node
	if err := p.api.Get(ctx, kubeapi.NodePath(p.node)+"?resourceVersion=0", &node); err != nil {
		return cardView{}, fmt.Errorf("read node %s: %w", p.node, err)
	}
	capacity := node.Status.Capacity
	if len(capacity) == 0 {
		capacity = node.Status.Allocatable
	}
	v := cardView{hostname: node.Metadata.Labels[labelHostname]}
	if v.hostname == "" {
		v.hostname = p.node
	}
	// ACK first: a node switched from HAMi keeps HAMi's registration, and one
	// switched from a plain plugin keeps nvidia.com/gpu in its capacity.
	if isACKSharing(node, capacity) {
		return p.classifyACK(ctx, node, capacity, v)
	}
	if hasHAMiRegistration(node) {
		return p.classifyHAMi(ctx, node, capacity, v)
	}
	var whole, parts []string
	for name, qty := range capacity {
		if qty == "0" {
			continue
		}
		switch kind, _ := accelerator.ClassifyKubeResource(name); kind {
		case accelerator.KubeResourceCard:
			whole = append(whole, name)
		case accelerator.KubeResourceShared:
			parts = append(parts, name)
		}
	}
	sort.Strings(whole)
	sort.Strings(parts)
	if len(whole) > 0 {
		v.mode, v.resource = modePlugin, whole[0]
	}
	if len(parts) > 0 {
		v.mode, v.shared = modeFractional, fractionalReason(parts)
		return v, nil
	}
	if v.resource != "" {
		byIndex, err := p.cards(ctx)
		if err != nil {
			return cardView{}, err
		}
		if n, _ := strconv.Atoi(capacity[v.resource]); len(byIndex) > 0 && n > len(byIndex) {
			v.mode = modeFractional
			v.shared = fmt.Sprintf("Kubernetes advertises %d %s on %d cards (time-slicing replicas or MPS share them); the platform borrows whole cards only", n, v.resource, len(byIndex))
		}
	}
	return v, nil
}

// fractionalReason says which way of handing out parts of cards the node's
// resources are, so the person who has to change it knows what to look at.
func fractionalReason(parts []string) string {
	var kinds []string
	seen := map[string]bool{}
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	for _, r := range parts {
		domain, name, _ := strings.Cut(r, "/")
		switch {
		case strings.HasPrefix(name, "mig-") || strings.Contains(name, ".partitioned"):
			add("MIG partitions")
		case strings.Contains(name, ".shared") || strings.Contains(name, ".sliced"):
			add("time-sliced replicas")
		case domain == "huawei.com":
			add("Ascend vNPU templates without HAMi's registration on the node")
		case strings.HasPrefix(name, "gpumem") || strings.HasPrefix(name, "gpucores"):
			add("HAMi-style memory and compute shares without a hami.io/node-*-register annotation on the node")
		default:
			add("partitions of a card")
		}
	}
	return "Kubernetes hands out parts of this node's cards (" + strings.Join(parts, ", ") + ": " + strings.Join(kinds, ", ") + "); the platform borrows whole cards only"
}

// snapshot is the node's classification as of the last load.
func (p *cardPool) snapshot() cardView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.view
}

// refresh reloads the pool and returns the node's classification.
func (p *cardPool) refresh(ctx context.Context) (cardView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return cardView{}, err
	}
	return p.view, nil
}
