// lint_args.go reports dictionary decoding problems without changing the
// lossless ReverseArgs reader. Catalog imports surface these as warnings;
// newly authored user arguments are validated at the deployment boundary.
// Both consume the same walk, so their dictionary facts cannot diverge.

package deploy

import (
	"fmt"
	"slices"
	"strings"

	enginenames "gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// ArgProblem is one argv token that names a dictionary parameter and states a
// value that parameter cannot hold. Value is empty when the flag was the last
// token and carried no value at all.
type ArgProblem struct {
	Flag   string
	Value  string
	Key    string
	Reason string
}

// String renders a problem as the phrase an import error embeds.
func (p ArgProblem) String() string {
	if p.Value == "" {
		return fmt.Sprintf("%s (%s) %s", p.Flag, p.Key, p.Reason)
	}
	return fmt.Sprintf("%s (%s) value %q %s", p.Flag, p.Key, p.Value, p.Reason)
}

// LintArgs reports the tokens of an already-tokenized engine argv that name a
// dictionary parameter and cannot be decoded into it.
//
// What it deliberately stays quiet about is as much of the contract as what it
// reports, because each silence is a case where the dictionary has no opinion:
//
//   - a flag no key owns is the author's to spell however the engine spells it;
//   - a flag several keys share (vLLM's --speculative-config, which carries
//     three keys, and --kv-transfer-config, which carries four) cannot be
//     attributed to one key, so no key's type applies to its value;
//   - a boolean written as a bare switch states its own value;
//   - an engine with no parameter dictionary (callable, whose launch argv
//     belongs to a catalog-owned adapter) has no key to judge against.
//
// The first three are silences of the shared walk. The last one is this
// function's own: reverseArgs reads any registered engine against the vLLM
// column, which is harmless for a reader asked about an engine it knows and
// meaningless as a judgement, so an engine outside the dictionary is refused a
// verdict here rather than given a wrong one.
func LintArgs(engine string, args []string) []ArgProblem {
	selected, err := SelectEngine(engine)
	if err != nil || !slices.Contains(enginenames.DialectNames(), selected.Name()) {
		return nil
	}
	_, _, _, problems, err := reverseArgs(selected.Name(), args)
	if err != nil {
		return nil
	}
	return problems
}

// ValidateExtraArgsForEngine judges only user arguments at a request boundary.
// A dictionary flag whose value cannot be decoded is an error there, not an
// advanced passthrough. Unknown flags remain the deployer's escape hatch;
// catalog and stored argv retain ReverseArgs' lossless reading contract.
func ValidateExtraArgsForEngine(engine string, args []string) error {
	problems := LintArgs(engine, args)
	if len(problems) > 0 {
		return paramErrorf(ParamFieldExtraArgs, "invalid extra_args: %s", problems[0])
	}
	return nil
}

// ExtraArgsEngineWarnings names residual user flags that another engine's
// dictionary documents. Absence from the target dictionary does not establish
// that its CLI refuses a flag: aliases and additional flags stay passthrough.
func ExtraArgsEngineWarnings(engine string, args []string) []messages.Warning {
	selected, err := SelectEngine(engine)
	if err != nil || !slices.Contains(enginenames.DialectNames(), selected.Name()) {
		return nil
	}
	_, _, residual, _, err := reverseArgs(selected.Name(), args)
	if err != nil {
		return nil
	}
	var warnings []messages.Warning
	seen := map[string]bool{}
	add := func(flag, documentedEngine string) {
		if seen[flag] {
			return
		}
		seen[flag] = true
		warnings = append(warnings, messages.NewWarning(messages.WarningExtraArgsEngineUnverified, map[string]any{
			"flag": flag, "engine": selected.Name(), "documented_engine": documentedEngine,
		}))
	}
	targetFlags := flagIndex(selected.Name())
	for _, other := range enginenames.DialectNames() {
		if other == selected.Name() {
			continue
		}
		otherFlags := flagIndex(other)
		for _, token := range residual {
			flag, _, _ := splitInlineFlag(token)
			if len(targetFlags[flag]) > 0 {
				continue // a composite flag may belong to several target keys
			}
			if len(otherFlags[flag]) > 0 {
				add(flag, other)
				continue
			}
			// A composed short flag can glue its value into the token. The
			// dictionary owns its prefix, as it owns -O for the -O3 spelling.
			for name, entries := range otherFlags {
				if !strings.HasPrefix(name, "-") || strings.HasPrefix(name, "--") || !strings.HasPrefix(token, name) {
					continue
				}
				for _, entry := range entries {
					if entry.spec.Composed {
						add(name, other)
					}
				}
			}
		}
	}
	return warnings
}

// undecodableReason says what the key would have accepted. An enum names its
// options because the spelling is the whole of the author's mistake.
func undecodableReason(spec engineParam) string {
	switch spec.Type {
	case "boolean":
		return "is not a boolean"
	case "integer":
		return "is not an integer"
	case "number":
		return "is not a number"
	case "json":
		return "is not valid JSON"
	}
	if spec.Type == "enum" && len(spec.Enum) > 0 {
		return "is not one of " + strings.Join(spec.Enum, ", ")
	}
	return "cannot be read as " + spec.Type
}
