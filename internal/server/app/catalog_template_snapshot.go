package app

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/templateexport"
)

// Runtime fields belong to the deployment's immutable selection, not the
// catalog row that an administrator may have updated since it started.
func templateSpecFromSnapshot(selection deploy.CatalogSelection, image string) (inferencecatalog.InferenceSpec, error) {
	if selection.WeightProvider == "" || selection.WeightRepoID == "" || selection.Quantization == "" {
		return inferencecatalog.InferenceSpec{}, templateexport.Unavailable(templateexport.ReasonLaunchMissing, nil)
	}
	if len(selection.RuntimeAssets) > 0 {
		return inferencecatalog.InferenceSpec{}, templateexport.Unavailable(templateexport.ReasonTemplateAssets, nil)
	}
	if len(selection.Components) > 0 {
		return inferencecatalog.InferenceSpec{}, templateexport.Unavailable(templateexport.ReasonComponents, nil)
	}
	spec := inferencecatalog.InferenceSpec{
		Name: selection.SpecName, Scenario: selection.Scenario, Engine: selection.Engine, Image: selection.Image,
		ImageSources: selection.ImageSourcesFor(image),
		Weights: inferencecatalog.Weights{Quantization: selection.Quantization, Sources: []inferencecatalog.WeightSource{{
			Provider: selection.WeightProvider, RepoID: selection.WeightRepoID, Revision: selection.WeightRevision, Subfolder: selection.WeightSubfolder,
		}}},
		WeightBytes: selection.WeightBytes, RuntimeWeightBytes: selection.RuntimeWeightBytes,
		Args: selection.ExtraArgs, Env: selection.Env,
		Entrypoint: selection.Entrypoint, PreStartArgv: selection.PreStartArgv, PreStartScript: selection.PreStartScript,
		PDMode: selection.PDMode, ShmBytes: selection.ShmBytes, MemlockUnlimited: selection.MemlockUnlimited,
	}
	for _, field := range []struct {
		raw json.RawMessage
		out any
	}{{selection.Placement, &spec.Placement}, {selection.APIContract, &spec.API}} {
		if len(field.raw) > 0 {
			if err := json.Unmarshal(field.raw, field.out); err != nil {
				return inferencecatalog.InferenceSpec{}, err
			}
		}
	}
	return spec, nil
}

// A saved edit can intentionally leave old instances running (apply=none).
// Re-resolve current inputs against the captured allocation, never reschedule,
// and refuse a document that would describe a different executable generation.
func verifyTemplateLaunch(dep *store.Deployment, instance store.Instance, node *store.Node, model *store.Model, selection deploy.CatalogSelection, in *templateexport.Input) error {
	desired, err := instance.DesiredState()
	if err != nil || desired.UUID != instance.UUID || desired.DeploymentID != dep.ID || desired.Image == "" || len(desired.Entrypoint)+len(desired.Argv) == 0 {
		return templateexport.Unavailable(templateexport.ReasonLaunchMissing, nil)
	}
	if desired.Image != in.Image || (dep.Image != "" && dep.Image != desired.Image) || (desired.Engine != "" && desired.Engine != in.Engine) || desired.VRAMLimitBytes != in.VRAMLimitBytes {
		return templateexport.Unavailable(templateexport.ReasonLaunchChanged, nil)
	}
	request, _, _, err := planning.DeploymentLaunch(dep, nil, node, model, nil)
	if err != nil {
		return err
	}
	parallel, err := deploy.LaunchParallelism(in.Engine, desired.ResolvedLaunch)
	if err != nil {
		if len(deploy.TPFlagsForEngine(in.Engine)) > 0 {
			return templateexport.Unavailable(templateexport.ReasonLaunchMissing, nil)
		}
		// A dialect-free Serve Adapter may not expose any TP flag. Its
		// captured resolver variables take precedence, and complete executable
		// comparison below proves any allocation-derived expansion we use.
		parallel = request.Placement.Parallelism(1).Assigned(in.GPUCount)
	}
	stated := request.Placement.Parallelism(1)
	if stated.TP > 0 {
		parallel.TP = stated.TP
	}
	parallel.PP = stated.Normalized().PP
	var idents []string
	_ = json.Unmarshal([]byte(instance.GPUIdents), &idents)
	if !slices.Equal(desired.GPUIdents, idents) {
		return templateexport.Unavailable(templateexport.ReasonLaunchChanged, nil)
	}
	indexes := make([]int, 0, len(idents))
	for _, ident := range idents {
		for _, card := range node.GPUs {
			if card.Ident == ident {
				indexes = append(indexes, card.Index)
				break
			}
		}
	}
	// Timeout edits are live policy changes, not a new container generation.
	// Replay the captured budget so they do not change the engine environment
	// used to verify this instance's immutable executable.
	request.StartTimeoutSec = 0
	request.DefaultStartTimeoutSec = desired.StartTimeoutSeconds
	request.Source.Image = in.Image
	request.ModelMount = desired.ModelMount
	request.Topology.NNodes = 1
	request.Selection = selection
	plan, err := planning.ResolveAllocatedLaunch(request, indexes, len(indexes), parallel, desired.APIPort, desired.PlacementVariables, planning.ReadScheduleRecord(instance.ScheduleExpl).Breakdown.WeightBytes, false)
	if err != nil {
		return templateexport.Unavailable(templateexport.ReasonLaunchChanged, nil)
	}
	actual, planned := desired.ResolvedLaunch, plan.Resolved
	if !slices.Equal(actual.Entrypoint, planned.Entrypoint) || !slices.Equal(actual.Argv, planned.Argv) ||
		!slices.Equal(actual.PreStartArgv, planned.PreStartArgv) || actual.PreStartScript != planned.PreStartScript ||
		!maps.Equal(actual.Env, planned.Env) || !slices.Equal(actual.DockerArgs, planned.DockerArgs) ||
		actual.ShmBytes != planned.ShmBytes || !slices.Equal(actual.Mounts, planned.Mounts) || actual.ModelMount != planned.ModelMount {
		return templateexport.Unavailable(templateexport.ReasonLaunchChanged, nil)
	}
	// Keep raw snapshot variables portable. The comparison above proves their
	// expansion for this allocation; concrete EnvLayers would bake its port or
	// model path into a template intended for another allocation.
	in.TemplateEnv, in.UserEnv = maps.Clone(in.TemplateEnv), maps.Clone(in.UserEnv)
	for _, key := range request.Source.Overrides.EnvUnset {
		delete(in.TemplateEnv, strings.TrimSpace(key))
	}
	for _, env := range []map[string]string{in.TemplateEnv, in.UserEnv} {
		for key := range env {
			if _, present := actual.Env[key]; !present {
				delete(env, key)
			}
		}
	}
	if strings.TrimSpace(string(actual.APIContract)) != "" && in.TemplateSpec != nil {
		if err := json.Unmarshal(actual.APIContract, &in.TemplateSpec.API); err != nil {
			return err
		}
	}
	return nil
}
