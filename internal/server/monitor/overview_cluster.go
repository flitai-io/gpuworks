package monitor

import (
	"context"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// DiagnosisWindow is the fixed lookback ComputeWindow uses for diagnosis and
// triage, independent of the chart range rng a caller asks BuildCluster or
// BuildService for. Diagnoses answer "is it broken right now"; charts answer
// "what did the last N hours look like" - the two questions have different
// natural windows, and plan §5's rules are all specified in terms of a
// trailing few minutes regardless of what range the user has the page set
// to.
const DiagnosisWindow = 5 * time.Minute

// staleAfter mirrors the plan §6 API contract: a snapshot whose newest
// sample is older than this counts as stale (the page shows it, dimmed,
// rather than silently going quiet).
const staleAfter = 30 * time.Second

// StaleAt reports whether a snapshot whose newest sample is from asOf (zero:
// it has none) counts as stale at now. A snapshot served from the cache after
// it was built is judged again at the time it is served, not when it was
// built.
func StaleAt(asOf, now time.Time) bool {
	return asOf.IsZero() || now.Sub(asOf) > staleAfter
}

// ClusterSnapshot is the /monitor cluster-layer view (plan §3.1).
type ClusterSnapshot struct {
	AsOf    time.Time
	Stale   bool
	Signals ClusterSignals

	// Services is sorted by name; monitorapi orders it by health.
	Services []ServiceRow

	VRAMByNode []VRAMNode

	// ClusterTokenRate is the cluster-wide generation-token throughput over
	// rng, one point per bucket.
	ClusterTokenRate []tsdb.Point

	// Events is a deliberately empty hook: plan §3.1's capacity-changing
	// event markers on the cluster tok/s chart (instance entering
	// running/failed/stopped, sleep/wake, node online/offline) need the
	// instance_events table, which Inventory/Deps do not carry.
	Events []EventMarker
}

// ClusterSignals is the cluster-layer "四组八格" signal row. Every field
// below is a fleet-wide reduction of the per-deployment Window values
// BuildCluster already computed (a request-volume-weighted average for the
// three error-rate fractions, a fleet-wide sum for the two rate signals, a
// max across deployments for TTFT/TPOT) - not a second, separate query.
type ClusterSignals struct {
	ReadyReplicas    int
	DesiredReplicas  int
	NotReadyServices []string

	HasWaiting bool
	WaitingP95 float64
	// WaitingSeries is the same cluster-wide combined queue-depth series
	// WaitingP95 was reduced from (clusterWaitingP95's own query, DiagnosisWindow
	// at WindowStep buckets), kept so a caller can draw the waiting signal's
	// sparkline without re-querying tsdb for data already fetched here.
	WaitingSeries []tsdb.Point

	HasTokens          bool
	InputTokensPerSec  float64
	OutputTokensPerSec float64
	// PromptTokensPerSecSeries is the cluster-wide combined prompt-token
	// rate, DiagnosisWindow-at-WindowStep bucketed - the token_rate
	// signal's own sparkline source, from the same fleet series BuildCluster
	// already fetched (no separate query the way the wire's cluster_tokens
	// chart's own, rng-scoped series needs one).
	PromptTokensPerSecSeries []tsdb.Point

	// HasErrRate/ErrRate*Pct mirror Window's ErrRate5xx/429/Asleep, rolled
	// up fleet-wide as a request-volume-weighted average (a deployment with
	// ten times the traffic of another should not move the fleet number by
	// the same amount as one with almost none) and expressed as percents
	// (0..100), matching the wire contract's error_rate signal detail.
	// ErrRateTotalPct is the sum of the three - "total error rate" plan
	// §3.1 shows as the signal's own headline value.
	HasErrRate       bool
	ErrRate5xxPct    float64
	ErrRate429Pct    float64
	ErrRateAsleepPct float64
	ErrRateTotalPct  float64
	// ErrorRateSeries is the cluster-wide combined total-error-rate percent
	// (5xx+429+asleep_503 share of all proxy traffic), DiagnosisWindow-at-
	// WindowStep bucketed, computed directly from the fleet-wide proxy
	// series already fetched - not the same reduction as ErrRateTotalPct
	// (a weighted average of each deployment's own rate): this is the true
	// fleet-wide combined ratio at each bucket, the same "combine first,
	// reduce second" shape WaitingP95/WaitingSeries already use.
	ErrorRateSeries []tsdb.Point

	HasRequestRate bool
	RequestsPerSec float64
	// RequestsPerSecSeries is the cluster-wide combined proxy request rate
	// (every class summed), DiagnosisWindow-at-WindowStep bucketed, from the
	// same fleet-wide proxy series - no separate query.
	RequestsPerSecSeries []tsdb.Point
	// HasInflight/InflightAvg are the gateway's requests in flight across
	// the fleet, averaged over DiagnosisWindow: the sum of every
	// deployment's Window.InflightAvg, which over the same buckets is the
	// mean of the combined gauge - the concurrency RequestsPerSec was
	// served at.
	HasInflight bool
	InflightAvg float64

	// ProxyNoRecent is true when gpuworks_proxy_requests_total (the family
	// behind both request_rate and error_rate) has no sample anywhere in
	// DiagnosisWindow fleet-wide, but BuildCluster's one extra fleet-wide
	// presence probe over the chart range (rng) found it somewhere in that
	// wider span - i.e. every deployment is genuinely just quiet right now,
	// not "never instrumented". proxy/accounting.go's flush only emits
	// samples for a deployment with activity (or a live in-flight request)
	// since the last flush, so a deployment with zero requests in the last
	// five minutes writes nothing at all for that window - collected=false
	// would misreport that as a platform gap rather than an idle service.
	// When this is true, HasRequestRate/HasErrRate are forced true with
	// their rates at zero (RequestsPerSec, ErrRate5xx/429/Asleep all 0 -
	// "error_rate detail all 0") and the wire's request_rate/error_rate
	// signals (plus preemptions/waiting/token_rate, which are collected
	// already but share the same root cause) carry
	// detail.no_recent_requests = true instead of a fabricated-looking zero
	// with no explanation.
	ProxyNoRecent bool

	// HasTTFT/HasTPOT and their P95/P99 fields are the worst (max) replica
	// across the whole fleet, in milliseconds (the wire signal's own unit -
	// Window keeps seconds, the engine histogram's native unit).
	HasTTFT   bool
	TTFTP95Ms float64
	TTFTP99Ms float64
	// TTFTP95Series is the cluster-wide max-across-replicas TTFT p95, in
	// milliseconds, DiagnosisWindow-at-WindowStep bucketed - from the same
	// fleet-wide TTFT quantile-gauge series already fetched.
	TTFTP95Series []tsdb.Point
	HasTPOT       bool
	TPOTP95Ms     float64
	TPOTP99Ms     float64
	// TPOTP95Series mirrors TTFTP95Series for TPOT.
	TPOTP95Series []tsdb.Point

	// TTFTNoRecent/TPOTNoRecent mirror ProxyNoRecent for the two
	// quantile-gauge histogram families: the ingest pipeline's
	// quantile-at-ingest fold only emits a TTFT/TPOT sample for a tick that
	// actually completed at least one request that tick (see
	// applyQuantiles' "total <= 0" early return in
	// internal/server/obsingest/quantile.go) - a quiet window has
	// literally no sample, even though the family has data earlier in rng.
	// When true, HasTTFT/HasTPOT are forced true but TTFTP95Ms/TPOTP95Ms
	// (and their P99 siblings) stay at their zero value and the wire signal
	// omits value/denominator entirely (a p95 of "0ms" would misreport an
	// instant response) - only detail.no_recent_requests (plus the existing
	// upper_bound) is set.
	TTFTNoRecent bool
	TPOTNoRecent bool

	HasPreemptions    bool
	PreemptionsPerSec float64
	// PreemptionsPerSecSeries is the cluster-wide combined preemption rate,
	// DiagnosisWindow-at-WindowStep bucketed, from the same fleet-wide
	// preemptions counter series already fetched.
	PreemptionsPerSecSeries []tsdb.Point
}

// ServiceRow is one row of the cluster-layer service table (plan §3.1).
type ServiceRow struct {
	DeploymentID    uint
	Name            string
	Engine          string
	NodeNames       []string
	ReplicasReady   int
	ReplicasDesired int

	// HasRunning/RunningAvg mirror the Window BuildCluster already computed
	// for this deployment's diagnosis/triage - carried through onto the row
	// so a caller building the wire overview (services[].running) never
	// needs to call ComputeWindow a second time per deployment purely to
	// get this one number back (that used to mean 2x the tsdb queries for
	// every deployment in the overview).
	HasRunning bool
	RunningAvg float64

	HasWaiting bool
	WaitingP95 float64

	HasKVUsage bool
	KVUsageMax float64

	HasKVHitRate bool
	KVHitRate    float64

	HasTokens          bool
	PromptTokensPerSec float64
	GenTokensPerSec    float64

	HasRequestRate bool
	RequestsPerSec float64

	HasTTFT    bool
	TTFTP95Sec float64
}

// VRAMNode is one row of the cluster-layer VRAM three-segment bar, per node.
type VRAMNode struct {
	NodeUUID       string
	NodeName       string
	TotalBytes     int64
	AllocatedBytes int64

	HasUsedBytes bool
	UsedBytes    int64
	// UsedAtMS is this node's own newest "actual usage" sample timestamp
	// (unix ms), meaningful only when HasUsedBytes is true. It is
	// deliberately per-node rather than reusing the snapshot-wide AsOf: a
	// node whose agent stopped reporting minutes ago must show its own
	// stale reading time, not the freshest timestamp some other, healthy
	// node in the same fleet happened to produce.
	UsedAtMS int64
}

// EventMarker is a capacity-changing state transition to annotate a chart
// with. Always empty for now; see ClusterSnapshot.Events.
type EventMarker struct {
	TimeMS  int64
	Kind    string
	Message string
}

// BuildCluster assembles the cluster-layer snapshot for the given chart
// range.
func BuildCluster(ctx context.Context, deps Deps, rng time.Duration) (ClusterSnapshot, error) {
	if deps.TSDB == nil {
		return ClusterSnapshot{}, fmt.Errorf("monitor: BuildCluster requires a non-nil Deps.TSDB")
	}
	if err := ctx.Err(); err != nil {
		return ClusterSnapshot{}, err
	}
	now := deps.now()
	inv, err := deps.Inventory(ctx)
	if err != nil {
		return ClusterSnapshot{}, err
	}
	instancesByDeployment := groupInstancesByDeployment(inv.Instances)
	nodeNames := nodeNameIndex(inv.Nodes)

	snap := ClusterSnapshot{Events: []EventMarker{}}

	// asOf tracks the newest sample timestamp seen across every source this
	// snapshot reads - not just the cluster token-rate series - so a fleet
	// with running/KV/temperature/VRAM samples but no token counters (or no
	// deployments at all) still reports a real AsOf instead of the zero time
	// (see Window.LatestSampleAt's doc comment).
	var asOf time.Time
	noteAsOf := func(t time.Time) {
		if !t.IsZero() && (asOf.IsZero() || t.After(asOf)) {
			asOf = t
		}
	}

	// Every metric ComputeWindow needs is fetched fleet-wide exactly once
	// here (no per-deployment matcher), indexed exactly once
	// (BuildFleetWindowIndex), then sliced per deployment in memory
	// (GroupWindowSeries, O(1) map lookups per deployment) - O(metrics)
	// tsdb queries and O(metrics + fleet series) indexing per snapshot,
	// instead of O(deployments x metrics) queries and re-indexing (plan §8
	// second phase capacity requirement; see scale-review §2.4).
	fleet, err := FetchFleetWindowSeries(deps.TSDB, now, now.Add(-DiagnosisWindow), WindowStep.Milliseconds())
	if err != nil {
		return ClusterSnapshot{}, err
	}
	fleetIdx := BuildFleetWindowIndex(fleet)

	// The fleet-wide fetch above only spans DiagnosisWindow (5 minutes), so
	// a deployment with zero proxy/quantile-gauge activity in exactly that
	// span reads as collected=false even when it had traffic ten minutes
	// ago - a real gap in the last few minutes, not a platform gap, but
	// indistinguishable from one without also knowing whether the family
	// exists anywhere in the wider chart range the user is looking at. Each
	// of the three probes below is exactly one additional fleet-wide
	// QueryLatest (no per-deployment matcher, no per-deployment query) -
	// see ClusterSignals.ProxyNoRecent/TTFTNoRecent/TPOTNoRecent's doc
	// comments for how their results are folded in below.
	rangeSinceMS := now.Add(-rng).UnixMilli()
	proxyRangeDeployments, err := rangeDeploymentPresence(deps.TSDB, metricProxyRequestsTotal, rangeSinceMS)
	if err != nil {
		return ClusterSnapshot{}, err
	}
	ttftRangeDeployments, err := rangeDeploymentPresence(deps.TSDB, "gpuworks_infer_ttft_seconds", rangeSinceMS)
	if err != nil {
		return ClusterSnapshot{}, err
	}
	tpotRangeDeployments, err := rangeDeploymentPresence(deps.TSDB, "gpuworks_infer_tpot_seconds", rangeSinceMS)
	if err != nil {
		return ClusterSnapshot{}, err
	}

	type clusterItem struct {
		dep       DeploymentRef
		instances []InstanceRef
		window    Window
	}
	items := make([]clusterItem, 0, len(inv.Deployments))
	// resolvedPrompt/resolvedTTFT gather every deployment's series after its
	// sources were resolved (window_sources.go), so the fleet sparklines
	// include what the gateway stood in for, like the values beside them.
	var resolvedPrompt, resolvedTTFT []tsdb.Series
	for _, dep := range inv.Deployments {
		instances := instancesByDeployment[dep.ID]
		series := GroupWindowSeries(fleetIdx, dep.ID, instances)
		resolvedPrompt = append(resolvedPrompt, series.PromptTokens...)
		resolvedTTFT = append(resolvedTTFT, series.TTFT...)
		pd, err := pdSeriesFor(deps.TSDB, dep, instances, &series, now)
		if err != nil {
			return ClusterSnapshot{}, err
		}
		w := ComputeWindow(series, WindowParams{Now: now, Span: DiagnosisWindow})
		if pd != nil {
			ComputePDWindow(&w, *pd, dep.PDTransferMetrics)
		}
		items = append(items, clusterItem{dep: dep, instances: instances, window: w})
	}

	// errWeight/err*Weighted accumulate a request-volume-weighted average of
	// each deployment's error-rate fractions into snap.Signals.ErrRate*Pct
	// (see ClusterSignals' doc comment for why a weighted average, not a
	// plain mean of each deployment's own rate).
	var errWeight, err5xxWeighted, err429Weighted, errAsleepWeighted float64

	// fleetAnyRequestInWindow/TTFT/TPOT track whether ANY deployment's own
	// DiagnosisWindow (not the widened rng probes above) actually saw the
	// signal - i.e. the un-widened truth Diagnose/Triage below always sees.
	// ProxyNoRecent/TTFTNoRecent/TPOTNoRecent (set after the loop) only ever
	// fire when every one of these stays false: a fleet with any real
	// recent activity already reports the aggregate honestly and needs no
	// "quiet window" fallback.
	var fleetAnyRequestInWindow, fleetAnyTTFTInWindow, fleetAnyTPOTInWindow bool

	for _, it := range items {
		dep, instances, w := it.dep, it.instances, it.window
		noteAsOf(w.LatestSampleAt)
		fleetAnyRequestInWindow = fleetAnyRequestInWindow || w.HasRequestRate
		fleetAnyTTFTInWindow = fleetAnyTTFTInWindow || w.HasTTFT
		fleetAnyTPOTInWindow = fleetAnyTPOTInWindow || w.HasTPOT

		// reqW is a copy of w used only for this deployment's request/error
		// rate presentation (the service row's request_rate cell and the
		// fleet-wide request_rate/error_rate aggregation below) - Triage and
		// Diagnose still run against the original, un-widened w a few lines
		// down, so a deployment that only "looks" collected because of the
		// wider rng probe can never make a rule fire that the real
		// DiagnosisWindow data would not have (plan's rules must not fire on
		// stale data).
		reqW := w
		if !w.HasRequestRate && proxyRangeDeployments[depKeyString(dep.ID)] {
			// This deployment's own proxy series exist somewhere in rng but
			// not in the last five minutes: report the true zero rate
			// (RequestsPerSec/ErrRate* are already their zero value from
			// ComputeWindow) instead of the misleading "not collected".
			reqW.HasRequestRate = true
			reqW.HasErrRate = true
		}

		row := ServiceRow{
			DeploymentID:    dep.ID,
			Name:            dep.Name,
			Engine:          dep.Engine,
			NodeNames:       resolveNodeNames(dep, instances, nodeNames),
			ReplicasReady:   ReadyReplicas(dep, instances),
			ReplicasDesired: dep.ReplicasDesired,

			HasRunning: w.HasRunning,
			RunningAvg: w.RunningAvg,

			HasWaiting: w.HasWaiting,
			WaitingP95: w.WaitingP95,

			HasKVUsage: w.HasKVUsage,
			KVUsageMax: w.KVUsageMax,

			HasKVHitRate: w.HasKVHitRate,
			KVHitRate:    w.KVHitRate,

			HasTokens:          w.HasTokens,
			PromptTokensPerSec: w.PromptTokensPerSec,
			GenTokensPerSec:    w.GenTokensPerSec,

			HasRequestRate: reqW.HasRequestRate,
			RequestsPerSec: reqW.RequestsPerSec,

			// ttft_p95_ms deliberately keeps using the un-widened w, not
			// reqW's sibling treatment: a service row carries no detail
			// field to explain a "collected but no recent value" ttft, so
			// it stays null rather than presenting an unqualified 0ms.
			HasTTFT:    w.HasTTFT,
			TTFTP95Sec: w.TTFTP95Sec,
		}
		snap.Services = append(snap.Services, row)
		snap.Signals.ReadyReplicas += row.ReplicasReady
		snap.Signals.DesiredReplicas += row.ReplicasDesired
		if row.ReplicasReady < row.ReplicasDesired {
			snap.Signals.NotReadyServices = append(snap.Signals.NotReadyServices, dep.Name)
		}
		if w.HasTokens {
			snap.Signals.HasTokens = true
			snap.Signals.InputTokensPerSec += w.PromptTokensPerSec
			snap.Signals.OutputTokensPerSec += w.GenTokensPerSec
		}
		if reqW.HasRequestRate {
			snap.Signals.HasRequestRate = true
			snap.Signals.RequestsPerSec += reqW.RequestsPerSec
		}
		if w.HasInflight {
			snap.Signals.HasInflight = true
			snap.Signals.InflightAvg += w.InflightAvg
		}
		if reqW.HasErrRate {
			snap.Signals.HasErrRate = true
			errWeight += reqW.RequestsPerSec
			err5xxWeighted += reqW.ErrRate5xx * reqW.RequestsPerSec
			err429Weighted += reqW.ErrRate429 * reqW.RequestsPerSec
			errAsleepWeighted += reqW.ErrRateAsleep * reqW.RequestsPerSec
		}
		if w.HasTTFT {
			snap.Signals.HasTTFT = true
			if p95 := w.TTFTP95Sec * 1000; p95 > snap.Signals.TTFTP95Ms {
				snap.Signals.TTFTP95Ms = p95
			}
			if p99 := w.TTFTP99Sec * 1000; p99 > snap.Signals.TTFTP99Ms {
				snap.Signals.TTFTP99Ms = p99
			}
		}
		if w.HasTPOT {
			snap.Signals.HasTPOT = true
			if p95 := w.TPOTP95Sec * 1000; p95 > snap.Signals.TPOTP95Ms {
				snap.Signals.TPOTP95Ms = p95
			}
			if p99 := w.TPOTP99Sec * 1000; p99 > snap.Signals.TPOTP99Ms {
				snap.Signals.TPOTP99Ms = p99
			}
		}
		if w.HasPreemptions {
			snap.Signals.HasPreemptions = true
			snap.Signals.PreemptionsPerSec += w.PreemptionsPerSec
		}
	}
	if errWeight > 0 {
		snap.Signals.ErrRate5xxPct = err5xxWeighted / errWeight * 100
		snap.Signals.ErrRate429Pct = err429Weighted / errWeight * 100
		snap.Signals.ErrRateAsleepPct = errAsleepWeighted / errWeight * 100
		snap.Signals.ErrRateTotalPct = snap.Signals.ErrRate5xxPct + snap.Signals.ErrRate429Pct + snap.Signals.ErrRateAsleepPct
	}

	// The fleet-wide "no recent requests" fallback only fires when NO
	// deployment had the signal in its own DiagnosisWindow (fleetAny* above)
	// but the wider rng probe found the family somewhere - a fleet with any
	// real recent activity already has HasRequestRate/HasTTFT/HasTPOT true
	// from the loop above and never reaches here. This also covers the edge
	// case of a proxyRangeDeployments/ttftRangeDeployments/
	// tpotRangeDeployments entry for a deployment id no longer in inv (e.g.
	// deleted moments ago): the fleet-wide flag still reflects it even
	// though no ServiceRow exists to carry it.
	snap.Signals.ProxyNoRecent = !fleetAnyRequestInWindow && len(proxyRangeDeployments) > 0
	if snap.Signals.ProxyNoRecent {
		snap.Signals.HasRequestRate = true
		snap.Signals.HasErrRate = true
	}
	snap.Signals.TTFTNoRecent = !fleetAnyTTFTInWindow && len(ttftRangeDeployments) > 0
	if snap.Signals.TTFTNoRecent {
		snap.Signals.HasTTFT = true
	}
	snap.Signals.TPOTNoRecent = !fleetAnyTPOTInWindow && len(tpotRangeDeployments) > 0
	if snap.Signals.TPOTNoRecent {
		snap.Signals.HasTPOT = true
	}
	sortServiceRows(snap.Services)

	waitingSeries, waitingP95, hasWaitingP95 := clusterWaitingP95(fleet.Waiting)
	snap.Signals.HasWaiting = hasWaitingP95
	snap.Signals.WaitingP95 = waitingP95
	snap.Signals.WaitingSeries = waitingSeries

	// The rest of the cluster signals' sparklines are likewise reduced
	// directly from fleet-wide series FetchFleetWindowSeries already
	// fetched above - no additional tsdb query (plan §8 second phase
	// capacity requirement; see also the query-count test).
	if combined := SumBy(Rate(resolvedPrompt), ""); len(combined) > 0 {
		snap.Signals.PromptTokensPerSecSeries = combined[0].Points
	}
	snap.Signals.RequestsPerSecSeries = clusterProxyRequestRate(fleet.ProxyRequestsTotal)
	snap.Signals.ErrorRateSeries = clusterErrorRateSeries(fleet.ProxyRequestsTotal)
	snap.Signals.PreemptionsPerSecSeries = clusterPreemptionsRate(fleet.Preemptions)
	snap.Signals.TTFTP95Series = clusterQuantileP95Ms(resolvedTTFT)
	snap.Signals.TPOTP95Series = clusterQuantileP95Ms(fleet.TPOT)

	var vramLatest time.Time
	snap.VRAMByNode, vramLatest, err = vramByNode(deps, inv, now)
	if err != nil {
		return ClusterSnapshot{}, err
	}
	noteAsOf(vramLatest)

	snap.ClusterTokenRate, err = clusterTokenRate(deps, now, rng)
	if err != nil {
		return ClusterSnapshot{}, err
	}
	if p, ok := LatestValue(toSeries(snap.ClusterTokenRate)); ok {
		noteAsOf(time.UnixMilli(p.TimeMS).UTC())
	}

	snap.AsOf = asOf
	snap.Stale = StaleAt(asOf, now)
	return snap, nil
}
