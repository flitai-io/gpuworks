package planning

import (
	"path"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// TranslatesServedName reports whether a node's agent would translate the
// value of --served-model-name to a host path: a host-process agent that does
// not advertise proto.CapLaunchNameVerbatim. caps is what the agent
// advertised, nil when it is offline or unknown, which counts as old since
// what it supports is unknown. Container agents run the name inside the
// container, where nothing is translated, whatever their version. It is
// LaunchRequest.ImplicitServedName for every launch the controller places and
// every preview of one, so both build the same command.
func TranslatesServedName(runtimeDriver string, caps []string) bool {
	return runtimeDriver == proto.RuntimeDriverHost && !slices.Contains(caps, proto.CapLaunchNameVerbatim)
}

// ExpectedServedName is the name an instance's agent checks its engine
// against (proto.InstanceDesiredState.ServedModelName), "" for none. On an
// agent that translates names (implicit, TranslatesServedName) a name that is
// a mount target or a path below one is what that agent rewrites to a host
// path before the engine sees it, so expecting it would refuse the agent's
// own engine as foreign. The builders leave such a name out of an engine
// launch already; a manual command keeps whatever it was written with.
func ExpectedServedName(resolved deploy.ResolvedLaunch, implicit bool) string {
	name := deploy.ServedModelName(resolved)
	if !implicit || name == "" {
		return name
	}
	targets := []string{resolved.ModelMount}
	for _, mount := range resolved.Mounts {
		targets = append(targets, mount.Target)
	}
	for _, target := range targets {
		if target == "" {
			continue
		}
		target = path.Clean(target)
		if target == "/" {
			continue
		}
		if name == target || strings.HasPrefix(name, target+"/") {
			return ""
		}
	}
	return name
}
