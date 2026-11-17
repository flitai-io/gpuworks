package proxy

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// injectsUsage reports whether the gateway asks a deployment's engine for
// usage on streamed requests (usage_inject.go). Only engines known to accept
// stream_options do: vLLM and SGLang send no usage unless asked, while a
// callable engine may refuse a field it does not know (and the ones the
// platform ships, TensorFold and Strata, report usage unasked).
func injectsUsage(engine string) bool {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case engines.VLLM, engines.SGLang:
		return true
	}
	return false
}

// cacheReportOmitsZero reports whether a deployment's engine reports cache
// hits on every reply but omits a zero count: SGLang with the cache report
// on (enable_cache_report, --enable-cache-report) writes
// prompt_tokens_details only when something was cached. vLLM with
// --enable-prompt-tokens-details writes cached_tokens: 0, so it needs no
// such reading.
func cacheReportOmitsZero(dep *store.Deployment) bool {
	if dep == nil || !strings.EqualFold(strings.TrimSpace(dep.Engine), engines.SGLang) {
		return false
	}
	var launch struct {
		EnableCacheReport *bool `json:"enable_cache_report"`
	}
	if dep.QuickParams != "" && json.Unmarshal([]byte(dep.QuickParams), &launch) == nil && launch.EnableCacheReport != nil {
		return *launch.EnableCacheReport
	}
	return slices.Contains(strings.Fields(dep.ExtraArgs), "--enable-cache-report")
}

// requestFacts is what the gateway knows about a request before its reply.
type requestFacts struct {
	id           string
	trace        tracecontext.Span
	model        string
	deploymentID uint
	caller       uxrecord.Caller
	path         string
	dispatchedAt time.Time
	body         bodyFacts
	// pd is the request's PD prefill stage; nil when it had none.
	pd *uxrecord.PDStage
	// priorityClass is the class the request ran at (priority.go).
	priorityClass string
}

// msSince converts an offset from dispatch into milliseconds since receipt.
func msSince(dispatchMS int64, d time.Duration) int32 {
	return clampMS(dispatchMS + d.Milliseconds())
}

func clampMS(ms int64) int32 {
	return int32(min(max(ms, 0), math.MaxInt32))
}

func clampCount(n uint64) int32 {
	return int32(min(n, math.MaxInt32))
}

// buildRecord is a finished request's record.
func buildRecord(f requestFacts, instanceUUID string, status int, duration time.Duration, stats responseStats, outcome uxrecord.Outcome, firstByte time.Time) uxrecord.Request {
	dispatchMS := f.dispatchedAt.UnixMilli() - f.caller.ReceivedMS
	rec := uxrecord.Request{
		ID: f.id, Trace: f.trace, Model: f.model, Sampling: f.body.sampling, ReceivedMS: f.caller.ReceivedMS, DeploymentID: f.deploymentID, InstanceUUID: instanceUUID,
		APIKeyID: f.caller.APIKeyID, UserID: f.caller.UserID, Source: f.caller.Source,
		Path: f.path, Stream: f.body.stream || stats.events > 0, BodyBytes: f.body.bytes, Images: f.body.images,
		DispatchMS: clampMS(dispatchMS), FirstByteMS: uxrecord.Unknown,
		FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: uxrecord.Unknown, LastChunkMS: uxrecord.Unknown,
		EndMS:  msSince(dispatchMS, duration),
		Events: clampCount(uint64(stats.events)), ThinkEvents: clampCount(uint64(stats.thinkEvents)),
		MaxGapMS: clampMS(stats.maxGap.Milliseconds()),
		Status:   status, Outcome: outcome, FinishReason: stats.finishReason,
		Usage: stats.usage, CachedTokens: uxrecord.Unknown, ReasoningTokens: uxrecord.Unknown,
		EngineQueueMS: uxrecord.Unknown, EnginePrefillMS: uxrecord.Unknown, EngineDecodeMS: uxrecord.Unknown,
		Weight: 1, PD: f.pd, PriorityClass: f.priorityClass,
	}
	if !firstByte.IsZero() {
		rec.FirstByteMS = msSince(dispatchMS, firstByte.Sub(f.dispatchedAt))
	}
	if stats.hasThink {
		rec.FirstThinkMS = msSince(dispatchMS, stats.firstThink)
	}
	if stats.hasAnswer {
		rec.FirstAnswerMS = msSince(dispatchMS, stats.firstAnswer)
	}
	if stats.events > 0 {
		rec.LastChunkMS = msSince(dispatchMS, stats.lastEvent)
	}
	if stats.usage {
		rec.PromptTokens, rec.CompletionTokens = clampCount(stats.prompt), clampCount(stats.completion)
		if stats.cachedKnown {
			rec.CachedTokens = clampCount(stats.cached)
		}
		if stats.reasoningKnown {
			rec.ReasoningTokens = clampCount(stats.reasoning)
		}
	}
	secondsToMS := func(stat string) int32 {
		value, ok := stats.engine[stat]
		if !ok {
			return uxrecord.Unknown
		}
		return clampMS(int64(math.Round(value * 1000)))
	}
	rec.EngineQueueMS = secondsToMS(inferencemetrics.RequestQueueSeconds)
	rec.EnginePrefillMS = secondsToMS(inferencemetrics.RequestPrefillSeconds)
	rec.EngineDecodeMS = secondsToMS(inferencemetrics.RequestDecodeSeconds)
	return rec
}
