package deploy

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

type Mount = proto.MountSpec
type EnvLayers = proto.LaunchEnvLayers

// DefaultModelMount is the container path a deployment's weights are mounted
// at unless a template component or an override names another one. It is the
// deployment-side contract shared by the resolver, the runtime preview and the
// controller.
const DefaultModelMount = "/model"

// DefaultMasterPort is the port a distributed launch's master rank listens on
// when the request names none. It is the deployment-side fact the form and the
// preflight read instead of carrying their own literals.
const DefaultMasterPort = 29500

// ResolveLaunchInput contains the authoritative inputs to launch resolution:
// the engine and its parameters, the selected template's launch fields,
// placement output and the deployment's container overrides. It is
// intentionally an input object rather than another runtime representation:
// only ResolvedLaunch may leave this package.
// TargetValidationMode separates capability collection from concrete allocation.
type TargetValidationMode uint8

const (
	ValidateTargetCapabilities TargetValidationMode = iota
	ValidateAssignedTarget
	CollectTargetCapabilities
)

type ResolveLaunchInput struct {
	// Startup budgets are supplied by the calling policy, after which this
	// resolver coordinates them with the final managed engine environment.
	StartTimeoutSec        int
	DefaultStartTimeoutSec int
	TargetValidation       TargetValidationMode
	Engine                 Engine
	Params                 Params
	ContainerOverrides     ContainerOverrides
	// ComponentModelMount is the template component's primary mount; it
	// replaces DefaultModelMount, and an override may not contradict it.
	ComponentModelMount string
	// Image is the effective runtime image selected for this launch. Fixed
	// release images can require a runtime-specific compatibility overlay.
	Image           string
	Entrypoint      []string
	PreStartArgv    []string
	PreStartScript  string
	RuntimeAssets   []proto.RuntimeAsset
	ComponentMounts []proto.MountSpec
	APIContract     any
	// PlacementVariables are the placement resolver's variables; the launch
	// adds the facts it owns (launchVariables) before expanding the template.
	PlacementVariables map[string]any
	// AllowUnresolved is set by a preview whose placement is not settled: the
	// variables it cannot state yet stay symbolic instead of failing.
	AllowUnresolved bool
	HealthPath      string
	// ModelWeightBytes is the estimator's weight total for this deployment. It
	// is the only input to the KV disk tier's automatic capacity ceiling, and
	// nothing else reads it; a caller that has no estimate leaves it zero and
	// the mount goes out without a size hint.
	ModelWeightBytes int64
	// TemplateShmBytes is the /dev/shm size the selected template spec states
	// (shm_bytes); zero when the deployment has no template or the spec states
	// none. See launchShmBytes for how it combines with the engine default, the
	// CPU KV tier and the deployment's own shm_bytes.
	TemplateShmBytes int64
	// TemplateMemlockUnlimited is the selected template spec's
	// memlock_unlimited: the container starts with --ulimit memlock=-1:-1
	// unless the deployment's own docker args state a memlock ulimit.
	TemplateMemlockUnlimited bool
}

// ResolveLaunch is the only public path that merges catalog facts, image
// defaults, deployment choices and placement output into an executable launch.
// Preview and controller must both call it.
func ResolveLaunch(input ResolveLaunchInput) (ResolvedLaunch, error) {
	launch, _, err := ResolveLaunchWithWarnings(input)
	return launch, err
}

// ResolveLaunchWithWarnings is ResolveLaunch plus what the platform cannot
// prove about the launch: warnings never fail it, and a caller that has
// somewhere to show them (the evaluation) does.
func ResolveLaunchWithWarnings(input ResolveLaunchInput) (ResolvedLaunch, []messages.Warning, error) {
	if input.Engine == nil {
		return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch: engine is required")
	}
	overrides := input.ContainerOverrides
	command, err := ResolveEffectiveCommand(overrides, input.Entrypoint)
	if err != nil {
		return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch: %w", err)
	}
	modelMount := DefaultModelMount
	if component := strings.TrimSpace(input.ComponentModelMount); component != "" {
		if overrides.ModelMountPath != "" && overrides.ModelMountPath != component {
			return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch: cannot override component primary mount %s with %s", component, overrides.ModelMountPath)
		}
		modelMount = component
	}
	if overrides.ModelMountPath != "" {
		modelMount = overrides.ModelMountPath
	}

	params := input.Params
	entrypoint := append([]string(nil), input.Entrypoint...)
	preStartArgv := append([]string(nil), input.PreStartArgv...)
	preStartScript := input.PreStartScript
	if command.Mode == CommandEngine && strings.TrimSpace(overrides.CmdDisplay) == "" {
		if err := requireTemplateVariables(input.Engine, params.TemplateArgs, entrypoint, preStartArgv, []string{preStartScript}, envValues(params.TemplateEnv)); err != nil {
			return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch: %w", err)
		}
	}
	variables := launchVariables(input.Engine, params, modelMount, input.PlacementVariables, input.AllowUnresolved)
	params.TemplateArgs, err = expandValues(params.TemplateArgs, variables, input.AllowUnresolved)
	if err == nil {
		params.TemplateEnv, err = placementeval.ExpandMap(params.TemplateEnv, variables, input.AllowUnresolved)
	}
	if err == nil {
		entrypoint, err = expandValues(entrypoint, variables, input.AllowUnresolved)
	}
	if err == nil {
		preStartArgv, err = expandValues(preStartArgv, variables, input.AllowUnresolved)
	}
	if err == nil && preStartScript != "" {
		preStartScript, err = expandScript(preStartScript, variables, input.AllowUnresolved)
	}
	if err != nil {
		return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch variables: %w", err)
	}
	var allocatorRewrite ascendAllocatorRewrite
	if input.Engine.Name() == EngineVLLM {
		if allocatorRewrite, err = prepareVLLMSleepable(&params); err != nil {
			return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch: %w", err)
		}
	}

	built := input.Engine.Build(params)
	for _, extra := range []map[string]string{computeNetworkEnv(params), runveilEnv(input.Engine.Name(), params)} {
		if len(extra) == 0 {
			continue
		}
		if built.Env == nil {
			built.Env = map[string]string{}
		}
		for k, v := range extra {
			built.Env[k] = v
		}
	}
	mode := command.Mode
	manualScript := ""
	if mode == CommandManual && strings.TrimSpace(overrides.CmdDisplay) != "" {
		candidate := strings.TrimSpace(overrides.CmdDisplay)
		// The deployment command editor owns a complete shell command and emits
		// a readable block containing comments, environment assignments and the
		// launch command. JSON arrays retain the catalog argv contract.
		if !strings.HasPrefix(candidate, "[") {
			manualScript = candidate
		}
	}
	var commandArgv []string
	if manualScript == "" {
		commandArgv, err = parseOptionalCommand(overrides.CmdDisplay)
		if err != nil {
			return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch command: %w", err)
		}
	}

	finalEntrypoint := command.Entrypoint
	// Template variables are expanded above; preserve that expanded form when
	// the template remains the effective entrypoint.
	if command.templateEntrypoint {
		finalEntrypoint = entrypoint
	}
	launch := AssembleCommand(input.Engine, built, ImageCommand{
		Mode:       mode,
		Entrypoint: finalEntrypoint,
		Cmd:        commandArgv,
	})
	for key, value := range built.EnvLayers.Template {
		launch.Env[key] = value
	}
	// env_unset removes what any lower layer set; the deployment's own values
	// still apply after it.
	for _, key := range overrides.EnvUnset {
		delete(launch.Env, strings.TrimSpace(key))
	}
	for key, value := range overrides.Env {
		launch.Env[key] = value
	}
	if err := validateVLLMSleepableEnv(input.Engine.Name(), params, launch.Env); err != nil {
		return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch: %w", err)
	}
	if overrides.CmdDisplay != "" {
		launch.Argv = append([]string(nil), commandArgv...)
		if mode == CommandManual && manualScript == "" && len(launch.Entrypoint) == 0 && len(commandArgv) > 0 {
			launch.Entrypoint = append([]string(nil), commandArgv[:1]...)
			launch.Argv = append([]string(nil), commandArgv[1:]...)
		}
	}
	if mode == CommandEngine && len(finalEntrypoint) > 0 {
		if overrides.CmdDisplay != "" {
			// A deployment command override is the complete argv. Replacing the
			// executable must not re-extract parameters from the adapter command
			// and overwrite that explicit argv.
			launch.Entrypoint = append([]string(nil), finalEntrypoint...)
		} else {
			// Engine mode starts from the adapter-built command, so its effective
			// entrypoint still needs to replace the adapter executable while
			// retaining the adapter-generated model and engine parameters.
			launch = WithEntrypoint(input.Engine, launch, finalEntrypoint)
		}
	}
	if manualScript != "" {
		// The command editor takes ownership of the complete startup script,
		// including preparation. Running template preparation again here would
		// execute the same side effects twice after switching to manual mode.
		if overrides.CommandIncludesPreparation {
			preStartArgv = nil
			preStartScript = ""
		}
		// A complete manual command owns the executable as well as its arguments,
		// including when the inference template normally supplies an entrypoint.
		launch.Entrypoint = []string{"/bin/sh"}
		manualScript = replaceManualCommandPort(manualScript, params.APIPort)
		manualScript = replaceManualServedName(manualScript, params.EngineServedName())
		// Export assignments from the readable environment section before the
		// launch command runs. This also keeps command text created by older UIs
		// executable without rewriting the persisted override.
		launch.Argv = []string{"-c", "set -a\n" + manualScript}
	}

	if modelMount != DefaultModelMount {
		launch.Entrypoint = ReplaceModelPathArgs(launch.Entrypoint, DefaultModelMount, modelMount)
		launch.Argv = ReplaceModelPathArgs(launch.Argv, DefaultModelMount, modelMount)
	}
	launch.CommandMode = string(mode)
	launch.PreStartArgv = preStartArgv
	launch.PreStartScript = preStartScript
	launch.RuntimeAssets = append([]proto.RuntimeAsset(nil), input.RuntimeAssets...)
	launch.EnvLayers = proto.LaunchEnvLayers{
		System:   cloneEnv(built.Env),
		Template: cloneEnv(built.EnvLayers.Template),
		User:     cloneEnv(overrides.Env),
	}
	engram := inspectEngramLaunch(launch.Entrypoint, launch.Argv)
	engramShared := engram.state.shared
	launch.DockerArgs = launchDockerArgs(overrides.DockerArgs, params, input.TemplateMemlockUnlimited || engramShared)
	launch.DockerArgs = withEngramSharedMemoryDockerArgs(launch.DockerArgs, engram.args)
	shmDefault := input.Engine.Profile().ShmBytes
	if engramShared && shmDefault < engramSharedMemoryDefaultBytes {
		shmDefault = engramSharedMemoryDefaultBytes
	}
	// The CPU KV tier lives in /dev/shm, so the size follows the tier the
	// command was built with; see launchShmBytes for the rule.
	launch.ShmBytes, err = launchShmBytes(shmDefault, input.TemplateShmBytes, overrides,
		launchKVCPUTierBytes(input.Engine, params.Quick, command, overrides))
	if err != nil {
		return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch: %w", err)
	}
	launch.Mounts = convertMounts(overrides.ExtraMounts)
	launch.Mounts = append(launch.Mounts, input.ComponentMounts...)
	// The disk KV tier writes into a container path that has to survive the
	// container and stay measurable, so the launch asks the agent for a managed
	// host directory there. Appending last means a user mount on the same target
	// is reported as the conflict it is rather than silently shadowed.
	if mount, needed := kvDiskMount(params.Quick, input.ModelWeightBytes); needed {
		launch.Mounts = append(launch.Mounts, mount)
	}
	launch.Mounts = append(launch.Mounts, computeNetworkMounts(params)...)
	if err := validateResolvedMounts(launch.Mounts, modelMount); err != nil {
		return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch mounts: %w", err)
	}
	launch.ModelMount = modelMount
	launch.HealthPath = input.HealthPath
	launch.PlacementVariables = cloneVariables(input.PlacementVariables)
	if raw, ok := input.APIContract.(json.RawMessage); ok {
		launch.APIContract = append(json.RawMessage(nil), raw...)
	} else if input.APIContract != nil {
		launch.APIContract, err = json.Marshal(input.APIContract)
		if err != nil {
			return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch API contract: %w", err)
		}
	}
	if len(launch.APIContract) > 0 {
		var contract struct {
			HealthEndpoint string `json:"health_endpoint"`
		}
		if err := json.Unmarshal(launch.APIContract, &contract); err != nil {
			return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch API contract: %w", err)
		}
		if strings.TrimSpace(contract.HealthEndpoint) != "" {
			launch.HealthPath = strings.TrimSpace(contract.HealthEndpoint)
		}
	}
	// The engine's last word over the fully merged launch, before the merged
	// result is validated.
	launch = hooks(input.Engine).FinalizeLaunch(launch)
	compat := ascendRuntimeCompatFor(input.Engine.Name(), input.Image, params.RuntimeDriver, launch.Env)
	preCompatScript := launch.PreStartScript
	launch.PreStartScript = appendAscendRuntimeCompatibilityScript(compat, launch.PreStartScript)
	if err := rejectUnrunnableLaunch(launch, input.Image, compat, preCompatScript); err != nil {
		return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch: %w", err)
	}
	// A create request with automatic placement has no GPU count yet. Validate
	// the concrete topology after scheduling; applying the temporary builder TP
	// of one here would reject a valid multi-GPU template before placement.
	if !(input.AllowUnresolved && params.AssignedGPUs <= 0 && params.TensorParallel <= 0) {
		if err := validateEngineTopology(input.Engine, launch, params); err != nil {
			return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch: %w", err)
		}
	}
	launch.RequestedFeatures = ResolveRequestedFeatures(input.Engine.Name(), launch, params)
	var targetErr error
	if input.TargetValidation == CollectTargetCapabilities {
		targetErr = nil
	} else if input.TargetValidation == ValidateTargetCapabilities {
		targetErr = ValidateTargetFeatures(input.Engine.Name(), launch, params)
	} else {
		targetErr = ValidateTargetLaunch(input.Engine.Name(), launch, params)
	}
	if targetErr != nil {
		return ResolvedLaunch{}, nil, targetErr
	}
	warnings := launchWarnings(launch, params, input.Image, compat, engram)
	if warning, ok := allocatorRewrite.warning(launch.Env); ok {
		warnings = append(warnings, warning)
	}
	if err := resolveStartupBudget(input, &launch); err != nil {
		return ResolvedLaunch{}, nil, fmt.Errorf("resolve launch: %w", err)
	}
	return launch, warnings, nil
}

var manualCommandPortPattern = regexp.MustCompile(`(?m)(^|[ \t])--port(?:[ \t]+|=)[^ \t\r\n;|&]+`)
var manualCommandServedNamePattern = regexp.MustCompile(`(?m)(^|[ \t])--served-model-name(?:[ \t]+|=)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^ \t\r\n;|&]+)`)

// replaceManualCommandPort keeps the manually edited engine command aligned
// with the host port reserved later by the scheduler. The deploy preview uses
// 30000 before a concrete instance exists, while retries and concurrent
// deployments can receive another port.
func replaceManualCommandPort(command string, port int) string {
	if port <= 0 {
		return command
	}
	return manualCommandPortPattern.ReplaceAllString(command, "${1}--port "+strconv.Itoa(port))
}

// replaceManualServedName applies a configured engine alias to a manual launch.
func replaceManualServedName(command, servedName string) string {
	if strings.TrimSpace(servedName) == "" {
		return command
	}
	quoted := "'" + strings.ReplaceAll(servedName, "'", "'\"'\"'") + "'"
	return manualCommandServedNamePattern.ReplaceAllString(command, "${1}--served-model-name "+quoted)
}

func validateResolvedMounts(mounts []proto.MountSpec, modelMount string) error {
	modelMount = path.Clean(strings.TrimSpace(modelMount))
	seen := map[string]struct{}{modelMount: {}}
	for _, mount := range mounts {
		target := strings.TrimSpace(mount.Target)
		if target == "" || !strings.HasPrefix(target, "/") || path.Clean(target) != target || target == "/" {
			return fmt.Errorf("invalid target %q", mount.Target)
		}
		if _, duplicate := seen[target]; duplicate {
			return messages.NewProductError(messages.ErrorMountTargetDuplicate,
				fmt.Sprintf("two mounts target %s inside the container", target),
				map[string]any{"target": target})
		}
		for existing := range seen {
			if mountPathContains(existing, target) || mountPathContains(target, existing) {
				return fmt.Errorf("nested mount targets %q and %q overlap", existing, target)
			}
		}
		seen[target] = struct{}{}
	}
	return nil
}

func mountPathContains(parent, child string) bool {
	parent, child = path.Clean(parent), path.Clean(child)
	return parent != "." && parent != "/" && strings.HasPrefix(child, parent+"/")
}

func parseOptionalCommand(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	return ParseCommandDisplay(value)
}

func cloneVariables(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

// ReplaceModelPathArgs updates positional and --model/--model-path forms.
func ReplaceModelPathArgs(args []string, from, to string) []string {
	if len(args) == 0 || from == to {
		return append([]string(nil), args...)
	}
	out := append([]string(nil), args...)
	for i := range out {
		if out[i] == from {
			out[i] = to
		}
		for _, flag := range []string{"--model=", "--model-path="} {
			if len(out[i]) > len(flag) && out[i][:len(flag)] == flag && out[i][len(flag):] == from {
				out[i] = flag + to
			}
		}
		if (out[i] == "--model" || out[i] == "--model-path") && i+1 < len(out) && out[i+1] == from {
			out[i+1] = to
		}
	}
	return out
}
