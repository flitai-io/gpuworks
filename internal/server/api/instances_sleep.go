package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Sleep and wake for one replica. The order names any instance of the
// replica; the intent is written on all of the replica's instances
// (planning.ReplicaMembers, store.OrderInstancesSleep), and the deployment's
// other replicas keep serving. Whether the replica may take the order is
// planning.InstanceSleepRefusal.

// sleepInstance orders the replica an instance fills to sleep (?level=1|2,
// ?mode=wait|abort, ?wake_on_request=true|false, as for the deployment).
func (a *apiHandlers) sleepInstance(w http.ResponseWriter, r *http.Request) {
	defer a.orders().Lock()()
	level, mode, ok := sleepOrderParams(w, r)
	if !ok {
		return
	}
	dep, members, ok := a.instanceSleepTarget(w, r, true)
	if !ok {
		return
	}
	wakes, ok := wakeOnRequestParam(w, r, dep)
	if !ok {
		return
	}
	if a.refuseUnsupportedSleepTargets(w, dep.Engine, members) {
		return
	}
	if !a.orderSleep(w, dep, planning.SleepOrder{Pref: planning.SleepPreference(level), Mode: mode, Held: !wakes, Instances: uuidsOf(members)}, writeReplicaOrderErr) {
		return
	}
	a.sleepOrdered(r, dep.ID, level, mode, "replica", "", uuidsOf(members), wakes)
	writeJSON(w, http.StatusOK, dto.SleepOrder{Status: "sleeping", Level: level, Mode: mode, Instances: uuidsOf(members), WakeOnRequest: wakes})
}

// wakeInstance wakes the replica an instance fills once its GPUs can take it
// back, the same admission as for the deployment, for this replica alone.
func (a *apiHandlers) wakeInstance(w http.ResponseWriter, r *http.Request) {
	defer a.orders().Lock()()
	dep, members, ok := a.instanceSleepTarget(w, r, false)
	if !ok {
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
	how, ok := wakeWaitParams(w, r)
	if !ok {
		return
	}
	if how.wait || how.yield {
		a.wakeAndWait(w, r, dep, match, members, false, how)
		return
	}
	woken, refused, err := planning.OrderWake(a.d.Store, dep, match, members, false, false)
	if err != nil {
		writeReplicaOrderErr(w, err)
		return
	}
	if len(refused) > 0 {
		writeWakeRefusal(w, refused)
		return
	}
	a.wakeOrdered(r, dep.ID, uuidsOf(woken))
	writeJSON(w, http.StatusOK, dto.WakeResult{Status: "waking", Instances: uuidsOf(members)})
}

// instanceSleepTarget loads the instance a sleep (sleeping) or wake order
// names, its deployment and the instances of the replica it fills, and refuses
// what planning.InstanceSleepRefusal refuses: a deployment that cannot sleep
// (400), an instance that fills no replica or the last awake replica of a role
// the mode needs (409, the deployment's state). It writes the response on
// refusal.
func (a *apiHandlers) instanceSleepTarget(w http.ResponseWriter, r *http.Request, sleeping bool) (*store.Deployment, []store.Instance, bool) {
	inst, err := a.d.Store.InstanceByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil, nil, false
	}
	dep, err := a.d.Store.GetDeployment(inst.DeploymentID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil, nil, false
	}
	insts, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, nil, false
	}
	match, err := a.deploymentUnitMatch(dep, insts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return nil, nil, false
	}
	if refusal := planning.InstanceSleepRefusal(dep, match, inst, sleeping); refusal != nil {
		status := http.StatusBadRequest
		if refusal.Code == ErrorInstanceNotServing || refusal.Code == ErrorSleepRoleCannotEmpty {
			status = http.StatusConflict
		}
		writeSleepRefusal(w, status, refusal)
		return nil, nil, false
	}
	return dep, planning.ReplicaMembers(match.SlotOf(inst.UUID)), true
}

// uuidsOf is the instances' UUIDs in sorted order, so responses list them
// deterministically.
func uuidsOf(insts []store.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, in := range insts {
		out = append(out, in.UUID)
	}
	slices.Sort(out)
	return out
}

// wakeRefusalDTO is a refusal as the wire lists it. A GPU held outside the
// platform has no shortfall to add: how much it lacks is unknown.
func wakeRefusalDTO(r planning.WakeRefusal) dto.WakeRefusal {
	out := dto.WakeRefusal{Instances: uuidsOf(r.Instances), Node: nodeDisplayName(r.Node), GPUIndexes: []int{}, Occupants: []string{}}
	var shortfallBytes int64
	for _, s := range r.Verdict.Shortfalls {
		out.GPUIndexes = append(out.GPUIndexes, s.GPUIndex)
		out.Occupants = appendUnique(out.Occupants, s.Occupants...)
		if s.HeldOutside {
			out.HeldOutside = true
			continue
		}
		shortfallBytes += s.NeedBytes - s.FreeBytes
	}
	out.ShortfallGiB = fmt.Sprintf("%.1f", float64(shortfallBytes)/(1<<30))
	return out
}

// writeWakeRefusal answers a wake the GPUs cannot take with 409: the message
// and params describe the first refusal, "refused" lists every one. A GPU a
// process outside the platform holds is its own code: freeing it is not a
// matter of stopping a deployment.
func writeWakeRefusal(w http.ResponseWriter, refusals []planning.WakeRefusal) {
	first := wakeRefusalDTO(refusals[0])
	shortfalls := make([]map[string]any, 0, len(refusals[0].Verdict.Shortfalls))
	for _, s := range refusals[0].Verdict.Shortfalls {
		shortfalls = append(shortfalls, map[string]any{
			"gpu_index": s.GPUIndex, "need_bytes": s.NeedBytes, "free_bytes": s.FreeBytes, "occupants": s.Occupants, "held_outside": s.HeldOutside,
		})
	}
	all := make([]dto.WakeRefusal, 0, len(refusals))
	for _, r := range refusals {
		all = append(all, wakeRefusalDTO(r))
	}
	params := map[string]any{
		"node": first.Node, "gpu_indexes": first.GPUIndexes, "shortfall_gib": first.ShortfallGiB,
		"occupants": first.Occupants, "shortfalls": shortfalls, "refused": all,
	}
	if first.HeldOutside {
		writeProductError(w, http.StatusConflict, ErrorWakeGPUHeldOutside,
			fmt.Sprintf("GPU %v on node %s is held by a process outside the platform; the sleeping instance stays asleep until it is gone", first.GPUIndexes, first.Node), params)
		return
	}
	writeProductError(w, http.StatusConflict, ErrorWakeInsufficientGPUMemory,
		fmt.Sprintf("GPU %v on node %s cannot take the sleeping instance back: %s GiB short; stop or sleep %s first", first.GPUIndexes, first.Node, first.ShortfallGiB, strings.Join(first.Occupants, ", ")),
		params)
}

// writeReplicaOrderErr answers an order that could not be written to a
// replica's instances. A member removed since the replica was read is the
// replicas changing under the request (409), not a server failure.
func writeReplicaOrderErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeProductError(w, http.StatusConflict, ErrorReplicasChanged, "the replica changed since the request was read; read it again", nil)
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}
