package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Borrowing cards through HAMi.
//
// HAMi registers every card as several shares (nvidia.com/gpu: 10 for one
// card) and its device plugin hands a pod only what HAMi's own scheduler
// assigned and wrote to the pod's annotations, so a hold pod that names its
// node (nodeName) is never served. The hold pod instead goes through that
// scheduler and names the card it wants, with the card's whole compute and
// memory, and which card it got is what HAMi wrote on the pod, not what the
// kubelet lists (its device IDs are "<UUID>-<share>"). Because a card can be
// named, nothing is borrowed and returned again: the platform takes the
// cards it wants, one hold pod each.
//
// The NVIDIA shape was checked against HAMi 2.11 on a cluster. The Ascend
// shape follows HAMi's documented annotations and registration and has not
// been run on hardware.

const (
	hamiRegisterPrefix  = "hami.io/node-"
	hamiRegisterSuffix  = "-register"
	hamiAllocatedPrefix = "hami.io/"
	hamiAllocatedSuffix = "-devices-allocated"
	hamiBindPhase       = "hami.io/bind-phase"

	// DefaultHAMiScheduler is the name of HAMi's scheduler in a default
	// install. A cluster that renamed it tells the agent through
	// GPUWORKS_KUBE_HAMI_SCHEDULER.
	DefaultHAMiScheduler = "hami-scheduler"

	resNVIDIAGPU       = "nvidia.com/gpu"
	resNVIDIACores     = "nvidia.com/gpucores"
	resNVIDIAMemPct    = "nvidia.com/gpumem-percentage"
	annNVIDIAUseUUID   = "nvidia.com/use-gpuuuid"
	hamiAscendUsePart1 = "hami.io/use-"
	hamiAscendUsePart2 = "-uuid"
)

// hamiCard is a card as HAMi registered it on the node (the entries of the
// hami.io/node-<vendor>-register annotation).
type hamiCard struct {
	devmem  int // MiB
	devcore int // percent
	count   int // shares
}

// hamiNode is what the node's HAMi registration says.
type hamiNode struct {
	vendor    string
	scheduler string
	cards     map[string]hamiCard // by card UUID, healthy ones
}

// hamiRegistered is one entry of the registration annotation.
type hamiRegistered struct {
	ID      string `json:"id"`
	Count   int    `json:"count"`
	Devmem  int    `json:"devmem"`
	Devcore int    `json:"devcore"`
	Type    string `json:"type"`
	Health  bool   `json:"health"`
}

// hamiRegisterKeys are the node's HAMi registration annotations, sorted.
func hamiRegisterKeys(node kubeapi.Node) []string {
	var keys []string
	for k := range node.Metadata.Annotations {
		if strings.HasPrefix(k, hamiRegisterPrefix) && strings.HasSuffix(k, hamiRegisterSuffix) && len(k) > len(hamiRegisterPrefix)+len(hamiRegisterSuffix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func hasHAMiRegistration(node kubeapi.Node) bool { return len(hamiRegisterKeys(node)) > 0 }

// classifyHAMi reads the node as one HAMi serves; whatever keeps the platform
// from borrowing through it becomes the reason cards are reported as used
// outside the platform.
func (p *cardPool) classifyHAMi(_ context.Context, node kubeapi.Node, capacity map[string]string, v cardView) (cardView, error) {
	refuse := func(format string, args ...any) (cardView, error) {
		v.mode, v.shared = modeFractional, fmt.Sprintf(format, args...)+"; the platform borrows whole cards only"
		return v, nil
	}
	keys := hamiRegisterKeys(node)
	if len(keys) > 1 {
		return refuse("HAMi registers cards of several kinds on this node (%s)", strings.Join(keys, ", "))
	}
	key := keys[0]
	var entries []hamiRegistered
	if err := json.Unmarshal([]byte(node.Metadata.Annotations[key]), &entries); err != nil {
		return refuse("HAMi's card registration %s on this node cannot be read (%v)", key, err)
	}
	vendor := ""
	switch lower := strings.ToLower(key); {
	case strings.Contains(lower, "nvidia"):
		vendor = accelerator.VendorNVIDIA
	case strings.Contains(lower, "ascend"):
		vendor = accelerator.VendorAscend
	default:
		return refuse("HAMi manages cards on this node (%s) that the platform cannot borrow through it", key)
	}
	resource := resNVIDIAGPU
	if vendor == accelerator.VendorAscend {
		resource = ""
		var found []string
		for name, qty := range capacity {
			if k, vd := accelerator.ClassifyKubeResource(name); qty != "0" && k == accelerator.KubeResourceCard && vd == accelerator.VendorAscend {
				found = append(found, name)
			}
		}
		sort.Strings(found)
		if len(found) > 0 {
			resource = found[0]
		}
	}
	h := &hamiNode{vendor: vendor, scheduler: p.hamiScheduler, cards: map[string]hamiCard{}}
	shares := 0
	for _, e := range entries {
		if e.Health && e.ID != "" {
			h.cards[e.ID] = hamiCard{devmem: e.Devmem, devcore: e.Devcore, count: e.Count}
			shares += e.Count
		}
	}
	if resource == "" || capacity[resource] == "" || capacity[resource] == "0" {
		return refuse("HAMi registered %d cards on this node but the node advertises no card resource (HAMi's device plugin is not running here)", len(h.cards))
	}
	if n, _ := strconv.Atoi(capacity[resource]); n != shares {
		return refuse("HAMi registered %d shares of %d cards on this node but the node advertises %s %d (a stale HAMi registration, or another plugin serves the node)", shares, len(h.cards), resource, n)
	}
	var others []string
	for name, qty := range capacity {
		if k, _ := accelerator.ClassifyKubeResource(name); qty != "0" && k == accelerator.KubeResourceShared && !isHAMiResource(name) {
			others = append(others, name)
		}
	}
	if len(others) > 0 {
		sort.Strings(others)
		return refuse("%s", strings.TrimSuffix(fractionalReason(others), "; the platform borrows whole cards only"))
	}
	v.mode, v.resource, v.hami = modeHAMi, resource, h
	return v, nil
}

// isHAMiResource is one of the memory and compute resources HAMi's older
// versions register on the node beside the card shares.
func isHAMiResource(name string) bool {
	_, n, _ := strings.Cut(name, "/")
	return strings.HasPrefix(n, "gpumem") || strings.HasPrefix(n, "gpucores")
}

// hamiDevice is one card in a pod's allocation annotation.
type hamiDevice struct {
	uuid   string
	vendor string
	memMiB int
	cores  int
}

// parseHAMiAllocated reads the value of a pod's hami.io/*-devices-allocated
// annotation: one string per container, each ended by ";", holding its
// devices ended by ":", each "<uuid>,<vendor>,<memMiB>,<cores%>", for
// example "GPU-5b6f…,NVIDIA,46068,100:;".
func parseHAMiAllocated(s string) ([]hamiDevice, error) {
	var out []hamiDevice
	for _, container := range strings.Split(s, ";") {
		for _, dev := range strings.Split(container, ":") {
			if strings.TrimSpace(dev) == "" {
				continue
			}
			f := strings.Split(dev, ",")
			if len(f) < 4 {
				return nil, fmt.Errorf("device %q is not <uuid>,<vendor>,<memory>,<cores>", dev)
			}
			mem, err1 := strconv.Atoi(f[2])
			cores, err2 := strconv.Atoi(f[3])
			if err1 != nil || err2 != nil || f[0] == "" {
				return nil, fmt.Errorf("device %q has a memory or cores that is not a number", dev)
			}
			out = append(out, hamiDevice{uuid: f[0], vendor: f[1], memMiB: mem, cores: cores})
		}
	}
	return out, nil
}

// hamiAllocations are the cards HAMi gave a pod, from its annotations. A pod
// whose scheduler gave up on binding it holds nothing.
func hamiAllocations(pod kubeapi.Pod) ([]hamiDevice, error) {
	if pod.Metadata.Annotations[hamiBindPhase] == "failed" || pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
		return nil, nil
	}
	keys := make([]string, 0, 1)
	for k := range pod.Metadata.Annotations {
		if strings.HasPrefix(k, hamiAllocatedPrefix) && strings.HasSuffix(k, hamiAllocatedSuffix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var out []hamiDevice
	for _, k := range keys {
		devs, err := parseHAMiAllocated(pod.Metadata.Annotations[k])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out = append(out, devs...)
	}
	return out, nil
}

// verifyWhole checks that HAMi gave a hold pod exactly the card asked for, all
// of it: owning part of a card is not owning the card.
func (h *hamiNode) verifyWhole(pod kubeapi.Pod, card string) error {
	devs, err := hamiAllocations(pod)
	if err != nil {
		return fmt.Errorf("the allocation HAMi wrote on the hold pod cannot be read: %w", err)
	}
	switch {
	case len(devs) == 0:
		return fmt.Errorf("HAMi wrote no allocation on the hold pod")
	case len(devs) != 1 || devs[0].uuid != card:
		got := make([]string, len(devs))
		for i, d := range devs {
			got[i] = d.uuid
		}
		return fmt.Errorf("HAMi gave the hold pod %s instead of card %s", strings.Join(got, ", "), card)
	}
	reg, ok := h.cards[card]
	if !ok {
		return fmt.Errorf("HAMi does not list card %s as a healthy card of this node", card)
	}
	d := devs[0]
	if d.memMiB != reg.devmem {
		return fmt.Errorf("HAMi gave only %d of card %s's %d MiB of memory", d.memMiB, card, reg.devmem)
	}
	if h.vendor == accelerator.VendorNVIDIA && d.cores != 100 {
		return fmt.Errorf("HAMi gave only %d%% of card %s's compute", d.cores, card)
	}
	return nil
}

// hamiHoldPod is the hold pod for one card. Its limits ask for the whole
// card; the pod goes through HAMi's scheduler and names the card.
func (p *cardPool) hamiHoldPod(v cardView, name, card string) kubeapi.Pod {
	pod := p.baseHoldPod(name)
	pod.Spec.SchedulerName = v.hami.scheduler
	pod.Spec.NodeSelector = map[string]string{labelHostname: v.hostname}
	pod.Metadata.Annotations = map[string]string{}
	limits := map[string]string{v.resource: "1"}
	if v.hami.vendor == accelerator.VendorAscend {
		// No memory in the request is a whole card to HAMi's Ascend plugin.
		model := strings.TrimPrefix(v.resource, "huawei.com/")
		pod.Metadata.Annotations[hamiAscendUsePart1+model+hamiAscendUsePart2] = card
	} else {
		limits[resNVIDIACores], limits[resNVIDIAMemPct] = "100", "100"
		pod.Metadata.Annotations[annNVIDIAUseUUID] = card
	}
	c := &pod.Spec.Containers[0]
	c.Resources.Limits = limits
	return pod
}

// borrowHAMi takes the wanted cards that no hold pod holds: one hold pod each,
// through HAMi's scheduler, kept only if HAMi gave exactly that card whole.
// The caller holds borrowMu and not mu.
func (p *cardPool) borrowHAMi(ctx context.Context, v cardView, want []string) error {
	var cards []string
	holders, err := p.externalHolderSet(ctx, v, true)
	if err != nil {
		return err
	}
	for _, card := range p.unheld(want) {
		if _, ok := v.hami.cards[card]; !ok {
			return &errCardsNotGiven{cards: []string{card}, final: true,
				detail: fmt.Sprintf("HAMi does not list card %s as a healthy card of this node", card)}
		}
		if h := holders.unbooked[card]; len(h) > 0 {
			return fmt.Errorf("card %s is allocated to %s by Kubernetes, outside HAMi's books", card, h[0].Name)
		}
		cards = append(cards, card)
	}
	pods := map[string]string{} // hold pod -> card
	var names []string
	for _, card := range cards {
		name := holdPrefix + randomSuffix()
		if err := p.api.Create(ctx, kubeapi.PodsPath(p.ns), p.hamiHoldPod(v, name, card), nil); err != nil {
			p.deletePods(ctx, names)
			return fmt.Errorf("create hold pod: %w", err)
		}
		pods[name] = card
		names = append(names, name)
	}
	outcomes := p.waitScheduled(ctx, names)
	why := map[string]string{} // card -> why it was not given
	final := false
	var drop []string
	for _, name := range names {
		card := pods[name]
		o := outcomes[name]
		if !o.running {
			why[card] = o.why
			if o.idle {
				why[card] += fmt.Sprintf(" (the hold pod asks for the scheduler %q; if the cluster's HAMi scheduler has another name, set GPUWORKS_KUBE_HAMI_SCHEDULER on the agent)", v.hami.scheduler)
			}
			drop = append(drop, name)
			continue
		}
		var pod kubeapi.Pod
		if err := p.api.Get(ctx, kubeapi.PodPath(p.ns, name), &pod); err != nil {
			why[card], final = "the hold pod cannot be read back: "+err.Error(), true
			drop = append(drop, name)
			continue
		}
		if err := v.hami.verifyWhole(pod, card); err != nil {
			why[card], final = err.Error(), true
			drop = append(drop, name)
		}
	}
	p.deletePods(ctx, drop)
	if err := p.syncHeld(ctx, v); err != nil {
		return err
	}
	still := p.unheld(want)
	if len(still) == 0 {
		return nil
	}
	return p.notGiven(ctx, v, still, why, final)
}

// notGiven is the error for wanted cards a scheduler did not give: its reason
// for each, and the pods that use the card (a card partly used by another pod
// cannot be given whole). A card someone else holds is not a race to retry.
func (p *cardPool) notGiven(ctx context.Context, v cardView, still []string, why map[string]string, final bool) error {
	holders, err := p.externalHolders(ctx, v, true)
	if err != nil {
		p.log.Warn("cannot list who uses the cards that were not given", "err", err)
	}
	var parts []string
	for _, card := range still {
		part := "card " + card
		if w := why[card]; w != "" {
			part += ": " + w
		}
		if hs := holders[card]; len(hs) > 0 {
			names := make([]string, len(hs))
			for i, h := range hs {
				names[i] = h.Name
			}
			part += " (in use by " + strings.Join(names, ", ") + ")"
			final = true
		}
		parts = append(parts, part)
	}
	return &errCardsNotGiven{cards: still, final: final, detail: strings.Join(parts, "; ")}
}

// hamiHolders are the pods other than the platform's hold pods that HAMi gave
// a card of this node to, from the annotations HAMi wrote on them, keyed by
// card and then by pod.
func (p *cardPool) hamiHolders(pods []kubeapi.Pod, into map[string]map[string]proto.CardProcess) {
	for _, pod := range pods {
		if p.isHoldPod(pod) {
			continue
		}
		devs, err := hamiAllocations(pod)
		if err != nil {
			p.log.Warn("a pod's HAMi allocation cannot be read", "pod", pod.Metadata.Namespace+"/"+pod.Metadata.Name, "err", err)
			continue
		}
		for _, d := range devs {
			detail := fmt.Sprintf("%d MiB", d.memMiB)
			if d.cores > 0 {
				detail += fmt.Sprintf(", %d%% of the compute", d.cores)
			}
			put(into, d.uuid, pod, proto.CardProcess{
				Name: "Pod " + pod.Metadata.Namespace + "/" + pod.Metadata.Name + " (" + detail + ")", MemoryBytes: int64(d.memMiB) << 20,
			})
		}
	}
}
