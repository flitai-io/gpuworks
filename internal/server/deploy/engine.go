// engine.go defines the Engine interface every inference engine adapter
// implements, plus the process-wide engine registry and profile lookup.

package deploy

import (
	"errors"
	"sort"
	"sync"

	enginenames "gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// Engine names. These re-export internal/engines so server code has one place
// to name an engine; internal/engines is the leaf definition, needed because
// modelmeta (reached from here via inferenceapi) and the agent cannot import
// this package.
const (
	EngineVLLM     = enginenames.VLLM
	EngineSGLang   = enginenames.SGLang
	EngineCallable = enginenames.Callable

	// DefaultEngine is used when a deployment names no engine.
	DefaultEngine = enginenames.Default
)

// SleepCaps describes optional engine sleep support.
type SleepCaps struct {
	Supported bool     `json:"supported"`
	Levels    []string `json:"levels,omitempty"`
}

// Profile contains engine behavior consumed by server and agent-neutral specs.
type Profile struct {
	// OperationDefinitions is protocol vocabulary, not a claim that every model supports it.
	OperationDefinitions []inferenceapi.Definition `json:"operation_definitions"`
	Name                 string                    `json:"name"`
	DefaultImage         string                    `json:"default_image"`
	HealthPath           string                    `json:"health_path"`
	ShmBytes             int64                     `json:"shm_bytes"`
	MetricPath           string                    `json:"metric_path"`
	Sleep                SleepCaps                 `json:"sleep"`
	MetricPrefix         string                    `json:"metric_prefix"`
	// MetricMap is the engine's built-in mapping as exposition family to
	// stored series (EngineMetricMap); the mapping and the rules it obeys live
	// in internal/inferencemetrics. A deployment's observation is resolved by
	// ResolveObservation, not read from here.
	MetricMap        map[string]string `json:"metric_map,omitempty"`
	ParamDict        map[string]string `json:"param_dict,omitempty"`
	ParamDefinitions []ParamDefinition `json:"param_specs,omitempty"`
	QuickFormKeys    []string          `json:"quick_form_keys,omitempty"`
	// KVDiskMinKernel is the lowest Linux major.minor release on which this
	// engine's KV disk tier starts at all; empty means the engine states no
	// floor. Clients compare it against the kernel a node reports instead of
	// carrying a kernel number of their own.
	KVDiskMinKernel string `json:"kv_disk_min_kernel,omitempty"`
	// PDModes are the PD disaggregation modes this engine runs, with the
	// rules the deploy form applies (package pdmode is their catalog); the
	// form offers exactly these and restates none of their rules.
	PDModes []PDModeProfile `json:"pd_modes,omitempty"`
	// KVBackends are the KV offload backends a deployment of this engine can
	// select besides the engine's own offload (ADR 0069); absent when there
	// are none. A target's profile lists those that run on its vendor.
	KVBackends []KVBackendProfile `json:"kv_backends,omitempty"`
	// MultiNodeSupported is set only when a target-specific capability policy
	// overrides the engine profile. Nil leaves the engine's existing behavior
	// intact; a pointer allows an explicit false to reach clients.
	MultiNodeSupported *bool `json:"multi_node_supported,omitempty"`
	// ReserveBase is the per-GPU memory this engine holds back besides its
	// weights and its KV cache, in bytes, when the estimator's graph-shaped
	// default model does not describe it (estimate.ReserveBytes takes it as
	// its reserveBase). Zero means the engine declares none and the default
	// model applies. The estimator cannot look this up itself - it does not
	// know which engine it is charging - so the profile carries it. It is a
	// server-side fact, not part of the published profile.
	ReserveBase int64 `json:"-"`
	// GPUMemUtilDefault is the utilization a deployment is sized with when it
	// states none and this engine has no gpu_memory_utilization row of its own
	// - a catalog-owned Serve Adapter still has to be estimated, and zero is
	// not a smaller estimate but a deployment the scheduler refuses to plan.
	// A dialect engine states the value in the dictionary and leaves this zero.
	// It is a server-side fact, not part of the published profile.
	GPUMemUtilDefault float64 `json:"-"`
}

// Engine assembles an inference server command from common deployment params.
type Engine interface {
	Name() string
	Profile() Profile
	Build(Params) ResolvedLaunch
}

// EngineHooks is the optional half of an engine adapter's contract, in one
// place. Engine itself carries what every adapter must answer; these are the
// capabilities an adapter may answer differently, and a caller reaches them
// through hooks so a capability nobody implements still has one documented
// default instead of a silent type-assertion miss at its call site.
//
// An adapter embeds BaseEngine and overrides what it needs. Adding a
// capability means adding it here, which makes every adapter's coverage
// visible instead of discoverable only where it is asserted.
type EngineHooks interface {
	// FinalizeLaunch adjusts the fully merged launch as the engine's last word.
	// Build sees only Params; an engine whose platform adjustments depend on
	// the merged result — the operator's environment, the command mode, the
	// final argv — does it here instead of guessing inside Build.
	FinalizeLaunch(ResolvedLaunch) ResolvedLaunch
	// DefaultApplicable reports whether a dictionary key's default may be
	// stated for this concrete launch when the deployment says nothing about
	// it. A default is withheld where the launch may run a program, or a
	// release, that refuses the flag; the key is then left to the engine
	// (EngineDecidesParameterKeys), and the deployer can still state it.
	DefaultApplicable(key string, launch LaunchShape) bool
	// CanUnsetTemplateFlag reports whether an exact flag/value pair this
	// adapter generated is safe to drop. The common materializer never
	// interprets an engine's private JSON or connector names.
	CanUnsetTemplateFlag(flag, value string) bool
	// ValidateTopology checks the GPU topology flags of a resolved command
	// against the placement. It runs after catalog variables and every command
	// layer are resolved, so preview and controller read one effective argv.
	ValidateTopology(ResolvedLaunch, Params) error
	// SymbolizePreviewTP marks an engine-injected placeholder TP while
	// placement is unresolved. Each engine owns the flags that claim its TP.
	SymbolizePreviewTP(launch ResolvedLaunch, templateResidual, userResidual []string) ResolvedLaunch
	// SplitCommand separates the executable portion of a built command from
	// the parameter vector that has to survive replacing that executable.
	// Engines that encode both in Entrypoint answer with the split; the
	// default reports the launch's own entrypoint/argv pair.
	SplitCommand(ResolvedLaunch) (exe, params []string)
	// TPFlags lists every flag spelling that carries this engine's
	// tensor-parallel dimension, the spelling the adapter injects first.
	TPFlags() []string
	// PDConnectorFlags lists the flags this adapter composes for a PD
	// instance's KV connector. A template or user layer that states one
	// would replace the composed connector and silently turn the instance
	// into an ordinary replica, so a PD deployment refuses them.
	PDConnectorFlags() []string
	// KVConflictFlags lists the flags that would replace the configuration
	// a KV offload backend renders (ADR 0069), matched in every spelling the
	// engine accepts (matchConflictFlags).
	KVConflictFlags() []string
	// RecognizeKVBackend reads a flag/value pair back into the KV offload
	// backend whose configuration it is, with the backend's own dictionary
	// values: the inverse of the backend's renderer, so a template exported
	// from a deployment selects the backend again instead of carrying an
	// opaque connector.
	RecognizeKVBackend(flag, value string) (KVBackendRecognition, bool)
	// APIBindHost is the address the served API binds when the launch names
	// none; the template variable ${API_HOST} states it.
	APIBindHost() string
	// RequiredTemplateVariables names the template variables an engine-mode
	// launch has to reference. An adapter that appends no platform argument
	// of its own depends on the template to place them.
	RequiredTemplateVariables() []string
}

// RuntimeBindHost is the API bind address a node's runtime requires, or ""
// to leave it to the engine. A host-runtime node (a Mac) runs engines as plain
// processes in the node's own network: an engine there must bind loopback, or
// it serves every network the Mac is on without the gateway's authentication.
// The gateway reaches it through the agent's tunnel either way. Elsewhere the
// engine's own default stands: callable adapters bind 0.0.0.0 inside their
// container's network.
func RuntimeBindHost(runtimeDriver string) string {
	if runtimeDriver == proto.RuntimeDriverHost {
		return "127.0.0.1"
	}
	return ""
}

// BaseEngine is the empty implementation of EngineHooks: the documented
// default for every capability an adapter does not override. Adapters embed it
// rather than leaving the method set incomplete, because a capability that is
// missing from the method set silently reads as "engine says nothing".
type BaseEngine struct{}

func (BaseEngine) FinalizeLaunch(launch ResolvedLaunch) ResolvedLaunch { return launch }

func (BaseEngine) DefaultApplicable(string, LaunchShape) bool { return true }

func (BaseEngine) CanUnsetTemplateFlag(string, string) bool { return false }

func (BaseEngine) ValidateTopology(ResolvedLaunch, Params) error { return nil }

func (BaseEngine) SymbolizePreviewTP(launch ResolvedLaunch, _, _ []string) ResolvedLaunch {
	return launch
}

func (BaseEngine) SplitCommand(launch ResolvedLaunch) ([]string, []string) {
	return launch.Entrypoint, launch.Argv
}

func (BaseEngine) TPFlags() []string { return nil }

func (BaseEngine) PDConnectorFlags() []string { return nil }

func (BaseEngine) KVConflictFlags() []string { return nil }

func (BaseEngine) RecognizeKVBackend(string, string) (KVBackendRecognition, bool) {
	return KVBackendRecognition{}, false
}

// KVBackendRecognition is a stated configuration read back as a KV offload
// backend's selection: the backend's id, and the values of its own
// dictionary keys the configuration states.
type KVBackendRecognition struct {
	ID     string
	Values map[string]any
}

func (BaseEngine) APIBindHost() string { return "127.0.0.1" }

func (BaseEngine) RequiredTemplateVariables() []string { return nil }

// hooks returns the optional capabilities of an engine. An adapter that does
// not implement EngineHooks at all gets the base defaults, which is what the
// per-capability type assertions used to fall back to one by one.
func hooks(engine Engine) EngineHooks {
	if engineHooks, ok := engine.(EngineHooks); ok {
		return engineHooks
	}
	return BaseEngine{}
}

// RequiredTemplateVariables names the template variables an engine-mode
// launch of the named engine has to reference; the catalog import refuses a
// spec that never places them. An unknown engine requires nothing.
func RequiredTemplateVariables(name string) []string {
	engine, err := SelectEngine(name)
	if err != nil {
		return nil
	}
	return hooks(engine).RequiredTemplateVariables()
}

// TPFlagsForEngine names the flag spellings that carry the tensor-parallel
// dimension for an engine. Callers outside this package (a benchmark snapshot
// reading an argv, a preview) ask here rather than restating the spellings.
// An unknown engine has no flags: nothing is claimed rather than guessed.
func TPFlagsForEngine(name string) []string {
	engine, err := SelectEngine(name)
	if err != nil {
		return nil
	}
	return hooks(engine).TPFlags()
}

// PDConnectorFlagsForEngine names the flags the named engine's adapter
// composes for a PD instance; an unknown engine composes none.
func PDConnectorFlagsForEngine(name string) []string {
	engine, err := SelectEngine(name)
	if err != nil {
		return nil
	}
	return hooks(engine).PDConnectorFlags()
}

// SplitCommandForEngine splits a built command with the named engine's own
// knowledge of its executable shape. Callers that only hold an engine name — a
// proxy reading a stored launch, for instance — ask here instead of
// recognizing one engine's spelling themselves. An unknown engine keeps the
// launch's own entrypoint/argv pair, which is the base contract.
func SplitCommandForEngine(name string, launch ResolvedLaunch) (exe, params []string) {
	engine, err := SelectEngine(name)
	if err != nil {
		return launch.Entrypoint, launch.Argv
	}
	return hooks(engine).SplitCommand(launch)
}

// ParamFlagForEngine is the flag the parameter dictionary records for one key
// on one engine: empty when the engine has no row for that key (it cannot
// express the parameter) or the row is a platform parameter. A reader outside
// this package that needs one engine's flag asks here, so it neither matches
// engine names nor restates a spelling the table already owns.
func ParamFlagForEngine(engine, key string) string {
	selected, err := SelectEngine(engine)
	if err != nil {
		return ""
	}
	spec, known := engineParams(selected.Name())[key]
	if !known || spec.Platform {
		return ""
	}
	return spec.Flag
}

var (
	enginesMu     sync.RWMutex
	engines       = map[string]Engine{}
	defaultImages = map[string]string{}
	// sleepCaps is each registered engine's declared sleep capability, read
	// once at registration: it is a constant of the engine, and the hot reads
	// (every deployment DTO asks whether it may sleep) must not rebuild the
	// whole profile for one field.
	sleepCaps = map[string]SleepCaps{}
)

func RegisterEngine(e Engine) {
	caps := e.Profile().Sleep
	enginesMu.Lock()
	defer enginesMu.Unlock()
	engines[e.Name()] = e
	sleepCaps[e.Name()] = caps
}

// SleepCapsForEngine is the sleep capability ProfileForEngine(name).Sleep
// declares, without building the profile; the zero value for an unknown
// engine, as an unknown engine's profile has none. An empty name is the
// default engine.
func SleepCapsForEngine(name string) SleepCaps {
	if name == "" {
		name = DefaultEngine
	}
	enginesMu.RLock()
	defer enginesMu.RUnlock()
	return sleepCaps[name]
}

func EngineByName(name string) (Engine, bool) {
	enginesMu.RLock()
	defer enginesMu.RUnlock()
	e, ok := engines[name]
	return e, ok
}

func EngineNames() []string {
	enginesMu.RLock()
	defer enginesMu.RUnlock()
	out := make([]string, 0, len(engines))
	for name := range engines {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func SelectEngine(name string) (Engine, error) {
	if name == "" {
		name = DefaultEngine
	}
	if e, ok := EngineByName(name); ok {
		return e, nil
	}
	return nil, errors.New("unknown inference engine: " + name)
}

func ProfileForEngine(name string) (Profile, error) {
	e, err := SelectEngine(name)
	if err != nil {
		return Profile{}, err
	}
	profile := profileWithOverrides(e)
	profile.PDModes = pdModeProfiles(profile.Name)
	profile.KVBackends = kvBackendProfiles(profile.Name, "")
	return profile, nil
}

func SetDefaultImage(engine, image string) {
	if engine == "" || image == "" {
		return
	}
	enginesMu.Lock()
	defer enginesMu.Unlock()
	defaultImages[engine] = image
}

func profileWithOverrides(e Engine) Profile {
	p := e.Profile()
	p.OperationDefinitions = inferenceapi.Definitions()
	enginesMu.RLock()
	img := defaultImages[e.Name()]
	enginesMu.RUnlock()
	if img != "" {
		p.DefaultImage = img
	}
	return p
}

func init() {
	RegisterEngine(vLLMEngine{})
	RegisterEngine(sglangEngine{})
	RegisterEngine(callableEngine{})
}
