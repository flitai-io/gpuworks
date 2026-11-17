package app

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
)

// A node whose chosen accelerator has no usable CLI or cards yet watches
// for one to answer, instead of staying that way until something restarts
// the agent by hand.
//
// Two real cases land here: a Kubernetes DaemonSet pod started before the
// GPU Operator's driver container published nvidia-smi (gpu.go's busScanRoot
// still names the right vendor from the PCI bus, but its CLI genuinely does
// not answer yet), and any host — Kubernetes or not — whose driver is
// installed after the agent started. Both leave the node reporting zero
// GPUs until something re-runs vendor selection; Run starts this watch
// exactly then.

// acceleratorReprobeInterval is how often the watch asks redetect() again.
// Production ticks a real timer at this interval; tests drive the ticks
// channel themselves, so it is never read in this file.
var acceleratorReprobeInterval = 30 * time.Second

// lateAcceleratorRestart replaces this process, the same way
// accelerator_choice.go's reexecAgent does for an operator's own vendor
// choice; a test overrides it to observe a restart without replacing the
// test binary.
var lateAcceleratorRestart = reexecAgent

// watchForLateAccelerator runs for the life of ctx. redetect is the same
// selection the agent made at start (gpu.New or gpu.NewFor through
// AcceleratorHost.ProberFor), asked again on every tick: cheaply, mostly
// stat/path lookups until a vendor's CLI genuinely answers (see gpu.go's
// fallbackProber and nvidia_host.go's locateNVIDIASMI), never a heavy probe
// by itself. Once redetect reports a prober whose tool answers, the watch
// restarts the agent so the next start picks it up clean: the prober,
// device injection, domestic runtime assets and partition probes are
// otherwise all built once at start (accelerator_choice.go's own doc
// explains why), the same reason an operator's own vendor switch restarts
// rather than swaps the running process's prober. A node already running
// something defers the restart to a later tick instead of disrupting it.
// ctx ending (the agent shutting down) or redetect never finding a working
// tool simply leaves the watch running until one of them does; it is a
// no-op between ticks, never a busy loop.
func (n *NodeRuntime) watchForLateAccelerator(ctx context.Context, redetect func() gpu.Prober, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			candidate := redetect()
			if candidate == nil || !candidate.Available() {
				continue
			}
			if n.engine != nil && !n.engine.Idle() {
				n.log.Info("an accelerator tool answered after the agent started, but instances are running on this node; restarting once it is idle",
					"vendor", candidate.Vendor())
				continue
			}
			n.log.Info("an accelerator tool answered after the agent started; restarting to manage it", "vendor", candidate.Vendor())
			if err := lateAcceleratorRestart(); err != nil {
				n.log.Error("restarting to pick up a newly answering accelerator tool failed; it will try again", "err", err)
				continue
			}
			return
		}
	}
}

// startLateAcceleratorWatch runs watchForLateAccelerator with a real
// ticker, for the life of ctx. Run calls it only when the automatically
// chosen prober has no usable CLI or cards yet.
func (n *NodeRuntime) startLateAcceleratorWatch(ctx context.Context, redetect func() gpu.Prober) {
	ticker := time.NewTicker(acceleratorReprobeInterval)
	defer ticker.Stop()
	n.watchForLateAccelerator(ctx, redetect, ticker.C)
}
