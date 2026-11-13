package benchrun

import (
	"crypto/rand"
	"encoding/binary"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func (e *execution) pointEvidence(dc dataset.Config, reqs []loadgen.Request, requests []metrics.Request, canceled bool) *metrics.PointEvidence {
	plan := metrics.EvidencePlan{PlannedSessions: len(reqs), PlannedRequests: loadgen.TotalRequests(reqs), OutputPolicy: "natural"}
	sessions := make([]metrics.SessionEvidence, len(reqs))
	for i, req := range reqs {
		id := req.SessionID
		if id == "" {
			id = metrics.SessionID(i)
		}
		sessions[i] = metrics.SessionEvidence{SessionID: id, PlannedTurns: 1 + len(req.FollowUps)}
		if i == 0 {
			plan.TurnsPerSession, plan.OutputBudget = sessions[i].PlannedTurns, req.MaxTokens
		}
		if plan.TurnsPerSession != sessions[i].PlannedTurns {
			plan.TurnsPerSession = 0
		}
		if plan.OutputBudget != req.MaxTokens {
			plan.OutputBudget = 0
		}
		if req.IgnoreEOS {
			plan.OutputPolicy = "ignore_eos_requested"
		}
	}
	seed := dc.Seed
	if dc.Kind == "agentic" {
		if expanded, err := dataset.ExpandAgenticConfig(dc); err == nil {
			seed = expanded.Seed
		}
	}
	execution := metrics.EvidenceExecution{Seed: seed, SeedDerivation: "unchanged", CachePolicy: "unknown", CacheState: "unknown", RetryPolicy: "none", TokenizerSource: e.run.TokenizerRef, LocalTokenCountMethod: "character_estimate", DetailPolicy: "origin_configured", WarmupRequests: e.doc.WarmupRequests, WarmupSucceeded: e.warmupSucceeded}
	if dc.Kind == "agentic" {
		execution.SeedDerivation = "cumulative_stride_v1"
	}
	if e.tok != nil {
		execution.LocalTokenCountMethod = "local_tokenizer_estimate"
	}
	return metrics.BuildPointEvidence(plan, sessions, requests, execution, canceled)
}

// Resolve unspecified seeds before generating samples so their actual seed is
// retained. This matches the dataset generator's random-seed distribution.
func newPointSeed() int64 {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		if seed := int64(binary.LittleEndian.Uint64(raw[:])); seed != 0 {
			return seed
		}
	}
	return time.Now().UnixNano()
}
