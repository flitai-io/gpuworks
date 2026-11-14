package placement

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var variablePattern = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)

// Template variables the platform states for every launch. A template uses
// them to place platform-owned facts - where the weights are, which port the
// gateway and health check dial, which rank of a distributed group this
// process is - anywhere in its own command, instead of an engine adapter
// appending them in a fixed shape the template cannot see.
const (
	VarGPUCount        = "GPU_COUNT"
	VarTPSize          = "TP_SIZE"
	VarModelPath       = "MODEL_PATH"
	VarServedModelName = "SERVED_MODEL_NAME"
	VarAPIHost         = "API_HOST"
	VarAPIPort         = "API_PORT"
	VarNNodes          = "NNODES"
	VarNodeRank        = "NODE_RANK"
	VarHeadless        = "HEADLESS"
	VarNodeAddr        = "NODE_ADDR"
	VarMasterAddr      = "MASTER_ADDR"
	VarMasterPort      = "MASTER_PORT"
	// A PD disaggregated instance also states which role it plays and where
	// its KV side channel listens (the first port of its extra port block).
	VarPDRole            = "PD_ROLE"
	VarPDSideChannelHost = "PD_SIDE_CHANNEL_HOST"
	VarPDSideChannelPort = "PD_SIDE_CHANNEL_PORT"
)

// VariablePrefix is the documented spelling of every platform variable:
// ${GPUWORKS_NODE_RANK}. The bare names are older and stay valid in a
// template's launch fields; a manual command, which runs in a shell with
// variables of its own, is given the prefixed names only.
const VariablePrefix = "GPUWORKS_"

// Canonical is the name a variable is stated under: the bare one.
func Canonical(name string) string {
	if bare, ok := strings.CutPrefix(name, VariablePrefix); ok && bare != "" {
		return bare
	}
	return name
}

// Prefixed is the variable set under the prefixed names only.
func Prefixed(variables map[string]any) map[string]any {
	out := make(map[string]any, len(variables))
	for name, value := range variables {
		out[VariablePrefix+Canonical(name)] = value
	}
	return out
}

// Lookup finds a variable by either spelling.
func Lookup(variables map[string]any, name string) (any, bool) {
	if value, ok := variables[name]; ok {
		return value, true
	}
	if bare := Canonical(name); bare != name {
		value, ok := variables[bare]
		return value, ok
	}
	return nil, false
}

// launchOwned are the variables only the launch can know. A placement resolver
// may not return them: a resolver runs before a node, a port or a rank exists,
// so a value it returned would be a guess that silently shadows the fact.
// GPU_COUNT and TP_SIZE are not here: resolvers have always been allowed to
// return them (TP_SIZE pins a degree, GPU_COUNT is overwritten by the count).
var launchOwned = map[string]struct{}{
	VarModelPath: {}, VarServedModelName: {}, VarAPIHost: {}, VarAPIPort: {}, VarNNodes: {}, VarNodeRank: {},
	VarHeadless: {}, VarNodeAddr: {}, VarMasterAddr: {}, VarMasterPort: {},
	VarPDRole: {}, VarPDSideChannelHost: {}, VarPDSideChannelPort: {},
}

// LaunchOwned reports whether name is a variable only the launch states.
func LaunchOwned(name string) bool {
	_, owned := launchOwned[Canonical(name)]
	return owned
}

// PlatformVariable reports whether the platform states name for some launch:
// the launch-owned facts plus GPU_COUNT and TP_SIZE. A template without a
// placement resolver can reference nothing else.
func PlatformVariable(name string) bool {
	name = Canonical(name)
	return LaunchOwned(name) || name == VarGPUCount || name == VarTPSize
}

// References lists the ${NAME} variables values mention, in first-use order.
func References(values ...string) []string {
	var names []string
	seen := map[string]struct{}{}
	for _, value := range values {
		for _, match := range variablePattern.FindAllStringSubmatch(value, -1) {
			if _, ok := seen[match[1]]; !ok {
				seen[match[1]] = struct{}{}
				names = append(names, match[1])
			}
		}
	}
	return names
}

func Expand(values []string, variables map[string]any, allowUnresolved bool) ([]string, error) {
	result := make([]string, len(values))
	for index, value := range values {
		var expandErr error
		result[index] = variablePattern.ReplaceAllStringFunc(value, func(match string) string {
			if expandErr != nil {
				return match
			}
			name := variablePattern.FindStringSubmatch(match)[1]
			replacement, ok := Lookup(variables, name)
			if !ok {
				if !allowUnresolved {
					expandErr = fmt.Errorf("template variable %s is unresolved", name)
				}
				return match
			}
			switch typed := replacement.(type) {
			case string:
				return typed
			case int:
				return strconv.Itoa(typed)
			case int64:
				return strconv.FormatInt(typed, 10)
			case bool:
				return strconv.FormatBool(typed)
			default:
				expandErr = fmt.Errorf("template variable %s has unsupported Go type %T", name, replacement)
				return match
			}
		})
		if expandErr != nil {
			return nil, fmt.Errorf("expand argument %d: %w", index, expandErr)
		}
	}
	return result, nil
}

func RuntimeVariables(result Result, gpuCount, tensorParallel int, modelPath string) map[string]any {
	variables := make(map[string]any, len(result.Variables)+3)
	for name, value := range result.Variables {
		variables[name] = value
	}
	variables[VarGPUCount] = int64(gpuCount)
	variables[VarModelPath] = modelPath
	if _, supplied := variables[VarTPSize]; !supplied {
		variables[VarTPSize] = int64(tensorParallel)
	}
	// PP_SIZE has no default on purpose. Pipeline parallelism already has one
	// owner: the engine injects --pipeline-parallel-size from the node count for
	// a multi-node launch and injects nothing for a single node. A placement
	// default would be a second owner of the same number, free to disagree with
	// the engine, and a spec that references ${PP_SIZE} without returning it
	// would launch on a degree nobody chose instead of failing. A profile that
	// wants a pipeline split of its own returns PP_SIZE and names it in args;
	// the template flag then blocks the engine's injection, so exactly one of
	// the two ever writes the flag.
	return variables
}

func ExpandMap(values map[string]string, variables map[string]any, allowUnresolved bool) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for name, value := range values {
		expanded, err := Expand([]string{value}, variables, allowUnresolved)
		if err != nil {
			return nil, fmt.Errorf("expand %s: %w", name, err)
		}
		result[name] = expanded[0]
	}
	return result, nil
}

// Symbolize converts manifest placeholders to the shell-neutral form shown in
// an offline command preview. No shell evaluates the result.
func Symbolize(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = variablePattern.ReplaceAllString(value, `$$$1`)
	}
	return result
}
