package api

import (
	"net/http"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// Log severity levels accepted by the logs API; the filter is >= semantics
// (level=warn matches warn, error and fatal).
const (
	logLevelDebug = "debug"
	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"
	logLevelFatal = "fatal"
)

// Log sources accepted by the logs API. Instance logs are the default view;
// node/agent/server are infrastructure logs.
const (
	logSourceInstance = "instance"
	logSourceNode     = "node"
	logSourceAgent    = "agent"
	logSourceServer   = "server"
)

var logLevels = map[string]bool{logLevelDebug: true, logLevelInfo: true, logLevelWarn: true, logLevelError: true, logLevelFatal: true}
var logSources = map[string]bool{logSourceInstance: true, logSourceNode: true, logSourceAgent: true, logSourceServer: true}

// LogEntry is one line as served over the wire.
type LogEntry struct {
	ID       int64  `json:"id"`
	Instance string `json:"instance"`
	Node     string `json:"node"`
	Level    string `json:"level"`
	Source   string `json:"source"`
	TimeMS   int64  `json:"time_ms"`
	Line     string `json:"line"`
}

// LogsPage is one newest-first page of log lines with optional totals.
type LogsPage struct {
	Items       []LogEntry `json:"items"`
	Total       int64      `json:"total,omitempty"`
	TotalCapped bool       `json:"total_capped,omitempty"`
	// Truncated marks a full page with more matching rows behind it - the
	// store's own call (logstore.Result.Truncated), surfaced as
	// X-Logs-Truncated.
	Truncated bool `json:"truncated,omitempty"`
}

// LogsWindow is a context window: chronological lines around one match.
type LogsWindow struct {
	Items      []LogEntry `json:"items"`
	MatchIndex int        `json:"match_index"`
}

// LogsBucket is one histogram time bucket keyed by severity.
type LogsBucket struct {
	StartMS int64            `json:"start_ms"`
	Counts  map[string]int64 `json:"counts"`
}

// logsInstancesMax bounds the "instances" query parameter. It deliberately
// mirrors logstore.MaxInstances by value rather than by import: this
// package is decoupled from logstore/tsdb via the LogsSearch/LogsHistogram
// callbacks (see api.Deps), so the API's own pre-validation (returning a
// stable product error code before ever calling into the store) has to
// carry its own copy of the cap instead of referencing the store's
// constant directly.
const logsInstancesMax = 64

// LogsFilter mirrors the query parameters of the logs endpoints.
type LogsFilter struct {
	Query    string
	Instance string
	// Instances is the parsed "instances" query parameter (comma-separated
	// UUIDs): an explicit set of instances to merge into one search/
	// histogram, mutually exclusive with Instance and capped at
	// logsInstancesMax. Empty (nil) means unset.
	Instances []string
	Node      string
	Level     string
	Source    string
	FromMS    int64
	ToMS      int64
	CursorTS  int64
	CursorID  int64
	Limit     int
	WithTotal bool
	// SourceSet records whether the query carried a source parameter at all:
	// absent keeps the historical default view (instance logs only), while an
	// explicit empty value means every source.
	SourceSet bool
}

// parseLogsFilter reads the shared query parameters. The logstore treats
// zero fields as "no constraint".
func parseLogsFilter(q map[string][]string) (LogsFilter, bool) {
	get := func(key string) string {
		if vs, ok := q[key]; ok && len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	intOf := func(key string) int64 {
		n, _ := strconv.ParseInt(get(key), 10, 64)
		return n
	}
	f := LogsFilter{
		Query:     get("q"),
		Instance:  get("instance"),
		Instances: parseInstancesParam(get("instances")),
		Node:      get("node"),
		Level:     get("level"),
		Source:    get("source"),
		FromMS:    intOf("from"),
		ToMS:      intOf("to"),
		CursorTS:  intOf("cursor_ts"),
		CursorID:  intOf("cursor_id"),
		WithTotal: get("with_total") == "1" || get("with_total") == "true",
	}
	f.SourceSet = func() bool { _, ok := q["source"]; return ok }()
	// The logs search ceiling: a window may hold far more lines than a viewer
	// should render at once, so requests are clamped here to logsSearchMaxLimit
	// (mirroring logstore.MaxLimit). Whether the match set is actually bigger
	// than the page is the store's own call (logstore.Result.Truncated,
	// surfaced here via the X-Logs-Truncated header) - not something this
	// handler derives by probing with limit+1 itself.
	limit, _ := strconv.Atoi(get("limit"))
	if limit <= 0 {
		limit = logsSearchDefaultLimit
	}
	if limit > logsSearchMaxLimit {
		limit = logsSearchMaxLimit
	}
	f.Limit = limit
	if f.FromMS > 0 && f.ToMS > 0 && f.FromMS >= f.ToMS {
		f.FromMS, f.ToMS = 0, 0 // rejected below with a product error
	}
	return f, true
}

const (
	logsSearchDefaultLimit = 200
	logsSearchMaxLimit     = 5000
	// logsContextMaxLines caps each side of a context window.
	logsContextMaxLines = 1000
)

// parseInstancesParam splits the "instances" query parameter (comma-
// separated UUIDs) into a slice, dropping surrounding whitespace, empty
// segments (a trailing comma or repeated commas must not produce a phantom
// empty-string instance) and duplicates - a client that lists the same
// instance twice must not eat into the logsInstancesMax cap for it. An
// absent or empty parameter yields nil, which LogsFilter.Instances treats
// as unset.
func parseInstancesParam(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// validateLogsFilter rejects enum values the store cannot match.
func validateLogsFilter(f LogsFilter) (string, bool) {
	if f.Level != "" && !logLevels[f.Level] {
		return f.Level, false
	}
	if f.Source != "" && !logSources[f.Source] {
		return f.Source, false
	}
	return "", true
}

// validateLogsInstances rejects the instances combos logstore.Filter cannot
// execute (see logstore.Filter.Instances' doc comment): Instance and
// Instances set together, or Instances beyond logsInstancesMax. Returns the
// product error code to report, or "" when the combo is valid.
func validateLogsInstances(f LogsFilter) string {
	if f.Instance != "" && len(f.Instances) > 0 {
		return ErrorLogsFilterInstancesConflict
	}
	if len(f.Instances) > logsInstancesMax {
		return ErrorLogsFilterInstancesTooMany
	}
	return ""
}

// logsSearch serves keyword search and the recent-tail default over the
// partitioned logstore.
// GET /api/v1/logs/search?q=..&instance=..&node=..&level=warn&source=instance
//
//	&from=..&to=..&cursor_ts=..&cursor_id=..&limit=..&with_total=1
func (a *apiHandlers) logsSearch(w http.ResponseWriter, r *http.Request) {
	if !validateTextField(w, "q", r.URL.Query().Get("q"), 4096) {
		return
	}
	if a.d.LogsSearch == nil {
		writeJSON(w, http.StatusOK, LogsPage{Items: []LogEntry{}})
		return
	}
	f, _ := parseLogsFilter(r.URL.Query())
	if !f.SourceSet {
		f.Source = logSourceInstance
	}
	if code := validateLogsInstances(f); code != "" {
		writeProductError(w, http.StatusBadRequest, code, "invalid instances filter", map[string]any{"count": len(f.Instances), "maximum": logsInstancesMax})
		return
	}
	if bad, ok := validateLogsFilter(f); !ok {
		writeProductError(w, http.StatusBadRequest, ErrorLogsFilterInvalid, "invalid log filter value", map[string]any{"value": bad})
		return
	}
	if q := r.URL.Query(); q.Get("from") != "" || q.Get("to") != "" {
		var from, to int64
		from, _ = strconv.ParseInt(q.Get("from"), 10, 64)
		to, _ = strconv.ParseInt(q.Get("to"), 10, 64)
		if from > 0 && to > 0 && from >= to {
			writeProductError(w, http.StatusBadRequest, ErrorQueryWindowInvalid, "from must be before to", nil)
			return
		}
	}
	// Truncation is the store's own call now (logstore.Result.Truncated):
	// it probes one row past the effective limit internally, which is the
	// only way to get this right at the MaxLimit boundary - a limit+1 probe
	// done here, on top of the store's own clamp, could get silently
	// clamped right back down and lose the signal.
	page, err := a.d.LogsSearch(f)
	if err != nil {
		a.writeLogQueryError(w, err)
		return
	}
	if page.Items == nil {
		page.Items = []LogEntry{}
	}
	if page.Truncated {
		w.Header().Set("X-Logs-Truncated", "1")
	}
	writeJSON(w, http.StatusOK, page)
}

// logsContext serves a grep-style window of lines around one match, within
// the match's own instance. before/after are capped at logsContextMaxLines each.
// GET /api/v1/logs/context?instance=..&ts=..&id=..&before=5&after=5
func (a *apiHandlers) logsContext(w http.ResponseWriter, r *http.Request) {
	if a.d.LogsContext == nil {
		writeJSON(w, http.StatusOK, LogsWindow{Items: []LogEntry{}})
		return
	}
	q := r.URL.Query()
	instance := q.Get("instance")
	node := q.Get("node")
	ts, _ := strconv.ParseInt(q.Get("ts"), 10, 64)
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	before, _ := strconv.Atoi(q.Get("before"))
	after, _ := strconv.Atoi(q.Get("after"))
	// Infrastructure rows (agent/server sources) carry no instance UUID: the
	// window is then scoped to the node instead. Each side is one indexed
	// range scan with a LIMIT, run only on an explicit click, so the cap is
	// about response size, not query cost.
	if (instance == "" && node == "") || ts <= 0 || id <= 0 || before < 0 || after < 0 || before > logsContextMaxLines || after > logsContextMaxLines {
		writeProductError(w, http.StatusBadRequest, ErrorLogsContextInvalid, "context needs a row reference (instance or node), ts, id and a 0-1000 before/after window", nil)
		return
	}
	window, err := a.d.LogsContext(instance, node, ts, id, before, after)
	if err != nil {
		a.writeLogQueryError(w, err)
		return
	}
	if window.Items == nil {
		window.Items = []LogEntry{}
	}
	writeJSON(w, http.StatusOK, window)
}

// logsHistogram counts lines per time bucket and severity for the same
// filters as search. Bucket size is in ms and clamped to 1s..24h.
// GET /api/v1/logs/histogram?q=..&level=..&from=..&to=..&bucket=60000
func (a *apiHandlers) logsHistogram(w http.ResponseWriter, r *http.Request) {
	if !validateTextField(w, "q", r.URL.Query().Get("q"), 4096) {
		return
	}
	if a.d.LogsHistogram == nil {
		writeJSON(w, http.StatusOK, dto.NewList[LogsBucket](nil))
		return
	}
	f, _ := parseLogsFilter(r.URL.Query())
	if !f.SourceSet {
		f.Source = logSourceInstance
	}
	if code := validateLogsInstances(f); code != "" {
		writeProductError(w, http.StatusBadRequest, code, "invalid instances filter", map[string]any{"count": len(f.Instances), "maximum": logsInstancesMax})
		return
	}
	if bad, ok := validateLogsFilter(f); !ok {
		writeProductError(w, http.StatusBadRequest, ErrorLogsFilterInvalid, "invalid log filter value", map[string]any{"value": bad})
		return
	}
	bucket, _ := strconv.ParseInt(r.URL.Query().Get("bucket"), 10, 64)
	if bucket < 1000 || bucket > 24*60*60*1000 {
		bucket = 60_000
	}
	buckets, err := a.d.LogsHistogram(f, bucket)
	if err != nil {
		a.writeLogQueryError(w, err)
		return
	}
	if buckets == nil {
		buckets = []LogsBucket{}
	}
	writeJSON(w, http.StatusOK, dto.NewList(buckets))
}

// Backend details belong in server logs, never in a log-query response.
func (a *apiHandlers) writeLogQueryError(w http.ResponseWriter, err error) {
	if a.d.Log != nil {
		a.d.Log.Error("log query failed", "error", err)
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "log query failed"})
}
