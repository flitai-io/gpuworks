// Package monitor computes the deterministic /monitor cluster, service and
// diagnosis views (plan artifacts/archive/2026-09-19-monitor-center/plan.md) from raw
// tsdb series. It is pure computation: no HTTP, and no import of store, api
// or controller. Callers hand it their state through the small Inventory and
// Deps types below instead of letting it reach into store directly - the
// app/api layer adapts store entities into these.
package monitor

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// DeploymentRef is the subset of a store.Deployment the monitor page needs.
type DeploymentRef struct {
	ID              uint
	Name            string
	Engine          string
	ReplicasDesired int
	// ReplicasReady is the domain-derived count when readiness needs facts
	// absent from InstanceRef (PD distributed-group completeness).
	// Nil keeps ordinary deployments counted from their running leaders.
	ReplicasReady *int
	NodeNames     []string
	ServedName    string
	SleepDesired  int
	// State is the state the deployment list shows
	// (deployment.EffectiveState): preparation failures, scheduling timeouts,
	// a delete in flight and a deployment the gateway refuses for sleep are
	// all already in it.
	State string
	// PD marks a PD disaggregated deployment (window_pd.go);
	// PDTransferMetrics says its PD mode's engine exports KV transfer
	// metrics (pdmode.Mode.TransferBytesMetric: NIXL does, vllm-ascend's
	// Mooncake connector does not).
	PD                bool
	PDTransferMetrics bool
	// HealthCheck and HealthCheckThresholdSec are the deployment's health
	// check settings (store.Deployment).
	HealthCheck             bool
	HealthCheckThresholdSec int
}

// InstanceRef is the subset of a store.Instance the monitor page needs.
//
// Headless marks a distributed-inference worker rank that participates in
// the engine's process group but exposes no API endpoint of its own
// (proto.InstanceDesiredState.Headless). It is not a "replica" from the
// product's point of view: plan §3.1's ready/desired counts and §3.2's
// replica table both describe API-facing instances, and counting a
// leader-plus-followers TP/PP group as N replicas would overstate capacity
// by exactly the follower count.
type InstanceRef struct {
	UUID         string
	DeploymentID uint
	NodeUUID     string
	State        string
	GPUIndexes   []int
	SleepLevel   int
	// Asleep is whether the gateway refuses this instance for sleep
	// (planning.InstanceAsleep): it reports a sleep level, or it is under a
	// sleep order its engine has not carried out yet. The level alone is not
	// the answer — an instance ordered to sleep is refused before its engine
	// reports the level — so the monitor reads this, not SleepLevel, to decide
	// whether an instance can serve. SleepLevel stays for display.
	Asleep   bool
	Headless bool
	// Role is the instance's PD role, empty outside PD deployments.
	Role string
}

// NodeRef is the subset of a store.Node the monitor page needs.
type NodeRef struct {
	UUID  string
	Name  string
	State string
}

// GPURef is the subset of a store.GPU (plus its scheduling allocation) the
// monitor page needs, matching the allocation/AllocatedBytes semantics
// internal/server/api/gpus.go already assembles from planning.GPUAllocations.
type GPURef struct {
	NodeUUID         string
	Index            int
	UUID             string
	MemoryTotalBytes int64
	AllocatedBytes   int64
	Healthy          bool
}

// Inventory is the current-state snapshot the app layer supplies. It carries
// no metrics - those live in tsdb and are queried through Deps.TSDB.
type Inventory struct {
	Deployments []DeploymentRef
	Instances   []InstanceRef
	Nodes       []NodeRef
	GPUs        []GPURef
}

// Deps are the injected collaborators every monitor computation needs.
type Deps struct {
	TSDB tsdb.Store

	// Inventory returns the current deployments/instances/nodes/GPUs.
	Inventory func(ctx context.Context) (Inventory, error)

	// Now returns the current time; overridable so tests get a fixed clock.
	// A nil Now means time.Now().
	Now func() time.Time
}

// now returns d.Now() when set, else the wall clock.
func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}
