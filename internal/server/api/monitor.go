package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// MonitorService serves the /monitor cluster overview, per-deployment detail
// and ad-hoc metric series views (plan
// artifacts/archive/2026-09-19-monitor-center/plan.md §6). Its methods are typed only
// in dto and stdlib, never in internal/server/monitor: that package owns the
// actual snapshot computation and diagnosis rules, but internal/server/api
// already sits at its api_fanout budget ceiling (scripts/arch-constraints.md
// - measured 36 of 36 before this file existed), so this interface cannot
// add monitor as a new import without raising a budget the monitoring plan
// explicitly does not authorize raising. internal/server/app implements this
// interface and does the monitor.ClusterSnapshot/ServiceSnapshot -> dto
// conversion itself; the handlers below own the HTTP contract instead -
// range/step/by/fn parsing and validation, 404 mapping, and writing the
// already-built dto value.
type MonitorService interface {
	Overview(ctx context.Context, rng time.Duration) (dto.MonitorOverview, error)
	Deployment(ctx context.Context, id uint, rng time.Duration) (dto.MonitorDeployment, error)
	// Requests serves GET /monitor/deployments/{id}/requests and
	// Distributions GET /monitor/distributions (monitorapi/requests.go).
	Requests(ctx context.Context, id uint, q dto.MonitorRequestsQuery) (dto.MonitorRequests, error)
	// LookupRequests serves GET /monitor/requests/lookup: requests by
	// trace id or request id across deployments (monitorapi/lookup.go).
	LookupRequests(ctx context.Context, q dto.MonitorRequestLookupQuery) (dto.MonitorRequestLookup, error)
	Distributions(ctx context.Context, deploymentID uint, rng time.Duration) (dto.MonitorDistributions, error)
	// Latency serves GET /monitor/deployments/{id}/latency
	// (monitorapi/latency.go).
	Latency(ctx context.Context, id uint, rng time.Duration) (dto.MonitorLatency, error)
	// Health is a deployment's health check on the wire, given its own
	// settings (monitorapi/health.go).
	Health(id uint, enabled bool, thresholdSec int) dto.DeploymentHealth
	Series(ctx context.Context, metric, by, fn string, rng, step time.Duration) ([]dto.MonitorSeries, error)
	// Metrics serves GET /monitor/metrics: the inference metric vocabulary
	// (internal/inferencemetrics) as the stored series it defines.
	Metrics() []dto.MonitorMetricSeries
	// At is the service as of end, for a range the operator picked
	// (from/to): charts, distributions and latency cover the range ending
	// there; what the inventory says now stays current.
	At(end time.Time) MonitorService
}

// ErrMonitorDeploymentNotFound is returned by MonitorService.Deployment when
// id does not name a known deployment; monitorDeployment maps it to 404.
var ErrMonitorDeploymentNotFound = errors.New("monitor: deployment not found")

// ErrMonitorSeriesTooLarge is returned by MonitorService.Series when a
// request's projected series or point count would exceed
// monitorapi.MaxSeriesCount/MaxSeriesPoints; monitorSeries maps it to 400
// with a stable code rather than truncating the response silently.
var ErrMonitorSeriesTooLarge = errors.New("monitor: series request too large")

// monitorRanges is the fixed set of chart ranges the wire contract allows
// (plan §6): any other "range" value is a 400, not a best-effort parse.
var monitorRanges = map[string]time.Duration{
	"15m":  15 * time.Minute,
	"1h":   time.Hour,
	"6h":   6 * time.Hour,
	"24h":  24 * time.Hour,
	"168h": 168 * time.Hour,
}

// Bounds of a picked range (from/to): raw samples are kept at most 30 days.
const (
	monitorWindowMin = time.Minute
	monitorWindowMax = 30 * 24 * time.Hour
)

// monitorWindow reads a chart route's range: from and to (Unix ms) for a
// picked one, which answers from the service as of to, or range (a preset
// ending now). Giving both is refused; so is a picked range outside its
// bounds or ending in the future. ok false has answered 400.
func (a *apiHandlers) monitorWindow(w http.ResponseWriter, r *http.Request) (MonitorService, time.Duration, bool) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	if from == "" && to == "" {
		rng, ok := parseMonitorRange(q.Get("range"))
		if !ok {
			writeProductError(w, http.StatusBadRequest, ErrorMonitorRangeInvalid, "invalid range", nil)
			return nil, 0, false
		}
		return a.d.Monitor, rng, true
	}
	invalid := func(detail string) (MonitorService, time.Duration, bool) {
		writeProductError(w, http.StatusBadRequest, ErrorMonitorRangeInvalid, detail, map[string]any{"detail": detail})
		return nil, 0, false
	}
	if q.Get("range") != "" {
		return invalid("give range or from and to, not both")
	}
	fromMS, errFrom := strconv.ParseInt(from, 10, 64)
	toMS, errTo := strconv.ParseInt(to, 10, 64)
	if errFrom != nil || errTo != nil || fromMS <= 0 || toMS <= fromMS {
		return invalid("from and to are Unix milliseconds, from before to")
	}
	if time.UnixMilli(toMS).After(time.Now().UTC().Add(time.Minute)) {
		return invalid("to is in the future")
	}
	rng := time.Duration(toMS-fromMS) * time.Millisecond
	if rng < monitorWindowMin || rng > monitorWindowMax {
		return invalid("a picked range is between one minute and 30 days")
	}
	if a.d.Monitor == nil {
		return nil, rng, true
	}
	return a.d.Monitor.At(time.UnixMilli(toMS)), rng, true
}

func parseMonitorRange(raw string) (time.Duration, bool) {
	if raw == "" {
		return time.Hour, true
	}
	d, ok := monitorRanges[raw]
	return d, ok
}

// defaultMonitorStep derives a series bucket width from its range (plan §6)
// when the caller does not name one explicitly.
func defaultMonitorStep(rng time.Duration) time.Duration {
	switch rng {
	case 15 * time.Minute:
		return 10 * time.Second
	case time.Hour:
		return 30 * time.Second
	case 6 * time.Hour:
		return time.Minute
	case 24 * time.Hour:
		return 5 * time.Minute
	case 168 * time.Hour:
		return 30 * time.Minute
	default:
		// A picked range: about 240 points, in whole 10 s steps.
		step := (rng/240 + 10*time.Second - 1) / (10 * time.Second) * (10 * time.Second)
		return max(step, 10*time.Second)
	}
}

var monitorGroupKeys = map[string]bool{"": true, "node": true, "deployment": true, "instance": true, "gpu": true}

var monitorAggregations = map[string]bool{"": true, "avg": true, "sum": true, "max": true, "rate": true}

// monitorOverview serves GET /api/v1/monitor/overview.
func (a *apiHandlers) monitorOverview(w http.ResponseWriter, r *http.Request) {
	svc, rng, ok := a.monitorWindow(w, r)
	if !ok {
		return
	}
	if svc == nil {
		writeJSON(w, http.StatusOK, emptyMonitorOverview())
		return
	}
	out, err := svc.Overview(r.Context(), rng)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// monitorDeployment serves GET /api/v1/monitor/deployments/{id}.
func (a *apiHandlers) monitorDeployment(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	if id == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorDeploymentIDInvalid, "invalid deployment id", nil)
		return
	}
	svc, rng, ok := a.monitorWindow(w, r)
	if !ok {
		return
	}
	if svc == nil {
		writeErr(w, http.StatusNotFound, ErrMonitorDeploymentNotFound)
		return
	}
	out, err := svc.Deployment(r.Context(), id, rng)
	if err != nil {
		if errors.Is(err, ErrMonitorDeploymentNotFound) {
			a.writeMonitorDeploymentGone(w, id, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// monitorSeries serves GET /api/v1/monitor/series.
func (a *apiHandlers) monitorSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metric := q.Get("metric")
	if metric == "" {
		writeProductError(w, http.StatusBadRequest, ErrorMetricRequired, "metric required", nil)
		return
	}
	by := q.Get("by")
	if !monitorGroupKeys[by] {
		writeProductError(w, http.StatusBadRequest, ErrorMonitorGroupInvalid, "invalid group", nil)
		return
	}
	fn := q.Get("fn")
	if !monitorAggregations[fn] {
		writeProductError(w, http.StatusBadRequest, ErrorMonitorAggregationInvalid, "invalid aggregation", nil)
		return
	}
	if fn == "" {
		fn = "avg"
	}
	svc, rng, ok := a.monitorWindow(w, r)
	if !ok {
		return
	}
	// step is always clamped up to the range's own recommended minimum,
	// never just defaulted when absent: an explicit, finer-than-recommended
	// step over a long range could otherwise ask for an enormous bucket
	// count (168h at 1s, say - a 7-day range needs its step floor to keep
	// the response bounded), which is exactly the unbounded-response shape
	// the series/point caps below exist to catch - clamping here means a
	// well-behaved caller never gets close to them.
	step := parseDur(q.Get("step"), 0)
	if minStep := defaultMonitorStep(rng); step < minStep {
		step = minStep
	}
	if svc == nil {
		writeJSON(w, http.StatusOK, dto.NewList([]dto.MonitorSeries{}))
		return
	}
	out, err := svc.Series(r.Context(), metric, by, fn, rng, step)
	if err != nil {
		if errors.Is(err, ErrMonitorSeriesTooLarge) {
			writeProductError(w, http.StatusBadRequest, ErrorMonitorSeriesTooLarge, "series request too large", nil)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if out == nil {
		out = []dto.MonitorSeries{}
	}
	a.nameSeries(out)
	writeJSON(w, http.StatusOK, dto.NewList(out))
}

// monitorMetrics serves GET /api/v1/monitor/metrics: the catalog of every
// stored series the platform defines.
func (a *apiHandlers) monitorMetrics(w http.ResponseWriter, _ *http.Request) {
	if a.d.Monitor == nil {
		writeJSON(w, http.StatusOK, dto.NewList[dto.MonitorMetricSeries](nil))
		return
	}
	writeJSON(w, http.StatusOK, dto.NewList(a.d.Monitor.Metrics()))
}

// emptyMonitorOverview is what monitorOverview answers when Monitor is not
// wired (e.g. a Deps{} built for an unrelated test): every list wire-shaped
// empty rather than null, matching a real overview's zero-data shape.
func emptyMonitorOverview() dto.MonitorOverview {
	return dto.MonitorOverview{
		Signals:     []dto.MonitorSignal{},
		Services:    []dto.MonitorServiceRow{},
		StateCounts: []dto.MonitorStateCount{},
		VRAMByNode:  []dto.MonitorVRAMNode{},
		ClusterTokens: dto.MonitorClusterTokens{
			Prompt:     dto.MonitorSeries{Labels: map[string]string{}, Points: []dto.Point{}},
			Generation: dto.MonitorSeries{Labels: map[string]string{}, Points: []dto.Point{}},
		},
		Events: []dto.MonitorEvent{},
	}
}

// writeMonitorDeploymentGone answers 404 for a deployment the monitor does
// not know: MONITOR_DEPLOYMENT_DELETED with the names a deleted one left, so
// a link to it still reads as what it was; err for one never known.
func (a *apiHandlers) writeMonitorDeploymentGone(w http.ResponseWriter, id uint, err error) {
	if a.d.Store != nil {
		if gone, lookupErr := a.d.Store.DeletedDeployments([]uint{id}); lookupErr == nil {
			if d, ok := gone[id]; ok {
				writeProductError(w, http.StatusNotFound, ErrorMonitorDeploymentDeleted, "deployment "+d.Name+" was deleted",
					map[string]any{"deployment_id": id, "name": d.Name, "served_name": d.ServedName})
				return
			}
		}
	}
	writeErr(w, http.StatusNotFound, err)
}
