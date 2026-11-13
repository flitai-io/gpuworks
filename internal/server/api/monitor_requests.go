package api

import (
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// ErrMonitorRequestsQueryInvalid is the sentinel the monitor service returns
// for a request query it refuses (an unknown cursor or result name).
var ErrMonitorRequestsQueryInvalid = errors.New("monitor requests query invalid")

// monitorRequests serves GET /api/v1/monitor/deployments/{id}/requests: the
// deployment's in-flight requests (view=live) or its finished ones within
// range, or from..to (view=history, paged by cursor). key narrows to one API
// key and instance to one replica; history also filters by result (a comma
// list) and by end-to-end time (min_ms, max_ms).
func (a *apiHandlers) monitorRequests(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	if id == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorDeploymentIDInvalid, "invalid deployment id", nil)
		return
	}
	q := r.URL.Query()
	view := q.Get("view")
	if view == "" {
		view = "live"
	}
	if view != "live" && view != "history" {
		writeProductError(w, http.StatusBadRequest, ErrorMonitorRequestsViewInvalid, "view must be live or history", nil)
		return
	}
	rng, ok := parseMonitorRange(q.Get("range"))
	if !ok {
		writeProductError(w, http.StatusBadRequest, ErrorMonitorRangeInvalid, "invalid range", nil)
		return
	}
	query := dto.MonitorRequestsQuery{View: view, RangeMS: rng.Milliseconds(), Cursor: q.Get("cursor"), InstanceUUID: q.Get("instance")}
	if msg := parseHistoryFilters(q, &query); msg != "" {
		writeProductError(w, http.StatusBadRequest, ErrorMonitorRequestsQueryInvalid, msg, nil)
		return
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 500 {
			writeProductError(w, http.StatusBadRequest, ErrorMonitorRequestsQueryInvalid, "limit must be 1..500", nil)
			return
		}
		query.Limit = limit
	}
	if raw := q.Get("key"); raw != "" {
		key, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || key == 0 {
			writeProductError(w, http.StatusBadRequest, ErrorMonitorRequestsQueryInvalid, "key must be an API key id", nil)
			return
		}
		query.APIKeyID = uint(key)
	}
	if a.d.Monitor == nil {
		writeErr(w, http.StatusNotFound, ErrMonitorDeploymentNotFound)
		return
	}
	out, err := a.d.Monitor.Requests(r.Context(), id, query)
	switch {
	case errors.Is(err, ErrMonitorDeploymentNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, ErrMonitorRequestsQueryInvalid):
		writeProductError(w, http.StatusBadRequest, ErrorMonitorRequestsQueryInvalid, "invalid cursor or result name", nil)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// parseHistoryFilters reads the history filters into query, or says what is
// wrong with them: from and to are Unix milliseconds and come together,
// results is a comma list of result names (the monitor service knows them
// and refuses an unknown one), min_ms and max_ms bound the end-to-end time.
func parseHistoryFilters(q url.Values, query *dto.MonitorRequestsQuery) string {
	millis := func(name string) (int64, bool) {
		raw := q.Get(name)
		if raw == "" {
			return 0, true
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		return v, err == nil && v > 0
	}
	from, okFrom := millis("from")
	to, okTo := millis("to")
	if !okFrom || !okTo || (from == 0) != (to == 0) || (from > 0 && from >= to) {
		return "from and to must be Unix milliseconds, given together, from before to"
	}
	query.FromMS, query.ToMS = from, to
	if raw := q.Get("results"); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			query.Results = append(query.Results, strings.TrimSpace(name))
		}
	}
	bound := func(name string) (int32, bool) {
		raw := q.Get(name)
		if raw == "" {
			return 0, true
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		return int32(v), err == nil && v > 0 && v <= math.MaxInt32
	}
	minMS, okMin := bound("min_ms")
	maxMS, okMax := bound("max_ms")
	if !okMin || !okMax || (minMS > 0 && maxMS > 0 && minMS > maxMS) {
		return "min_ms and max_ms must be positive milliseconds, min not above max"
	}
	query.MinEndMS, query.MaxEndMS = minMS, maxMS
	return ""
}

// monitorRequestLookup serves GET /api/v1/monitor/requests/lookup: the
// finished requests of a trace (trace_id) or the one request of a request id
// (request_id), across deployments. from and to (Unix milliseconds, given
// together) bound the search; without them the monitor service picks a
// window (monitorapi/lookup.go).
func (a *apiHandlers) monitorRequestLookup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	window := dto.MonitorRequestsQuery{}
	if msg := parseHistoryFilters(url.Values{"from": q["from"], "to": q["to"]}, &window); msg != "" {
		writeProductError(w, http.StatusBadRequest, ErrorMonitorRequestsQueryInvalid, msg, nil)
		return
	}
	query := dto.MonitorRequestLookupQuery{TraceID: q.Get("trace_id"), RequestID: q.Get("request_id"), FromMS: window.FromMS, ToMS: window.ToMS}
	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 500 {
			writeProductError(w, http.StatusBadRequest, ErrorMonitorRequestsQueryInvalid, "limit must be 1..500", nil)
			return
		}
		query.Limit = limit
	}
	if a.d.Monitor == nil {
		writeErr(w, http.StatusNotFound, ErrMonitorDeploymentNotFound)
		return
	}
	out, err := a.d.Monitor.LookupRequests(r.Context(), query)
	switch {
	case errors.Is(err, ErrMonitorRequestsQueryInvalid):
		// The service says what it refused (no id, a malformed trace id, a
		// window too long).
		writeProductError(w, http.StatusBadRequest, ErrorMonitorRequestsQueryInvalid, err.Error(), nil)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// monitorLatency serves GET /api/v1/monitor/deployments/{id}/latency: the
// latency breakdown of the deployment's whole replies within range.
func (a *apiHandlers) monitorLatency(w http.ResponseWriter, r *http.Request) {
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
	out, err := svc.Latency(r.Context(), id, rng)
	switch {
	case errors.Is(err, ErrMonitorDeploymentNotFound):
		writeErr(w, http.StatusNotFound, err)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// monitorDistributions serves GET /api/v1/monitor/distributions: the input /
// output and uncached-input / first-token distributions of the requests
// finished within range, of one deployment or (without deployment) all.
func (a *apiHandlers) monitorDistributions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	svc, rng, ok := a.monitorWindow(w, r)
	if !ok {
		return
	}
	var deployment uint
	if raw := q.Get("deployment"); raw != "" {
		if deployment = parseUint(raw); deployment == 0 {
			writeProductError(w, http.StatusBadRequest, ErrorDeploymentIDInvalid, "invalid deployment id", nil)
			return
		}
	}
	if svc == nil {
		writeErr(w, http.StatusNotFound, ErrMonitorDeploymentNotFound)
		return
	}
	out, err := svc.Distributions(r.Context(), deployment, rng)
	switch {
	case errors.Is(err, ErrMonitorDeploymentNotFound):
		writeErr(w, http.StatusNotFound, err)
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}
