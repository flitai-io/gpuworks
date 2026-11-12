package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// unitRetryRequest names one unit of a deployment: its role (empty outside
// PD) and its position among the role's units (ADR 0029).
type unitRetryRequest struct {
	Role  string `json:"role"`
	Index int    `json:"index"`
}

// replicaView is how a deployment's instances fill its replicas, as the
// controller matches them (over the deployment's own nodes), with the node
// names the response shows. Every handler and response reads replica
// membership from here.
type replicaView struct {
	match planning.UnitMatch
	// names is each deployment node's display name, by UUID; ordered is the
	// node UUIDs in the deployment's stored order.
	names   map[string]string
	ordered []string
	// pd and mode are the deployment's roles: nil pd for an ordinary
	// deployment, and mode as the placement names it (its zero value when the
	// gateway does not know the name, which reads as no role being optional).
	pd   *planning.PDPlacement
	mode pdmode.Mode
}

func newReplicaView(rows deployment.Rows, d *store.Deployment, insts []store.Instance) (replicaView, error) {
	view := replicaView{names: map[string]string{}}
	uuids := map[uint]string{}
	for _, id := range d.NodeIDList() {
		if n := rows.Node(id); n != nil {
			view.names[n.UUID] = nodeDisplayName(n)
			view.ordered = append(view.ordered, n.UUID)
			uuids[n.ID] = n.UUID
		}
	}
	pd, err := planning.PDOf(d.Placement)
	if err != nil {
		return view, err
	}
	view.pd = pd
	if pd != nil {
		view.mode, _ = pdmode.Lookup(pd.Mode)
	}
	view.match = planning.MatchUnits(planning.UnitsOfDeployment(d, pd, uuids), insts, uuids, d.PlacementStrategy)
	return view, nil
}

// deploymentUnitMatch is how a deployment's instances fill its replicas.
func (a *apiHandlers) deploymentUnitMatch(dep *store.Deployment, insts []store.Instance) (planning.UnitMatch, error) {
	view, err := newReplicaView(deployment.NewStoreRows(a.d.Store, a.d.Preparation), dep, insts)
	return view.match, err
}

// retryUnit gives a unit its retry budget back: its failed attempts are
// retired (their containers removed, their timeline kept), and the
// controller rebuilds the unit on its next pass. It is the operator saying
// the cause is fixed; other units are not touched.
func (a *apiHandlers) retryUnit(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req unitRetryRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if dep.State != store.DeployActive {
		writeProductError(w, http.StatusConflict, ErrorUnitRetryNotActive, "only an active deployment's units are retried", map[string]any{"state": string(dep.State)})
		return
	}
	insts, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	match, err := a.deploymentUnitMatch(dep, insts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var slot *planning.UnitSlot
	for i := range match.Slots {
		if match.Slots[i].Unit.Role == req.Role && match.Slots[i].Unit.Index == req.Index {
			slot = &match.Slots[i]
			break
		}
	}
	if slot == nil {
		writeProductError(w, http.StatusNotFound, ErrorUnitUnknown, "the deployment has no such unit", map[string]any{"role": req.Role, "index": req.Index})
		return
	}
	attempts := match.Failed[slot.Key]
	if slot.Inst != nil || len(attempts) == 0 {
		writeProductError(w, http.StatusConflict, ErrorUnitNotFailed, "the unit has no failed attempts to retry", map[string]any{"role": req.Role, "index": req.Index})
		return
	}
	for _, inst := range attempts {
		if err := a.d.Store.SetInstanceState(inst.UUID, proto.InstRemoving, "retry requested"); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	params := map[string]any{"role": req.Role, "unit": req.Index, "attempts": len(attempts)}
	// Where the attempts given up on ran: they are removed after this.
	if nodes, err := a.d.Store.NodeUUIDsOfInstances(uuidsOf(attempts)); err == nil && len(nodes) > 0 {
		params[store.EventParamNodeUUIDs] = nodes
	}
	a.operatorEvent(r, dep.ID, store.EventUnitRetried, params)
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusAccepted, a.deploymentDTO(updated, true))
}

// replicaAttempts is a replica's spent retry budget as the deployment
// response shows it.
type replicaAttempts struct {
	count       int
	lastFailure string
	exhausted   bool
}

// spentAttempts reads a replica's failed attempts: how many there are, the
// newest one's message, and whether the controller gave up on the replica
// (it has no instance and its newest attempt carries the give-up record).
func spentAttempts(rows deployment.Rows, slot planning.UnitSlot, attempts []store.Instance) replicaAttempts {
	if len(attempts) == 0 {
		return replicaAttempts{}
	}
	newest := planning.NewestAttempt(attempts)
	return replicaAttempts{count: len(attempts), lastFailure: newest.Message, exhausted: slot.Inst == nil && rows.RetryExhausted(newest.UUID)}
}

// ordinaryRetryExhausted finds the replica of an ordinary deployment that
// gave up, so the response can offer its retry. Single-node replicas share
// one retry budget, so any empty one names the same attempts; the first is
// reported because a retry of a filled replica is refused.
func ordinaryRetryExhausted(rows deployment.Rows, view replicaView) *dto.DeploymentRetryExhausted {
	for _, slot := range view.match.Slots {
		spent := spentAttempts(rows, slot, view.match.Failed[slot.Key])
		if spent.exhausted {
			return &dto.DeploymentRetryExhausted{Index: slot.Unit.Index, FailedAttempts: spent.count, LastFailure: spent.lastFailure}
		}
	}
	return nil
}
