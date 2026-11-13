package monitorapi

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/server/healthcheck"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// eventPageSize is how many folded timeline entries overviewEvents reads
// per page when it pages a deployment's history looking for events inside
// the requested range: small enough that a deployment with no history in
// range costs one cheap page, large enough that a normally chatty
// deployment's whole range fits in a handful of pages.
const eventPageSize = 200

// eventMaxPages bounds how many pages overviewEvents will read for one
// deployment: eventMaxPages * eventPageSize = 2000 folded timeline entries
// is a deliberate bound on how much history one overview call scans per
// deployment, not just a loop-termination guard. In the overwhelmingly
// common case the loop already stops on its own the first time a page's
// oldest entry falls outside the requested range (folding already
// collapses runs of identical progress updates into one row each - see
// store.DeploymentTimeline's doc comment - so a normal deployment's whole
// range fits in far fewer than 2000 rows); a deployment whose in-range
// folded milestones exceed this bound simply stops there rather than
// reading an unbounded amount of history for one request.
const eventMaxPages = 10

// sparkPoints is how many of a signal's most recent buckets its spark
// carries (plan §6's shared Signal shape).
const sparkPoints = 15

// buildOverview assembles the GET /api/v1/monitor/overview response (plan
// §3.1) from monitor.BuildCluster's snapshot plus this package's own
// supplementary queries: a cluster-wide prompt-token series (BuildCluster
// only exposes the generation half as ClusterTokenRate) and
// capacity-changing events from instance_events (via Deps.DeploymentEvents).
func (s *Service) buildOverview(ctx context.Context, rng time.Duration) (dto.MonitorOverview, error) {
	now := s.deps.now()
	inv, err := s.deps.Inventory(ctx)
	if err != nil {
		return dto.MonitorOverview{}, err
	}
	deps := s.depsFor(inv, now)
	snap, err := monitor.BuildCluster(ctx, deps, rng)
	if err != nil {
		return dto.MonitorOverview{}, err
	}

	nodeNames := nodeNameIndex(inv.Nodes)
	instByDep := instancesByDeployment(inv.Instances)
	depByID := make(map[uint]monitor.DeploymentRef, len(inv.Deployments))
	for _, d := range inv.Deployments {
		depByID[d.ID] = d
	}

	services := make([]dto.MonitorServiceRow, 0, len(snap.Services))
	for _, row := range snap.Services {
		insts := instByDep[row.DeploymentID]
		dep := depByID[row.DeploymentID]
		health := s.Health(row.DeploymentID, dep.HealthCheck, dep.HealthCheckThresholdSec)
		services = append(services, serviceRowDTO(row, deploymentPlacement(dep, insts, nodeNames), deploymentState(dep, insts), health))
	}
	sortByHealth(services)

	promptPoints, err := s.clusterTokenRatePoints(ctx, "gpuworks_infer_prompt_tokens_total", now, rng)
	if err != nil {
		return dto.MonitorOverview{}, err
	}

	events, err := s.overviewEvents(ctx, inv, now, rng)
	if err != nil {
		return dto.MonitorOverview{}, err
	}

	asOfMS := timeMillis(snap.AsOf)
	return dto.MonitorOverview{
		AsOf:       asOfMS,
		Stale:      snap.Stale,
		Signals:    signalsDTO(snap.Signals),
		Services:   services,
		VRAMByNode: vramNodesDTO(snap.VRAMByNode, gpuCountByNode(inv.GPUs)),
		ClusterTokens: dto.MonitorClusterTokens{
			Prompt:     seriesFromPoints(promptPoints),
			Generation: seriesFromPoints(snap.ClusterTokenRate),
		},
		Events: events,
	}, nil
}

// clusterTokenRatePoints mirrors monitor's own clusterTokenRate for a
// different metric: the cluster-wide per-second rate of metric, bucketed
// the same way BuildCluster buckets ClusterTokenRate (monitor.ChartStep).
func (s *Service) clusterTokenRatePoints(ctx context.Context, metric string, now time.Time, rng time.Duration) ([]tsdb.Point, error) {
	if s.deps.TSDB == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stepMS := monitor.ChartStep(rng)
	startMS := now.Add(-rng).UnixMilli()
	endMS := now.UnixMilli()
	series, err := s.deps.TSDB.QueryRange(metric, nil, startMS, endMS, stepMS)
	if err != nil {
		return nil, err
	}
	total := monitor.SumBy(monitor.Rate(series), "")
	if len(total) == 0 {
		return nil, nil
	}
	return total[0].Points, nil
}

// signalsDTO builds the cluster overview's fixed eight-entry signal row, in
// the wire contract's fixed order (plan §6): ready_replicas, error_rate,
// request_rate, token_rate, ttft, tpot, waiting, preemptions. The row
// reports facts only: values, their breakdown and their recent course,
// never a verdict on them. Each of error_rate/request_rate/ttft/tpot/
// preemptions is Collected only when the corresponding
// monitor.ClusterSignals field is (no proxy accounting or engine histograms
// at all in the fleet yet) - collected=false then carries no value or
// spark, per the wire contract's null-vs-value rule.
//
// ready_replicas carries no spark: it is derived from *current* instance
// state, not from a stored metric series, so there is no historical series
// to slice a sparkline from.
//
// error_rate/request_rate/preemptions/waiting/token_rate additionally carry
// detail.no_recent_requests = true whenever s.ProxyNoRecent is set (see
// monitor.ClusterSignals' doc comment): the fleet had zero proxy traffic in
// the diagnosis window but the family exists somewhere in the wider chart
// range, so collected stays true and Value reports the true zero rather
// than a misleading collected=false. ttft/tpot use s.TTFTNoRecent/
// TPOTNoRecent instead (their own family can be quiet even while the proxy
// itself is not) and, unlike the other five, omit Value/Denominator
// entirely in that state (dto.MonitorSignal's doc comment).
func signalsDTO(s monitor.ClusterSignals) []dto.MonitorSignal {
	out := make([]dto.MonitorSignal, 0, 8)

	var readyDetail map[string]any
	if len(s.NotReadyServices) > 0 {
		readyDetail = map[string]any{"not_ready": s.NotReadyServices}
	}
	out = append(out, dto.MonitorSignal{
		Key: "ready_replicas", Collected: true,
		Value: floatPtr(float64(s.ReadyReplicas)), Denominator: floatPtr(float64(s.DesiredReplicas)),
		Detail: readyDetail,
	})

	var errDetail map[string]any
	if s.HasErrRate {
		errDetail = map[string]any{
			"5xx": round1(s.ErrRate5xxPct), "429": round1(s.ErrRate429Pct), "asleep_503": round1(s.ErrRateAsleepPct),
		}
		addNoRecentRequests(errDetail, s.ProxyNoRecent)
	}
	out = append(out, dto.MonitorSignal{
		Key: "error_rate", Collected: s.HasErrRate,
		// Value is rounded to one decimal like every detail percentage
		// (round1) - the headline number and its breakdown must use the
		// same precision, or a UI summing the detail could read as
		// slightly more precise than the headline it is decomposing.
		Value: collectedFloatPtr(s.HasErrRate, round1(s.ErrRateTotalPct)), Detail: errDetail,
		Spark: sparkFor(s.HasErrRate, s.ErrorRateSeries),
	})

	// request_rate's Denominator is the gateway's average requests in
	// flight over the same window, the concurrency the rate was served at;
	// a quiet fleet (ProxyNoRecent) reports the true zero beside its zero
	// rate.
	out = append(out, dto.MonitorSignal{
		Key: "request_rate", Collected: s.HasRequestRate,
		Value:       collectedFloatPtr(s.HasRequestRate, round1(s.RequestsPerSec)),
		Denominator: collectedFloatPtr(s.HasRequestRate && (s.HasInflight || s.ProxyNoRecent), round1(s.InflightAvg)),
		Detail:      noRecentRequestsDetail(s.HasRequestRate && s.ProxyNoRecent),
		Spark:       sparkFor(s.HasRequestRate, s.RequestsPerSecSeries),
	})

	out = append(out, dto.MonitorSignal{
		Key: "token_rate", Collected: s.HasTokens,
		Value: collectedFloatPtr(s.HasTokens, round1(s.InputTokensPerSec)), Denominator: collectedFloatPtr(s.HasTokens, round1(s.OutputTokensPerSec)),
		Detail: noRecentRequestsDetail(s.HasTokens && s.ProxyNoRecent),
		Spark:  sparkFor(s.HasTokens, s.PromptTokensPerSecSeries),
	})

	// upperBoundDetail marks the ttft/tpot signals with upper_bound: true:
	// both values are p95/p99 of per-tick quantile gauges maxed over the
	// window (see monitor.Window's doc comment), a conservative upper bound
	// rather than an exact percentile over raw samples, so the UI can label
	// them accordingly instead of presenting a false-precision number.
	// ttftTpotValuePtr additionally nils out Value/Denominator when
	// *NoRecent is set even though Collected stays true (dto.MonitorSignal's
	// doc comment: a "0ms" p95 would misreport an instant response instead
	// of "nothing completed this window").
	out = append(out, dto.MonitorSignal{
		Key: "ttft", Collected: s.HasTTFT,
		Value: ttftTpotValuePtr(s.HasTTFT, s.TTFTNoRecent, round1(s.TTFTP95Ms)), Denominator: ttftTpotValuePtr(s.HasTTFT, s.TTFTNoRecent, round1(s.TTFTP99Ms)),
		Detail: mergeDetail(upperBoundDetail(s.HasTTFT), noRecentRequestsDetail(s.HasTTFT && s.TTFTNoRecent)), Spark: sparkFor(s.HasTTFT, s.TTFTP95Series),
	})
	out = append(out, dto.MonitorSignal{
		Key: "tpot", Collected: s.HasTPOT,
		Value: ttftTpotValuePtr(s.HasTPOT, s.TPOTNoRecent, round1(s.TPOTP95Ms)), Denominator: ttftTpotValuePtr(s.HasTPOT, s.TPOTNoRecent, round1(s.TPOTP99Ms)),
		Detail: mergeDetail(upperBoundDetail(s.HasTPOT), noRecentRequestsDetail(s.HasTPOT && s.TPOTNoRecent)), Spark: sparkFor(s.HasTPOT, s.TPOTP95Series),
	})

	out = append(out, dto.MonitorSignal{
		Key: "waiting", Collected: s.HasWaiting,
		Value:  collectedFloatPtr(s.HasWaiting, round1(s.WaitingP95)),
		Detail: noRecentRequestsDetail(s.HasWaiting && s.ProxyNoRecent),
		Spark:  sparkFor(s.HasWaiting, s.WaitingSeries),
	})

	out = append(out, dto.MonitorSignal{
		Key: "preemptions", Collected: s.HasPreemptions,
		Value:  collectedFloatPtr(s.HasPreemptions, round1(s.PreemptionsPerSec)),
		Detail: noRecentRequestsDetail(s.HasPreemptions && s.ProxyNoRecent),
		Spark:  sparkFor(s.HasPreemptions, s.PreemptionsPerSecSeries),
	})
	return out
}

// upperBoundDetail is the ttft/tpot signals' upper_bound: true detail flag
// when collected, nil (omitted) otherwise - the value nullability rule
// extended to this one detail key rather than the whole map.
func upperBoundDetail(collected bool) map[string]any {
	if !collected {
		return nil
	}
	return map[string]any{"upper_bound": true}
}

// sparkFor returns the last sparkPoints points of series as a spark, or nil
// (omitted on the wire) when the signal was not collected at all.
func sparkFor(collected bool, series []tsdb.Point) []dto.Point {
	if !collected || len(series) == 0 {
		return nil
	}
	return lastPoints(series, sparkPoints)
}

func vramNodesDTO(nodes []monitor.VRAMNode, gpuCounts map[string]int) []dto.MonitorVRAMNode {
	out := make([]dto.MonitorVRAMNode, 0, len(nodes))
	for _, n := range nodes {
		row := dto.MonitorVRAMNode{
			NodeUUID: n.NodeUUID, Name: n.NodeName, GPUCount: gpuCounts[n.NodeUUID],
			TotalBytes: n.TotalBytes, AllocatedBytes: n.AllocatedBytes,
		}
		if n.HasUsedBytes {
			used := n.UsedBytes
			at := n.UsedAtMS
			row.UsedBytes, row.UsedAsOf = &used, &at
		}
		out = append(out, row)
	}
	return out
}

// serviceRowDTO converts one monitor.ServiceRow (plus this package's own
// placement/state and the deployment's health check) into the cluster
// overview's wire row. row.HasRunning/RunningAvg come straight from
// ServiceRow: BuildCluster already computed them from its Window, so this
// never needs its own ComputeWindow call.
func serviceRowDTO(row monitor.ServiceRow, placement, state string, health dto.DeploymentHealth) dto.MonitorServiceRow {
	return dto.MonitorServiceRow{
		DeploymentID: row.DeploymentID, Name: row.Name, Engine: row.Engine, Placement: placement,
		Replicas:       dto.MonitorReplicaCount{Ready: row.ReplicasReady, Desired: row.ReplicasDesired},
		State:          state,
		RequestRate:    collectedFloatPtr(row.HasRequestRate, round1(row.RequestsPerSec)),
		TTFTP95Ms:      collectedFloatPtr(row.HasTTFT, round1(row.TTFTP95Sec*1000)),
		Running:        collectedFloatPtr(row.HasRunning, round1(row.RunningAvg)),
		Waiting:        collectedFloatPtr(row.HasWaiting, round1(row.WaitingP95)),
		KVUsagePct:     collectedFloatPtr(row.HasKVUsage, round1(row.KVUsageMax*100)),
		KVHitRatePct:   collectedFloatPtr(row.HasKVHitRate, round1(row.KVHitRate*100)),
		PromptTokS:     collectedFloatPtr(row.HasTokens, round1(row.PromptTokensPerSec)),
		GenerationTokS: collectedFloatPtr(row.HasTokens, round1(row.GenTokensPerSec)),
		Health:         health,
	}
}

// healthOrder ranks a health status for the services table: the ones whose
// check failed or timed out first, then the ones with nothing to check, the
// healthy, and last the ones not checked at all.
var healthOrder = map[string]int{
	string(healthcheck.StatusFailed):    0,
	string(healthcheck.StatusTimeout):   1,
	string(healthcheck.StatusNoReplica): 2,
	string(healthcheck.StatusPending):   3,
	string(healthcheck.StatusOK):        4,
	string(healthcheck.StatusOff):       5,
}

// sortByHealth orders the services table by healthOrder, then by name.
func sortByHealth(rows []dto.MonitorServiceRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := healthOrder[rows[i].Health.Status], healthOrder[rows[j].Health.Status]
		if ri != rj {
			return ri < rj
		}
		return rows[i].Name < rows[j].Name
	})
}

// overviewEvents assembles the cluster overview's capacity-changing event
// markers (plan §3.1): instance transitions into running/failed/stopped,
// read from instance_events rows with kind="state" whose Data JSON names
// one of those target states.
//
// Sleep/wake and node online/offline are deliberately not produced: neither
// is recorded as an instance_events row today. A sleep/wake transition only
// changes Instance.SleepLevel while State stays "running", and
// store.UpdateInstanceStatus only appends an event when State or Phase
// changes; node online/offline is tracked purely as the current Node.State
// column (store.SetNodeState), with no history table at all.
//
// Per deployment, it pages Deps.DeploymentEvents newest-first until either a
// page's oldest entry falls before the requested range - the normal case -
// or eventMaxPages is reached (a deliberate bound: see its doc comment).
// This reads the range window server-side rather than a single flat
// row-count cap, which could silently stop short of the range on a chatty
// deployment while still reading pages nobody asked for on a quiet one.
func (s *Service) overviewEvents(ctx context.Context, inv monitor.Inventory, now time.Time, rng time.Duration) ([]dto.MonitorEvent, error) {
	if s.deps.DeploymentEvents == nil {
		return []dto.MonitorEvent{}, nil
	}
	sinceMS := now.Add(-rng).UnixMilli()
	names := make(map[uint]string, len(inv.Deployments))
	for _, d := range inv.Deployments {
		names[d.ID] = d.Name
	}
	var out []dto.MonitorEvent
	for _, d := range inv.Deployments {
		evs, err := s.deploymentEventsInRange(ctx, d.ID, sinceMS)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			if e.Kind != "state" {
				continue
			}
			kind, ok := monitorEventKind(e.Data)
			if !ok {
				continue
			}
			out = append(out, dto.MonitorEvent{
				TimeMS: e.CreatedAtMS, Kind: kind, DeploymentID: d.ID, InstanceUUID: e.InstanceUUID, Label: names[d.ID],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeMS > out[j].TimeMS })
	if out == nil {
		out = []dto.MonitorEvent{}
	}
	return out, nil
}

// deploymentEventsInRange pages one deployment's folded timeline
// (Deps.DeploymentEvents) newest-first and returns only the entries at or
// after sinceMS, stopping as soon as a page's oldest entry falls before it.
func (s *Service) deploymentEventsInRange(ctx context.Context, deploymentID uint, sinceMS int64) ([]EventRef, error) {
	var out []EventRef
	var before uint
	for page := 0; page < eventMaxPages; page++ {
		evs, err := s.deps.DeploymentEvents(ctx, deploymentID, before, eventPageSize)
		if err != nil {
			return nil, err
		}
		if len(evs) == 0 {
			break
		}
		stop := false
		for _, e := range evs {
			if e.CreatedAtMS < sinceMS {
				stop = true
				break
			}
			out = append(out, e)
		}
		if stop || len(evs) < eventPageSize {
			break
		}
		before = evs[len(evs)-1].ID
	}
	return out, nil
}

func monitorEventKind(data string) (string, bool) {
	if data == "" {
		return "", false
	}
	var params struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(data), &params); err != nil {
		return "", false
	}
	switch params.State {
	case proto.InstRunning:
		return "instance_running", true
	case proto.InstFailed:
		return "instance_failed", true
	case proto.InstStopped:
		return "instance_stopped", true
	default:
		return "", false
	}
}
