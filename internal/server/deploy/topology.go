package deploy

import (
	"fmt"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
)

// Data-parallel codes answer a stated data-parallel degree the assigned GPUs
// cannot run. Placement assigns GPUs for tensor × pipeline × data parallelism,
// so each engine's topology validator judges the degree against its own
// arithmetic and names the dictionary key the deploy form shows it beside.
const (
	// ErrorDataParallelGPUCount reports data-parallel replicas that each take
	// their own tensor × pipeline GPUs (vLLM, and SGLang without DP attention)
	// when the product does not equal the GPUs placement assigned.
	ErrorDataParallelGPUCount = "DATA_PARALLEL_GPU_COUNT"
	// ErrorDataParallelAttentionDivisor reports an SGLang DP-attention degree
	// that does not divide the tensor-parallel size it shares GPUs with.
	ErrorDataParallelAttentionDivisor = "DATA_PARALLEL_ATTENTION_DIVISOR"
)

// dataParallelKey is the dictionary key that carries each engine's
// data-parallel flag.
const dataParallelKey = "data_parallel_size"

func dataParallelGPUCountError(engine string, dp, tp, pp, world int) error {
	required := tp * pp * dp
	flag := engineParams(engine)[dataParallelKey].Flag
	return messages.NewProductError(ErrorDataParallelGPUCount,
		fmt.Sprintf("data parallel %d × tensor parallel %d × pipeline parallel %d requires %d GPUs, but placement assigned %d; set %s to 1 or make the product equal %d", dp, tp, pp, required, world, flag, world),
		map[string]any{"key": dataParallelKey, "flag": flag, "dp": dp, "tp": tp, "pp": pp, "required": required, "assigned": world})
}

func validateEngineTopology(engine Engine, launch ResolvedLaunch, params Params) error {
	if err := hooks(engine).ValidateTopology(launch, params); err != nil {
		return fmt.Errorf("%s topology: %w", engine.Name(), err)
	}
	return nil
}

func assignedWorldSize(params Params) int {
	local := params.AssignedGPUs
	if local <= 0 {
		// A caller that assigned no GPUs (a preview before scheduling) launches
		// the per-node share its stated parallelism implies.
		local, _ = parallelism.Spec{TP: params.TensorParallel, PP: params.PipelineParallel, NNodes: params.NNodes}.GPUsPerNode()
	}
	if local <= 0 {
		local = 1
	}
	nodes := params.NNodes
	if nodes <= 0 {
		nodes = 1
	}
	return local * nodes
}

func launchArguments(launch ResolvedLaunch) []string {
	result := make([]string, 0, len(launch.Entrypoint)+len(launch.Argv))
	result = append(result, launch.Entrypoint...)
	result = append(result, launch.Argv...)
	return result
}

func validatePositiveTopologyFlags(arguments []string, names ...string) error {
	for _, name := range names {
		_, _, _, err := topologyInteger(arguments, name)
		if err != nil {
			return err
		}
	}
	return nil
}

// topologySwitch reads a store_true style switch from arguments: the bare flag
// turns it on, an inline boolean value states it either way and a --no- prefix
// turns it off; the last spelling wins, as it does in the engine's parser. A
// symbolic inline value leaves the state unknown until it is expanded.
func topologySwitch(arguments []string, name string) (on, symbolic bool, err error) {
	negated := "--no-" + strings.TrimPrefix(name, "--")
	for _, argument := range arguments {
		flag, raw, hasValue := strings.Cut(argument, "=")
		switch flag {
		case negated:
			on, symbolic = false, false
		case name:
			if !hasValue {
				on, symbolic = true, false
				continue
			}
			if strings.Contains(raw, "$") {
				on, symbolic = false, true
				continue
			}
			parsed, parseErr := strconv.ParseBool(strings.TrimSpace(raw))
			if parseErr != nil {
				return false, false, fmt.Errorf("%s takes no value or a boolean, got %q", name, raw)
			}
			on, symbolic = parsed, false
		}
	}
	return on, symbolic, nil
}

func topologyInteger(arguments []string, names ...string) (int, bool, bool, error) {
	accepted := make(map[string]struct{}, len(names))
	for _, name := range names {
		accepted[name] = struct{}{}
	}
	var value int
	found, symbolic := false, false
	for index := 0; index < len(arguments); index++ {
		name, raw, matched := splitTopologyArgument(arguments[index], accepted)
		if !matched {
			continue
		}
		if raw == "" {
			if index+1 >= len(arguments) {
				return 0, false, false, fmt.Errorf("%s requires a value", name)
			}
			index++
			raw = arguments[index]
		}
		if strings.Contains(raw, "$") {
			if found && !symbolic {
				return 0, false, false, fmt.Errorf("%s has conflicting concrete and symbolic values", name)
			}
			found, symbolic = true, true
			continue
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return 0, false, false, fmt.Errorf("%s requires a positive integer, got %q", name, raw)
		}
		if found && (symbolic || parsed != value) {
			return 0, false, false, fmt.Errorf("%s has conflicting values", name)
		}
		value, found = parsed, true
	}
	return value, found, symbolic, nil
}

func splitTopologyArgument(argument string, accepted map[string]struct{}) (string, string, bool) {
	name, raw, hasEquals := strings.Cut(argument, "=")
	if _, ok := accepted[name]; !ok {
		return "", "", false
	}
	if !hasEquals {
		raw = ""
	}
	return name, raw, true
}

// PlacementDataParallel returns the GPU-multiplying data-parallel degree for
// scheduling. The launch validator remains responsible for engine topology.
func PlacementDataParallel(engine string, quickDP int, templateArgs, userArgs []string) (int, error) {
	dp := quickDP
	if dp < 1 {
		dp = 1
	}
	args := append(append([]string(nil), templateArgs...), userArgs...)
	flags := vllmDataParallelFlags
	if engine == EngineSGLang {
		flags = sglangDataParallelFlags
	}
	value, set, symbolic, err := topologyInteger(args, flags...)
	if err != nil {
		return 0, err
	}
	if symbolic {
		return 0, fmt.Errorf("data parallel degree must be concrete for placement")
	}
	if set {
		dp = value
	}
	if engine == EngineSGLang {
		attention, symbolic, err := topologySwitch(args, "--enable-dp-attention")
		if err != nil {
			return 0, err
		}
		if symbolic {
			return 0, fmt.Errorf("DP attention mode must be concrete for placement")
		}
		if attention {
			return 1, nil
		}
	}
	return dp, nil
}
