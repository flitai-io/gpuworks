package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// fieldContainerCmd is the deploy form control a manual command is edited in.
const fieldContainerCmd = "container_cmd"

// checkManualCommandRanks refuses a new multi-node or PD request whose manual
// command cannot tell its launches apart: the deployment stores one command,
// every rank runs it, and a command that references neither its rank nor its
// role starts them all alike. Evaluate and create run it on the same request;
// the controller never does, so a deployment stored before the rule keeps
// launching. A command the overrides cannot decode is refused elsewhere.
func checkManualCommandRanks(req *dto.DeployRequest) error {
	overrides, err := RequestContainerOverrides(req.ContainerOverrides)
	if err != nil {
		return nil //nolint:nilerr // Overrides that do not decode are refused by the request validation, with its own error.
	}
	if deploy.CommandMode(overrides.CommandMode) != deploy.CommandManual || strings.TrimSpace(overrides.CmdDisplay) == "" {
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
	missing := deploy.ManualCommandRankBlind(overrides.CmdDisplay, nnodes, pd != nil)
	if len(missing) == 0 {
		return nil
	}
	return productError(messages.ErrorManualCommandRankBlind,
		"the manual command of a multi-node or PD deployment runs on every rank and must reference "+strings.Join(missing, ", "),
		map[string]any{"missing": missing})
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
func setManualCommandDraft(resp *dto.EvaluateResponse, ranks []deploy.ResolveLaunchInput) {
	if len(ranks) < 2 {
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
