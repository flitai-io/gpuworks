package app

import (
	"context"

	"gpuworks.ai/gpuworks/internal/accelerator"

	"gpuworks.ai/gpuworks/internal/agent/collector"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// StartBackground runs the metrics collector and per-instance log shippers for
// the lifetime of the agent process. Emissions route through the currently
// bound connection sender and are dropped while offline (the server re-derives
// on reconnect; metrics are best-effort).
func (n *NodeRuntime) StartBackground(ctx context.Context, nodeUUID string) {
	col := collector.New(nodeUUID, n.prober, n.runningInstances, func(b proto.MetricBatch) {
		n.emit(proto.TMetrics, b)
	}, n.log)
	col.SetModelsRoot(n.ModelsRoot())
	col.SetDCGMProbe(gpu.NewDCGMProbe(n.dcgmMode))
	col.SetSimulatedHost(n.host)
	runCollector := n.collectorRunner
	if runCollector == nil {
		runCollector = func(ctx context.Context, col *collector.Collector) { col.Run(ctx) }
	}
	go runCollector(ctx, col)
	go n.runStorages(ctx)
	n.startCardSmoke(ctx)
	if n.prober != nil && accelerator.NormalizeVendor(n.prober.Vendor()) == accelerator.VendorNVIDIA && !gpu.IsSynthetic(n.prober) {
		xid := &gpu.XidWatcher{}
		n.xid.Store(xid)
		go xid.Run(ctx)
	}
	if n.journal == nil {
		n.journal = journald{}
	}
	// A simulated node's host is not the machine it stands for: its logs
	// are not that machine's to ship.
	if n.host.ShipsHostLogs() {
		go n.shipNodeLogs(ctx)
	}
	go n.shipSelfLogs(ctx)
	go n.retryLogs(ctx, logRetryInterval)
	if n.engine != nil {
		go n.shipLogs(ctx)
		// Startup probes stop looking once an instance runs; this reports a
		// container that exits afterwards so the controller can rebuild it.
		go n.engine.RunLivenessMonitor(ctx)
		go n.watchRuntimeFallback(ctx)
		// The KV disk tier's capacity is the node's to enforce: vLLM's
		// filesystem tier never deletes anything. One sweeper covers every
		// deployment on this node and reports each directory's usage through
		// the same metric channel as the collector.
		go engine.NewKVDiskSweeper(nodeUUID, n.engine.KVDiskTargets, func(samples []proto.MetricSample) {
			n.emit(proto.TMetrics, proto.MetricBatch{Samples: samples})
		}, n.log).Run(ctx)
	}
}

// runningInstances adapts engine specs for the collector.
func (n *NodeRuntime) runningInstances() []collector.RunningInstance {
	if n.engine == nil {
		return nil
	}
	var out []collector.RunningInstance
	for _, s := range n.engine.MetricSpecs() {
		out = append(out, collector.RunningInstance{
			UUID: s.UUID, APIPort: s.APIPort, Engine: s.Engine,
			MetricPath: s.MetricPath, MetricPrefix: s.MetricPrefix, MetricMap: s.MetricMap, Observation: s.Observation,
			GPUIndexes: s.GPUIndexes, VRAMLimit: s.VRAMLimitBytes, DGPUCache: n.engine.DGPUCachePath(s.UUID),
			DeploymentID: s.DeploymentID,
		})
	}
	return out
}
