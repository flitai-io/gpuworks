package metrics

import (
	"math"
	"sort"
	"sync"
	"time"
)

var Percentiles = []int{1, 5, 10, 25, 50, 75, 90, 95, 99}

// Request holds the observable facts of one HTTP attempt. Legacy duration
// fields remain for existing callers; new timing evidence uses event times.
type Request struct {
	SessionID                string
	Turn, PlannedTurns       int
	Canceled                 bool
	FinishReason, ErrorStage string
	HTTPStatus               int
	// PriorityClass is the request priority class the endpoint says the
	// request ran at (a gpuworks gateway's X-GPUWorks-Priority-Class); empty
	// when it says none.
	PriorityClass string

	StartedAt      time.Time
	FirstOutputAt  time.Time
	FirstContentAt time.Time
	LastOutputAt   time.Time
	FinishedAt     time.Time

	ServerInputTokens         *int
	ServerOutputTokens        *int
	LocalInputTokens          *int
	LocalOutputTokens         *int
	ReasoningTokens           *int
	InputSource, OutputSource string

	TTFT                  time.Duration
	TPOT                  time.Duration
	Latency               time.Duration
	ITL                   []time.Duration // generated-chunk intervals, not individual token intervals
	InputTokens           int
	OutputTokens          int
	CachedTokens          int
	CachedTokensAvailable bool
	Chunks                int
	ApproximateTokens     bool
	Error                 error
}

type Snapshot struct {
	AggregationError    string                `json:"aggregation_error,omitempty"`
	Count               int                   `json:"count"`
	Success             int                   `json:"success"`
	Errors              int                   `json:"errors"`
	InputTokens         int64                 `json:"input_tokens"`
	PeakInputTokens     int                   `json:"peak_input_tokens"`
	OutputTokens        int64                 `json:"output_tokens"`
	TTFT                map[int]time.Duration `json:"ttft"`
	TPOT                map[int]time.Duration `json:"tpot"`
	Latency             map[int]time.Duration `json:"latency"`
	ITL                 map[int]time.Duration `json:"itl"`
	OutputTput          float64               `json:"output_tput"`
	TotalTput           float64               `json:"total_tput"`
	RequestTput         float64               `json:"request_tput"`
	KVHitRate           *float64              `json:"kv_hit_rate,omitempty"`
	CacheReportCoverage *float64              `json:"cache_report_coverage,omitempty"`
	Speculative         *Speculative          `json:"speculative,omitempty"`
	ApproximateTokens   bool                  `json:"approximate_tokens"`
	WindowStart         time.Time             `json:"window_start,omitempty"`
	WindowEnd           time.Time             `json:"window_end,omitempty"`
}

type Speculative struct {
	Chunks             int     `json:"chunks"`
	Tokens             int     `json:"tokens"`
	TokensPerIteration float64 `json:"tokens_per_iteration"`
	AcceptanceRate     float64 `json:"acceptance_rate"`
}

// ProcessSample is the stable wire shape used for second-level progress curves.
// Durations are milliseconds so server and agent load origins emit identical
// TSDB values without depending on Go's time.Duration JSON representation.
type ProcessSample struct {
	OutputTput  float64 `json:"output_tput"`
	TotalTput   float64 `json:"total_tput"`
	RequestTput float64 `json:"request_tput"`
	TTFTP50     float64 `json:"ttft_p50"`
	TPOTP50     float64 `json:"tpot_p50"`
	LatencyP99  float64 `json:"latency_p99"`
	Success     int     `json:"success"`
	Errors      int     `json:"errors"`
}

func (s Snapshot) ProcessSample() ProcessSample {
	return ProcessSample{
		OutputTput: s.OutputTput, TotalTput: s.TotalTput, RequestTput: s.RequestTput,
		TTFTP50: durationMillis(s.TTFT[50]), TPOTP50: durationMillis(s.TPOT[50]),
		LatencyP99: durationMillis(s.Latency[99]),
		Success:    s.Success, Errors: s.Errors,
	}
}

// PeakThroughput contains aggregate token-rate peaks calculated over a rolling
// window. Output includes generated tokens only; Total includes input/prefill
// and generated tokens.
type PeakThroughput struct {
	Output float64
	Total  float64
}

type tokenPhase struct {
	start, end time.Time
	output     float64
	total      float64
}

// RollingPeakThroughput calculates the maximum aggregate token rate in any
// rolling window. Token work is prorated by overlap: input tokens occupy the
// request's prefill interval (start..TTFT), while output tokens occupy its
// decode interval (TTFT..latency). Failed requests contribute no tokens.
//
// If the observed point is shorter than window, the observed duration is used,
// which makes the peak equal to the full-point average. observedEnd should be
// captured immediately after Runner.Run returns.
func RollingPeakThroughput(requests []Request, observedEnd time.Time, window time.Duration) PeakThroughput {
	if window <= 0 {
		window = 10 * time.Second
	}
	var phases []tokenPhase
	var instants []tokenPhase
	var observedStart time.Time
	for _, r := range requests {
		if r.Error != nil || r.StartedAt.IsZero() {
			continue
		}
		end := r.StartedAt.Add(r.Latency)
		if end.Before(r.StartedAt) {
			end = r.StartedAt
		}
		if observedStart.IsZero() || r.StartedAt.Before(observedStart) {
			observedStart = r.StartedAt
		}
		prefillEnd := r.StartedAt.Add(r.TTFT)
		if prefillEnd.Before(r.StartedAt) {
			prefillEnd = r.StartedAt
		}
		if prefillEnd.After(end) {
			prefillEnd = end
		}
		appendPhase := func(start, finish time.Time, output, total float64) {
			phase := tokenPhase{start: start, end: finish, output: output, total: total}
			if finish.After(start) {
				phases = append(phases, phase)
			} else if output != 0 || total != 0 {
				instants = append(instants, phase)
			}
		}
		appendPhase(r.StartedAt, prefillEnd, 0, float64(r.InputTokens))
		appendPhase(prefillEnd, end, float64(r.OutputTokens), float64(r.OutputTokens))
	}
	if observedStart.IsZero() || !observedEnd.After(observedStart) {
		return PeakThroughput{}
	}
	duration := observedEnd.Sub(observedStart)
	if duration < window {
		window = duration
	}
	if window <= 0 {
		return PeakThroughput{}
	}
	lastStart := observedEnd.Add(-window)
	candidates := []time.Time{observedStart, lastStart}
	addCandidate := func(candidate time.Time) {
		if candidate.Before(observedStart) {
			candidate = observedStart
		}
		if candidate.After(lastStart) {
			candidate = lastStart
		}
		candidates = append(candidates, candidate)
	}
	for _, phase := range append(append([]tokenPhase(nil), phases...), instants...) {
		addCandidate(phase.start)
		addCandidate(phase.end)
		addCandidate(phase.start.Add(-window))
		addCandidate(phase.end.Add(-window))
	}
	var peak PeakThroughput
	windowSeconds := window.Seconds()
	for _, start := range candidates {
		end := start.Add(window)
		var outputTokens, totalTokens float64
		for _, phase := range phases {
			overlapStart := phase.start
			if start.After(overlapStart) {
				overlapStart = start
			}
			overlapEnd := phase.end
			if end.Before(overlapEnd) {
				overlapEnd = end
			}
			if !overlapEnd.After(overlapStart) {
				continue
			}
			fraction := float64(overlapEnd.Sub(overlapStart)) / float64(phase.end.Sub(phase.start))
			outputTokens += phase.output * fraction
			totalTokens += phase.total * fraction
		}
		for _, event := range instants {
			if !event.start.Before(start) && !event.start.After(end) {
				outputTokens += event.output
				totalTokens += event.total
			}
		}
		peak.Output = math.Max(peak.Output, outputTokens/windowSeconds)
		peak.Total = math.Max(peak.Total, totalTokens/windowSeconds)
	}
	return peak
}

type Accumulator struct {
	mu       sync.Mutex
	requests []Request
	window   time.Duration
}

func New(window time.Duration) *Accumulator {
	if window <= 0 {
		window = 10 * time.Second
	}
	return &Accumulator{window: window}
}
func (a *Accumulator) Add(r Request) { a.mu.Lock(); a.requests = append(a.requests, r); a.mu.Unlock() }
func (a *Accumulator) Requests() []Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Request(nil), a.requests...)
}
func (a *Accumulator) Snapshot(now time.Time) Snapshot {
	a.mu.Lock()
	rs := append([]Request(nil), a.requests...)
	a.mu.Unlock()
	return summarize(rs, now, time.Time{}, true)
}

// ProgressSnapshot is Snapshot without the ITL percentiles, which are the one
// statistic over every streamed chunk rather than every request: at a few
// thousand 1k-token requests that is millions of values to copy and sort, so
// a live progress sample must not pay for it. ProcessSample is unaffected.
func (a *Accumulator) ProgressSnapshot(now time.Time) Snapshot {
	a.mu.Lock()
	rs := append([]Request(nil), a.requests...)
	a.mu.Unlock()
	return summarize(rs, now, time.Time{}, false)
}
func (a *Accumulator) WindowSnapshot(now time.Time) Snapshot {
	a.mu.Lock()
	rs := append([]Request(nil), a.requests...)
	a.mu.Unlock()
	cutoff := now.Add(-a.window)
	return summarize(rs, now, cutoff, true)
}

func summarize(rs []Request, now, cutoff time.Time, withITL bool) Snapshot {
	s := Snapshot{TTFT: map[int]time.Duration{}, TPOT: map[int]time.Duration{}, Latency: map[int]time.Duration{}, ITL: map[int]time.Duration{}}
	var ttft, tpott, lat, itl []float64
	if withITL {
		n := 0
		for _, r := range rs {
			n += len(r.ITL)
		}
		itl = make([]float64, 0, n)
	}
	var in, out int64
	var cached, cacheTotal int64
	var cacheReports int
	var chunks, tokens, speculativeDenominator, speculativeNumerator int64
	inputValid, outputValid, cacheValid, speculativeValid := true, true, true, true
	add := func(total *int64, value int, valid *bool) {
		if *valid {
			*total, *valid = addTokenCount(*total, value)
		}
	}
	for _, r := range rs {
		if !r.StartedAt.IsZero() && !cutoff.IsZero() && r.StartedAt.Before(cutoff) {
			continue
		}
		s.Count++
		if r.Error != nil {
			s.Errors++
			continue
		}
		s.Success++
		add(&in, r.InputTokens, &inputValid)
		if r.InputTokens > s.PeakInputTokens {
			s.PeakInputTokens = r.InputTokens
		}
		add(&out, r.OutputTokens, &outputValid)
		if r.CachedTokensAvailable {
			cacheReports++
			if r.ServerInputTokens != nil {
				add(&cacheTotal, *r.ServerInputTokens, &cacheValid)
				add(&cached, r.CachedTokens, &cacheValid)
			} else if r.InputSource == "" && !r.ApproximateTokens { // legacy in-memory callers
				add(&cacheTotal, r.InputTokens, &cacheValid)
				add(&cached, r.CachedTokens, &cacheValid)
			}
		}
		if r.ApproximateTokens {
			s.ApproximateTokens = true
		}
		if r.InputSource == "" || !r.FirstOutputAt.IsZero() {
			ttft = append(ttft, float64(r.TTFT))
		}
		lat = append(lat, float64(r.Latency))
		if perToken, ok := r.TimePerOutputToken(); ok {
			tpott = append(tpott, float64(perToken))
		}
		if withITL {
			for _, d := range r.ITL {
				itl = append(itl, float64(d))
			}
		}
		add(&chunks, r.Chunks, &speculativeValid)
		add(&tokens, r.OutputTokens, &speculativeValid)
		if r.Chunks > 1 && r.OutputTokens > 1 {
			add(&speculativeDenominator, r.Chunks-1, &speculativeValid)
			add(&speculativeNumerator, r.OutputTokens-1, &speculativeValid)
		}
	}
	s.InputTokens = in
	s.OutputTokens = out
	if !inputValid || !outputValid {
		s.AggregationError = "token totals exceed the supported integer range; aggregate throughput is unavailable"
	}
	for _, p := range Percentiles {
		s.TTFT[p] = durationQuantile(ttft, p)
		s.TPOT[p] = durationQuantile(tpott, p)
		s.Latency[p] = durationQuantile(lat, p)
		s.ITL[p] = durationQuantile(itl, p)
	}
	elapsed := elapsedSeconds(rs, now, cutoff)
	if elapsed > 0 {
		s.RequestTput = float64(s.Success) / elapsed
		if s.AggregationError == "" {
			s.OutputTput = float64(out) / elapsed
			s.TotalTput = (float64(in) + float64(out)) / elapsed
		}
	}
	if cacheValid && cacheTotal > 0 {
		v := float64(cached) / float64(cacheTotal)
		s.KVHitRate = &v
	}
	if s.Success > 0 {
		coverage := float64(cacheReports) / float64(s.Success)
		s.CacheReportCoverage = &coverage
	}
	if speculativeValid && chunks <= int64(^uint(0)>>1) && tokens <= int64(^uint(0)>>1) && speculativeDenominator > 0 && speculativeNumerator > 0 {
		l := float64(speculativeNumerator) / float64(speculativeDenominator)
		s.Speculative = &Speculative{Chunks: int(chunks), Tokens: int(tokens), TokensPerIteration: l, AcceptanceRate: 1 - 1/l}
	}
	if !cutoff.IsZero() {
		s.WindowStart = cutoff
		s.WindowEnd = now
	}
	return s
}

// TimePerOutputToken is the request's decode-phase time per generated token,
// (latency - TTFT) / (output tokens - 1). It exists only for requests whose
// stream delivered at least two content chunks and more than one token: a
// single-chunk or non-streamed response has no observable decode interval, and
// counting it as zero would drag every TPOT statistic toward zero.
func (r Request) TimePerOutputToken() (time.Duration, bool) {
	if r.Error != nil || r.OutputTokens <= 1 || r.TPOT <= 0 {
		return 0, false
	}
	return r.TPOT / time.Duration(r.OutputTokens-1), true
}

// Interactivity is the per-user output speed in tokens per second per user:
// the rate one streaming request receives tokens once decoding has started,
// 1000 / TPOT(ms). It excludes TTFT by definition (InferenceMAX
// "interactivity", AIPerf "output token throughput per user"). Applied to a
// TPOT percentile it yields the mirrored speed percentile: the P50 TPOT gives
// the P50 speed. A missing TPOT yields 0.
func Interactivity(tpotMillis float64) float64 {
	if tpotMillis <= 0 || math.IsNaN(tpotMillis) || math.IsInf(tpotMillis, 0) {
		return 0
	}
	return 1000 / tpotMillis
}

// RequestAverages are per-request means in milliseconds. TPOT averages only the
// requests that have a TPOT (see Request.TimePerOutputToken), so short or
// single-chunk responses cannot dilute it.
type RequestAverages struct {
	TTFT, TPOT, Latency float64
}

func AverageRequestMetrics(rs []Request) RequestAverages {
	var ttft, tpot, latency float64
	var ttftN, latencyN, tpotN int
	for _, r := range rs {
		if r.Error != nil {
			continue
		}
		if r.InputSource == "" || !r.FirstOutputAt.IsZero() {
			ttft += durationMillis(r.TTFT)
			ttftN++
		}
		latency += durationMillis(r.Latency)
		latencyN++
		if perToken, ok := r.TimePerOutputToken(); ok {
			tpot += durationMillis(perToken)
			tpotN++
		}
	}
	var out RequestAverages
	if ttftN > 0 {
		out.TTFT = ttft / float64(ttftN)
	}
	if latencyN > 0 {
		out.Latency = latency / float64(latencyN)
	}
	if tpotN > 0 {
		out.TPOT = tpot / float64(tpotN)
	}
	return out
}

// AverageTokenLengths returns the mean input and output tokens per successful
// request.
func (s Snapshot) AverageTokenLengths() (float64, float64) {
	if s.Success <= 0 {
		return 0, 0
	}
	return float64(s.InputTokens) / float64(s.Success), float64(s.OutputTokens) / float64(s.Success)
}

func durationMillis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// DurationMillis converts a duration to fractional milliseconds, the unit of
// every persisted benchmark latency.
func DurationMillis(d time.Duration) float64 { return durationMillis(d) }
func elapsedSeconds(rs []Request, now, cutoff time.Time) float64 {
	var min, max time.Time
	hasObservedFinish := false
	for _, r := range rs {
		if r.StartedAt.IsZero() || (!cutoff.IsZero() && r.StartedAt.Before(cutoff)) {
			continue
		}
		if min.IsZero() || r.StartedAt.Before(min) {
			min = r.StartedAt
		}
		if !r.FinishedAt.IsZero() {
			hasObservedFinish = true
		}
		end := requestFinish(r)
		if end.After(max) {
			max = end
		}
	}
	if min.IsZero() {
		return 0
	}
	if max.Before(now) && !hasObservedFinish {
		max = now
	}
	return max.Sub(min).Seconds()
}
func durationQuantile(v []float64, p int) time.Duration {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	idx := int(math.Ceil(float64(p)/100*float64(len(v)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(v) {
		idx = len(v) - 1
	}
	return time.Duration(v[idx])
}
