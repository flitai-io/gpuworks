package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Node scheduling wire names. Schedulable is the stored zero value.
const nodeSchedulableName = "schedulable"

func nodeSchedulingName(s store.NodeScheduling) string {
	if s == store.NodeSchedulable {
		return nodeSchedulableName
	}
	return string(s)
}

func parseNodeScheduling(name string) (store.NodeScheduling, bool) {
	if name == nodeSchedulableName {
		return store.NodeSchedulable, true
	}
	s := store.NodeScheduling(name)
	return s, s != store.NodeSchedulable && s.Valid()
}

// setNodeScheduling cordons a node, drains it, or makes it schedulable
// again. Cordoned, it takes no new replica and what runs there keeps
// running; draining, the controller also replaces each replica a pool placed
// there on another node, one at a time per deployment, before retiring it
// (controller.nodeDrain). Schedulable again, every waiting deployment is
// retried at once. A node a scheduling holder holds cannot be made
// schedulable (409 with the holder's code); it can be cordoned or drained.
func (a *apiHandlers) setNodeScheduling(w http.ResponseWriter, r *http.Request) {
	var req dto.NodeSchedulingRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	scheduling, ok := parseNodeScheduling(req.Scheduling)
	if !ok {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "scheduling must be schedulable, cordoned or draining", map[string]any{"field": "scheduling", "detail": req.Scheduling})
		return
	}
	node, err := a.d.Store.SetNodeScheduling(chi.URLParam(r, "uuid"), scheduling)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if errors.Is(err, store.ErrNodeHeld) {
		// An extension holds the node: the error carries its code and
		// params, which the operator is shown.
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.nodeView(node))
}

// setCardScheduling stops one card's scheduling, or makes it schedulable
// again. Both decisions persist until the operator changes them; reports
// keep health facts current without revoking scheduling. The node comes back,
// cards included, as GET /nodes/{uuid} returns it.
func (a *apiHandlers) setCardScheduling(w http.ResponseWriter, r *http.Request) {
	var req dto.CardSchedulingRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	to := store.CardScheduling(req.Scheduling)
	if !to.Valid() {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "scheduling must be stopped or schedulable", map[string]any{"field": "scheduling", "detail": req.Scheduling})
		return
	}
	ident := accelerator.CardIdent(req.UUID, req.PCIeBDF)
	if ident == "" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "pcie_bdf or uuid names the card", map[string]any{"field": "pcie_bdf"})
		return
	}
	uuid := chi.URLParam(r, "uuid")
	_, err := a.d.Store.SetCardScheduling(uuid, ident, to)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, store.ErrCardNotOwned):
		writeProductError(w, http.StatusConflict, ErrorCardNotOwned, "the card is not this node's to use", map[string]any{"pcie_bdf": req.PCIeBDF})
		return
	case errors.Is(err, store.ErrCardHeld):
		// An extension holds the card: the error carries its code and
		// params, which the operator is shown.
		writeErr(w, http.StatusConflict, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	node, err := a.d.Store.NodeWithGPUs(uuid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.nodeView(node))
}

// nodeReplicas lists what still runs on a node and whether a drain moves it
// (dto.NodeReplica), so a drain can be followed to empty.
func (a *apiHandlers) nodeReplicas(w http.ResponseWriter, r *http.Request) {
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	insts, err := a.d.Store.InstancesForNode(node.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.NodeReplicas{Scheduling: nodeSchedulingName(node.Scheduling), Items: []dto.NodeReplica{}}
	out.DrainOffline = node.Scheduling == store.NodeDraining && a.d.Online != nil && !a.d.Online(node.UUID)
	kinds := map[uint]string{}
	names := map[uint]string{}
	feedback := map[uint]dto.NodeReplica{}
	for _, inst := range insts {
		if inst.State == proto.InstStopped || inst.State == proto.InstFailed {
			continue
		}
		kind, seen := kinds[inst.DeploymentID]
		if !seen {
			dep, err := a.d.Store.GetDeployment(inst.DeploymentID)
			if err != nil {
				continue // deleted meanwhile
			}
			if kind, err = a.drainKind(dep, node); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			kinds[dep.ID], names[dep.ID] = kind, dep.Name
			if node.Scheduling == store.NodeDraining && kind != "pinned" {
				detail, err := a.nodeDrainFeedback(dep.ID, node.ID, inst)
				if err != nil {
					writeErr(w, http.StatusInternalServerError, err)
					return
				}
				feedback[dep.ID] = detail
			}
		}
		item := dto.NodeReplica{Instance: inst.UUID, DeploymentID: inst.DeploymentID, Deployment: names[inst.DeploymentID], State: inst.State, Kind: kind}
		if inst.State != proto.InstDraining && inst.State != proto.InstRemoving {
			item.DrainReason, item.DrainReasonAt, item.DrainMessage = feedback[inst.DeploymentID].DrainReason, feedback[inst.DeploymentID].DrainReasonAt, feedback[inst.DeploymentID].DrainMessage
		}
		out.Items = append(out.Items, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// drainKind says whether a drain moves a deployment's replica off node: a
// pool replica is replaced elsewhere unless its requirements name only this
// node; a multi-node or PD rank names its node.
func (a *apiHandlers) drainKind(dep *store.Deployment, node *store.Node) (string, error) {
	pd, err := planning.PDOf(dep.Placement)
	if err != nil {
		return "", err
	}
	if pd != nil || dep.NNodes > 1 {
		return "named", nil
	}
	reqs, err := a.d.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		return "", err
	}
	if pinned, ok := planning.PinnedNode(reqs); ok && pinned == node.UUID {
		return "pinned", nil
	}
	return "pool", nil
}

// evictNode takes every replica off a cordoned or draining node now, without
// waiting for replacements: each generation with an instance there drains its
// leader (the gateway sends it nothing new and it is retired once idle), and
// the controller rebuilds the replica wherever it may run - a pool replica on
// another node, a named or pinned one once the node takes replicas again. It
// costs no replica a retry. A schedulable node is refused: evicting from it
// would only put the replicas back.
func (a *apiHandlers) evictNode(w http.ResponseWriter, r *http.Request) {
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if node.Scheduling.TakesReplicas() {
		writeProductError(w, http.StatusConflict, ErrorNodeSchedulable, "cordon or drain the node before evicting its replicas", nil)
		return
	}
	insts, err := a.d.Store.InstancesForNode(node.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	byDeployment := map[uint][]string{}
	for _, inst := range insts {
		byDeployment[inst.DeploymentID] = append(byDeployment[inst.DeploymentID], inst.UUID)
	}
	out := dto.NodeEviction{Evicted: []string{}}
	for depID, uuids := range byDeployment {
		dep, err := a.d.Store.GetDeployment(depID)
		if err != nil || dep.State != store.DeployActive {
			continue // gone, stopped or being deleted: nothing to rebuild
		}
		all, err := a.d.Store.InstancesForDeployment(dep.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		match, err := a.deploymentUnitMatch(dep, all)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		leaders := map[string]bool{}
		for _, uuid := range uuids {
			// A follower names its whole generation: a collective cannot
			// lose one process.
			if slot := match.SlotOf(uuid); slot != nil && slot.Inst != nil {
				leaders[slot.Inst.UUID] = true
			}
		}
		for leader := range leaders {
			err := a.d.Store.TransitionInstances(dep.ID, []string{leader}, store.ReplicaFillingStates(), proto.InstDraining, "evicted from node "+node.DisplayName())
			if errors.Is(err, store.ErrStateChanged) {
				continue
			}
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			out.Evicted = append(out.Evicted, leader)
			a.operatorEvent(r, dep.ID, store.EventReplicaRestarted, map[string]any{"instance": leader, "drain": true, "evicted_from": node.UUID})
		}
	}
	sort.Strings(out.Evicted)
	writeJSON(w, http.StatusAccepted, out)
}
