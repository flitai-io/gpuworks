// kv_backends.go binds the KV offload backend catalog (package kvbackend,
// ADR 0069) to this package: which rows a deployment can select per engine,
// the dictionary row that selects one, the profile list the deploy form
// renders, and which renderer gives a row its engine configuration.

package deploy

import (
	"gpuworks.ai/gpuworks/internal/server/kvbackend"
	"gpuworks.ai/gpuworks/pkg/localization"
)

// paramKVOffloadBackend is the dictionary key that selects a backend.
const paramKVOffloadBackend = "kv_offload_backend"

// kvBackendSet is the catalog this package works from, and the vLLM halves of
// rows the shipped ones do not cover (only a test catalog has any).
type kvBackendSet struct {
	registry    kvbackend.Registry
	vllmOffload map[string]vllmOffloadBackend
}

// kvBackends is assigned once from the shipped catalog, like the parameter
// dictionary beside it; tests that need a backend the product does not ship
// swap it whole (withKVBackends), never a row.
var kvBackends = kvBackendSet{registry: kvbackend.Builtin()}

// vllmBackend answers the vLLM half of a row: its renderer and reader. The
// shipped ones are looked up at call time rather than held in kvBackends:
// they read the dictionary, which itself reads kvBackends.
func (s kvBackendSet) vllmBackend(id string) (vllmOffloadBackend, bool) {
	if backend, ok := s.vllmOffload[id]; ok {
		return backend, true
	}
	backend, ok := vllmOffloadBackends()[id]
	return backend, ok
}

// selectedKVBackend is the row q selects on engine: its stated id, or the
// engine's default row when it states none. ok is false for an id the engine
// does not have and for an engine with no rows (SGLang today).
func selectedKVBackend(engine string, q LaunchConfig) (kvbackend.Backend, bool) {
	if engine == "" {
		engine = DefaultEngine
	}
	return kvBackends.registry.Resolve(engine, q.KVOffloadBackend)
}

// selectableKVBackends are the rows a deployment selects by id; the
// dictionary publishes kv_offload_backend only for an engine that has one.
func selectableKVBackends(engine string) []kvbackend.Backend {
	return kvBackends.registry.Selectable(engine)
}

// kvBackendSelector resolves the dictionary's selector row for one engine:
// a closed enum of the engine's selectable rows, labelled with each row's
// product name. It has no flag: the engine adapter renders the selected row
// (ADR 0069), so it never joins a composite flag group, argv or ReverseArgs.
func kvBackendSelector(definition paramDefinition, engine string) (engineParam, bool) {
	rows := selectableKVBackends(engine)
	if len(rows) == 0 {
		return engineParam{}, false
	}
	spec := definition.ParamDefinition
	spec.Flag, spec.FlagOff = "", ""
	spec.Options = make([]ParamOption, 0, len(rows))
	for _, row := range rows {
		label := make(localization.Text, len(requiredParameterLocales))
		for _, language := range requiredParameterLocales {
			label[language] = row.Label
		}
		spec.Options = append(spec.Options, ParamOption{Value: row.ID, Label: label})
	}
	spec.Enum = optionValues(spec.Options)
	return engineParam{ParamDefinition: spec, Selector: true}, true
}

// KVBackendProfile is one selectable KV offload backend as the deploy form
// sees it: the form renders one card per entry and, when it is chosen, stops
// stating the native keys it replaces. Nothing about a backend is known to
// the form beyond this.
type KVBackendProfile struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Replaces []string `json:"replaces"`
	PDRoles  []string `json:"pd_roles"`
}

// kvBackendProfiles lists the engine's selectable rows that run on vendor;
// an empty vendor (no target yet) lists them all. Backend.Supports is the
// same predicate the launch gate (validateKVBackendTarget) applies.
func kvBackendProfiles(engine, vendor string) []KVBackendProfile {
	var out []KVBackendProfile
	for _, row := range selectableKVBackends(engine) {
		if vendor != "" && !row.Supports(vendor) {
			continue
		}
		out = append(out, KVBackendProfile{ID: row.ID, Label: row.Label, Replaces: nonNil(row.Replaces), PDRoles: nonNil(row.PDRoles)})
	}
	return out
}
