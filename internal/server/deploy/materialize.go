// materialize.go is the write boundary. A deployment request arrives as three
// descriptions of the same launch - typed request fields, free-form extra_args
// and catalog spec args - and leaves here as exactly one: a typed configuration
// plus the argv tokens the dictionary does not describe. Nothing downstream
// merges anything again.
//
// For a request it merges and then states the dictionary's own defaults, so a
// form that sends only the keys the deployer touched still gets the command
// line its controls showed. A stored row is different: it predates this
// boundary, so re-materializing it invents nothing - a parameter none of its
// layers state stays unset, an unset parameter produces no flag, and the
// engine's own default applies. That is what makes an upgrade safe -
// re-materializing a stored row cannot change the command line it was already
// running.

package deploy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

const (
	// ErrorLaunchConfigConflict reports that a request stated one value for a
	// parameter in a typed field and a different one in extra_args. Params carry
	// key, flag and both values so the caller can say which two disagree.
	ErrorLaunchConfigConflict = "LAUNCH_CONFIG_CONFLICT"
	// ErrorLaunchConfigUnsetConflict reports that a request asks to omit a
	// dictionary key while also stating that key in quick_params or extra_args.
	// Rejecting the ambiguity keeps a disabled layer from silently swallowing a
	// hand-written value.
	ErrorLaunchConfigUnsetConflict = "LAUNCH_CONFIG_UNSET_CONFLICT"
)

// MaterializeInput is everything a launch configuration can be stated in.
type MaterializeInput struct {
	Engine string
	// Command is the final command context for a new request. Nil keeps legacy
	// callers that have no entrypoint information from making a dialect claim.
	Command *EffectiveCommand
	// Raw is the request's quick_params exactly as submitted: whether a key is
	// present is what makes it explicit, which is why this is not LaunchConfig.
	Raw map[string]any
	// Unset removes these dictionary keys from every launch layer. It is a
	// request-time tombstone used by controls that preserve an edited draft
	// while disabling its emission. It never becomes part of LaunchConfig.
	Unset        []string
	TemplateArgs []string // catalog spec args, already tokenized
	ExtraArgs    []string // user extra_args, already tokenized
	// EngineVersion is the engine release the runtime image carries
	// (ImageEngineVersion), empty when unknown. Default applicability reads
	// it: a flag the release may refuse gets no default.
	EngineVersion string
	// Cards are the accelerators this deployment may run on, when they are
	// known: the dictionary rows whose default depends on the card are capped
	// against them (capDefaultsToCards). Empty leaves every default as the
	// dictionary states it, which is what a caller with no node yet must get.
	Cards []CardFacts
	// Base is the stored configuration an edit of a deployment starts from,
	// nil for a new request. Its values rank below the request's fields and
	// extra_args and above the template's arguments; a field that restates
	// its base value is the deployment's own, not something the edit states,
	// so only what the edit changed is held to the dictionary's bounds. No
	// dictionary default is added to an edit: a stored row never gains one
	// it did not have (applyDictionaryDefaults).
	Base *LaunchConfig
}

// MovedFlag records a dictionary flag that was written in extra_args and now
// lives in a field. The caller echoes it so the form can show what happened to
// the text the user typed.
type MovedFlag struct {
	Key    string `json:"key"`
	Flag   string `json:"flag"`
	Value  any    `json:"value"`
	Source string `json:"source"`
}

// movedFromExtraArgs is the only source a value is ever moved from.
const movedFromExtraArgs = "extra_args"

// Materialized is the single representation of a launch configuration.
type Materialized struct {
	// Quick holds exactly the parameters the request stated, in the dictionary's
	// canonical form. A key nobody stated is unset.
	Quick LaunchConfig
	// TemplateResidual and UserResidual are the tokens no dictionary key owns,
	// in the order they were written.
	TemplateResidual []string
	UserResidual     []string
	// Moved lists values lifted out of extra_args into fields.
	Moved []MovedFlag
	// TemplateParams is what the catalog spec said, whether or not it won.
	TemplateParams map[string]any
	// EngineDecidesParamKeys are the keys whose dictionary default this launch
	// leaves unstated (EngineDecidesParameterKeys): the form shows them as
	// automatic until the deployer turns them on or off.
	EngineDecidesParamKeys []string
}

// Materialize merges the request into one configuration. Precedence is
// explicit field > extra_args > template args; a key none of them states stays
// unset. Two things are errors: a field and an extra_args token stating
// different values for the same key, and an explicitly stated value the
// dictionary's bounds reject - which is checked here because this is the last
// place that knows a key was stated at all.
func Materialize(in MaterializeInput) (Materialized, error) {
	return materialize(in, false)
}

// materialize carries the one behavioural difference between a request and a
// stored row: a row predates this boundary, so its extra_args already won over
// its fields and re-deciding that would rewrite history. legacy keeps the old
// precedence and never reports a conflict.
func materialize(in MaterializeInput, legacy bool) (Materialized, error) {
	engine, err := SelectEngine(in.Engine)
	if err != nil {
		return Materialized{}, err
	}
	params := capDefaultsToCards(engineParams(engine.Name()), in.Cards)
	// A caller without a concrete engine command cannot establish that the
	// image supports native context fitting. Stored rows and legacy callers
	// must keep their original omission rather than gain a new launch flag.
	if engine.Name() == EngineVLLM && (in.Command == nil || !in.Command.InterpretEngineDialect) {
		spec := params["max_model_len"]
		spec.Default = nil
		params["max_model_len"] = spec
	}
	unset := make(map[string]struct{}, len(in.Unset))
	for _, rawKey := range in.Unset {
		key := strings.TrimSpace(rawKey)
		if _, known := params[key]; !known {
			return Materialized{}, paramErrorf(ParamFieldQuickParamsUnset, "invalid quick_params_unset: unknown key %q", rawKey)
		}
		unset[key] = struct{}{}
	}

	base, baseOther, err := baseValues(params, in.Base)
	if err != nil {
		return Materialized{}, err
	}
	explicit := map[string]any{}
	values := map[string]any{}
	for key, value := range baseOther {
		if _, restated := in.Raw[key]; !restated {
			values[key] = value
		}
	}
	for key, raw := range in.Raw {
		if _, disabled := unset[key]; disabled && raw != nil {
			return Materialized{}, unsetConflict(key, params[key].Flag, "quick_params", raw)
		}
		spec, known := params[key]
		if !known {
			if key == paramKVOffloadBackend {
				// The engine has no backend to select (kvBackendSelector), so
				// the selection names nothing; it was always dropped here.
				continue
			}
			// A key this engine has no flag for is still the caller's data: it
			// may be an internal field (cpu_cores) or a parameter another
			// engine owns, and validation is where that is judged.
			values[key] = raw
			continue
		}
		if raw == nil {
			continue
		}
		if canonical, ok := canonicalValue(spec, raw); ok {
			explicit[key] = canonical
			continue
		}
		// Keep the value the caller sent so validation can name it.
		explicit[key] = raw
	}

	// A composite flag can belong to several dictionary keys and therefore
	// stays residual during ordinary reverse parsing. It can be removed only
	// when every key sharing that flag is explicitly unset; anything narrower
	// would guess which part of an opaque JSON value the caller meant.
	affectedCompositeFlags, fullyUnsetCompositeFlags := compositeFlagsAffectedByUnset(engine.Name(), unset)
	templateArgs, err := removeUnsetTemplateFlags(engine, in.TemplateArgs, affectedCompositeFlags, fullyUnsetCompositeFlags, unset)
	if err != nil {
		return Materialized{}, err
	}
	templateParams, _, templateResidual, _, err := reverseArgs(engine.Name(), templateArgs)
	if err != nil {
		return Materialized{}, err
	}
	if flag, value, found := firstFlag(in.ExtraArgs, affectedCompositeFlags); found {
		return Materialized{}, unsetConflict(firstUnsetKeyForFlag(engine.Name(), flag, unset), flag, "extra_args", value)
	}
	extraParams, extraFlags, userResidual, _, err := reverseArgs(engine.Name(), in.ExtraArgs)
	if err != nil {
		return Materialized{}, err
	}
	for key := range unset {
		if value, stated := extraParams[key]; stated {
			return Materialized{}, unsetConflict(key, extraFlags[key], "extra_args", wireValue(params[key], value))
		}
	}

	var moved []MovedFlag
	for key, extraValue := range extraParams {
		fieldValue, isExplicit := explicit[key]
		if !isExplicit {
			moved = append(moved, MovedFlag{Key: key, Flag: extraFlags[key], Value: wireValue(params[key], extraValue), Source: movedFromExtraArgs})
			continue
		}
		if sameValue(fieldValue, extraValue) || legacy {
			// The same value said twice is a duplicate, not a disagreement.
			continue
		}
		return Materialized{}, messages.NewProductError(ErrorLaunchConfigConflict,
			fmt.Sprintf("%s is set to %v in the form and %v in extra_args", key, fieldValue, extraValue),
			map[string]any{
				"key": key, "flag": extraFlags[key],
				"field_value": wireValue(params[key], fieldValue), "extra_value": wireValue(params[key], extraValue),
			})
	}
	sort.Slice(moved, func(i, j int) bool { return moved[i].Key < moved[j].Key })

	var engineDecides []string
	if in.Command != nil && in.Command.InterpretEngineDialect {
		engineDecides, err = EngineDecidesParameterKeys(engine.Name(), LaunchShape{
			Entrypoint:    in.Command.Entrypoint,
			Args:          append(append([]string(nil), in.TemplateArgs...), in.ExtraArgs...),
			EngineVersion: in.EngineVersion,
		})
		if err != nil {
			return Materialized{}, err
		}
	}

	for _, key := range sortedKeys(params) {
		if _, disabled := unset[key]; disabled {
			continue
		}
		spec := params[key]
		value, source, stated := resolveValue(key, explicit, extraParams, templateParams, legacy)
		if baseValue, kept := base[key]; kept && (!stated || source == sourceTemplate || sameValue(value, baseValue)) {
			// The edit left this key as the deployment has it.
			value, source, stated = baseValue, sourceStored, true
		}
		if !stated {
			continue
		}
		if source == sourceRequest && !legacy {
			// Only what this request states is measured. A catalog spec is a
			// curated artifact the server passes through, and a stored row
			// predates these rules entirely - failing either here would reject
			// a deployment for something its deployer did not write.
			if err := checkStatedBounds(spec, value); err != nil {
				return Materialized{}, err
			}
		}
		values[key] = wireValue(spec, value)
	}
	if !legacy && in.Base == nil {
		applyDictionaryDefaults(params, unset, engineDecides, values)
	}

	encoded, err := json.Marshal(values)
	if err != nil {
		return Materialized{}, fmt.Errorf("invalid quick_params: %w", err)
	}
	out := Materialized{
		TemplateResidual:       templateResidual,
		UserResidual:           userResidual,
		Moved:                  moved,
		TemplateParams:         wireValues(params, templateParams),
		EngineDecidesParamKeys: engineDecides,
	}
	if err := json.Unmarshal(encoded, &out.Quick); err != nil {
		return Materialized{}, fmt.Errorf("invalid quick_params: %w", err)
	}
	return out, nil
}

// applyDictionaryDefaults states the dictionary's own defaults for a new
// request, so the form sends only the keys the deployer touched and still gets
// the command line the controls showed. Stored rows never come through here:
// re-materializing a row cannot be allowed to change the command it runs, and
// the dictionary is younger than most rows.
//
// The rules mirror what the deploy form controls display:
//   - a key the request or its template stated keeps that value (precedence
//     sits in the loop above; defaults are the floor, not a source that wins);
//   - a key quick_params_unset removed stays removed;
//   - a key the engine adapter leaves to the engine on this launch stays
//     unset (EngineDecidesParameterKeys: the entrypoint or the release may
//     refuse its flag), and the form shows it as automatic, not as the
//     default;
//   - a row whose DependsOn dependency is not enabled stays unset, so tier
//     companions do not wake up with their defaults while the tier is off.
//
// The pass repeats until it settles because a default can enable the
// dependency another default waits on (the memory tier's size enabling the
// disk tier's companions); the graph is shallow, so this is two rounds at
// most in practice and cannot loop: a key is only ever added once.
func applyDictionaryDefaults(params map[string]engineParam, unset map[string]struct{}, engineDecides []string, values map[string]any) {
	var blocked map[string]struct{}
	if len(engineDecides) > 0 {
		blocked = make(map[string]struct{}, len(engineDecides))
		for _, key := range engineDecides {
			blocked[key] = struct{}{}
		}
	}
	for applied := true; applied; {
		applied = false
		for _, key := range sortedKeys(params) {
			spec := params[key]
			if spec.Default == nil {
				continue
			}
			if _, disabled := unset[key]; disabled {
				continue
			}
			if _, withheld := blocked[key]; withheld {
				continue
			}
			if _, stated := values[key]; stated {
				continue
			}
			if spec.DependsOn != "" && !dependencyValueEnabled(values[spec.DependsOn]) {
				continue
			}
			canonical, ok := canonicalValue(spec, spec.Default)
			if !ok {
				continue
			}
			values[key] = wireValue(spec, canonical)
			applied = true
		}
	}
}

func unsetConflict(key, flag, source string, value any) error {
	return messages.NewProductError(ErrorLaunchConfigUnsetConflict,
		fmt.Sprintf("%s is disabled by quick_params_unset and also set by %s", key, source),
		map[string]any{"key": key, "flag": flag, "source": source, "value": value})
}

func compositeFlagsAffectedByUnset(engine string, unset map[string]struct{}) (affected, fullyUnset map[string]struct{}) {
	affected = map[string]struct{}{}
	fullyUnset = map[string]struct{}{}
	for flag, entries := range flagIndex(engine) {
		if len(entries) < 2 {
			continue
		}
		all, any := true, false
		for _, entry := range entries {
			if _, ok := unset[entry.spec.Key]; ok {
				any = true
			} else {
				all = false
			}
		}
		if any {
			affected[flag] = struct{}{}
		}
		if all {
			fullyUnset[flag] = struct{}{}
		}
	}
	return affected, fullyUnset
}

func firstUnsetKeyForFlag(engine, flag string, unset map[string]struct{}) string {
	for _, entry := range flagIndex(engine)[flag] {
		if _, ok := unset[entry.spec.Key]; ok {
			return entry.spec.Key
		}
	}
	return flag
}

func firstFlag(args []string, flags map[string]struct{}) (string, string, bool) {
	for index, token := range args {
		flag, inline, hasInline := splitInlineFlag(token)
		if _, ok := flags[flag]; !ok {
			continue
		}
		if hasInline {
			return flag, inline, true
		}
		if index+1 < len(args) {
			return flag, args[index+1], true
		}
		return flag, "", true
	}
	return "", "", false
}

func removeUnsetTemplateFlags(engine Engine, args []string, affected, fullyUnset, unset map[string]struct{}) ([]string, error) {
	if len(affected) == 0 {
		return args, nil
	}
	out := make([]string, 0, len(args))
	unsetter := hooks(engine)
	for index := 0; index < len(args); index++ {
		flag, inline, hasInline := splitInlineFlag(args[index])
		if _, ok := affected[flag]; !ok {
			out = append(out, args[index])
			continue
		}
		value := inline
		if !hasInline && index+1 < len(args) {
			index++
			value = args[index]
		}
		_, all := fullyUnset[flag]
		if !all || !unsetter.CanUnsetTemplateFlag(flag, value) {
			return nil, unsetConflict(firstUnsetKeyForFlag(engine.Name(), flag, unset), flag, "template_args", value)
		}
	}
	return out, nil
}

// valueSource says who stated a value: this request (a form field or its
// extra_args) or the catalog spec it selected.
type valueSource int

const (
	sourceRequest valueSource = iota
	sourceTemplate
	// sourceStored is the deployment's stored configuration an edit keeps
	// (MaterializeInput.Base).
	sourceStored
)

// baseValues reads the stored configuration an edit starts from: its
// dictionary keys as canonical values, and apart from them the keys this
// engine's dictionary does not describe. Those are kept as they are - the row
// runs with them, and an edit that did not touch one must not drop it.
func baseValues(params map[string]engineParam, base *LaunchConfig) (dictionary, other map[string]any, err error) {
	if base == nil {
		return nil, nil, nil
	}
	encoded, err := json.Marshal(base)
	if err != nil {
		return nil, nil, fmt.Errorf("stored quick_params: %w", err)
	}
	raw := map[string]any{}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return nil, nil, fmt.Errorf("stored quick_params: %w", err)
	}
	dictionary, other = map[string]any{}, map[string]any{}
	for key, value := range raw {
		spec, known := params[key]
		if !known {
			other[key] = value
			continue
		}
		if canonical, ok := canonicalValue(spec, value); ok {
			value = canonical
		}
		dictionary[key] = value
	}
	return dictionary, other, nil
}

// resolveValue answers with the value the highest-precedence source stated, who
// stated it, and whether anyone did.
func resolveValue(key string, explicit, extra, template map[string]any, legacy bool) (any, valueSource, bool) {
	order := []struct {
		values map[string]any
		source valueSource
	}{{explicit, sourceRequest}, {extra, sourceRequest}, {template, sourceTemplate}}
	if legacy {
		// A stored row predates this boundary: its extra_args already beat its
		// fields, and re-deciding that would rewrite a running command line.
		order[0], order[1] = order[1], order[0]
	}
	for _, from := range order {
		if value, ok := from.values[key]; ok {
			return value, from.source, true
		}
	}
	return nil, sourceTemplate, false
}

// checkStatedBounds measures a value against its dictionary row. It runs only
// on values a source actually stated, which is why a zero here is a real zero
// and not the absence a LaunchConfig field would read back as.
func checkStatedBounds(spec engineParam, value any) error {
	number, ok := toFloat(value)
	if !ok {
		return nil
	}
	if number < 0 && !isVLLMContextAuto(spec, number) {
		return paramErrorf(spec.Key, "invalid quick_params: %s must not be negative", spec.Key)
	}
	if spec.Min != nil && number < *spec.Min {
		return paramErrorf(spec.Key, "invalid quick_params: %s must be at least %v", spec.Key, *spec.Min)
	}
	if spec.Max != nil && number > *spec.Max {
		return paramErrorf(spec.Key, "invalid quick_params: %s must be at most %v", spec.Key, *spec.Max)
	}
	return nil
}

// wireValue renders a canonical value in the shape LaunchConfig stores it in: a
// json-typed key is a text field, everything else is already a scalar.
func wireValue(spec engineParam, value any) any {
	if spec.Type == "json" {
		return jsonText(value)
	}
	return value
}

func wireValues(params map[string]engineParam, values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = wireValue(params[key], value)
	}
	return out
}

func sortedKeys(params map[string]engineParam) []string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
