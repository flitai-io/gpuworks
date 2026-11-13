package templateexport

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// ErrorUnavailable reports a deployment that cannot be saved as a template.
// Params carry reason (one of the Reason constants) and, for some reasons,
// the values that decided it.
const ErrorUnavailable = messages.ErrorTemplateExportUnavailable

// Reasons a deployment cannot be exported. They are stable wire values.
const (
	ReasonNotRunning       = "not_running"
	ReasonDistributed      = "distributed"
	ReasonPD               = "pd"
	ReasonNoGPUFacts       = "no_gpu_facts"
	ReasonTemplateAssets   = "template_assets"
	ReasonComponents       = "components"
	ReasonWeightsNotPublic = "weights_not_public"
	ReasonRevisionUnpinned = "revision_unpinned"
	ReasonRevisionsDiffer  = "revisions_differ"
	ReasonTemplateMissing  = "template_missing"
	ReasonModelMissing     = "model_missing"
	ReasonLaunchChanged    = "launch_changed"
	ReasonLaunchMissing    = "launch_snapshot_missing"
	ReasonMixedGPUs        = "heterogeneous_gpus"
	// ReasonCommandOverridden: the deployment replaced the engine command,
	// so its launch is not the engine's command plus args.
	ReasonCommandOverridden = "command_overridden"
)

// Unavailable is the error for a deployment that cannot be saved as a
// template for reason.
func Unavailable(reason string, params map[string]any) error {
	all := map[string]any{"reason": reason}
	for key, value := range params {
		all[key] = value
	}
	return messages.NewProductError(ErrorUnavailable, "the deployment cannot be saved as a template: "+reason, all)
}

// Note is one fact about an export the YAML comments and the evidence state.
// Code is stable; Params are the values a client interpolates.
type Note struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// Note codes.
const (
	// NoteVerifiedOn states the cards the spec ran on (gpu, count).
	NoteVerifiedOn = "verified_on"
	// NoteLocalWeights: the template references registered local files and
	// does not bundle or make those files downloadable on another cluster.
	NoteLocalWeights = "local_weights"
	// NoteOperationPassed and NoteOperationFailed report an operation the
	// export sent a request for (operation, http_status, message).
	NoteOperationPassed = "operation_passed"
	NoteOperationFailed = "operation_failed"
	// NoteOperationNotTested is an operation the export does not send a
	// request for, because one is slow or costly (video, speech).
	NoteOperationNotTested = "operation_not_tested"
	// NoteKVBytesMissing: the catalog has no kv_bytes_per_token for the
	// model and engine, so the spec states none (engine).
	NoteKVBytesMissing = "kv_bytes_missing"
	// NoteModelConfigMissing: the model's config.json was not available, so
	// the spec has no model_config for estimation.
	NoteModelConfigMissing = "model_config_missing"
	// NotePlacementOmitted: the spec states no placement; the cards and the
	// memory the deployment had are recorded here (gpu_count, gpu_memory_mib).
	NotePlacementOmitted = "placement_omitted"
	// NoteVRAMLimit: the deployment ran under a dGPU memory limit per card
	// (vram_limit_mib). It is the deployment's choice, not a requirement.
	NoteVRAMLimit = "vram_limit"
	// NoteEnvExcluded: user env keys left out (keys).
	NoteEnvExcluded = "env_excluded"
	// NoteParametersDropped: configuration keys no engine flag carries
	// (keys).
	NoteParametersDropped = "parameters_dropped"
	// NoteContainerOverridesOmitted: mounts and docker args are not exported.
	NoteContainerOverridesOmitted = "container_overrides_omitted"
)

func launchNotes(in Input, launch deploy.ExportedArgs, excluded []string, model inferencecatalog.InferenceModel) []Note {
	card := in.GPUs[0]
	notes := []Note{{Code: NoteVerifiedOn, Params: map[string]any{"gpu": card.Name, "count": in.GPUCount}}}
	if len(model.Specs) > 0 {
		for _, source := range model.Specs[0].Weights.Sources {
			if source.Provider == "local" {
				notes = append(notes, Note{Code: NoteLocalWeights, Params: map[string]any{"repo_id": source.RepoID}})
			}
		}
	}
	if len(model.Specs) > 0 && model.Specs[0].Placement == nil {
		memory := card.MemoryMiB
		if in.VRAMLimitBytes > 0 {
			memory = in.VRAMLimitBytes >> 20
		}
		notes = append(notes, Note{Code: NotePlacementOmitted, Params: map[string]any{"gpu_count": in.GPUCount, "gpu_memory_mib": memory}})
	}
	if in.VRAMLimitBytes > 0 {
		notes = append(notes, Note{Code: NoteVRAMLimit, Params: map[string]any{"vram_limit_mib": in.VRAMLimitBytes >> 20}})
	}
	if len(excluded) > 0 {
		notes = append(notes, Note{Code: NoteEnvExcluded, Params: map[string]any{"keys": excluded}})
	}
	if len(launch.Dropped) > 0 {
		notes = append(notes, Note{Code: NoteParametersDropped, Params: map[string]any{"keys": launch.Dropped}})
	}
	if in.HasContainerOverrides {
		notes = append(notes, Note{Code: NoteContainerOverridesOmitted})
	}
	return notes
}

func checkNotes(checks []Check) []Note {
	notes := make([]Note, 0, len(checks))
	for _, check := range checks {
		params := map[string]any{"operation": check.Operation}
		switch {
		case !check.Tested:
			notes = append(notes, Note{Code: NoteOperationNotTested, Params: params})
		case check.Passed:
			params["http_status"] = check.HTTPStatus
			notes = append(notes, Note{Code: NoteOperationPassed, Params: params})
		default:
			params["http_status"] = check.HTTPStatus
			params["message"] = check.Message
			notes = append(notes, Note{Code: NoteOperationFailed, Params: params})
		}
	}
	return notes
}
