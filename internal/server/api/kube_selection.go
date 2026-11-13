package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Narrowing which nodes an entry selects removes the agents of the nodes it
// no longer selects, once the manifest is applied again; instances still
// running there would keep running and holding their cards but be no longer
// managed, reachable through the gateway or returned. The server does not know
// the cluster's node labels, so before such a change it asks an online agent of
// the entry which of the nodes that run instances the new selection still
// selects, and refuses until the administrator confirms. Confirming asks the
// instances on those nodes to stop (and, when the server knows the nodes leave,
// cordons them so nothing is rebuilt there) before the change is saved; the
// request does not wait for the agents (kube_selection_stop.go).

// kubeSelectionAsk bounds the whole question to the entry's agents.
const kubeSelectionAsk = 20 * time.Second

// kubeSelection is what of an entry's manifest picks the nodes the DaemonSet
// runs on: the node selector, and for the pools of a cloud cluster the label
// their ids are values of.
type kubeSelection struct {
	selector  map[string]string
	poolLabel string
	poolIDs   []string // sorted
}

func newKubeSelection(c store.KubeCluster, pools []store.KubeClusterPool) (kubeSelection, error) {
	params, err := kubeManifestParams(c, pools)
	if err != nil {
		return kubeSelection{}, err
	}
	ids := slices.Clone(params.PoolIDs)
	slices.Sort(ids)
	return kubeSelection{selector: params.NodeSelector, poolLabel: params.PoolLabel, poolIDs: ids}, nil
}

func (s kubeSelection) same(o kubeSelection) bool {
	return maps.Equal(s.selector, o.selector) && s.poolLabel == o.poolLabel && slices.Equal(s.poolIDs, o.poolIDs)
}

// settleEntrySelection is settleKubeSelection for an entry changed from old
// (holding oldPools) to next (holding nextPools).
func (a *apiHandlers) settleEntrySelection(w http.ResponseWriter, r *http.Request, old, next store.KubeCluster, oldPools, nextPools []store.KubeClusterPool, confirmed bool) ([]dto.KubeStoppedInstance, bool) {
	before, err := newKubeSelection(old, oldPools)
	if err == nil {
		var after kubeSelection
		if after, err = newKubeSelection(next, nextPools); err == nil {
			return a.settleKubeSelection(w, r, old, before, after, confirmed)
		}
	}
	writeErr(w, http.StatusInternalServerError, err)
	return nil, false
}

// settleKubeSelection decides what a change of an entry's node selection does
// to the instances on the nodes it leaves out, and runs only when the
// selection changes. When no node that runs instances leaves, nothing is asked
// and nothing stops. When some do, an unconfirmed request is answered with 409
// KUBE_SELECTION_DROPS_INSTANCES and false is returned; a confirmed one
// recomputes the nodes at that moment and asks the instances on them to stop
// (kubeStopLeaving), returning those, and the caller saves the change.
func (a *apiHandlers) settleKubeSelection(w http.ResponseWriter, r *http.Request, entry store.KubeCluster, before, after kubeSelection, confirmed bool) ([]dto.KubeStoppedInstance, bool) {
	stopped := []dto.KubeStoppedInstance{}
	if before.same(after) {
		return stopped, true
	}
	drops, verified, err := a.kubeSelectionDrops(r.Context(), entry, after)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, false
	}
	if len(drops) == 0 {
		return stopped, true
	}
	if confirmed {
		if stopped, err = a.kubeStopLeaving(entry, drops, verified); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return nil, false
		}
		return stopped, true
	}
	names := make([]string, 0, len(drops))
	nodes := make([]dto.KubeSelectionDrop, 0, len(drops))
	for _, d := range drops {
		names = append(names, d.KubeNode)
		nodes = append(nodes, d.KubeSelectionDrop)
	}
	fallback := fmt.Sprintf("The new node selection leaves out %d node(s) that still run instances (%s). Confirming asks those instances to stop first, then saves; their agents are removed when the manifest is applied again. Left running they would keep holding cards but no longer be managed, reachable through the gateway, or returned. Nothing changes in the cluster until the manifest is applied again.",
		len(drops), strings.Join(names, ", "))
	if !verified {
		fallback = "No agent of the cluster could be asked which nodes the new selection keeps, so these nodes may be affected. " + fallback
	}
	writeProductError(w, http.StatusConflict, ErrorKubeSelectionDropsInstances, fallback, map[string]any{"nodes": nodes, "verified": verified})
	return nil, false
}

// kubeDrop is a node the selection does not select while it runs instances,
// with what the server needs of it beyond what the refusal shows.
type kubeDrop struct {
	dto.KubeSelectionDrop
	nodeID      uint
	schedulable bool
}

// kubeSelectionDrops lists the entry's nodes that run instances and that the
// selection does not select. verified is false when no online agent of the
// entry answered, and every node that runs instances is then listed: it may
// be affected.
func (a *apiHandlers) kubeSelectionDrops(ctx context.Context, entry store.KubeCluster, next kubeSelection) (drops []kubeDrop, verified bool, err error) {
	with, err := a.d.Store.KubeNodesWithInstances(entry.ID)
	if err != nil || len(with) == 0 {
		return nil, true, err
	}
	names := make([]string, 0, len(with))
	for _, n := range with {
		names = append(names, n.Node.KubeNodeName)
	}
	selected, verified := a.askKubeSelected(ctx, entry.ID, names, next)
	for _, n := range with {
		if keep, known := selected[n.Node.KubeNodeName]; known && keep {
			continue
		} else if !known {
			verified = false
		}
		drops = append(drops, kubeDrop{
			KubeSelectionDrop: dto.KubeSelectionDrop{NodeUUID: n.Node.UUID, NodeName: n.Node.Name, KubeNode: n.Node.KubeNodeName, Deployments: n.Deployments},
			nodeID:            n.Node.ID,
			schedulable:       n.Node.Scheduling.TakesReplicas(),
		})
	}
	return drops, verified, nil
}

// askKubeSelected asks the entry's online agents in turn which of the named
// nodes the selection selects, until one answers.
func (a *apiHandlers) askKubeSelected(ctx context.Context, clusterID uint, names []string, sel kubeSelection) (map[string]bool, bool) {
	if a.d.CallNode == nil {
		return nil, false
	}
	peers, err := a.d.Store.OnlineKubePeers(clusterID, 0)
	if err != nil || len(peers) == 0 {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, kubeSelectionAsk)
	defer cancel()
	params := proto.KubeNodesSelectedParams{NodeNames: names, Selector: sel.selector, PoolLabel: sel.poolLabel, PoolIDs: sel.poolIDs}
	for _, peer := range peers {
		raw, err := a.d.CallNode(ctx, peer.UUID, proto.RPCKubeNodesSelected, params)
		if err != nil {
			continue
		}
		var answer proto.KubeNodesSelectedResult
		if json.Unmarshal(raw, &answer) == nil && answer.Selected != nil {
			return answer.Selected, true
		}
	}
	return nil, false
}
