package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fieldContainerCmd is the deploy form control a manual command is edited in.
const fieldContainerCmd = "container_cmd"

// checkManualCommandRanks refuses a new multi-node or PD request whose command
// cannot tell its launches apart: the deployment stores one command and every
// rank runs it. A manual command that references neither its rank nor its
// role starts them all alike; a Cmd override in any other mode is an argv the
// platform hands to every rank as written, with no variable to tell them
// apart, so it is refused outright. Evaluate and create run the check on the
// same request; the controller never does, so a deployment stored before the
// rule keeps launching. A command the overrides cannot decode is refused
// elsewhere.
func checkManualCommandRanks(req *dto.DeployRequest) error {
	overrides, err := RequestContainerOverrides(req.ContainerOverrides)
	if err != nil {
		return nil //nolint:nilerr // Overrides that do not decode are refused by the request validation, with its own error.
	}
	if strings.TrimSpace(overrides.CmdDisplay) == "" {
		return nil
	}
	nnodes := req.NNodes
	pd := placementPD(req)
	if pd != nil {
		// A PD instance of several nodes is a multi-node group of its own.
		for role := range pd.Roles {
			for _, unit := range pd.Units(role) {
				nnodes = max(nnodes, len(unit.Ranks()))
			}
		}
	}
	if deploy.CommandMode(overrides.CommandMode) != deploy.CommandManual {
		if nnodes > 1 || pd != nil {
			return productError(messages.ErrorCommandOverrideRankBlind,
				"a Cmd override of a multi-node or PD deployment is the same argv on every rank; use a manual command, which the server expands per rank", nil)
		}
		return nil
	}
	missing := deploy.ManualCommandRankBlind(overrides.CmdDisplay, nnodes, pd != nil)
	if len(missing) == 0 {
		return nil
	}
	return productError(messages.ErrorManualCommandRankBlind,
		"the manual command of a multi-node or PD deployment runs on every rank and must reference "+strings.Join(missing, ", "),
		map[string]any{"missing": missing})
}

// manualCommandPortTaken warns about each port a manual command script states
// that is already taken (network.TakenEnginePorts) on node, the node the
// preview serves the API from. A stated port is replaced by the assigned one
// either way; a taken one is what a deployer who wrote it is told. An edit's
// own instances (excludeDeployment) take no port from it, nor do the
// listeners on the ports they hold. The check is best effort: a node whose
// instances cannot be read warns about nothing.
func (s *Service) manualCommandPortTaken(overrides deploy.ContainerOverrides, node *store.Node, excludeDeployment uint) []messages.Warning {
	ports := deploy.ManualCommandPorts(overrides)
	if len(ports) == 0 || node == nil {
		return nil
	}
	instances, err := s.d.Store.InstancesForNode(node.ID)
	if err != nil {
		return nil
	}
	var others, own []store.Instance
	for _, instance := range instances {
		if excludeDeployment != 0 && instance.DeploymentID == excludeDeployment {
			own = append(own, instance)
		} else {
			others = append(others, instance)
		}
	}
	taken := network.TakenEnginePorts(network.Inventory(*node), others)
	for port := range network.TakenEnginePorts(nil, own) {
		delete(taken, port)
	}
	var warnings []messages.Warning
	for _, port := range ports {
		if taken[port] {
			warnings = append(warnings, deploy.ManualCommandPortTakenWarning(port, node.DisplayName()))
		}
	}
	return warnings
}

// manualDraftInput is the engine-mode launch a rank would run, whatever
// command the request currently states: the draft is what a manual command
// starts from, so it is built from the launch the platform would assemble.
func manualDraftInput(input deploy.ResolveLaunchInput) deploy.ResolveLaunchInput {
	input.ContainerOverrides.CmdDisplay = ""
	input.ContainerOverrides.CommandMode = ""
	input.ContainerOverrides.CommandIncludesPreparation = false
	return input
}

// setManualCommandDraft answers a multi-node or PD preview with the manual
// command script its ranks share and the variables they state, built from the
// resolver inputs the preview planned each rank with (planning.LaunchPlan
// .LaunchInput), so the draft cannot drift from the launches shown beside it.
// A draft that cannot be built faithfully is a warning, never an error: the
// draft only seeds the editor.
//
// A spec that owns its command has already put that script in the draft
// (applySpecManualCommand); it stays, and only the variables are added.
func setManualCommandDraft(resp *dto.EvaluateResponse, ranks []deploy.ResolveLaunchInput) {
	if len(ranks) < 2 {
		return
	}
	if resp.ManualCommandDraft != "" {
		inputs := make([]deploy.ResolveLaunchInput, 0, len(ranks))
		for _, rank := range ranks {
			if rank.Engine != nil {
				inputs = append(inputs, manualDraftInput(rank))
			}
		}
		resp.ManualCommandVariables = deploy.ManualCommandVariables(inputs)
		return
	}
	inputs := make([]deploy.ResolveLaunchInput, len(ranks))
	for index, rank := range ranks {
		if rank.Engine == nil {
			// The plan refused this rank before it resolved a launch.
			resp.Warnings = append(resp.Warnings, messages.NewWarning(messages.WarningManualDraftUnavailable, nil))
			return
		}
		inputs[index] = manualDraftInput(rank)
	}
	draft, ok, err := deploy.ManualCommandDraft(inputs)
	if err != nil || !ok {
		resp.Warnings = append(resp.Warnings, messages.NewWarning(messages.WarningManualDraftUnavailable, nil))
		return
	}
	resp.ManualCommandDraft = draft
	resp.ManualCommandVariables = deploy.ManualCommandVariables(inputs)
}
