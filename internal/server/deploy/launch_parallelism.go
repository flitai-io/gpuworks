package deploy

import (
	"fmt"

	enginenames "gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
)

// LaunchParallelism reads concrete topology from the immutable executable using
// the engine's existing alias and conflict rules. It does not infer TP from the
// card count: DP, PP and diffusion can make those different.
func LaunchParallelism(engine string, launch ResolvedLaunch) (parallelism.Spec, error) {
	args, _, ok := LaunchArgs(launch)
	if !ok {
		return parallelism.Spec{}, fmt.Errorf("the launch command cannot be parsed")
	}
	tp, found, symbolic, err := topologyInteger(args, TPFlagsForEngine(engine)...)
	if err != nil || !found || symbolic {
		return parallelism.Spec{}, fmt.Errorf("the launch has no unambiguous tensor parallel degree")
	}
	pp, found, symbolic, err := topologyInteger(args, enginenames.PipelineFlags(engine)...)
	if err != nil || symbolic {
		return parallelism.Spec{}, fmt.Errorf("the launch has no unambiguous pipeline parallel degree")
	}
	if !found {
		pp = 1
	}
	return parallelism.Spec{TP: tp, PP: pp, NNodes: 1}, nil
}
