package app

import (
	"context"
	goruntime "runtime"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	nerdctldrv "gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	udockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/udocker"
)

// otherRuntimes are the container runtimes on this node besides active that
// answer now, as sources of images the node already holds: one the node
// switched away from keeps every image it pulled. Each is built afresh, so
// none shares state with the active driver, and asking one sets nothing up.
func (n *NodeRuntime) otherRuntimes(ctx context.Context, active string) []runtime.LocalImageSource {
	var sources []runtime.LocalImageSource
	for _, build := range []func() runtime.Driver{
		func() runtime.Driver { return dockerdrv.New() },
		func() runtime.Driver { return nerdctldrv.New() },
		func() runtime.Driver { return udockerdrv.New(n.stateDir) },
	} {
		driver := build()
		if strings.EqualFold(driver.Name(), active) || driverUnavailableOn(goruntime.GOOS, driver.Name()) != "" {
			continue
		}
		source, ok := driver.(runtime.LocalImageSource)
		if !ok {
			continue
		}
		if usable, _ := driver.Detect(ctx); usable {
			sources = append(sources, source)
		}
	}
	return sources
}
