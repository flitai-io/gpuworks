// param_dictionary.go projects the parameter dictionary (param_specs.go) onto
// one engine and decodes values against it. Everything that needs to know what
// a parameter defaults to, which spellings it accepts, which flag carries it
// and which flag negates it reads it from here, so the table stays the only
// place those facts are written down.

package deploy

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	enginenames "gpuworks.ai/gpuworks/internal/engines"
)

// engineParam is one dictionary row resolved for a single engine: the embedded
// ParamDefinition carries this engine's Flag ("on" form for a boolean), FlagOff
// and option list, either flag being empty when the engine has no spelling for
// that polarity, and Default is this engine's default rather than the table's.
// Platform repeats the table's marker so readers that only hold a resolved row
// can still tell an engine parameter from one the platform consumes.
type engineParam struct {
	ParamDefinition
	Default  any
	Platform bool
	// Composed repeats flagPair.Composed for readers that only hold the
	// resolved row: the adapter writes this flag itself (see flagPair).
	Composed bool
}

// engineParams returns the dictionary rows that are part of this engine's
// configuration, keyed by parameter key. An engine parameter the engine has no
// flag for is absent: the engine cannot express it. A platform parameter has no
// flag at all, so its own row says which engines it applies to.
func engineParams(engine string) map[string]engineParam {
	if !containsString(enginenames.DialectNames(), engine) {
		// An engine with no launch dialect of its own - callable, whose argv
		// belongs to the catalog's Serve Adapter - has no dictionary either.
		// Every argument it carries is a passthrough token, which is what keeps
		// a template's own arguments in the command instead of being absorbed
		// into fields the adapter never reads.
		return nil
	}
	out := make(map[string]engineParam, len(parameterDefinitions))
	for _, definition := range parameterDefinitions {
		flags := definition.Flags[engine]
		if definition.Platform {
			if !containsString(definition.Engines, engine) {
				continue
			}
		} else if flags.On == "" && flags.Off == "" {
			continue
		}
		spec := definition.ParamDefinition
		spec.Flag, spec.FlagOff = flags.On, flags.Off
		spec.Aliases = definition.Aliases
		spec.Options = definition.optionsFor(engine)
		spec.Enum = optionValues(spec.Options)
		if containsString(definition.ClosedFor, engine) {
			spec.Type, spec.Control = "enum", "select"
		}
		value := definition.Default
		if override, ok := definition.DefaultByEngine[engine]; ok {
			value = override
		}
		out[definition.Key] = engineParam{ParamDefinition: spec, Default: value, Platform: definition.Platform, Composed: flags.Composed}
	}
	return out
}

// optionsFor is the option list this engine publishes for the row: the column
// the table declares for it, or the row's own list when it declares none.
func (definition paramDefinition) optionsFor(engine string) []ParamOption {
	if override, ok := definition.Options[engine]; ok {
		return override
	}
	return definition.ParamDefinition.Options
}

func optionValues(options []ParamOption) []string {
	if len(options) == 0 {
		return nil
	}
	values := make([]string, 0, len(options))
	for _, option := range options {
		values = append(values, option.Value)
	}
	return values
}

// DefaultGPUMemUtil is the utilization the engine itself runs at when the
// deployment states none. It is a fact about the engine, not a value the
// platform writes into a configuration: nothing emits it as a flag, and the one
// reader that needs a number - the estimator, through planning - resolves it
// here so its sizing matches what the engine will actually do.
//
// An engine with no dictionary of its own (a catalog-owned Serve Adapter, or a
// harness engine registered for tests) declares the assumption in its profile
// instead (Profile.GPUMemUtilDefault). An engine that declares nothing there
// either is charged the default engine's row, because zero would make the
// scheduler reject every one of its deployments outright - and that row is
// exactly what such an engine was charged while it still read the default
// column.
//
// Deliberately not a function of the card. The share a card can give away is
// capped where the platform picks one and writes it down (capDefaultsToCards);
// what this answers is the other case, where nothing was written down, no flag
// is emitted and the engine therefore runs at its own figure whatever card it
// lands on. Capping here would make the estimate disagree with the running
// engine rather than agree with it.
func DefaultGPUMemUtil(engine string) float64 {
	if value, ok := dictionaryDefault(engine, "gpu_memory_utilization"); ok {
		return value
	}
	if profile, err := ProfileForEngine(engine); err == nil && profile.GPUMemUtilDefault > 0 {
		return profile.GPUMemUtilDefault
	}
	value, _ := dictionaryDefault(DefaultEngine, "gpu_memory_utilization")
	return value
}

// UtilizationSizesMemory reports whether the engine takes the share of the
// card its gpu_memory_utilization names (the engines whose dictionary has the
// row). An engine without it, such as a callable server, takes what it needs.
func UtilizationSizesMemory(engine string) bool {
	_, known := engineParams(engine)["gpu_memory_utilization"]
	return known
}

// dictionaryDefault reads one row's default for an engine, and reports whether
// that engine has the row at all.
func dictionaryDefault(engine, key string) (float64, bool) {
	spec, known := engineParams(engine)[key]
	if !known {
		return 0, false
	}
	value, ok := toFloat(spec.Default)
	return value, ok
}

// defaultInt reads an integer default straight from the dictionary. It is what
// a composite flag falls back to when the deployer stated no value for one of
// the keys it folds together.
func defaultInt(engine, key string) int {
	value, _ := toFloat(engineParams(engine)[key].Default)
	return int(value)
}

func toFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		parsed, err := v.Float64()
		return parsed, err == nil
	}
	return 0, false
}

// canonicalValue decodes a value supplied for a dictionary key into the single
// representation the rest of the server uses: int64, float64, bool, the
// canonical spelling of an enum, or the decoded JSON of a json-typed key. The
// second result is false when the value cannot be read as that key's type,
// which is how an unrecognized catalog argument stays a passthrough token
// instead of becoming a silently wrong field.
func canonicalValue(spec engineParam, value any) (any, bool) {
	switch spec.Type {
	case "boolean":
		switch v := value.(type) {
		case bool:
			return v, true
		case string:
			parsed, err := strconv.ParseBool(strings.TrimSpace(v))
			return parsed, err == nil
		}
		if number, ok := toFloat(value); ok {
			return number != 0, true
		}
		return nil, false
	case "integer":
		if text, ok := value.(string); ok {
			parsed, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
			return parsed, err == nil
		}
		number, ok := toFloat(value)
		if !ok || number != float64(int64(number)) {
			return nil, false
		}
		return int64(number), true
	case "number":
		if text, ok := value.(string); ok {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
			return parsed, err == nil
		}
		number, ok := toFloat(value)
		return number, ok
	case "json":
		text, isText := value.(string)
		if !isText {
			// Already decoded (a request body carries structured JSON).
			return value, true
		}
		var decoded any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return nil, false
		}
		return decoded, true
	default:
		text, ok := value.(string)
		if !ok {
			text = fmt.Sprint(value)
		}
		text = strings.TrimSpace(text)
		if canonical, aliased := spec.Aliases[strings.ToLower(text)]; aliased {
			text = canonical
		}
		if spec.Type == "enum" && len(spec.Enum) > 0 {
			canonical, known := canonicalEnumValue(spec.Enum, text)
			if !known {
				return nil, false
			}
			// Catalogs and API clients spell an enum however upstream docs do
			// (EAGLE3, BFloat16). Case is not meaning, so it is folded here and
			// the dictionary's own spelling is what the configuration stores -
			// everything downstream then compares one spelling.
			text = canonical
		} else if canonical, known := canonicalEnumValue(spec.Enum, text); known {
			// A free-text key's options are suggestions, not a gate: a value
			// none of them names passes as written. One that names an option
			// in another case takes the option's spelling, which is the
			// engine's - SGLang refuses "eagle3" and vLLM "MTP".
			text = canonical
		}
		return text, true
	}
}

// jsonText renders a canonical json-typed value as the compact text the
// LaunchConfig field and the engine flag both carry.
func jsonText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// canonicalEnumValue matches an option case-insensitively and answers with the
// dictionary's spelling of it.
func canonicalEnumValue(options []string, want string) (string, bool) {
	for _, option := range options {
		if strings.EqualFold(option, want) {
			return option, true
		}
	}
	return "", false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// sameValue compares two canonical values. Everything the dictionary produces
// is a scalar or a JSON string, so a formatted comparison is exact and does not
// depend on which numeric width a decoder happened to pick.
func sameValue(left, right any) bool {
	leftNumber, leftOK := toFloat(left)
	rightNumber, rightOK := toFloat(right)
	if leftOK && rightOK {
		return leftNumber == rightNumber
	}
	return fmt.Sprint(left) == fmt.Sprint(right)
}

// paramDictForEngine is the key→flag map published to clients. A boolean whose
// engine spells only the negative form (SGLang's radix cache) advertises that
// form: it is the flag that parameter owns on that engine. A platform parameter
// is absent: it owns no flag, and this map's whole meaning is which flag
// carries which key (ADR 0021 decision 1).
func paramDictForEngine(engine string) map[string]string {
	params := engineParams(engine)
	out := make(map[string]string, len(params))
	for key, spec := range params {
		if spec.Platform {
			continue
		}
		if spec.Flag != "" {
			out[key] = spec.Flag
			continue
		}
		out[key] = spec.FlagOff
	}
	return out
}

// structuredParams is the ordered, engine-specific dictionary published as
// /engines param_specs. Order follows the table so the form is stable.
func structuredParams(engine string, dict map[string]string) []ParamDefinition {
	params := engineParams(engine)
	out := make([]ParamDefinition, 0, len(params))
	for _, definition := range parameterDefinitions {
		spec, known := params[definition.Key]
		if !known {
			continue
		}
		flag, published := dict[definition.Key]
		if !published && !spec.Platform {
			continue
		}
		wire := spec.ParamDefinition
		// A platform parameter publishes an empty flag, which is the honest
		// answer to "which flag carries this key on this engine": none.
		wire.Flag = flag
		wire.Default = spec.Default
		wire.Engines = []string{engine}
		out = append(out, wire)
	}
	return out
}
