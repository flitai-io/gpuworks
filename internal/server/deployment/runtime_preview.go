package deployment

import (
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func evaluateMounts(in []proto.MountSpec) []dto.EvaluateMount {
	out := make([]dto.EvaluateMount, 0, len(in))
	for _, m := range in {
		out = append(out, dto.EvaluateMount{Source: m.Source, Volume: m.Volume, Target: m.Target, ReadOnly: m.ReadOnly, SizeHint: m.SizeHint, Purpose: m.Purpose})
	}
	return out
}

// launchCommand is the full command line of a resolved launch.
func launchCommand(res deploy.ResolvedLaunch) []string {
	return append(append([]string{}, res.Entrypoint...), res.Argv...)
}

func evaluateEnvLayers(res deploy.ResolvedLaunch) *dto.EnvLayers {
	return &dto.EnvLayers{System: res.EnvLayers.System, Template: res.EnvLayers.Template, User: res.EnvLayers.User}
}

// setEvaluateLaunch shows a resolved launch as the preview's command: the
// one place the preview response takes its launch fields from.
func setEvaluateLaunch(resp *dto.EvaluateResponse, res deploy.ResolvedLaunch) {
	resp.StartupEstimate = dto.StartupEstimateOf(res.StartupEstimate)
	resp.StartTimeoutSec, resp.StartTimeoutSource = res.StartTimeoutSeconds, res.StartTimeoutSource
	resp.StartTimeoutConflict, resp.EngineStartTimeoutSec = res.StartTimeoutConflict, res.EngineStartTimeoutSeconds
	resp.PreStartArgv, resp.PreStartScript = res.PreStartArgv, res.PreStartScript
	resp.Entrypoint, resp.Argv, resp.Command, resp.Env = res.Entrypoint, res.Argv, launchCommand(res), res.Env
	resp.EnvLayers = evaluateEnvLayers(res)
	resp.CommandMode, resp.ModelMount, resp.DockerArgs, resp.Mounts = res.CommandMode, res.ModelMount, res.DockerArgs, evaluateMounts(res.Mounts)
	resp.ShmBytes = res.ShmBytes
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func previewComponentMounts(st *store.Store, components []deploy.ComponentBinding, node *store.Node) (string, []proto.MountSpec) {
	modelMount := deploy.DefaultModelMount
	if primary, ok := deploy.PrimaryComponent(components); ok {
		modelMount = primary.MountPath
	}
	var mounts []proto.MountSpec
	for _, component := range components {
		if component.Role == "primary" {
			continue
		}
		source := "$MODEL_COMPONENT_" + componentVariableName(component.Name) + "_PATH"
		if st != nil && node != nil && component.StoreModelID != 0 {
			if file, err := st.ModelFileByModelNode(component.StoreModelID, node.ID); err == nil && file.Path != "" && proto.ModelFileStateReady(file.State) {
				source = file.Path
			}
		}
		mounts = append(mounts, proto.MountSpec{Source: source, Target: component.MountPath, ReadOnly: true, Purpose: "model_component:" + component.Role})
	}
	return modelMount, mounts
}

func componentVariableName(value string) string {
	var out strings.Builder
	for _, r := range strings.ToUpper(value) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	return out.String()
}

// resolveRequestLaunch is the API boundary shared by evaluation and create:
// the request owns only its container overrides, and the template component's
// primary mount is applied identically before the deploy resolver runs. The
// warnings are for the evaluation to show; create ignores them.
func resolveRequestLaunch(input deploy.ResolveLaunchInput, raw map[string]any, components []deploy.ComponentBinding, modelMount string) (deploy.ResolvedLaunch, []messages.Warning, error) {
	overrides, err := RequestContainerOverrides(raw)
	if err != nil {
		return deploy.ResolvedLaunch{}, nil, err
	}
	input.ContainerOverrides = overrides
	if len(components) > 0 {
		input.ComponentModelMount = modelMount
	}
	return deploy.ResolveLaunchWithWarnings(input)
}

// RequestContainerOverrides decodes and validates a request's
// container_overrides object. The image is an artifact reference and
// contributes nothing (ADR 0031); what the request states is the whole layer.
func RequestContainerOverrides(raw map[string]any) (deploy.ContainerOverrides, error) {
	if raw == nil {
		return deploy.ContainerOverrides{}, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return deploy.ContainerOverrides{}, fmt.Errorf("invalid container overrides: %w", err)
	}
	return deploy.DecodeContainerOverrides(encoded)
}
