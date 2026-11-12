package kube

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime/kube/kubeapi"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Who outside the platform uses the node's cards.

// holders answers the agent's question "who outside the platform uses
// which card": the pods the kubelet gave a card to and, where HAMi or ACK
// GPU sharing allocates, the pods their schedulers did, with what they hold;
// the cards held for another mode than the node's now; or, when the node's
// cards are shared out in parts, every card, since any of them may be.
func (p *cardPool) holders(ctx context.Context) (map[string][]proto.CardProcess, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.load(ctx); err != nil {
		return nil, err
	}
	if p.view.shared == "" {
		out, err := p.externalHolders(ctx, p.view, false)
		if err != nil {
			return nil, err
		}
		for card := range p.stale {
			if p.isStale(card) {
				out[card] = append(out[card], proto.CardProcess{Name: p.staleReason(card)})
			}
		}
		return out, nil
	}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]proto.CardProcess{}
	for _, uuid := range byIndex {
		out[uuid] = []proto.CardProcess{{Name: p.view.shared}}
	}
	return out, nil
}

// externalHolders maps card UUID to the pods outside the platform that hold
// it, shaped as the foreign processes a card reports. The kubelet's
// allocations name the pods that got a device of a card; under HAMi and ACK
// GPU sharing the pods' annotations add what each holds, and the pods the
// kubelet knows nothing of. fresh asks for the API server's current state
// rather than a few seconds old one.
func (p *cardPool) externalHolders(ctx context.Context, v cardView, fresh bool) (map[string][]proto.CardProcess, error) {
	set, err := p.externalHolderSet(ctx, v, fresh)
	return set.all, err
}

// holderSet is who outside the platform holds each card.
type holderSet struct {
	all map[string][]proto.CardProcess
	// unbooked are the holders only the kubelet knows of, that the scheduler of
	// HAMi or ACK GPU sharing has no record of (a pod placed before the node
	// changed its mode, say): the scheduler cannot account for them when it
	// gives a card away.
	unbooked map[string][]proto.CardProcess
}

func (p *cardPool) externalHolderSet(ctx context.Context, v cardView, fresh bool) (holderSet, error) {
	none := holderSet{}
	byIndex, err := p.cards(ctx)
	if err != nil {
		return none, err
	}
	byCard := map[string]map[string]proto.CardProcess{}
	if p.kubelet != nil {
		allocs, err := p.kubelet.List(ctx)
		if err != nil {
			return none, err
		}
		for _, a := range allocs {
			if kind, _ := accelerator.ClassifyKubeResource(a.Resource); kind == accelerator.KubeResourceOther || (a.Namespace == p.ns && strings.HasPrefix(a.Pod, holdPrefix)) {
				continue
			}
			if card := p.cardOf(ctx, a.DeviceID, byIndex); card != "" {
				put(byCard, card, kubeapi.Pod{Metadata: kubeapi.ObjectMeta{Namespace: a.Namespace, Name: a.Pod}},
					proto.CardProcess{Name: "Pod " + a.Namespace + "/" + a.Pod})
			}
		}
	}
	booked := map[string]map[string]proto.CardProcess{}
	if v.mode == modeHAMi || v.mode == modeACK {
		pods, err := p.nodePods(ctx, fresh)
		if err != nil {
			return none, err
		}
		// The annotations say more than the kubelet: they replace its entry
		// for the same pod.
		if v.mode == modeHAMi {
			p.hamiHolders(pods, booked)
		} else {
			p.ackHolders(pods, byIndex, booked)
		}
	}
	out := holderSet{all: map[string][]proto.CardProcess{}, unbooked: map[string][]proto.CardProcess{}}
	for card, pods := range byCard {
		keys := make([]string, 0, len(pods))
		for k := range pods {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := booked[card][k]; !ok {
				out.unbooked[card] = append(out.unbooked[card], pods[k])
			}
		}
	}
	for card, pods := range booked {
		if byCard[card] == nil {
			byCard[card] = map[string]proto.CardProcess{}
		}
		for k, proc := range pods {
			byCard[card][k] = proc
		}
	}
	for card, pods := range byCard {
		keys := make([]string, 0, len(pods))
		for k := range pods {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.all[card] = append(out.all[card], pods[k])
		}
	}
	return out, nil
}

// put records that pod holds card as proc, replacing what was known of it.
func put(into map[string]map[string]proto.CardProcess, card string, pod kubeapi.Pod, proc proto.CardProcess) {
	if into[card] == nil {
		into[card] = map[string]proto.CardProcess{}
	}
	into[card][pod.Metadata.Namespace+"/"+pod.Metadata.Name] = proc
}

// nodePods lists the pods bound to this node in every namespace: the
// allocation annotations of other namespaces' pods are the only record of
// what HAMi and ACK's scheduler gave them. It needs a cluster-wide list
// permission that manifests of earlier versions did not grant; without it
// the answer is empty (and said so once), and the kubelet's allocations are
// all that shows. The answer is kept for a few seconds, since every status
// round asks.
func (p *cardPool) nodePods(ctx context.Context, fresh bool) ([]kubeapi.Pod, error) {
	p.podsMu.Lock()
	defer p.podsMu.Unlock()
	if !fresh && p.podsTTL > 0 && p.podsAt.Add(p.podsTTL).After(time.Now()) {
		return p.podsCache, nil
	}
	var list kubeapi.PodList
	q := "?fieldSelector=" + url.QueryEscape("spec.nodeName="+p.node)
	if !fresh {
		q += "&resourceVersion=0" // from the API server's watch cache, not etcd
	}
	err := p.api.Get(ctx, kubeapi.AllPodsPath+q, &list)
	if kubeapi.IsForbidden(err) {
		if !p.podsForbidden {
			p.podsForbidden = true
			p.log.Warn("the agent's service account may not list pods in every namespace, so what other pods hold of this node's cards is not shown beyond the kubelet's allocations; apply the cluster's current agent manifest", "err", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.podsCache, p.podsAt = list.Items, time.Now()
	return list.Items, nil
}
