package monitorapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// fakeRequests is a RequestSource that records what it was asked and can be
// told to fail each call.
type fakeRequests struct {
	deployments map[uint]bool
	live        uxrecord.LiveSnapshot
	liveLimits  []int
	history     []uxrecord.Request
	next        string
	historyErr  error
	historyArgs []HistoryQuery
	grids       map[string][]uxrecord.Cell
	gridErr     error
	gridArgs    []string
	coverage    [3]int64
	coverageErr error
	names       map[uint]string
	latency     uxrecord.LatencySums
	latencyErr  error
	latencyArgs []string
	// lookups answers a lookup by window index; lookupArgs records them.
	lookups    [][]uxrecord.Request
	lookupErr  error
	lookupArgs []LookupQuery
	depNames   map[uint]DeploymentName
}

func (f *fakeRequests) Lookup(_ context.Context, q LookupQuery) ([]uxrecord.Request, error) {
	f.lookupArgs = append(f.lookupArgs, q)
	var out []uxrecord.Request
	if i := len(f.lookupArgs) - 1; i < len(f.lookups) {
		out = f.lookups[i]
	}
	return out, f.lookupErr
}

func (f *fakeRequests) DeploymentNames(ids []uint) map[uint]DeploymentName {
	out := map[uint]DeploymentName{}
	for _, id := range ids {
		if name, ok := f.depNames[id]; ok {
			out[id] = name
		}
	}
	return out
}

func (f *fakeRequests) DeploymentExists(id uint) bool { return f.deployments[id] }

func (f *fakeRequests) Live(id uint, limit int) uxrecord.LiveSnapshot {
	f.liveLimits = append(f.liveLimits, limit)
	out := f.live
	if len(out.Requests) > limit {
		out.Requests = out.Requests[:limit]
	}
	return out
}

func (f *fakeRequests) History(_ context.Context, q HistoryQuery) ([]uxrecord.Request, string, error) {
	f.historyArgs = append(f.historyArgs, q)
	return f.history, f.next, f.historyErr
}

func (f *fakeRequests) Grid(_ context.Context, id uint, kind string, fromMS, toMS int64) ([]uxrecord.Cell, error) {
	f.gridArgs = append(f.gridArgs, fmt.Sprintf("%d/%s/%d", id, kind, toMS-fromMS))
	return f.grids[kind], f.gridErr
}

func (f *fakeRequests) Latency(_ context.Context, id uint, fromMS, toMS int64) (uxrecord.LatencySums, error) {
	f.latencyArgs = append(f.latencyArgs, fmt.Sprintf("%d/%d", id, toMS-fromMS))
	return f.latency, f.latencyErr
}

func (f *fakeRequests) Coverage(context.Context, uint, int64, int64) (int64, int64, int64, error) {
	return f.coverage[0], f.coverage[1], f.coverage[2], f.coverageErr
}

func (f *fakeRequests) KeyNames() map[uint]string { return f.names }

var (
	errRequestsNotFound = errors.New("not found")
	errBadQuery         = errors.New("bad query")
	requestsNow         = time.UnixMilli(1_800_000_000_000)
)

func requestsService(src RequestSource) *Service {
	return New(Deps{Requests: src, NotFound: errRequestsNotFound, BadQuery: errBadQuery, Now: func() time.Time { return requestsNow }})
}

func liveRequest(id string, key uint, dispatch, think, answer int32) uxrecord.Live {
	return uxrecord.Live{ID: id, DeploymentID: 7, APIKeyID: key, ReceivedMS: requestsNow.UnixMilli() - 900, AgeMS: 900,
		DispatchMS: dispatch, FirstThinkMS: think, FirstAnswerMS: answer, LastEventMS: uxrecord.Unknown}
}

func TestRequestsLiveListsPhasesAndFiltersByKey(t *testing.T) {
	src := &fakeRequests{
		deployments: map[uint]bool{7: true},
		live: uxrecord.LiveSnapshot{Waiting: 5, Thinking: 1, Answering: 1, Requests: []uxrecord.Live{
			liveRequest("w", 1, 3, uxrecord.Unknown, uxrecord.Unknown),
			liveRequest("t", 2, 3, 40, uxrecord.Unknown),
			liveRequest("a", 1, 3, uxrecord.Unknown, 60),
		}},
		names: map[uint]string{1: "chat-app"},
	}
	svc := requestsService(src)
	out, err := svc.Requests(context.Background(), 7, dto.MonitorRequestsQuery{View: "live"})
	if err != nil {
		t.Fatal(err)
	}
	if out.AsOf != requestsNow.UnixMilli() || out.Counts == nil || *out.Counts != (dto.MonitorRequestCounts{Waiting: 5, Thinking: 1, Answering: 1}) {
		t.Fatalf("live = %+v", out)
	}
	if len(out.Requests) != 3 || src.liveLimits[0] != liveDefaultLimit {
		t.Fatalf("requests = %+v, limits = %v", out.Requests, src.liveLimits)
	}
	phases := []string{out.Requests[0].Phase, out.Requests[1].Phase, out.Requests[2].Phase}
	if fmt.Sprint(phases) != "[waiting thinking answering]" {
		t.Fatalf("phases = %v", phases)
	}
	first := out.Requests[0]
	if first.APIKeyName != "chat-app" || first.Source != "api" || first.AgeMS == nil || *first.AgeMS != 900 ||
		first.FirstThinkMS != nil || first.LastChunkMS != nil || first.Status != nil || first.Weight != 1 {
		t.Fatalf("waiting request = %+v", first)
	}

	// A key filter asks for every listed request and keeps only that key's.
	out, err = svc.Requests(context.Background(), 7, dto.MonitorRequestsQuery{View: "live", APIKeyID: 1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if src.liveLimits[1] != liveMaxLimit || len(out.Requests) != 1 || out.Requests[0].ID != "w" || out.Counts.Waiting != 5 {
		t.Fatalf("filtered = %+v, limits = %v", out.Requests, src.liveLimits)
	}

	// The limit is capped.
	if _, err := svc.Requests(context.Background(), 7, dto.MonitorRequestsQuery{View: "live", Limit: 10_000}); err != nil {
		t.Fatal(err)
	}
	if src.liveLimits[2] != liveMaxLimit {
		t.Fatalf("limits = %v", src.liveLimits)
	}
}

func TestRequestsHistoryMapsRecords(t *testing.T) {
	record := uxrecord.Request{
		ID: "r1", ReceivedMS: 100, DeploymentID: 7, APIKeyID: 2, Source: uxrecord.SourcePlayground, Stream: true,
		DispatchMS: 2, FirstByteMS: 30, FirstThinkMS: 31, FirstAnswerMS: 400, LastChunkMS: 900, EndMS: 901,
		Events: 50, ThinkEvents: 20, MaxGapMS: 80, Status: 200, Outcome: uxrecord.OutcomeClientClosed, FinishReason: "stop",
		Usage: true, PromptTokens: 1000, CompletionTokens: 200, CachedTokens: uxrecord.Unknown, ReasoningTokens: 90,
		EngineQueueMS: 5, EnginePrefillMS: uxrecord.Unknown, EngineDecodeMS: 800, Weight: 4,
	}
	noUsage := uxrecord.Request{ID: "r2", Usage: false, PromptTokens: 0, CompletionTokens: 0, CachedTokens: uxrecord.Unknown,
		ReasoningTokens: uxrecord.Unknown, DispatchMS: uxrecord.Unknown, FirstByteMS: uxrecord.Unknown, FirstThinkMS: uxrecord.Unknown,
		FirstAnswerMS: uxrecord.Unknown, LastChunkMS: uxrecord.Unknown, EndMS: 3, MaxGapMS: 0, Status: 502,
		Outcome: uxrecord.OutcomeUpstreamError, EngineQueueMS: uxrecord.Unknown, EnginePrefillMS: uxrecord.Unknown, EngineDecodeMS: uxrecord.Unknown}
	src := &fakeRequests{deployments: map[uint]bool{7: true}, history: []uxrecord.Request{record, noUsage}, next: "more", names: map[uint]string{}, coverage: [3]int64{1234, 0, 0}}
	out, err := requestsService(src).Requests(context.Background(), 7,
		dto.MonitorRequestsQuery{View: "history", RangeMS: 3_600_000, Limit: 20, Cursor: "c", APIKeyID: 2,
			InstanceUUID: "inst-a", Results: []string{"client_closed", "error"}, MinEndMS: 500, MaxEndMS: 9000, MinTTFTMS: 20, MaxTTFTMS: 3000})
	if err != nil {
		t.Fatal(err)
	}
	wantQ := HistoryQuery{DeploymentID: 7, FromMS: requestsNow.UnixMilli() - 3_600_000, ToMS: requestsNow.UnixMilli(),
		APIKeyID: 2, InstanceUUID: "inst-a", Results: []uxrecord.Result{uxrecord.ResultClientClosed, uxrecord.ResultError},
		MinEndMS: 500, MaxEndMS: 9000, MinTTFTMS: 20, MaxTTFTMS: 3000, Limit: 20, Cursor: "c"}
	if len(src.historyArgs) != 1 || !reflect.DeepEqual(src.historyArgs[0], wantQ) {
		t.Fatalf("history query = %+v, want %+v", src.historyArgs, wantQ)
	}
	if out.Counts != nil || out.NextCursor != "more" || len(out.Requests) != 2 {
		t.Fatalf("history = %+v", out)
	}
	if out.FromMS != wantQ.FromMS || out.ToMS != wantQ.ToMS || out.RangeTotal == nil || *out.RangeTotal != 1234 {
		t.Fatalf("range = %d..%d total %v, want the applied range and the count table's 1234", out.FromMS, out.ToMS, out.RangeTotal)
	}
	if out.Requests[0].Result != "client_closed" || out.Requests[1].Result != "error" {
		t.Fatalf("results = %q %q", out.Requests[0].Result, out.Requests[1].Result)
	}
	// A queue-only report splits off its known queue; the remainder stays
	// waiting. The segments up to the first token add up to the TTFT.
	if fmt.Sprint(out.Requests[0].Segments) != "[{gateway 2 first_token} {queue 5 first_token} {waiting 24 first_token} {thinking 369 decode} {answering 500 decode} {tail 1 tail}]" {
		t.Fatalf("segments = %v", out.Requests[0].Segments)
	}
	// TTFT is the first reasoning; decode is the engine's: 200 tokens in
	// 800 ms. Without a cache figure the prefill speed is unknown.
	if r := out.Requests[0]; r.TTFTMS == nil || *r.TTFTMS != 31 || r.TPOTMS == nil || *r.TPOTMS != 4 || r.DecodeTokS == nil || *r.DecodeTokS != 250 || r.PrefillTokS != nil {
		t.Fatalf("ttft %v tpot %v decode %v prefill %v", r.TTFTMS, r.TPOTMS, r.DecodeTokS, r.PrefillTokS)
	}
	// A reply that does not stream has no TTFT, and its segments no stage.
	if r := out.Requests[1]; r.TTFTMS != nil || r.TPOTMS != nil || (len(r.Segments) > 0 && r.Segments[0].Stage != "") {
		t.Fatalf("bare ttft %v tpot %v segments %v", r.TTFTMS, r.TPOTMS, r.Segments)
	}

	// An explicit range replaces the one ending now; an unknown result is a
	// bad query.
	if _, err := requestsService(src).Requests(context.Background(), 7, dto.MonitorRequestsQuery{View: "history", RangeMS: 3_600_000, FromMS: 1000, ToMS: 2000}); err != nil {
		t.Fatal(err)
	}
	if q := src.historyArgs[1]; q.FromMS != 1000 || q.ToMS != 2000 {
		t.Fatalf("explicit range = %d..%d", q.FromMS, q.ToMS)
	}
	if _, err := requestsService(src).Requests(context.Background(), 7, dto.MonitorRequestsQuery{View: "history", Results: []string{"slow"}}); err != errBadQuery {
		t.Fatalf("unknown result: %v", err)
	}
	got := out.Requests[0]
	if got.Phase != "done" || got.Source != "playground" || got.Outcome != "client_closed" || *got.Status != 200 ||
		*got.PromptTokens != 1000 || got.CachedTokens != nil || *got.ReasoningTokens != 90 ||
		*got.EngineQueueMS != 5 || got.EnginePrefillMS != nil || *got.EngineDecodeMS != 800 || got.Weight != 4 || got.AgeMS != nil {
		t.Fatalf("record = %+v", got)
	}
	bare := out.Requests[1]
	if bare.PromptTokens != nil || bare.CompletionTokens != nil || bare.DispatchMS != nil || *bare.EndMS != 3 ||
		bare.Outcome != "upstream_error" || bare.Weight != 1 || *bare.MaxGapMS != 0 {
		t.Fatalf("usage-less record = %+v", bare)
	}
}

// A stream that ended without a token, an upstream error say, has no first
// token to split at: no TTFT, and its segments sit under no stage, even when
// the engine reported its queue, prefill and decode.
func TestRequestsHistoryStreamWithoutTokenHasNoStages(t *testing.T) {
	for _, r := range []uxrecord.Request{
		{ID: "err", Stream: true, DispatchMS: 1, FirstByteMS: uxrecord.Unknown, FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: uxrecord.Unknown,
			LastChunkMS: uxrecord.Unknown, EndMS: 3000, Status: 502, Outcome: uxrecord.OutcomeUpstreamError, CachedTokens: uxrecord.Unknown,
			ReasoningTokens: uxrecord.Unknown, EngineQueueMS: uxrecord.Unknown, EnginePrefillMS: uxrecord.Unknown, EngineDecodeMS: uxrecord.Unknown},
		{ID: "empty", Stream: true, DispatchMS: 1, FirstByteMS: 20, FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: uxrecord.Unknown,
			LastChunkMS: 900, EndMS: 950, Events: 3, Status: 200, FinishReason: "stop", Usage: true, PromptTokens: 100, CompletionTokens: 5,
			CachedTokens: 0, ReasoningTokens: uxrecord.Unknown, EngineQueueMS: 10, EnginePrefillMS: 40, EngineDecodeMS: 800},
	} {
		src := &fakeRequests{deployments: map[uint]bool{7: true}, history: []uxrecord.Request{r}, names: map[uint]string{}}
		out, err := requestsService(src).Requests(context.Background(), 7, dto.MonitorRequestsQuery{View: "history", RangeMS: 3_600_000, Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		got := out.Requests[0]
		if got.TTFTMS != nil || len(got.Segments) == 0 {
			t.Fatalf("%s: ttft %v segments %v", r.ID, got.TTFTMS, got.Segments)
		}
		for _, s := range got.Segments {
			if s.Stage != "" {
				t.Fatalf("%s: segments %v, want no stage", r.ID, got.Segments)
			}
		}
	}
}

func TestRequestsErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := requestsService(nil).Requests(ctx, 7, dto.MonitorRequestsQuery{View: "live"}); err != errRequestsNotFound {
		t.Fatalf("no source: %v", err)
	}
	src := &fakeRequests{deployments: map[uint]bool{7: true}}
	if _, err := requestsService(src).Requests(ctx, 8, dto.MonitorRequestsQuery{View: "history"}); err != errRequestsNotFound {
		t.Fatalf("unknown deployment: %v", err)
	}
	src.historyErr = fmt.Errorf("%w: cursor", ErrBadHistoryQuery)
	if _, err := requestsService(src).Requests(ctx, 7, dto.MonitorRequestsQuery{View: "history"}); err != errBadQuery {
		t.Fatalf("bad cursor: %v", err)
	}
	disk := errors.New("disk")
	src.historyErr = disk
	if _, err := requestsService(src).Requests(ctx, 7, dto.MonitorRequestsQuery{View: "history"}); !errors.Is(err, disk) {
		t.Fatalf("store failure: %v", err)
	}
}

func TestDistributions(t *testing.T) {
	ctx := context.Background()
	src := &fakeRequests{
		deployments: map[uint]bool{7: true},
		grids: map[string][]uxrecord.Cell{
			uxrecord.GridIO:           {{X: 9, Y: 7, N: 12}},
			uxrecord.GridUncachedTTFT: {{X: 4, Y: 2, N: 3}, {X: 5, Y: 2, N: 1}},
		},
		coverage: [3]int64{20, 12, 4},
	}
	out, err := requestsService(src).Distributions(ctx, 7, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if out.ToMS != requestsNow.UnixMilli() || out.ToMS-out.FromMS != (6*time.Hour).Milliseconds() {
		t.Fatalf("window = %d..%d", out.FromMS, out.ToMS)
	}
	if len(out.TokenBuckets) != uxrecord.TokenBuckets || out.TokenBuckets[0] != 0 || out.TokenBuckets[1] != 2 ||
		len(out.TTFTBoundsMS) != len(uxrecord.TTFTBoundsMS) {
		t.Fatalf("bounds = %v %v", out.TokenBuckets, out.TTFTBoundsMS)
	}
	if fmt.Sprint(out.InputOutput) != "[{9 7 12}]" || fmt.Sprint(out.UncachedTTFT) != "[{4 2 3} {5 2 1}]" {
		t.Fatalf("grids = %v %v", out.InputOutput, out.UncachedTTFT)
	}
	if out.Coverage != (dto.MonitorDistributionCoverage{Total: 20, WithUsage: 12, WithCache: 4}) {
		t.Fatalf("coverage = %+v", out.Coverage)
	}

	// Every deployment needs no existence check.
	if _, err := requestsService(src).Distributions(ctx, 0, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := requestsService(src).Distributions(ctx, 8, time.Hour); err != errRequestsNotFound {
		t.Fatalf("unknown deployment: %v", err)
	}
	src.gridErr = errors.New("grid")
	if _, err := requestsService(src).Distributions(ctx, 7, time.Hour); err == nil {
		t.Fatal("grid failure ignored")
	}
	src.gridErr, src.coverageErr = nil, errors.New("coverage")
	if _, err := requestsService(src).Distributions(ctx, 7, time.Hour); err == nil {
		t.Fatal("coverage failure ignored")
	}

	// Without a request source the grids are empty, never null.
	empty, err := requestsService(nil).Distributions(ctx, 7, time.Hour)
	if err != nil || empty.InputOutput == nil || empty.UncachedTTFT == nil || len(empty.TokenBuckets) == 0 {
		t.Fatalf("empty = %+v, %v", empty, err)
	}
}

// The breakdown's means come from the sums over every whole reply, so they
// add up to the end-to-end mean; percentiles come from the histograms of
// the replies that had each segment.
func TestLatencyBreakdown(t *testing.T) {
	sums := uxrecord.LatencySums{
		Requests: 4, EndToEndMS: 4000,
		SegmentMS: map[uxrecord.SegmentKind]float64{
			uxrecord.SegmentGateway: 200, uxrecord.SegmentWaiting: 800, uxrecord.SegmentThinking: 1000, uxrecord.SegmentAnswering: 2000,
		},
		ThinkingTokens: 100, ThinkingMS: 1000, DecodeTokens: 300, DecodeMS: 3000,
	}
	row := func(name string) int { return uxrecord.LatencyRow(name) }
	bin := uxrecord.LatencyBin
	src := &fakeRequests{deployments: map[uint]bool{7: true}, latency: sums, grids: map[string][]uxrecord.Cell{
		uxrecord.GridSegments: {
			{X: row("e2e"), Y: bin(800), N: 3}, {X: row("e2e"), Y: bin(1600), N: 1},
			{X: row("thinking"), Y: bin(500), N: 2},
			{X: row("gateway"), Y: bin(50), N: 4}, {X: row("waiting"), Y: bin(200), N: 4}, {X: row("answering"), Y: bin(500), N: 4},
		},
	}}
	out, err := requestsService(src).Latency(context.Background(), 7, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if out.Requests != 4 || out.EndToEnd == nil || out.EndToEnd.MeanMS != 1000 {
		t.Fatalf("e2e = %+v", out)
	}
	near := func(got, want float64) bool { return got > want*0.9 && got < want*1.1 }
	if !near(out.EndToEnd.P50MS, 800) || !near(out.EndToEnd.P95MS, 1600) {
		t.Errorf("e2e p50 %v p95 %v, want about 800 and 1600", out.EndToEnd.P50MS, out.EndToEnd.P95MS)
	}
	var kinds []string
	mean := 0.0
	for _, s := range out.Segments {
		kinds = append(kinds, s.Kind)
		mean += s.MeanMS
	}
	if fmt.Sprint(kinds) != "[gateway waiting thinking answering]" || mean != out.EndToEnd.MeanMS {
		t.Fatalf("segments %v add up to %v", kinds, mean)
	}
	thinking := out.Segments[2]
	if thinking.MeanMS != 250 || thinking.Requests != 2 || !near(thinking.P50MS, 500) {
		t.Errorf("thinking = %+v: mean over every reply, percentiles over the two that thought", thinking)
	}
	if out.Speeds.ThinkingTokS == nil || *out.Speeds.ThinkingTokS != 100 || *out.Speeds.DecodeTokS != 100 ||
		out.Speeds.PrefillTokS != nil || out.Speeds.AnsweringTokS != nil {
		t.Errorf("speeds = %+v", out.Speeds)
	}
	if src.latencyArgs[0] != "7/3600000" || src.gridArgs[0] != "7/seg/3600000" {
		t.Errorf("queries = %v %v", src.latencyArgs, src.gridArgs)
	}

	// No whole reply yet: no end-to-end, no segments.
	empty := &fakeRequests{deployments: map[uint]bool{7: true}}
	if out, err := requestsService(empty).Latency(context.Background(), 7, time.Hour); err != nil || out.EndToEnd != nil || len(out.Segments) != 0 {
		t.Fatalf("empty = %+v %v", out, err)
	}
	if _, err := requestsService(src).Latency(context.Background(), 8, time.Hour); err != errRequestsNotFound {
		t.Fatalf("unknown deployment: %v", err)
	}
}

func TestRequestQueueTimingWirePreservesUnknownZeroAndMeasured(t *testing.T) {
	for _, queue := range []int32{uxrecord.Unknown, 0, 20300} {
		record := uxrecord.Request{EngineQueueMS: queue, EnginePrefillMS: uxrecord.Unknown, EngineDecodeMS: uxrecord.Unknown}
		body, err := json.Marshal(recordDTO(record, nil))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprint(queue)
		if queue == uxrecord.Unknown {
			want = "null"
		}
		if string(fields["engine_queue_ms"]) != want || string(fields["engine_prefill_ms"]) != "null" {
			t.Fatalf("queue %d: wire queue=%s prefill=%s", queue, fields["engine_queue_ms"], fields["engine_prefill_ms"])
		}
	}
}
