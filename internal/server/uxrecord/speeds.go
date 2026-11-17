package uxrecord

// A request's time to its first token and the speeds after it, the figures a
// request's detail quotes beside its segments. They are worked out here, from
// the same record the segments are, so the detail and the latency breakdown
// (LatencyOf) measure prefill and decode one way.

// Stage is the part of a request's time a segment belongs to: the wait for
// the first token (TTFT), the decode after it, or the end of the stream.
type Stage string

const (
	StageFirstToken Stage = "first_token"
	StageDecode     Stage = "decode"
	StageTail       Stage = "tail"
)

// StageOf is the stage a segment belongs to. Only a streamed request that
// has its first token, or is still waiting for it, is split into stages; a
// reply that does not stream, or a stream that ended without a token (an
// error, a disconnect, an empty reply), has no first token to split at, so
// its segments belong to no stage ("").
func StageOf(kind SegmentKind, staged bool) Stage {
	if !staged {
		return ""
	}
	switch kind {
	case SegmentThinking, SegmentAnswering:
		return StageDecode
	case SegmentTail:
		return StageTail
	}
	return StageFirstToken
}

// FirstTokenOf is the earlier of a first reasoning and a first answer time,
// or Unknown when neither came.
func FirstTokenOf(firstThinkMS, firstAnswerMS int32) int32 {
	switch {
	case firstThinkMS >= 0 && firstAnswerMS >= 0:
		return min(firstThinkMS, firstAnswerMS)
	case firstThinkMS >= 0:
		return firstThinkMS
	default:
		return firstAnswerMS
	}
}

// Speeds are a finished request's prefill and decode speeds, in tokens per
// second, and the time per output token after the first, in milliseconds;
// each is 0 when a figure it needs is missing.
type Speeds struct {
	PrefillTokS float64
	DecodeTokS  float64
	TPOTMS      float64
}

// SpeedsOf works out a finished request's speeds the way LatencyOf sums
// them: prefill is the uncached prompt over the engine's prefill time; decode
// is the output over the engine's decode time when it gave one, else the
// tokens after the first over the stream from the first token to the last.
func SpeedsOf(r Request) Speeds {
	var out Speeds
	if !r.Usage {
		return out
	}
	if uncached, ms := r.UncachedTokens(), r.UncachedComputeMS(); uncached > 0 && ms > 0 {
		out.PrefillTokS = float64(uncached) * 1000 / float64(ms)
	}
	tokens, ms := 0.0, 0.0
	switch first := r.FirstTokenMS(); {
	case r.CompletionTokens > 1 && r.EngineDecodeMS > 0:
		tokens, ms = float64(r.CompletionTokens), float64(r.EngineDecodeMS)
	case r.CompletionTokens > 1 && first >= 0 && r.LastChunkMS > first:
		tokens, ms = float64(r.CompletionTokens-1), float64(r.LastChunkMS-first)
	}
	if tokens > 0 && ms > 0 {
		out.DecodeTokS = tokens * 1000 / ms
		out.TPOTMS = ms / tokens
	}
	return out
}
