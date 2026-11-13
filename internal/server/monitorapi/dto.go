package monitorapi

import (
	"fmt"
	"math"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// This file holds the small monitor.* -> dto.* conversion helpers shared by
// overview.go and deployment.go: null-vs-value pointer helpers,
// timestamp/point conversion, and the derived-display-field functions
// (deploymentPlacement, deploymentState/replicaState)
// that need the raw Inventory neither monitor.ClusterSnapshot nor
// ServiceSnapshot carries in full.

func timeMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func seriesFromPoints(points []tsdb.Point) dto.MonitorSeries {
	return dto.MonitorSeries{Labels: map[string]string{}, Points: pointsFromTSDB(points)}
}

func pointsFromTSDB(points []tsdb.Point) []dto.Point {
	out := make([]dto.Point, 0, len(points))
	for _, p := range points {
		out = append(out, dto.Point{float64(p.TimeMS), p.Value})
	}
	return out
}

// lastPoints returns at most n of points' most recent samples, in
// chronological order - the shape a signal tile's sparkline wants (plan
// §6's shared Signal shape).
func lastPoints(points []tsdb.Point, n int) []dto.Point {
	if len(points) > n {
		points = points[len(points)-n:]
	}
	return pointsFromTSDB(points)
}

func gpuCountByNode(gpus []monitor.GPURef) map[string]int {
	out := map[string]int{}
	for _, g := range gpus {
		out[g.NodeUUID]++
	}
	return out
}

func floatPtr(v float64) *float64 { return &v }

// round1 rounds v to one decimal place, used for signal detail percentages
// so the wire never carries raw float64 aggregation noise.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// collectedFloatPtr is the pointer-nullability half of the wire contract's
// "collected=false carries no value" rule (dto.MonitorSignal's doc comment):
// nil, not a zero-valued pointer, when the underlying Has* flag is false.
func collectedFloatPtr(collected bool, v float64) *float64 {
	if !collected {
		return nil
	}
	return floatPtr(v)
}

// ttftTpotValuePtr is collectedFloatPtr with one more nullability case
// specific to the ttft/tpot signals: even when collected is true, the value
// is omitted (nil) when noRecent is set - the diagnosis window itself
// produced no fresh quantile-gauge sample (see monitor.ClusterSignals'
// TTFTNoRecent/TPOTNoRecent doc comment and dto.MonitorSignal's), so a "0ms"
// p95 would misreport an instant response rather than "nothing completed
// this window".
func ttftTpotValuePtr(collected, noRecent bool, v float64) *float64 {
	if !collected || noRecent {
		return nil
	}
	return floatPtr(v)
}

// noRecentRequestsDetail is the detail.no_recent_requests = true marker
// (dto.MonitorSignal's doc comment) when noRecent is set, nil (omitted)
// otherwise - the same value/severity nullability rule applied to this one
// detail key.
func noRecentRequestsDetail(noRecent bool) map[string]any {
	if !noRecent {
		return nil
	}
	return map[string]any{"no_recent_requests": true}
}

// addNoRecentRequests folds the no_recent_requests marker into an
// already-built detail map in place (error_rate's own 5xx/429/asleep_503
// breakdown, which must keep its other keys rather than being replaced) -
// noRecentRequestsDetail's sibling for callers that already have a
// non-nil map to extend instead of a fresh one to build.
func addNoRecentRequests(detail map[string]any, noRecent bool) {
	if !noRecent {
		return
	}
	detail["no_recent_requests"] = true
}

// mergeDetail combines any number of detail maps (each possibly nil) into
// one, or nil (omitted on the wire) when every input was nil/empty - ttft/
// tpot's own shape, which independently decides upper_bound and
// no_recent_requests and needs both in the same Detail map.
func mergeDetail(maps ...map[string]any) map[string]any {
	var out map[string]any
	for _, m := range maps {
		for k, v := range m {
			if out == nil {
				out = map[string]any{}
			}
			out[k] = v
		}
	}
	return out
}

// deploymentPlacement renders the "TP4 · gpu3, gpu4" summary the wire
// contract's services[].placement and deployment.placement fields carry. TP
// is the widest per-instance GPU count seen among the deployment's replicas
// (0 when it has none yet, which omits the "TP_" segment entirely); node
// names fall back to instance-derived ones exactly like
// monitor.resolveNodeNames does, since the app-side adapter always leaves
// DeploymentRef.NodeNames empty.
func deploymentPlacement(dep monitor.DeploymentRef, instances []monitor.InstanceRef, nodeNames map[string]string) string {
	tp := 0
	for _, inst := range instances {
		if n := len(inst.GPUIndexes); n > tp {
			tp = n
		}
	}
	names := dep.NodeNames
	if len(names) == 0 {
		seen := map[string]bool{}
		for _, inst := range instances {
			name := nodeNames[inst.NodeUUID]
			if name == "" {
				name = inst.NodeUUID
			}
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	var b strings.Builder
	if tp > 0 {
		fmt.Fprintf(&b, "TP%d", tp)
	}
	if len(names) > 0 {
		if b.Len() > 0 {
			b.WriteString(" · ")
		}
		b.WriteString(strings.Join(names, ", "))
	}
	return b.String()
}

// deploymentState is the state the deployment list shows
// (monitor.DeploymentRef.State, deployment.EffectiveState) in the
// running|starting|stopped|failed|asleep|offline vocabulary the wire
// contract's services[].state carries (replicaState below is its
// per-instance sibling for replicas[].state). It derives nothing of its own,
// so the monitor cannot read a deployment differently from its list row: a
// deployment reads asleep exactly when the gateway refuses it for sleep.
func deploymentState(deployment monitor.DeploymentRef) string {
	switch deployment.State {
	case "running", "offline", "failed":
		return deployment.State
	case "asleep", "sleeping":
		return "asleep"
	case "pending", "pulling", "starting":
		return "starting"
	default: // stopped, deleting
		return "stopped"
	}
}

// replicaState is deploymentState's per-instance sibling, for
// replicas[].state in the /monitor/deployments/{id} response.
func replicaState(inst monitor.InstanceRef) string {
	switch {
	case inst.State == proto.InstFailed:
		return "failed"
	case inst.State == proto.InstRunning && inst.Asleep:
		return "asleep"
	case inst.State == proto.InstRunning:
		return "running"
	case inst.State == proto.InstPending, inst.State == proto.InstPulling, inst.State == proto.InstStarting, inst.State == proto.InstOptimizing:
		return "starting"
	default:
		return "stopped"
	}
}
