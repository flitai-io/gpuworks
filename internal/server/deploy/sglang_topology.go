package deploy

import (
	"fmt"

	enginenames "gpuworks.ai/gpuworks/internal/engines"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// sglangTensorParallelFlags is SGLang's ordered TP alias list: every spelling
// the engine accepts, with the one the adapter injects (--tp-size) first. The
// spellings themselves live in vllm_topology.go, beside the shared TP aliases;
// the order lives here, where TPFlags and the topology code that reads it are.
var sglangTensorParallelFlags = []string{tpFlagTPSize, tpFlagTP, tpFlagTensorParallelSize}

// sglangPipelineParallelFlags is SGLang's pipeline-parallel alias list, the
// injected spelling first.
var sglangPipelineParallelFlags = enginenames.PipelineFlags(EngineSGLang)

func (sglangEngine) SymbolizePreviewTP(launch ResolvedLaunch, templateResidual, userResidual []string) ResolvedLaunch {
	flags := (sglangEngine{}).TPFlags()
	return symbolizeBuilderTP(launch, flags[0], flags, templateResidual, userResidual)
}

func sglangSingleProcessLogging(arguments []string) bool {
	flags := append([]string{engineParams(EngineSGLang)["data_parallel_size"].Flag}, sglangParallelFlags...)
	for _, flag := range flags {
		value, set, symbolic, err := topologyInteger(arguments, flag)
		if err != nil || symbolic || (set && value != 1) {
			return false
		}
	}
	return true
}

func (sglangEngine) ValidateTopology(launch ResolvedLaunch, params Params) error {
	arguments := launchArguments(launch)
	if err := validatePositiveTopologyFlags(arguments,
		append([]string{"--num-gpus", "--sp-degree", engineParams(EngineSGLang)["data_parallel_size"].Flag, "--ring-degree", "--ulysses-degree"}, sglangParallelFlags...)...,
	); err != nil {
		return err
	}
	numGPUs, numSet, numSymbolic, err := topologyInteger(arguments, "--num-gpus")
	if err != nil {
		return err
	}
	tp, tpSet, tpSymbolic, err := topologyInteger(arguments, (sglangEngine{}).TPFlags()...)
	if err != nil {
		return err
	}
	pp, ppSet, ppSymbolic, err := topologyInteger(arguments, sglangPipelineParallelFlags...)
	if err != nil {
		return err
	}
	if !ppSet {
		pp = 1
	}
	world := assignedWorldSize(params)
	if numSet && !numSymbolic && numGPUs != world {
		return fmt.Errorf("num-gpus %d does not match the %d GPUs assigned by placement", numGPUs, world)
	}
	// Diffusion commands can split the same world across TP and SP; num-gpus
	// is then the authoritative total.
	if numSet {
		return nil
	}
	return validateSGLangDataParallel(arguments, tp, tpSet, tpSymbolic, pp, ppSymbolic, world)
}

// sglangDataParallelFlags is SGLang's data-parallel alias list, the injected
// spelling first.
var sglangDataParallelFlags = enginenames.DataParallelFlags(EngineSGLang)

// validateSGLangDataParallel holds the world of a language-model command. The
// data-parallel degree means two things: under --enable-dp-attention the
// attention replicas share the tensor-parallel GPUs, so the world stays
// tp × pp and dp has to divide tp; without it every replica takes its own
// tp × pp GPUs and the world is tp × pp × dp. A command that states no TP is
// judged only when it states a data-parallel degree above one, with the
// engine's default TP of one.
func validateSGLangDataParallel(arguments []string, tp int, tpSet, tpSymbolic bool, pp int, ppSymbolic bool, world int) error {
	dp, dpSet, dpSymbolic, err := topologyInteger(arguments, sglangDataParallelFlags...)
	if err != nil {
		return err
	}
	replicated := dpSet && !dpSymbolic && dp > 1
	if (!tpSet && !replicated) || tpSymbolic || ppSymbolic {
		return nil
	}
	if !tpSet {
		tp = 1
	}
	if replicated || dpSymbolic {
		// Only a degree that can exceed one decides which world applies; a
		// symbolic one is judged once the switch says the world is tp × pp.
		attention, attentionSymbolic, err := topologySwitch(arguments, "--enable-dp-attention")
		if err != nil {
			return err
		}
		if attentionSymbolic || (dpSymbolic && !attention) {
			return nil
		}
		if !attention {
			if tp*pp*dp != world {
				return dataParallelGPUCountError(EngineSGLang, dp, tp, pp, world)
			}
			return nil
		}
	}
	if tp*pp != world {
		return fmt.Errorf("tp-size %d × pp-size %d requires %d GPUs, but placement assigned %d", tp, pp, tp*pp, world)
	}
	if replicated && tp%dp != 0 {
		flag := engineParams(EngineSGLang)[dataParallelKey].Flag
		return messages.NewProductError(ErrorDataParallelAttentionDivisor,
			fmt.Sprintf("with --enable-dp-attention, data parallel %d must divide tensor parallel %d, whose GPUs the attention replicas share; set %s to a divisor of %d", dp, tp, flag, tp),
			map[string]any{"key": dataParallelKey, "flag": flag, "dp": dp, "tp": tp})
	}
	return nil
}
