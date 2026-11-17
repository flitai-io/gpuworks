package api

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// deploymentPDDTO is the PD section of a deployment response. Its role counts
// are the ready and asleep counts of the same walk the response's own
// ready_replicas and asleep_replicas come from (deploymentFactsOf), so a role
// never reads ready in one field and asleep in another, and the sleep state of
// the deployment is read against the roles the gateway needs (a prefill role
// its mode serves without does not make the deployment asleep when empty).
func deploymentPDDTO(rows deployment.Rows, d *store.Deployment, insts []store.Instance, view replicaView, facts deployment.Facts) *dto.DeploymentPD {
	pd, err := planning.PDOf(d.Placement)
	if err != nil || pd == nil {
		return nil
	}
	names, match := view.names, view.Match
	mode, _ := pdmode.Lookup(pd.Mode)
	slots := map[string]planning.UnitSlot{}
	for _, slot := range match.Slots {
		slots[fmt.Sprintf("%s/%d", slot.Unit.Role, slot.Unit.Index)] = slot
	}
	out := &dto.DeploymentPD{Mode: pd.Mode, Fallback: pd.EffectiveFallback(), KVLoadFailure: pd.EffectiveKVLoadFailure(),
		State: pdStateOf(view, facts), Roles: []dto.DeploymentPDRole{}}
	for _, role := range view.Roles() {
		counts := facts.Roles[role]
		unitCount := len(pd.Units(role))
		row := dto.DeploymentPDRole{Role: role, TP: pd.TP(role), PP: pd.Roles[role].PP, Desired: unitCount, Ready: counts.Awake,
			Asleep: counts.Asleep, MayBeEmpty: mode.MayBeEmpty(role), Units: []dto.DeploymentPDUnit{}}
		if row.Ready < row.Desired {
			out.Short = true
		}
		for index, unit := range pd.Units(role) {
			item := dto.DeploymentPDUnit{NodeUUID: unit.NodeUUID, NodeName: names[unit.NodeUUID], GPUs: unit.GPUIndexes}
			if slot, ok := slots[fmt.Sprintf("%s/%d", role, index)]; ok {
				unitAttempts(rows, &item, slot, match.Failed[slot.Key])
			}
			for _, rank := range unit.Followers {
				item.Followers = append(item.Followers, dto.DeploymentPDRank{NodeUUID: rank.NodeUUID, NodeName: names[rank.NodeUUID], GPUs: rank.GPUIndexes})
			}
			row.Units = append(row.Units, item)
		}
		out.Roles = append(out.Roles, row)
	}
	return out
}

// pdStateOf is a PD deployment's state from the gateway's own reading of its
// roles, not from a separate count: running when every role the gateway needs
// has an awake replica, partial when one of them has none, not_ready when
// nothing is awake and the roles are asleep (or absent), and the same states
// distinguish a role asleep from a role down the way the deployment's own
// state does.
func pdStateOf(view replicaView, facts deployment.Facts) string {
	anyAwake := false
	for _, role := range view.Roles() {
		if facts.Roles[role].Awake > 0 {
			anyAwake = true
		}
	}
	if !anyAwake {
		return planning.PDStateNotReady
	}
	for _, role := range view.Roles() {
		counts := facts.Roles[role]
		if counts.Awake > 0 || !view.RoleRequired(role, counts) {
			continue
		}
		return planning.PDStatePartial
	}
	return planning.PDStateRunning
}

// unitAttempts fills a unit's spent retry budget: how many launches failed,
// the newest failure, and whether the controller gave up on it.
func unitAttempts(rows deployment.Rows, item *dto.DeploymentPDUnit, slot planning.UnitSlot, attempts []store.Instance) {
	spent := spentAttempts(rows, slot, attempts)
	item.FailedAttempts, item.LastFailure, item.RetryExhausted = spent.count, spent.lastFailure, spent.exhausted
}
