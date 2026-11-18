package deploy

import (
	"fmt"
	"strings"

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
	return parallelismFromArgs(engine, args, 0)
}

// RecommendationLaunchParallelism verifies the degrees the proposed command
// will run. A manual command may omit a degree on a recognized server CLI,
// where both TP and PP default to one. An opaque script cannot confirm a plan.
func RecommendationLaunchParallelism(engine string, launch ResolvedLaunch) (parallelism.Spec, error) {
	if CommandMode(launch.CommandMode) != CommandManual {
		return LaunchParallelism(engine, launch)
	}
	argv := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
	if shell := inspectShellCommand(launch.Entrypoint, launch.Argv); shell.shell {
		script := strings.TrimSpace(strings.TrimPrefix(shell.script, manualEnvironmentPreamble))
		script = strings.ReplaceAll(script, "\\\n", "")
		if strings.Contains(script, "\n") {
			command, ok := resolvedRankCommand(script)
			if !ok {
				return parallelism.Spec{}, fmt.Errorf("cannot verify parallelism of a compound manual command")
			}
			script = command
		}
		if !shell.commandString || strings.ContainsAny(script, ";&|<>()#") {
			return parallelism.Spec{}, fmt.Errorf("cannot verify parallelism of a compound manual command")
		}
		words, err := staticShellWords(script)
		if err != nil {
			return parallelism.Spec{}, fmt.Errorf("cannot verify manual command parallelism: %w", err)
		}
		if len(words) > 0 && words[0] == "exec" {
			words = words[1:]
		}
		argv = words
	}
	args, known := enginenames.ServerArguments(engine, argv)
	if !known {
		return parallelism.Spec{}, fmt.Errorf("cannot verify parallelism of an unrecognized manual command")
	}
	for _, arg := range args {
		if flag, _, _ := strings.Cut(arg, "="); flag == "--config" {
			return parallelism.Spec{}, fmt.Errorf("cannot verify parallelism from an external command configuration")
		}
	}
	return parallelismFromArgs(engine, args, 1)
}

func parallelismFromArgs(engine string, args []string, defaultTP int) (parallelism.Spec, error) {
	tp, found, symbolic, err := topologyInteger(args, TPFlagsForEngine(engine)...)
	if err != nil || symbolic || (!found && defaultTP == 0) {
		return parallelism.Spec{}, fmt.Errorf("the launch has no unambiguous tensor parallel degree")
	}
	if !found {
		tp = defaultTP
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
