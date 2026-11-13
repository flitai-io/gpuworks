package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Why a live instance keeps its memory limit until it is restarted
// (dto.DGPUQuotaPending.Reason).
const (
	quotaPendingBackend = "backend"  // its partition backend reads the limit only at start
	quotaPendingAsleep  = "asleep"   // a lower limit could fail its wake
	quotaPendingStart   = "starting" // a lower limit could fail its start
	quotaPendingInUse   = "in_use"   // it holds more than the lower limit
	quotaPendingUnknown = "no_usage" // what it holds is not known, so a lower limit is not risked
)

// setDeploymentDGPUQuota answers PUT /deployments/{id}/dgpu: a new dGPU
// memory limit for a single-node dGPU deployment, in effect at once. The
// deployment's next instances start under it, and each running instance is
// held to it in place where that is safe: the store rewrites the limit its
// desired state carries (which the GPU ledger charges) and the agent
// rewrites the limit the partition backend enforces. An engine that sizes
// its pool once at start, as vLLM does, only uses a new limit after a
// restart, which stays the operator's call; one that allocates as it goes
// uses it straight away.
//
// An instance is left at its limit until its next restart when its backend
// reads the limit only at start, or when the limit goes down and the
// instance holds more than it, is starting or asleep, or reports nothing:
// a lower limit under what an engine holds fails its next allocation, which
// ends the engine. The response names each such instance and why.
//
// Refused outright: a limit no node could run the deployment under, and a
// card that could not keep the instances being raised at the new limit.
func (a *apiHandlers) setDeploymentDGPUQuota(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req dto.DeploymentDGPUQuotaRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if dep.State == store.DeployDeleting {
		writeProductError(w, http.StatusConflict, ErrorDeploymentDeleting, "the deployment is being deleted", nil)
		return
	}
	if dep.VRAMLimitBytes <= 0 {
		writeProductError(w, http.StatusBadRequest, ErrorDGPUQuotaNotPartitioned, "the deployment runs on whole cards; it has no dGPU quota", nil)
		return
	}
	pd, err := planning.PDOf(dep.Placement)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if pd != nil || dep.NNodes > 1 {
		shape := "multi_node"
		if pd != nil {
			shape = "pd"
		}
		writeProductError(w, http.StatusBadRequest, ErrorDGPUQuotaNotEditable, "a multi-node or PD deployment places its ranks one by one; its quota is not changed in place", map[string]any{"shape": shape})
		return
	}
	if math.IsNaN(req.VRAMLimitGiB) || req.VRAMLimitGiB <= 0 || req.VRAMLimitGiB > 1<<20 {
		writeProductError(w, http.StatusBadRequest, ErrorDGPUQuotaInvalid, "vram_limit_gib must be a positive number of GiB", map[string]any{"field": "vram_limit_gib", "value": req.VRAMLimitGiB})
		return
	}
	limit := int64(req.VRAMLimitGiB * (1 << 30))
	insts, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	live, pending, err := a.quotaOutcomes(insts, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if limit == dep.VRAMLimitBytes && len(live) == 0 {
		// Nothing to change: the deployment and every instance that could
		// take the limit already have it.
		writeJSON(w, http.StatusOK, a.quotaResult(dep, live, pending))
		return
	}
	next := *dep
	next.VRAMLimitBytes = limit
	reqs, err := a.d.Store.DeploymentRequirements(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.deployments().CheckStoredCanEverFit(r.Context(), &next, reqs); err != nil {
		status := http.StatusInternalServerError
		var refused *messages.ProductError
		if errors.As(err, &refused) {
			status = http.StatusBadRequest
		}
		writeErr(w, status, err)
		return
	}
	short, err := planning.QuotaRoom(a.d.Store, dep, limit, live)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if short != nil {
		writeProductError(w, http.StatusConflict, ErrorDGPUQuotaNoRoom, "a card the replicas hold cannot keep them at the new quota", map[string]any{
			"node": nodeDisplayName(short.Node), "gpu": short.GPUIndex, "free_gib": gib1(short.FreeBytes), "need_gib": gib1(short.NeedBytes),
		})
		return
	}
	if err := a.d.Store.SetDeploymentDGPUQuota(dep.ID, limit, live); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound // deleted while the request was read
		}
		writeErr(w, status, err)
		return
	}
	a.d.Store.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, store.EventDeploymentDGPUQuotaChanged, map[string]any{
		"from_bytes": dep.VRAMLimitBytes, "to_bytes": limit, "live": len(live), "pending": len(pending),
	})
	updated, err := a.d.Store.GetDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, a.quotaResult(updated, live, pending))
}

// quotaOutcomes sorts a deployment's live instances into those held to limit
// in place and those that keep their own until their next restart.
func (a *apiHandlers) quotaOutcomes(insts []store.Instance, limit int64) ([]string, []dto.DGPUQuotaPending, error) {
	var used map[string]int64
	live := []string{}
	pending := []dto.DGPUQuotaPending{}
	for i := range insts {
		inst := &insts[i]
		switch inst.State {
		case proto.InstStopped, proto.InstFailed, proto.InstPendingRemoval, proto.InstRemoving:
			continue // replaced or gone; its successor starts under the deployment's limit
		}
		spec, err := inst.DesiredState()
		if err != nil || spec.VRAMLimitBytes <= 0 {
			continue
		}
		backendID := spec.PartitionBackend
		if backendID == "" {
			backendID = accelerator.BackendNVIDIALogical
		}
		backend, ok := accelerator.Lookup(backendID)
		if !ok || !accelerator.LiveMemoryLimit(backend) {
			pending = append(pending, dto.DGPUQuotaPending{InstanceUUID: inst.UUID, Reason: quotaPendingBackend})
			continue
		}
		if limit >= spec.VRAMLimitBytes {
			if limit != spec.VRAMLimitBytes {
				live = append(live, inst.UUID)
			}
			continue
		}
		switch {
		case planning.InstanceSleepIntent(inst) > 0 || planning.ReportedSleepLevel(a.d.Store, inst) > 0:
			pending = append(pending, dto.DGPUQuotaPending{InstanceUUID: inst.UUID, Reason: quotaPendingAsleep})
			continue
		case inst.State != proto.InstRunning:
			pending = append(pending, dto.DGPUQuotaPending{InstanceUUID: inst.UUID, Reason: quotaPendingStart})
			continue
		}
		if used == nil {
			if used, err = a.dgpuHeld(); err != nil {
				return nil, nil, err
			}
		}
		held, known := used[inst.UUID]
		switch {
		case !known:
			pending = append(pending, dto.DGPUQuotaPending{InstanceUUID: inst.UUID, Reason: quotaPendingUnknown})
		case held > limit:
			pending = append(pending, dto.DGPUQuotaPending{InstanceUUID: inst.UUID, Reason: quotaPendingInUse, UsedBytes: held})
		default:
			live = append(live, inst.UUID)
		}
	}
	return live, pending, nil
}

// dgpuHeld is what each dGPU instance held on its fullest card at its last
// report: the partition backend's own count, which is what it compares a
// limit against.
func (a *apiHandlers) dgpuHeld() (map[string]int64, error) {
	out := map[string]int64{}
	if a.d.MetricsLatest == nil {
		return out, nil
	}
	points, err := a.d.MetricsLatest("gpuworks_dgpu_memory_used_bytes", nil, time.Now().UTC().Add(-2*time.Minute).UnixMilli())
	if err != nil {
		return nil, err
	}
	for _, point := range points {
		uuid := point.Labels["instance"]
		if uuid == "" {
			continue
		}
		if held, seen := out[uuid]; !seen || int64(point.Value) > held {
			out[uuid] = int64(point.Value)
		}
	}
	return out, nil
}

func (a *apiHandlers) quotaResult(dep *store.Deployment, live []string, pending []dto.DGPUQuotaPending) dto.DGPUQuotaResult {
	return dto.DGPUQuotaResult{Deployment: a.deploymentDTO(dep, true), Live: live, Pending: pending}
}

// gib1 is bytes as GiB to one decimal, the precision a refusal names.
func gib1(bytes int64) float64 { return math.Round(float64(bytes)/(1<<30)*10) / 10 }
