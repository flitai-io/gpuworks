package monitorapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// RequestSource is where the per-request views come from: the gateway's
// in-flight table and the request store (ux.db). The app wires both in;
// this package depends on neither.
type RequestSource interface {
	// DeploymentExists reports whether a deployment of that id exists.
	DeploymentExists(id uint) bool
	// Live is a deployment's in-flight requests, at most limit listed.
	Live(deploymentID uint, limit int) uxrecord.LiveSnapshot
	// History is a deployment's finished requests, newest first.
	History(ctx context.Context, q HistoryQuery) ([]uxrecord.Request, string, error)
	// Grid sums one distribution grid over a time range; deployment 0 is
	// every deployment.
	Grid(ctx context.Context, deploymentID uint, kind string, fromMS, toMS int64) ([]uxrecord.Cell, error)
	// Coverage counts a range's requests: all, with usage, with a cache figure.
	Coverage(ctx context.Context, deploymentID uint, fromMS, toMS int64) (total, withUsage, withCache int64, err error)
	// Latency sums a deployment's latency breakdown over a time range.
	Latency(ctx context.Context, deploymentID uint, fromMS, toMS int64) (uxrecord.LatencySums, error)
	// KeyNames names API keys by id.
	KeyNames() map[uint]string
	// Lookup finds finished requests by trace id or request id across
	// deployments (lookup.go).
	Lookup(ctx context.Context, q LookupQuery) ([]uxrecord.Request, error)
	// DeploymentNames names deployments by id, live or deleted (by the name
	// a deleted one left); one neither knows is left out.
	DeploymentNames(ids []uint) map[uint]DeploymentName
}

// DeploymentName is what a request view shows for a deployment.
type DeploymentName struct {
	Name    string
	Deleted bool
}

// ErrBadHistoryQuery is what a RequestSource's History wraps when it refuses
// a query (a cursor it did not issue).
var ErrBadHistoryQuery = errors.New("monitor: bad history query")

// HistoryQuery selects a page of a deployment's finished requests.
type HistoryQuery struct {
	DeploymentID         uint
	FromMS, ToMS         int64
	APIKeyID             uint
	InstanceUUID         string
	PriorityClass        string
	Results              []uxrecord.Result
	MinEndMS, MaxEndMS   int32
	MinTTFTMS, MaxTTFTMS int32
	Limit                int
	Cursor               string
}

const (
	liveDefaultLimit = 50
	liveMaxLimit     = 500
)

// Requests serves GET /monitor/deployments/{id}/requests.
// A query the request store refuses (a cursor it did not issue) comes back
// as Deps.BadQuery.
func (s *Service) Requests(ctx context.Context, id uint, q dto.MonitorRequestsQuery) (dto.MonitorRequests, error) {
	src := s.deps.Requests
	if src == nil || !src.DeploymentExists(id) {
		return dto.MonitorRequests{}, s.deps.NotFound
	}
	now := s.deps.now()
	out := dto.MonitorRequests{View: q.View, AsOf: now.UnixMilli(), Requests: []dto.MonitorRequest{}}
	names := src.KeyNames()
	if q.View == "live" {
		limit := q.Limit
		if limit <= 0 {
			limit = liveDefaultLimit
		}
		limit = min(limit, liveMaxLimit)
		// A key or replica filter picks from every listed request, so ask for
		// the most.
		fetch := limit
		if q.APIKeyID != 0 || q.InstanceUUID != "" {
			fetch = liveMaxLimit
		}
		snap := src.Live(id, fetch)
		out.Counts = &dto.MonitorRequestCounts{Waiting: snap.Waiting, Thinking: snap.Thinking, Answering: snap.Answering}
		for _, live := range snap.Requests {
			if len(out.Requests) == limit {
				break
			}
			if q.APIKeyID != 0 && live.APIKeyID != q.APIKeyID {
				continue
			}
			if q.InstanceUUID != "" && live.InstanceUUID != q.InstanceUUID {
				continue
			}
			out.Requests = append(out.Requests, liveDTO(live, names))
		}
		return out, nil
	}
	from, to := now.UnixMilli()-q.RangeMS, now.UnixMilli()
	if q.FromMS > 0 || q.ToMS > 0 {
		from, to = q.FromMS, q.ToMS
	}
	results := make([]uxrecord.Result, 0, len(q.Results))
	for _, name := range q.Results {
		result, ok := uxrecord.ParseResult(name)
		if !ok {
			return dto.MonitorRequests{}, s.badQuery()
		}
		results = append(results, result)
	}
	records, next, err := src.History(ctx, HistoryQuery{
		DeploymentID: id, FromMS: from, ToMS: to,
		APIKeyID: q.APIKeyID, InstanceUUID: q.InstanceUUID, PriorityClass: q.PriorityClass, Results: results,
		MinEndMS: q.MinEndMS, MaxEndMS: q.MaxEndMS, MinTTFTMS: q.MinTTFTMS, MaxTTFTMS: q.MaxTTFTMS, Limit: q.Limit, Cursor: q.Cursor,
	})
	if errors.Is(err, ErrBadHistoryQuery) {
		return dto.MonitorRequests{}, s.badQuery()
	}
	if err != nil {
		return dto.MonitorRequests{}, err
	}
	for _, record := range records {
		out.Requests = append(out.Requests, recordDTO(record, names))
	}
	out.NextCursor = next
	out.FromMS, out.ToMS = from, to
	// The count table holds every request, sampled out of the log or not;
	// it is kept in 5-minute buckets, so the count covers the buckets the
	// range touches.
	total, _, _, err := src.Coverage(ctx, id, from, to)
	if err != nil {
		return dto.MonitorRequests{}, fmt.Errorf("history range total: %w", err)
	}
	out.RangeTotal = &total
	return out, nil
}

// badQuery is the error for a request query this service refuses: the
// API's sentinel when it gave one.
func (s *Service) badQuery() error {
	if s.deps.BadQuery != nil {
		return s.deps.BadQuery
	}
	return ErrBadHistoryQuery
}

func emptyDistributions(now time.Time, rng time.Duration) dto.MonitorDistributions {
	out := dto.MonitorDistributions{
		FromMS: now.Add(-rng).UnixMilli(), ToMS: now.UnixMilli(),
		TokenBuckets: make([]int64, uxrecord.TokenBuckets), TTFTBoundsMS: append([]int32(nil), uxrecord.TTFTBoundsMS...),
		InputOutput: []dto.MonitorGridCell{}, UncachedTTFT: []dto.MonitorGridCell{},
	}
	for b := range out.TokenBuckets {
		out.TokenBuckets[b] = uxrecord.TokenBucketLow(b)
	}
	return out
}

// Distributions serves GET /monitor/distributions.
func (s *Service) Distributions(ctx context.Context, deploymentID uint, rng time.Duration) (dto.MonitorDistributions, error) {
	src := s.deps.Requests
	out := emptyDistributions(s.deps.now(), rng)
	if src == nil {
		return out, nil
	}
	if deploymentID != 0 && !src.DeploymentExists(deploymentID) {
		return dto.MonitorDistributions{}, s.deps.NotFound
	}
	for kind, target := range map[string]*[]dto.MonitorGridCell{uxrecord.GridIO: &out.InputOutput, uxrecord.GridUncachedTTFT: &out.UncachedTTFT} {
		cells, err := src.Grid(ctx, deploymentID, kind, out.FromMS, out.ToMS)
		if err != nil {
			return dto.MonitorDistributions{}, fmt.Errorf("distribution %s: %w", kind, err)
		}
		for _, cell := range cells {
			*target = append(*target, dto.MonitorGridCell{X: cell.X, Y: cell.Y, N: cell.N})
		}
	}
	total, withUsage, withCache, err := src.Coverage(ctx, deploymentID, out.FromMS, out.ToMS)
	if err != nil {
		return dto.MonitorDistributions{}, fmt.Errorf("distribution coverage: %w", err)
	}
	out.Coverage = dto.MonitorDistributionCoverage{Total: total, WithUsage: withUsage, WithCache: withCache}
	return out, nil
}

func ms(v int32) *int32 {
	if v < 0 {
		return nil
	}
	return &v
}

func liveDTO(r uxrecord.Live, names map[uint]string) dto.MonitorRequest {
	age := r.AgeMS
	return dto.MonitorRequest{
		ID: r.ID, TraceID: r.TraceID.String(), DeploymentID: r.DeploymentID, ReceivedMS: r.ReceivedMS, Phase: r.Phase(), AgeMS: &age, InstanceUUID: r.InstanceUUID,
		APIKeyID: r.APIKeyID, APIKeyName: names[r.APIKeyID], UserID: r.UserID, Source: r.Source.String(),
		Stream: r.Stream, BodyBytes: r.BodyBytes, Images: r.Images,
		DispatchMS: ms(r.DispatchMS), FirstThinkMS: ms(r.FirstThinkMS), FirstAnswerMS: ms(r.FirstAnswerMS),
		LastChunkMS: ms(r.LastEventMS), Events: r.Events, ThinkEvents: r.ThinkEvents, Weight: 1,
		Segments: segmentsDTO(uxrecord.LiveSegments(r), r.Stream), // still waiting is the first-token stage so far
		TTFTMS:   ttftMS(r.Stream, r.FirstThinkMS, r.FirstAnswerMS),
		PD:       livePDDTO(r),
	}
}

// livePDDTO is an in-flight request's PD stage: the prefill instance, the
// stage's time once it is over.
func livePDDTO(r uxrecord.Live) *dto.MonitorRequestPD {
	if r.PDPrefillInstance == "" {
		return nil
	}
	out := &dto.MonitorRequestPD{PrefillInstanceUUID: r.PDPrefillInstance}
	if r.DispatchMS >= 0 && r.PDPrefillStartMS >= 0 {
		out.PrefillMS = ms(max(0, r.DispatchMS-r.PDPrefillStartMS))
	}
	return out
}

// pdDTOOf is a finished request's PD stage.
func pdDTOOf(pd *uxrecord.PDStage) *dto.MonitorRequestPD {
	if pd == nil {
		return nil
	}
	return &dto.MonitorRequestPD{
		PrefillInstanceUUID: pd.PrefillInstance, PrefillMS: ms(pd.PrefillMS),
		PrefillQueueMS: ms(pd.PrefillQueueMS), PrefillComputeMS: ms(pd.PrefillComputeMS), Fallback: pd.Fallback,
	}
}

// segmentsDTO labels each segment with its stage when the request is staged
// (see uxrecord.StageOf).
func segmentsDTO(segments []uxrecord.Segment, staged bool) []dto.MonitorRequestSegment {
	out := make([]dto.MonitorRequestSegment, len(segments))
	for i, s := range segments {
		out[i] = dto.MonitorRequestSegment{Kind: string(s.Kind), MS: s.MS, Stage: string(uxrecord.StageOf(s.Kind, staged))}
	}
	return out
}

// ttftMS is a streamed reply's time to its first token; a reply that does
// not stream has none to see.
func ttftMS(stream bool, firstThinkMS, firstAnswerMS int32) *int32 {
	if !stream {
		return nil
	}
	return ms(uxrecord.FirstTokenOf(firstThinkMS, firstAnswerMS))
}

// rate is a speed, or null when SpeedsOf could not work it out.
func rate(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	return &v
}

func recordDTO(r uxrecord.Request, names map[uint]string) dto.MonitorRequest {
	status := r.Status
	out := dto.MonitorRequest{
		ID: r.ID, TraceID: r.Trace.Trace.String(), DeploymentID: r.DeploymentID, ReceivedMS: r.ReceivedMS, Phase: "done", InstanceUUID: r.InstanceUUID,
		APIKeyID: r.APIKeyID, APIKeyName: names[r.APIKeyID], PriorityClass: r.PriorityClass, UserID: r.UserID, Source: r.Source.String(),
		Stream: r.Stream, BodyBytes: r.BodyBytes, Images: r.Images,
		DispatchMS: ms(r.DispatchMS), FirstByteMS: ms(r.FirstByteMS), FirstThinkMS: ms(r.FirstThinkMS),
		FirstAnswerMS: ms(r.FirstAnswerMS), LastChunkMS: ms(r.LastChunkMS), EndMS: ms(r.EndMS),
		Events: r.Events, ThinkEvents: r.ThinkEvents, MaxGapMS: ms(r.MaxGapMS),
		Status: &status, Outcome: r.Outcome.String(), FinishReason: r.FinishReason, Result: string(uxrecord.ResultOf(r)),
		EngineQueueMS: ms(r.EngineQueueMS), EnginePrefillMS: ms(r.EnginePrefillMS), EngineDecodeMS: ms(r.EngineDecodeMS),
		Weight: max(r.Weight, 1), Segments: segmentsDTO(uxrecord.SegmentsOf(r), r.Stream && r.FirstTokenMS() >= 0), PD: pdDTOOf(r.PD),
		TTFTMS: ttftMS(r.Stream, r.FirstThinkMS, r.FirstAnswerMS),
	}
	speeds := uxrecord.SpeedsOf(r)
	out.TPOTMS, out.PrefillTokS, out.DecodeTokS = rate(speeds.TPOTMS), rate(speeds.PrefillTokS), rate(speeds.DecodeTokS)
	if r.Usage {
		out.PromptTokens, out.CompletionTokens = ms(r.PromptTokens), ms(r.CompletionTokens)
		out.CachedTokens, out.ReasoningTokens = ms(r.CachedTokens), ms(r.ReasoningTokens)
	}
	return out
}
