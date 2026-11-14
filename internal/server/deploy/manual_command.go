package deploy

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

// rankOwnedVariables are the launch variables whose value belongs to one rank
// of one instance rather than to the deployment: a manual command shared by
// every rank has to reference them instead of stating a value.
var rankOwnedVariables = []string{
	placementeval.VarAPIPort, placementeval.VarNodeRank, placementeval.VarNodeAddr,
	placementeval.VarMasterAddr, placementeval.VarMasterPort,
	placementeval.VarPDSideChannelHost, placementeval.VarPDSideChannelPort,
}

// variableReference is how a manual command writes a platform variable: under
// its prefixed name, the only spelling expanded there.
func variableReference(name string) string {
	return "${" + placementeval.VariablePrefix + name + "}"
}

// multiRank reports whether this launch is one of several that share a
// deployment's command: a rank of a multi-node group or a PD instance.
func (p Params) multiRank() bool {
	return p.NNodes > 1 || p.NodeRank != 0 || p.Headless || p.PD != nil
}

func (p Params) apiPortText() string {
	if p.Symbolic {
		return variableReference(placementeval.VarAPIPort)
	}
	return strconv.Itoa(p.APIPort)
}

func (p Params) nodeRankText() string {
	if p.Symbolic {
		return variableReference(placementeval.VarNodeRank)
	}
	return strconv.Itoa(p.NodeRank)
}

func (p Params) masterPortText() string {
	if p.Symbolic {
		return variableReference(placementeval.VarMasterPort)
	}
	return strconv.Itoa(p.MasterPort)
}

func (p Params) sideChannelPortText() string {
	if p.Symbolic {
		return variableReference(placementeval.VarPDSideChannelPort)
	}
	return strconv.Itoa(p.PD.SideChannelPort)
}

// expandManualCommand replaces the variables this launch states in a manual
// command. Only the prefixed names are variables there: the command runs in a
// shell the user owns, and a script stored before manual commands had
// variables may use ${MODEL_PATH} or ${API_PORT} for its own. A prefixed name
// this launch does not state is left as written too, for the shell to report.
func expandManualCommand(values []string, variables map[string]any) []string {
	expanded, err := placementeval.Expand(values, placementeval.Prefixed(variables), true)
	if err != nil {
		// Only an unsupported Go type fails a lenient expansion, and
		// launchVariables states strings and integers.
		return values
	}
	return expanded
}

// ManualCommandRankBlind lists the variables a manual command has to reference
// before it can be shared by the ranks of this deployment, and does not: a
// multi-node command that mentions neither its rank nor whether it is headless
// starts every node as the same rank, and a PD command that does not mention
// its role starts every instance in the same role. Empty means the command can
// tell its launches apart.
func ManualCommandRankBlind(command string, nnodes int, pd bool) []string {
	referenced := placementeval.References(command)
	mentions := func(name string) bool {
		return slices.Contains(referenced, placementeval.VariablePrefix+name)
	}
	var missing []string
	if pd && !mentions(placementeval.VarPDRole) {
		missing = append(missing, placementeval.VariablePrefix+placementeval.VarPDRole)
	}
	if nnodes > 1 && !mentions(placementeval.VarNodeRank) && !mentions(placementeval.VarHeadless) {
		missing = append(missing, placementeval.VariablePrefix+placementeval.VarNodeRank)
	}
	return missing
}

// manualLaunchClass is what the ranks sharing one command differ by in shape,
// not just in values: a headless rank serves no API, and the PD roles run
// different connectors. Ranks of one class run the same command text.
type manualLaunchClass struct {
	Role     string
	Headless bool
}

func (c manualLaunchClass) pattern(pd bool) string {
	headless := "0"
	if c.Headless {
		headless = "1"
	}
	if pd {
		return c.Role + "/" + headless
	}
	return headless
}

// ManualCommandDraft is the script a multi-rank deployment's manual command
// starts from: the command the engine builds for each class of rank, with the
// rank-owned facts written as variable references, under one case statement
// over the class. Expanded for a rank (ResolveLaunch does that), the shell
// executes exactly the argv the engine would have built for it.
//
// ranks are the engine-mode launch inputs of the previewed ranks. ok is false
// when no such script exists: ranks of one class differ in something no
// variable carries (the HCA names of two nodes, say), or the engine command
// already contains text the expansion would rewrite.
func ManualCommandDraft(ranks []ResolveLaunchInput) (script string, ok bool, err error) {
	if len(ranks) == 0 {
		return "", false, nil
	}
	pd := ranks[0].Params.PD != nil
	var order []manualLaunchClass
	commands := map[manualLaunchClass]string{}
	for _, input := range ranks {
		concrete, err := ResolveLaunch(input)
		if err != nil {
			return "", false, err
		}
		for _, token := range launchArguments(concrete) {
			for _, name := range placementeval.References(token) {
				if strings.HasPrefix(name, placementeval.VariablePrefix) && placementeval.PlatformVariable(name) {
					return "", false, nil
				}
			}
		}
		symbolic := input
		symbolic.Params.Symbolic = true
		if symbolic.Params.MasterAddr != "" {
			symbolic.Params.MasterAddr = variableReference(placementeval.VarMasterAddr)
		}
		launch, err := ResolveLaunch(symbolic)
		if err != nil {
			return "", false, fmt.Errorf("manual command draft: %w", err)
		}
		class := manualLaunchClass{Headless: input.Params.Headless}
		if input.Params.PD != nil {
			class.Role = input.Params.PD.Role
		}
		command := shellJoin(launchArguments(launch))
		if existing, seen := commands[class]; seen {
			if existing != command {
				return "", false, nil
			}
			continue
		}
		commands[class] = command
		order = append(order, class)
	}
	key := variableReference(placementeval.VarHeadless)
	if pd {
		key = variableReference(placementeval.VarPDRole) + "/" + key
	}
	var b strings.Builder
	b.WriteString("set -eu\ncase \"" + key + "\" in\n")
	for _, class := range order {
		b.WriteString("  " + class.pattern(pd) + ")\n    exec " + commands[class] + "\n    ;;\n")
	}
	b.WriteString("  *)\n    echo \"unexpected launch class " + key + "\" >&2\n    exit 1\n    ;;\nesac")
	return b.String(), true, nil
}

var shellSafeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellJoin writes argv as one shell command line. A rank-owned reference
// needs no quoting of its own: it is replaced by a number or an address before
// a shell reads the line. Any other ${NAME} is text of the argument and is
// quoted so the shell leaves it alone, as the engine's argv did.
func shellJoin(argv []string) string {
	words := make([]string, len(argv))
	for index, word := range argv {
		plain := word
		for _, name := range rankOwnedVariables {
			plain = strings.ReplaceAll(plain, variableReference(name), "0")
		}
		if shellSafeWord.MatchString(plain) {
			words[index] = word
			continue
		}
		words[index] = "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
	}
	return strings.Join(words, " ")
}
