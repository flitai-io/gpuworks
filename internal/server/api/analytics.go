package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// ErrAnalyticsUnavailable answers the analytics routes of a server built
// without the service (a harness).
var ErrAnalyticsUnavailable = errors.New("usage analytics is not available")

// Analytics is usage analytics as the API sees it: the data retention
// setting, applied the moment it is saved, and what it keeps.
type Analytics interface {
	Retention() dto.DataRetentionView
	// SetRetention validates and applies a new setting; an invalid value is
	// a *store.DataRetentionError.
	SetRetention(dto.DataRetention) (dto.DataRetentionView, error)
	RetentionStatus(ctx context.Context) (dto.DataRetentionStatus, error)

	// The statistics over a range. A query the service refuses is a
	// *messages.ProductError (answered 400 with its code and params).
	Usage(ctx context.Context, q AnalyticsQuery) (dto.UsageAnalytics, error)
	Quality(ctx context.Context, q AnalyticsQuery) (dto.QualityAnalytics, error)
	GPUs(ctx context.Context, q AnalyticsQuery) (dto.GPUAnalytics, error)
	GPUTime(ctx context.Context, q AnalyticsQuery) (dto.GPUTimeAnalytics, error)
	KeyUsage(ctx context.Context) (dto.KeyUsageList, error)
}

// AnalyticsQuery is a statistics query's parameters: the range (Unix ms)
// and its granularity (hour, day, or "" to pick by span), and the filters
// each statistic takes.
type AnalyticsQuery struct {
	FromMS, ToMS int64
	Granularity  string
	DeploymentID uint
	// APIKeyID filters the usage by key; Keyless asks for the requests
	// without one; Source is api, playground or "".
	APIKeyID uint
	Keyless  bool
	Source   string
	// The GPU statistics: rows (model, node, gpu), the heat map's colour
	// (util, power, temp, mem), and the cards: of the nodes a label filter
	// matched (NodeUUIDs; nil without one) and of a model.
	GroupBy, Metric string
	NodeUUIDs       []string
	GPUModel        string
}

func (a *apiHandlers) getDataRetention(w http.ResponseWriter, _ *http.Request) {
	if a.d.Analytics == nil {
		writeErr(w, http.StatusNotFound, ErrAnalyticsUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, a.d.Analytics.Retention())
}

func (a *apiHandlers) putDataRetention(w http.ResponseWriter, r *http.Request) {
	if a.d.Analytics == nil {
		writeErr(w, http.StatusNotFound, ErrAnalyticsUnavailable)
		return
	}
	// The setting is replaced whole: a value left out would silently become
	// zero or off, so every one must be there and nothing else.
	var raw map[string]json.RawMessage
	if !decodeJSONBody(w, r, &raw) {
		return
	}
	known := dataRetentionFields()
	for name := range raw {
		if !known[name] {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "unknown field "+name, map[string]any{"field": name, "detail": "unknown field"})
			return
		}
	}
	for name := range known {
		if _, ok := raw[name]; !ok {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, name+" is required", map[string]any{"field": name, "detail": "required"})
			return
		}
	}
	var req dto.DataRetention
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, &req); err != nil {
		field := ""
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			field = typeErr.Field
		}
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "a value has the wrong type", map[string]any{"field": field, "detail": "wrong type"})
		return
	}
	view, err := a.d.Analytics.SetRetention(req)
	var invalid *store.DataRetentionError
	switch {
	case errors.As(err, &invalid):
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, invalid.Error(), map[string]any{"field": invalid.Field, "detail": invalid.Detail})
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
	default:
		writeJSON(w, http.StatusOK, view)
	}
}

func (a *apiHandlers) getDataRetentionStatus(w http.ResponseWriter, r *http.Request) {
	if a.d.Analytics == nil {
		writeErr(w, http.StatusNotFound, ErrAnalyticsUnavailable)
		return
	}
	status, err := a.d.Analytics.RetentionStatus(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// dataRetentionFields are dto.DataRetention's JSON names.
func dataRetentionFields() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(dto.DataRetention{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		out[name] = true
	}
	return out
}

// analyticsQuery reads the query parameters every statistics route shares
// and the ones it names; a malformed one is a 400 product error.
func analyticsQuery(r *http.Request) (AnalyticsQuery, error) {
	v := r.URL.Query()
	var q AnalyticsQuery
	bad := func(field string) error {
		return messages.NewProductError(messages.ErrorAnalyticsValueInvalid, field+" is malformed", map[string]any{"field": field, "detail": "malformed"})
	}
	var err error
	if q.FromMS, err = strconv.ParseInt(v.Get("from"), 10, 64); err != nil {
		return q, messages.NewProductError(messages.ErrorAnalyticsRangeInvalid, "from is required", map[string]any{"field": "from", "detail": "required, Unix milliseconds"})
	}
	if q.ToMS, err = strconv.ParseInt(v.Get("to"), 10, 64); err != nil {
		return q, messages.NewProductError(messages.ErrorAnalyticsRangeInvalid, "to is required", map[string]any{"field": "to", "detail": "required, Unix milliseconds"})
	}
	q.Granularity = v.Get("granularity")
	if s := v.Get("deployment"); s != "" {
		id, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return q, bad("deployment")
		}
		q.DeploymentID = uint(id)
	}
	switch s := v.Get("key"); s {
	case "":
	case "none":
		q.Keyless = true
	default:
		id, err := strconv.ParseUint(s, 10, 64)
		if err != nil || id == 0 {
			return q, bad("key")
		}
		q.APIKeyID = uint(id)
	}
	switch q.Source = v.Get("source"); q.Source {
	case "", "api", "playground", "batch":
	default:
		return q, bad("source")
	}
	q.GroupBy, q.Metric = v.Get("group_by"), v.Get("metric")
	q.GPUModel = v.Get("model")
	return q, nil
}

// labelNodes are the UUIDs of the nodes the request's label filter
// (label=key=value, repeatable, as the node list takes it) matches now, or
// nil without one. A node since removed carries no labels, so a label
// filter leaves its statistics out.
func (a *apiHandlers) labelNodes(r *http.Request) ([]string, error) {
	if len(r.URL.Query()["label"]) == 0 {
		return nil, nil
	}
	reqs, err := parseLabelFilter(r)
	if err != nil {
		return nil, err
	}
	nodes, err := a.d.Store.FleetNodes()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, n := range a.filterNodes(nodes, listQuery{}, reqs) {
		out = append(out, n.UUID)
	}
	return out, nil
}

// analyticsRoute serves one statistics route.
func analyticsRoute[T any](a *apiHandlers, get func(Analytics, context.Context, AnalyticsQuery) (T, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.d.Analytics == nil {
			writeErr(w, http.StatusNotFound, ErrAnalyticsUnavailable)
			return
		}
		q, err := analyticsQuery(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		var refused *messages.ProductError
		if q.NodeUUIDs, err = a.labelNodes(r); errors.As(err, &refused) {
			writeErr(w, http.StatusBadRequest, err)
			return
		} else if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		out, err := get(a.d.Analytics, r.Context(), q)
		switch {
		case errors.As(err, &refused):
			writeErr(w, http.StatusBadRequest, err)
		case err != nil:
			writeErr(w, http.StatusInternalServerError, err)
		default:
			writeJSON(w, http.StatusOK, out)
		}
	}
}

func (a *apiHandlers) getKeyUsage(w http.ResponseWriter, r *http.Request) {
	if a.d.Analytics == nil {
		writeErr(w, http.StatusNotFound, ErrAnalyticsUnavailable)
		return
	}
	out, err := a.d.Analytics.KeyUsage(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
