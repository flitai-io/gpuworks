// Package sleep is the single home of the server's sleep/wake policy: what a
// sleeping instance still occupies on its GPU, whether a deployment may sleep
// at all, whether a sleeping deployment may be woken without oversubscribing
// its GPUs.
//
// It is a leaf: it never reads the store and knows no engine by name. Engine
// capability arrives as the engine's own deploy.SleepCaps; GPU occupancy
// arrives as snapshots the caller derived from the ledger. The plumbing that
// carries a desired sleep level to the engine (store setter, controller push,
// agent HTTP calls, gateway gating) stays where it is and consults this package
// for every decision.
package sleep

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Stable error codes for a sleep or wake order the server refuses. They are
// part of the API wire contract; api/errors.go lists the same strings in its
// inventory and asserts they match.
const (
	CodeNotEnabled        = "SLEEP_NOT_ENABLED"
	CodeEngineUnsupported = messages.ErrorSleepEngineUnsupported
)

// Deployment is the subset of a deployment's declaration that decides whether
// it may sleep. It is a value type so callers in api, controller and planning
// can build it from their own view of the deployment without sharing a store
// entity.
type Deployment struct {
	Sleepable bool
	Caps      deploy.SleepCaps
}

// Eligible reports whether the deployment may be asked to sleep or wake. The
// returned code is empty when it may; otherwise it is the stable refusal code.
//
// A multi-node replica sleeps like a single-node one: the order goes to its
// leader, whose engine sleeps and wakes the follower ranks with it (the
// followers take no order of their own), as verified on hardware for vLLM
// (ADR 0033).
func Eligible(d Deployment) string {
	if !d.Sleepable {
		return CodeNotEnabled
	}
	if !d.Caps.Supported {
		return CodeEngineUnsupported
	}
	return ""
}

// Declarable reports whether a new deployment may declare itself sleepable.
// It differs from Eligible only in that a deployment that does not ask to be
// sleepable is always fine to create.
func Declarable(d Deployment) string {
	if !d.Sleepable {
		return ""
	}
	return Eligible(d)
}

// CodeHostMemoryInsufficient refuses an L1 sleep whose weights the node's host
// memory cannot hold: L1 moves the weights into host RAM, L2 discards them.
const CodeHostMemoryInsufficient = "SLEEP_HOST_MEMORY_INSUFFICIENT"

// HostMemoryHeadroom is the host memory an L1 sleep must leave free on top of
// the weights it moves there, so the node keeps room for the system, the
// agent and the engines' own host buffers: 5% of the host, at least 4 GiB.
func HostMemoryHeadroom(totalBytes int64) int64 {
	return max(totalBytes/20, 4*gib)
}
