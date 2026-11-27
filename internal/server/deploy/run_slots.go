package deploy

import (
	"strconv"
	"strings"
	"sync"
)

// Run slots: how many sequences an engine runs at once, as its launch states
// it (the max_num_seqs parameter: vLLM --max-num-seqs, SGLang
// --max-running-requests). The platform writes no default for it and does
// not know the engine's own, so a launch that does not state the cap has no
// known slots, and nothing is inferred from its absence. Like
// SpecRunsPriority, this reads the launch the instance was given, whoever
// wrote the flag (template, form or the user's own arguments).
const runSlotsKey = "max_num_seqs"

// dataParallelAliases are the engines' other spellings of the data-parallel
// size, which the dictionary does not write but a user's arguments may.
var dataParallelAliases = map[string][]string{
	EngineVLLM:   {"-dp"},
	EngineSGLang: {"--data-parallel-size"},
}

type runSlotFlags struct {
	cap          string
	dataParallel []string
}

// runSlotSpellings is each engine's flags for the cap and for the
// data-parallel size, read once from the parameter dictionary.
var runSlotSpellings = sync.OnceValue(func() map[string]runSlotFlags {
	out := map[string]runSlotFlags{}
	for _, engine := range []string{EngineVLLM, EngineSGLang} {
		flags := runSlotFlags{dataParallel: dataParallelAliases[engine]}
		for _, definition := range parameterDefinitions {
			switch definition.Key {
			case runSlotsKey:
				flags.cap = definition.Flags[engine].On
			case dataParallelKey:
				if flag := definition.Flags[engine].On; flag != "" {
					flags.dataParallel = append(flags.dataParallel, flag)
				}
			}
		}
		if flags.cap != "" {
			out[engine] = flags
		}
	}
	return out
})

// RunSlots is the concurrent-sequence cap args (LaunchArgs of a launch of
// engine) state for one replica; ok is false when it is unknown: an engine
// without such a flag, a launch that does not state it, a value that is not
// a positive integer (or one the shell would still expand), or a launch with
// a data-parallel size above 1, whose engine may apply the cap per rank.
func RunSlots(engine string, args []string) (int, bool) {
	flags, known := runSlotSpellings()[engine]
	if !known {
		return 0, false
	}
	for _, flag := range flags.dataParallel {
		if !statesFlag(args, flag) {
			continue
		}
		// A size the launch does not fix counts as above 1.
		value, _ := LaunchFlag(args, flag)
		if size, err := strconv.Atoi(value); err != nil || size != 1 {
			return 0, false
		}
	}
	value, found := LaunchFlag(args, flags.cap)
	if !found {
		return 0, false
	}
	slots, err := strconv.Atoi(value)
	if err != nil || slots <= 0 {
		return 0, false
	}
	return slots, true
}

// statesFlag reports whether args carry flag at all, as `flag value` or
// `flag=value`, whatever the value.
func statesFlag(args []string, flag string) bool {
	for index, arg := range args {
		if (arg == flag && index+1 < len(args)) || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}
