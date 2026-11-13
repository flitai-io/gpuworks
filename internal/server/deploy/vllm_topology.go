package deploy

import (
	"fmt"

	enginenames "gpuworks.ai/gpuworks/internal/engines"
)

// Tensor-parallel flag spellings. vLLM and SGLang accept the same three
// spellings of the dimension and each engine injects its own primary, so the
// spellings live here once and every engine list below only orders them.
const (
	tpFlagTensorParallelSize = "--tensor-parallel-size"
	tpFlagTPSize             = "--tp-size"
	tpFlagTP                 = "--tp"
)

var vllmTensorParallelFlags = []string{tpFlagTensorParallelSize, tpFlagTPSize, tpFlagTP}

// vllmDataParallelFlags is vLLM's data-parallel alias list, the injected
// spelling first. --data-parallel-size-local is a per-node share of the same
// degree and never matches these exact names.
var vllmDataParallelFlags = enginenames.DataParallelFlags(EngineVLLM)

func (vLLMEngine) SymbolizePreviewTP(launch ResolvedLaunch, templateResidual, userResidual []string) ResolvedLaunch {
	flags := (vLLMEngine{}).TPFlags()
	return symbolizeBuilderTP(launch, flags[0], flags, templateResidual, userResidual)
}

// ValidateTopology holds vLLM's world to tp × pp × dp: every data-parallel
// replica takes its own tensor × pipeline GPUs. An omni launch multiplies in
// the diffusion sequence and CFG degrees (omniParallelDegree). A command that states no TP
// is not judged unless it states a data-parallel degree above one; the engine
// then runs its default TP of one per replica, and the replicas' GPU count is
// as certain as a stated TP would make it.
func (vLLMEngine) ValidateTopology(launch ResolvedLaunch, params Params) error {
	arguments := launchArguments(launch)
	tp, tpSet, tpSymbolic, err := topologyInteger(arguments, (vLLMEngine{}).TPFlags()...)
	if err != nil {
		return err
	}
	pp, ppSet, ppSymbolic, err := topologyInteger(arguments, enginenames.PipelineFlags(EngineVLLM)...)
	if err != nil {
		return err
	}
	dp, dpSet, dpSymbolic, err := topologyInteger(arguments, vllmDataParallelFlags...)
	if err != nil {
		return err
	}
	omni, omniSymbolic, err := omniParallelDegree(arguments)
	if err != nil {
		return err
	}
	replicated := dpSet && !dpSymbolic && dp > 1
	if (!tpSet && !replicated) || tpSymbolic || ppSymbolic || dpSymbolic || omniSymbolic {
		return nil
	}
	if !tpSet {
		tp = 1
	}
	if !ppSet {
		pp = 1
	}
	world := assignedWorldSize(params)
	if replicated {
		if tp*pp*dp*omni != world {
			return dataParallelGPUCountError(EngineVLLM, dp, tp, pp*omni, world)
		}
		return nil
	}
	if omni > 1 {
		if tp*pp*omni != world {
			return fmt.Errorf("tensor parallel %d × pipeline parallel %d × diffusion sequence/CFG parallel %d requires %d GPUs, but placement assigned %d", tp, pp, omni, tp*pp*omni, world)
		}
		return nil
	}
	if tp*pp != world {
		return fmt.Errorf("tensor parallel %d × pipeline parallel %d requires %d GPUs, but placement assigned %d", tp, pp, tp*pp, world)
	}
	return nil
}
