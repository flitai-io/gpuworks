package app

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Asking a card from inside a runner image.
//
// The periodic runtime check runs the vendor's runtime on the host. PPU hosts
// carry only the management library and many Ascend hosts only the driver, so
// there the check would never run. The runner images the platform deploys do
// carry the runtime, and once one is on the node a container costs about what
// the check itself does: 0.55 s a card on a PPU host with docker, a 31 GB
// image included (overlay layers are shared, nothing is copied).
//
// The container is the cheap kind only on docker and nerdctl. An udocker
// container is a rootfs unpacked from the image, minutes and tens of
// gigabytes for a runner image, so udocker nodes are left to the host check.
//
// No image is ever pulled for this: a node without a matching image keeps the
// verdict its vendor tool gave, and says why.

// containerSmokeDrivers are the runtimes whose containers share the image's
// layers instead of copying them.
var containerSmokeDrivers = map[string]bool{"docker": true, "nerdctl": true}

const (
	// containerSmokeStartBudget bounds getting the container to run. Past it
	// the runtime, not the card, is what did not answer.
	containerSmokeStartBudget = 30 * time.Second
	// containerSmokeScriptBudget bounds the check inside it, as on the host.
	containerSmokeScriptBudget = 45 * time.Second
)

// cardSmokeBuilder builds the container a card is asked from (the engine's
// BuildCardSmokeWorkload).
type cardSmokeBuilder interface {
	BuildCardSmokeWorkload(image string, gpuIndex int, name string, entrypoint []string) (runtime.WorkloadSpec, error)
}

// containerSmoke asks cards of one vendor from inside a runner image.
type containerSmoke struct {
	vendor    string
	framework string
	driver    func() runtime.Driver
	builder   cardSmokeBuilder
	// images lists the references worth trying: the images of the
	// instances this node runs, then the catalog's.
	images func() []string

	mu    sync.Mutex
	image string // chosen by the last Prerequisite
	// leftovers are containers of earlier attempts not yet seen gone: the
	// check inside was killed and is stuck in the driver, or the runtime
	// would not remove it. Until they are gone no new container is started,
	// so a card stuck in the driver gathers one container, not one a round.
	leftovers map[string]bool
}

// clearLeftovers removes what it can of earlier attempts' containers and says
// why no new one may start, or "".
func (c *containerSmoke) clearLeftovers(ctx context.Context, d runtime.Driver) string {
	c.mu.Lock()
	ids := make([]string, 0, len(c.leftovers))
	for id := range c.leftovers {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	var remaining []string
	for _, id := range ids {
		if containerGone(ctx, d, id) {
			c.mu.Lock()
			delete(c.leftovers, id)
			c.mu.Unlock()
			continue
		}
		remaining = append(remaining, id)
	}
	if len(remaining) == 0 {
		return ""
	}
	return "an earlier runtime check container has not exited (" + strings.Join(remaining, ", ") + "); no new one is started until it does"
}

// containerGone stops and removes a container and reports whether it is gone.
// An inspection that fails says nothing, so it is not gone.
func containerGone(ctx context.Context, d runtime.Driver, id string) bool {
	_ = d.Stop(ctx, id)
	_ = d.Remove(ctx, id)
	st, err := d.Status(ctx, id)
	return err == nil && !st.Exists
}

func (c *containerSmoke) leaveBehind(id string) {
	c.mu.Lock()
	if c.leftovers == nil {
		c.leftovers = map[string]bool{}
	}
	c.leftovers[id] = true
	c.mu.Unlock()
}

// pick chooses the image to ask from, or says why there is none.
func (c *containerSmoke) pick(ctx context.Context) string {
	d := c.driver()
	if d == nil {
		return "no container runtime is available on this node"
	}
	if !containerSmokeDrivers[d.Name()] {
		return "the check from inside a runner image is not run on " + d.Name() + " nodes, where a container unpacks the whole image"
	}
	if reason := c.clearLeftovers(ctx, d); reason != "" {
		return reason
	}
	seen := map[string]bool{}
	for _, ref := range c.images() {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] || accelerator.RunnerTagFramework(ref) != c.framework {
			continue
		}
		seen[ref] = true
		if present, err := d.ImageExists(ctx, ref); err == nil && present {
			c.mu.Lock()
			c.image = ref
			c.mu.Unlock()
			return ""
		}
	}
	return "no " + c.framework + " runner image is on this node to run the check in; none is pulled for it"
}

func (c *containerSmoke) chosen() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.image
}

// try asks the card at device index visible from inside the chosen image.
func (c *containerSmoke) try(ctx context.Context, visible string) (err error) {
	image := c.chosen()
	index, convErr := strconv.Atoi(visible)
	if image == "" || convErr != nil || index < 0 {
		return &gpu.HostRuntimeUnavailableError{Reason: fmt.Sprintf("no runner image or device index to check card %q from", visible)}
	}
	d := c.driver()
	if d == nil {
		return &gpu.HostRuntimeUnavailableError{Reason: "no container runtime is available on this node"}
	}
	script, ok := accelerator.ContainerSmokeScript(c.vendor)
	if !ok {
		return &gpu.HostRuntimeUnavailableError{Reason: "no container runtime check for vendor " + c.vendor}
	}
	unavailable := func(format string, args ...any) error {
		return &gpu.HostRuntimeUnavailableError{Reason: "the runtime check container " + fmt.Sprintf(format, args...)}
	}
	// Unique per attempt: Doctor and the periodic round may ask the same
	// card at once. One left behind by a killed agent carries the gpuworks-
	// prefix and no desired instance's name, so the orphan sweep removes it.
	name := fmt.Sprintf("gpuworks-card-smoke-%d-%s", index, strconv.FormatInt(time.Now().UnixNano(), 36))
	start, cancel := context.WithTimeout(ctx, containerSmokeStartBudget)
	defer cancel()
	if reason := c.clearLeftovers(start, d); reason != "" {
		return unavailable("cannot start: %s", reason)
	}
	ws, err := c.builder.BuildCardSmokeWorkload(image, index, name, []string{"python3", "-c", script})
	if err != nil {
		return unavailable("could not be built: %v", err)
	}
	id, err := d.Run(start, ws)
	if err != nil {
		return unavailable("did not start from %s: %v", image, err)
	}
	defer func() {
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
		if !containerGone(cleanup, d, id) {
			c.leaveBehind(id)
		}
	}()
	// From here a hang is the card's: the runner's own deadline (ctx)
	// grades it, as it does on the host.
	var status runtime.Status
	for {
		status, err = d.Status(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return unavailable("could not be inspected: %v", err)
		}
		if status.Exists && !status.Running {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return gpu.ContainerSmokeVerdict(c.vendor, containerLogs(ctx, d, id), status.ExitCode)
}

func containerLogs(ctx context.Context, d runtime.Driver, id string) string {
	rc, err := d.Logs(ctx, id, false, 100)
	if err != nil {
		return ""
	}
	defer rc.Close()
	data, _ := io.ReadAll(io.LimitReader(rc, 64*1024))
	return string(data)
}

// cardRuntimeCheck is how this node's cards are asked whether the runtime can
// use them: the vendor's runtime on the host when the host has it, otherwise
// from inside a runner image already on the node. Which one is decided each
// round (Prerequisite), because an image can arrive and CANN can be installed
// while the agent runs.
func (n *NodeRuntime) cardRuntimeCheck(vendor string) (gpu.HostRuntimeCheck, bool) {
	// The host half comes through the same seam Doctor's tests replace.
	host, hasHost := doctorHostRuntimeCheck(vendor)
	if n.kubeNode {
		// A container check would create a pod and borrow a card from the
		// kubelet to ask it. The host's runtime, asked only about the cards
		// the platform holds, is the one check a Kubernetes node makes.
		if !hasHost {
			return host, false
		}
		return n.holdCardsWhileAsking(host), true
	}
	framework, hasContainer := accelerator.ContainerSmokeFramework(vendor)
	if !hasContainer || n.engine == nil {
		return host, hasHost
	}
	c := &containerSmoke{
		vendor: accelerator.NormalizeVendor(vendor), framework: framework,
		driver: n.runtimeDriver, builder: n.engine,
		images: func() []string {
			n.imageRefsMu.RLock()
			catalog := append([]string(nil), n.imageRefs...)
			n.imageRefsMu.RUnlock()
			return append(n.engine.DesiredImages(), catalog...)
		},
	}
	return composeCardRuntimeCheck(vendor, host, hasHost, c), true
}

// composeCardRuntimeCheck puts the host check first and the container one
// behind it.
func composeCardRuntimeCheck(vendor string, host gpu.HostRuntimeCheck, hasHost bool, c *containerSmoke) gpu.HostRuntimeCheck {
	var mu sync.Mutex
	useHost := false
	check := gpu.HostRuntimeCheck{
		CLI:     strings.Fields(accelerator.ToolCommand(vendor) + " ")[0],
		Timeout: containerSmokeStartBudget + containerSmokeScriptBudget,
		Visible: func(g proto.GPUInfo) (string, bool) {
			if g.Index < 0 {
				return "", false
			}
			return strconv.Itoa(g.Index), true
		},
	}
	if hasHost {
		check.CLI, check.Visible = host.CLI, host.Visible
		check.Timeout = max(check.Timeout, host.Timeout)
	}
	check.Prerequisite = func() string {
		var reasons []string
		if hasHost {
			reason := host.Prerequisite()
			if reason == "" {
				mu.Lock()
				useHost = true
				mu.Unlock()
				return ""
			}
			reasons = append(reasons, reason)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		reason := c.pick(ctx)
		if reason == "" {
			mu.Lock()
			useHost = false
			mu.Unlock()
			return ""
		}
		return strings.Join(append(reasons, reason), "; ")
	}
	check.Try = func(ctx context.Context, visible string) error {
		mu.Lock()
		onHost := useHost
		mu.Unlock()
		if onHost {
			return host.Try(ctx, visible)
		}
		return c.try(ctx, visible)
	}
	return check
}
