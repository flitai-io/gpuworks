package rollup

import (
	"sync"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// UsageKey is one row of the token usage: an hour, the API key (0 without
// one: the playground, or a keyless request the gateway accepted because open
// access is on), the user the request ran as, the deployment and where the
// request came from.
type UsageKey struct {
	HourMS       int64
	APIKeyID     uint
	UserID       uint
	DeploymentID uint
	Source       uxrecord.Source
}

// Usage is what the requests of a UsageKey used. Token counts add up only
// the requests whose reply carried the engine's usage; cache figures only
// those that also reported one, so a hit rate divides Cached by
// CacheReportedPrompt, never by every prompt.
type Usage struct {
	Requests int64
	// NoUsage counts requests whose tokens are unknown: errors, replies the
	// client left, engines that reported none.
	NoUsage          int64
	PromptTokens     int64
	CompletionTokens int64
	// ReasoningTokens adds up the completion tokens spent reasoning, of the
	// ReasoningReported requests that said.
	ReasoningTokens   int64
	ReasoningReported int64
	// CachedTokens is the prompt tokens served from the engine's cache, of
	// the CacheReported requests whose CacheReportedPrompt it is part of.
	CachedTokens        int64
	CacheReported       int64
	CacheReportedPrompt int64
}

// Add adds o to u.
func (u *Usage) Add(o Usage) {
	u.Requests += o.Requests
	u.NoUsage += o.NoUsage
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.ReasoningReported += o.ReasoningReported
	u.CachedTokens += o.CachedTokens
	u.CacheReported += o.CacheReported
	u.CacheReportedPrompt += o.CacheReportedPrompt
}

// QualityKey is one row of the service quality: an hour and a deployment.
type QualityKey struct {
	HourMS       int64
	DeploymentID uint
}

// Quality is how a deployment's requests of an hour turned out.
type Quality struct {
	Requests     int64
	Completed    int64
	Length       int64
	ClientClosed int64
	Interrupted  int64
	Error        int64
	// TTFT is the time to the first token of the whole streamed replies;
	// EndToEnd the whole time of every whole reply.
	TTFT     Histogram
	EndToEnd Histogram
	// OutputTokens and DecodeMS add up the whole streamed replies with usage
	// and a timed stream: their completion tokens and the time from the
	// first token to the last event, for the output speed.
	OutputTokens int64
	DecodeMS     int64
}

// Add adds o to q.
func (q *Quality) Add(o Quality) {
	q.Requests += o.Requests
	q.Completed += o.Completed
	q.Length += o.Length
	q.ClientClosed += o.ClientClosed
	q.Interrupted += o.Interrupted
	q.Error += o.Error
	q.TTFT.Add(o.TTFT)
	q.EndToEnd.Add(o.EndToEnd)
	q.OutputTokens += o.OutputTokens
	q.DecodeMS += o.DecodeMS
}

// requestShards spreads deployments over independent locks: the gateway
// finishes about 1300 requests a second at fleet scale.
const requestShards = 32

// Requests accumulates the token usage and service quality of finished
// requests. Observe is safe for concurrent use and never blocks on I/O.
type Requests struct {
	shards [requestShards]requestShard
}

type requestShard struct {
	mu      sync.Mutex
	usage   map[UsageKey]*Usage
	quality map[QualityKey]*Quality
}

// NewRequests returns an empty accumulator.
func NewRequests() *Requests {
	r := &Requests{}
	for i := range r.shards {
		r.shards[i].usage = map[UsageKey]*Usage{}
		r.shards[i].quality = map[QualityKey]*Quality{}
	}
	return r
}

// Observe adds one finished request. It is attributed to the hour it ended
// in: a long request crossing midnight lands in the day it finished, whose
// roll-up has not run yet. A row seen before costs no allocation.
func (r *Requests) Observe(rec uxrecord.Request) {
	r.ObserveWeighted(rec, 1)
}

// ObserveWeighted adds a request that stands for weight requests: a row the
// request log kept for several it sampled away (uxstore), when the history
// is filled in from it.
func (r *Requests) ObserveWeighted(rec uxrecord.Request, weight int64) {
	if weight < 1 {
		weight = 1
	}
	endMS := rec.ReceivedMS
	if rec.EndMS > 0 {
		endMS += int64(rec.EndMS)
	}
	hour := HourOfMS(endMS)
	s := &r.shards[rec.DeploymentID%requestShards]
	s.mu.Lock()
	uk := UsageKey{HourMS: hour, APIKeyID: rec.APIKeyID, UserID: rec.UserID, DeploymentID: rec.DeploymentID, Source: rec.Source}
	u := s.usage[uk]
	if u == nil {
		u = new(Usage)
		s.usage[uk] = u
	}
	addUsage(u, &rec, weight)
	qk := QualityKey{HourMS: hour, DeploymentID: rec.DeploymentID}
	q := s.quality[qk]
	if q == nil {
		q = new(Quality)
		s.quality[qk] = q
	}
	addQuality(q, &rec, weight)
	s.mu.Unlock()
}

func addUsage(u *Usage, rec *uxrecord.Request, w int64) {
	u.Requests += w
	if !rec.Usage {
		u.NoUsage += w
		return
	}
	prompt := nonNegative(rec.PromptTokens) * w
	u.PromptTokens += prompt
	u.CompletionTokens += nonNegative(rec.CompletionTokens) * w
	if rec.ReasoningTokens >= 0 {
		u.ReasoningTokens += int64(rec.ReasoningTokens) * w
		u.ReasoningReported += w
	}
	if rec.CachedTokens >= 0 {
		u.CachedTokens += int64(rec.CachedTokens) * w
		u.CacheReported += w
		u.CacheReportedPrompt += prompt
	}
}

func addQuality(q *Quality, rec *uxrecord.Request, w int64) {
	q.Requests += w
	result := uxrecord.ResultOf(*rec)
	switch result {
	case uxrecord.ResultCompleted:
		q.Completed += w
	case uxrecord.ResultLength:
		q.Length += w
	case uxrecord.ResultClientClosed:
		q.ClientClosed += w
	case uxrecord.ResultInterrupted:
		q.Interrupted += w
	default:
		q.Error += w
	}
	if result != uxrecord.ResultCompleted && result != uxrecord.ResultLength {
		return
	}
	if rec.EndMS >= 0 {
		q.EndToEnd.ObserveN(float64(rec.EndMS), w)
	}
	first := firstTokenMS(rec)
	if rec.Stream && first >= 0 {
		q.TTFT.ObserveN(float64(first), w)
		if rec.Usage && rec.CompletionTokens > 0 && rec.LastChunkMS > first {
			q.OutputTokens += int64(rec.CompletionTokens) * w
			q.DecodeMS += int64(rec.LastChunkMS-first) * w
		}
	}
}

// firstTokenMS is when a stream's first reasoning or answer text came, or
// Unknown.
func firstTokenMS(rec *uxrecord.Request) int32 {
	first := rec.FirstThinkMS
	if a := rec.FirstAnswerMS; a >= 0 && (first < 0 || a < first) {
		first = a
	}
	if first < 0 {
		return uxrecord.Unknown
	}
	return first
}

func nonNegative(v int32) int64 {
	if v < 0 {
		return 0
	}
	return int64(v)
}

// TakeUsage returns and clears the token usage only; the quality keeps
// accumulating for its own, less frequent flush.
func (r *Requests) TakeUsage() map[UsageKey]Usage {
	out := map[UsageKey]Usage{}
	for i := range r.shards {
		s := &r.shards[i]
		s.mu.Lock()
		usage := s.usage
		s.usage = make(map[UsageKey]*Usage, len(usage))
		s.mu.Unlock()
		for k, v := range usage {
			out[k] = *v
		}
	}
	return out
}

// TakeQuality returns and clears the service quality only.
func (r *Requests) TakeQuality() map[QualityKey]Quality {
	out := map[QualityKey]Quality{}
	for i := range r.shards {
		s := &r.shards[i]
		s.mu.Lock()
		quality := s.quality
		s.quality = make(map[QualityKey]*Quality, len(quality))
		s.mu.Unlock()
		for k, v := range quality {
			out[k] = *v
		}
	}
	return out
}

// GiveBackUsage merges a usage delta a flush could not write back in.
func (r *Requests) GiveBackUsage(delta map[UsageKey]Usage) {
	for k, v := range delta {
		s := &r.shards[k.DeploymentID%requestShards]
		s.mu.Lock()
		if cur := s.usage[k]; cur != nil {
			cur.Add(v)
		} else {
			v := v
			s.usage[k] = &v
		}
		s.mu.Unlock()
	}
}

// GiveBackQuality merges a quality delta a flush could not write back in.
func (r *Requests) GiveBackQuality(delta map[QualityKey]Quality) {
	for k, v := range delta {
		s := &r.shards[k.DeploymentID%requestShards]
		s.mu.Lock()
		if cur := s.quality[k]; cur != nil {
			cur.Add(v)
		} else {
			v := v
			s.quality[k] = &v
		}
		s.mu.Unlock()
	}
}
