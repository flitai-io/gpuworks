package proxy

import (
	"encoding/json"
	"path"
	"strings"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// instanceModelName is the name the instance's engine answers under. It reads
// the concrete launch, since a template or manual command can override both
// the model path and its alias independently of the public routing name.
//
// hostProcess is whether the instance's node runs engines as host processes
// (planning.HostTranslatesModelPaths). There the agent rewrites the model path
// to its host source before the engine sees it, so an engine launched without
// --served-model-name registers that host path as its name, whatever the
// agent's version; hostEngineName applies the same rewrite.
func instanceModelName(dep *store.Deployment, instance *store.Instance, hostProcess bool) string {
	name, _ := instanceLaunchFacts(dep, instance, hostProcess)
	return name
}

// instanceLaunchFacts reads what routing needs from the instance's launch in
// one decode of its desired state: the name its engine answers under
// (instanceModelName) and its run slots (deploy.RunSlots, 0 when unknown).
func instanceLaunchFacts(dep *store.Deployment, instance *store.Instance, hostProcess bool) (string, int) {
	if dep.Engine != engines.SGLang && dep.Engine != engines.VLLM {
		return "", 0
	}
	var desired proto.InstanceDesiredState
	if json.Unmarshal([]byte(instance.DesiredStateJSON), &desired) != nil {
		return "", 0
	}
	args, scriptArgs, ok := deploy.LaunchArgs(desired.ResolvedLaunch)
	if !ok {
		return "", 0
	}
	slots, _ := deploy.RunSlots(dep.Engine, args)
	return launchModelName(dep, desired, args, scriptArgs, hostProcess), slots
}

// instanceRunSlots is the instance's run slots alone (instanceLaunchFacts):
// where its node runs does not change them.
func instanceRunSlots(dep *store.Deployment, instance *store.Instance) int {
	_, slots := instanceLaunchFacts(dep, instance, false)
	return slots
}

func launchModelName(dep *store.Deployment, desired proto.InstanceDesiredState, args, scriptArgs []string, hostProcess bool) string {
	engineName := func(name string) string {
		if hostProcess {
			return hostEngineName(desired, name)
		}
		return name
	}
	// The served name is a name, kept verbatim by the agent; only a
	// model path is rewritten on a host-process node.
	if value, found := deploy.LaunchFlag(args, "--served-model-name"); found {
		return value
	}
	if hasUnfixedFlag(args, "--served-model-name") {
		return ""
	}
	for _, flag := range []string{"--model-id", "--model-path", "--model"} {
		if value, found := deploy.LaunchFlag(args, flag); found {
			return engineName(value)
		}
		if hasUnfixedFlag(args, flag) {
			return ""
		}
	}
	// An engine that carries the model as a positional parameter is read
	// through the engine's own split of its command, so the separator token is
	// not restated here.
	if name, ok := positionalModelName(dep.Engine, proto.ResolvedLaunch{Entrypoint: desired.Entrypoint, Argv: desired.Argv}); ok {
		return engineName(name)
	}
	// A manual command keeps its tokens inside a shell script, where the engine
	// command starts after whatever the script did first (set -a, exports). Each
	// suffix of those tokens is offered to the same rule.
	for start := range scriptArgs {
		if name, ok := positionalModelName(dep.Engine, proto.ResolvedLaunch{Entrypoint: scriptArgs[start:]}); ok {
			return engineName(name)
		}
	}
	return engineName(desired.ModelMount)
}

// hostEngineName is name as a host-process agent hands it to the engine: a
// path at or below the model mount becomes the same path under the model's
// host directory (ModelPath), with a trailing slash kept, as the agent's
// translation of a launch does (internal/agent/runtime/hostproc). Any other
// name is left alone: the model is never under the other mounts, whose host
// sources only the agent resolves. The two rules are held together by
// internal/proto/testdata/hostproc_engine_names.json.
func hostEngineName(desired proto.InstanceDesiredState, name string) string {
	mount := path.Clean(desired.ModelMount)
	if desired.ModelPath == "" || desired.ModelMount == "" || mount == "/" || !strings.HasPrefix(name, "/") {
		return name
	}
	clean := path.Clean(name)
	mapped := ""
	if clean == mount {
		mapped = desired.ModelPath
	} else if rest, ok := strings.CutPrefix(clean, mount+"/"); ok {
		mapped = path.Join(desired.ModelPath, rest)
	} else {
		return name
	}
	if strings.HasSuffix(name, "/") && !strings.HasSuffix(mapped, "/") {
		mapped += "/"
	}
	return mapped
}

// positionalModelName reports a model path an engine carries as a positional
// parameter (vLLM's `serve <model>`) rather than as a flag. The engine's own
// SplitCommand answers whether the command is its own shape and where its
// parameters begin, so no executable spelling is restated here; a launch the
// engine does not recognize has no positional model of its own.
func positionalModelName(engine string, launch proto.ResolvedLaunch) (string, bool) {
	_, params := deploy.SplitCommandForEngine(engine, launch)
	if len(params) == 0 || strings.HasPrefix(params[0], "-") {
		return "", false
	}
	return params[0], true
}

// hasUnfixedFlag reports whether flag is set to a value the shell would still
// expand, which names no model the proxy can rely on.
func hasUnfixedFlag(args []string, flag string) bool {
	for index, arg := range args {
		if (arg == flag && index+1 < len(args)) || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}
