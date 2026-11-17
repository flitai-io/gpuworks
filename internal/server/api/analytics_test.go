package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type fakeAnalytics struct {
	view     dto.DataRetentionView
	set      []dto.DataRetention
	setErr   error
	status   dto.DataRetentionStatus
	statErr  error
	queries  []AnalyticsQuery
	queryErr error
}

func (f *fakeAnalytics) Retention() dto.DataRetentionView { return f.view }

func (f *fakeAnalytics) SetRetention(r dto.DataRetention) (dto.DataRetentionView, error) {
	f.set = append(f.set, r)
	if f.setErr != nil {
		return dto.DataRetentionView{}, f.setErr
	}
	f.view.Value = r
	return f.view, nil
}

func (f *fakeAnalytics) RetentionStatus(context.Context) (dto.DataRetentionStatus, error) {
	return f.status, f.statErr
}

func (f *fakeAnalytics) Usage(_ context.Context, q AnalyticsQuery) (dto.UsageAnalytics, error) {
	f.queries = append(f.queries, q)
	return dto.UsageAnalytics{Range: dto.AnalyticsRange{FromMS: q.FromMS, ToMS: q.ToMS, Granularity: "hour", Buckets: []int64{q.FromMS}},
		Series: []dto.UsageCounts{{Requests: 1}}, Rows: []dto.UsageRow{}}, f.queryErr
}

func (f *fakeAnalytics) Quality(_ context.Context, q AnalyticsQuery) (dto.QualityAnalytics, error) {
	f.queries = append(f.queries, q)
	return dto.QualityAnalytics{}, f.queryErr
}

func (f *fakeAnalytics) GPUs(_ context.Context, q AnalyticsQuery) (dto.GPUAnalytics, error) {
	f.queries = append(f.queries, q)
	return dto.GPUAnalytics{}, f.queryErr
}

func (f *fakeAnalytics) GPUTime(_ context.Context, q AnalyticsQuery) (dto.GPUTimeAnalytics, error) {
	f.queries = append(f.queries, q)
	return dto.GPUTimeAnalytics{}, f.queryErr
}

func (f *fakeAnalytics) KeyUsage(context.Context) (dto.KeyUsageList, error) {
	return dto.KeyUsageList{Items: []dto.KeyUsage{{Prefix: "gw-1", Today: dto.UsageCounts{Requests: 2}}}, TimeZone: "UTC"}, f.queryErr
}

const retentionBody = `{"metrics_hours":72,"logs_hours":48,"request_log_days":7,"request_log_max_mb":2048,` +
	`"usage_hourly_days":31,"usage_daily_days":400,"stats_hourly_days":31,"stats_daily_days":400,` +
	`"quality":true,"gpu":false,"gpu_time":true,"time_zone":"Asia/Shanghai"}`

func TestDataRetentionRoutes(t *testing.T) {
	fake := &fakeAnalytics{view: dto.DataRetentionView{
		Limits:         map[string]dto.DataRetentionLimit{"metrics_hours": {Min: 24, Max: 720}},
		ServerTimeZone: "Asia/Shanghai",
	}}
	signer, err := auth.NewTokenSigner("analytics-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	admin, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	viewer, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "viewer", Role: "viewer"})
	rig := struct {
		st      *store.Store
		session string
	}{st, admin}
	r := Router(Deps{Store: st, AuthSigner: signer, Analytics: fake})
	call := func(method, path, token, body string) (int, string) {
		rec := telemetryCall(r, method, path, token, body)
		return rec.Code, rec.Body.String()
	}
	if code, _ := call(http.MethodGet, "/api/v1/data-retention", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous get = %d", code)
	}
	if code, _ := call(http.MethodPut, "/api/v1/data-retention", "", retentionBody); code != http.StatusUnauthorized {
		t.Fatalf("anonymous put = %d", code)
	}
	if code, _ := call(http.MethodPost, "/api/v1/data-retention", rig.session, retentionBody); code != http.StatusMethodNotAllowed {
		t.Fatalf("post = %d", code)
	}

	// Anyone signed in reads it; only an administrator changes it.
	if code, _ := call(http.MethodGet, "/api/v1/data-retention", viewer, ""); code != http.StatusOK {
		t.Fatalf("viewer get = %d", code)
	}
	if code, body := call(http.MethodPut, "/api/v1/data-retention", viewer, retentionBody); code != http.StatusForbidden || len(fake.set) != 0 {
		t.Fatalf("viewer put = %d %s", code, body)
	}
	code, body := call(http.MethodPut, "/api/v1/data-retention", rig.session, retentionBody)
	if code != http.StatusOK || len(fake.set) != 1 {
		t.Fatalf("put = %d %s", code, body)
	}
	if got := fake.set[0]; got.LogsHours != 48 || got.GPU || !got.GPUTime || got.TimeZone != "Asia/Shanghai" || got.UsageDailyDays != 400 {
		t.Fatalf("decoded = %+v", got)
	}
	// The wire form: the value, the bounds and the server's zone.
	var view map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &view); err != nil || string(view["value"]) != retentionBody ||
		string(view["limits"]) != `{"metrics_hours":{"min":24,"max":720}}` || string(view["server_time_zone"]) != `"Asia/Shanghai"` {
		t.Fatalf("response = %s", body)
	}
	if code, body := call(http.MethodGet, "/api/v1/data-retention", rig.session, ""); code != http.StatusOK || !strings.Contains(body, `"logs_hours":48`) {
		t.Fatalf("get = %d %s", code, body)
	}

	// The setting is replaced whole: a missing, unknown or mistyped value is
	// refused before it reaches the service.
	for _, tc := range []struct{ body, field string }{
		{strings.Replace(retentionBody, `"gpu":false,`, ``, 1), "gpu"},
		{strings.Replace(retentionBody, `}`, `,"extra":1}`, 1), "extra"},
		{strings.Replace(retentionBody, `"logs_hours":48`, `"logs_hours":"48"`, 1), "logs_hours"},
	} {
		code, body := call(http.MethodPut, "/api/v1/data-retention", rig.session, tc.body)
		if code != http.StatusBadRequest || !strings.Contains(body, `"code":"FIELD_INVALID"`) || !strings.Contains(body, `"field":"`+tc.field+`"`) {
			t.Errorf("%s: %d %s", tc.field, code, body)
		}
	}
	if code, _ := call(http.MethodPut, "/api/v1/data-retention", rig.session, `not json`); code != http.StatusBadRequest {
		t.Fatalf("not json = %d", code)
	}
	if len(fake.set) != 1 {
		t.Fatalf("refused bodies reached the service: %d", len(fake.set))
	}
	// The service's validation error names its field.
	fake.setErr = &store.DataRetentionError{Field: "usage_hourly_days", Detail: "must be between 7 and 93"}
	if code, body := call(http.MethodPut, "/api/v1/data-retention", rig.session, retentionBody); code != http.StatusBadRequest ||
		!strings.Contains(body, `"field":"usage_hourly_days"`) || !strings.Contains(body, `"detail":"must be between 7 and 93"`) {
		t.Fatalf("invalid = %d %s", code, body)
	}
	fake.setErr = errors.New("disk full")
	if code, _ := call(http.MethodPut, "/api/v1/data-retention", rig.session, retentionBody); code != http.StatusInternalServerError {
		t.Fatalf("failure = %d", code)
	}

	fake.status = dto.DataRetentionStatus{
		Stores:      []dto.DataRetentionStore{{Kind: "usage", File: "main", HourlyRows: new(int64)}},
		LiveSinceMS: 5, Backfill: dto.StatisticsBackfill{UsageDone: true, UsageThroughMS: 3},
		UsageFlush: dto.UsageFlush{AtMS: 9, Rows: 2, MS: 1},
	}
	code, body = call(http.MethodGet, "/api/v1/data-retention/status", rig.session, "")
	want := `{"stores":[{"kind":"usage","file":"main","file_bytes":null,"hourly_rows":0,"daily_rows":null,"earliest_ms":null}],` +
		`"live_since_ms":5,"backfill":{"usage_done":true,"usage_through_ms":3,"quality_done":false,"quality_through_ms":0},` +
		`"retain":{"at_ms":0},"rollup":{"at_ms":0},"usage_flush":{"at_ms":9,"rows":2,"ms":1}}` + "\n"
	if code != http.StatusOK || body != want {
		t.Fatalf("status = %d\n%s\nwant\n%s", code, body, want)
	}
	fake.statErr = errors.New("locked")
	if code, _ := call(http.MethodGet, "/api/v1/data-retention/status", rig.session, ""); code != http.StatusInternalServerError {
		t.Fatalf("status failure = %d", code)
	}

	// The generic settings routes leave it to its own route.
	if code, body := call(http.MethodPut, "/api/v1/settings/data_retention", rig.session, retentionBody); code != http.StatusBadRequest || !strings.Contains(body, ErrorSettingHasOwnRoute) {
		t.Fatalf("generic put = %d %s", code, body)
	}
	if err := rig.st.SetSetting(store.SettingDataRetention, retentionBody); err != nil {
		t.Fatal(err)
	}
	if code, body := call(http.MethodGet, "/api/v1/settings", rig.session, ""); code != http.StatusOK || strings.Contains(body, "data_retention") {
		t.Fatalf("generic get = %d %s", code, body)
	}

	// Without the service: 404.
	bare := Router(Deps{Store: st, AuthSigner: signer})
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/data-retention", ""}, {http.MethodPut, "/api/v1/data-retention", retentionBody}, {http.MethodGet, "/api/v1/data-retention/status", ""},
	} {
		if rec := telemetryCall(bare, tc.method, tc.path, rig.session, tc.body); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s without the service = %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestAnalyticsRoutes(t *testing.T) {
	fake := &fakeAnalytics{}
	signer, err := auth.NewTokenSigner("analytics-routes", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	viewer, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "viewer", Role: "viewer"})
	r := Router(Deps{Store: st, AuthSigner: signer, Analytics: fake})
	call := func(path, token string) (int, string) {
		rec := telemetryCall(r, http.MethodGet, path, token, "")
		return rec.Code, rec.Body.String()
	}
	for _, path := range []string{"/api/v1/analytics/usage", "/api/v1/analytics/quality", "/api/v1/analytics/gpus", "/api/v1/analytics/gpu-time", "/api/v1/analytics/key-usage"} {
		if code, _ := call(path+"?from=1&to=2", ""); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d", path, code)
		}
		if rec := telemetryCall(r, http.MethodPost, path, viewer, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("post %s = %d", path, rec.Code)
		}
	}
	code, body := call("/api/v1/analytics/usage?from=1000&to=2000&granularity=day&deployment=17&key=3&source=api", viewer)
	want := `{"range":{"from_ms":1000,"to_ms":2000,"granularity":"hour","time_zone":"","earliest_ms":0,"live_since_ms":0,"buckets":[1000]},` +
		`"totals":{"requests":0,"no_usage":0,"prompt_tokens":0,"completion_tokens":0,"reasoning_tokens":0,"reasoning_reported":0,"cached_tokens":0,"cache_reported":0,"cache_reported_prompt":0},` +
		`"series":[{"requests":1,"no_usage":0,"prompt_tokens":0,"completion_tokens":0,"reasoning_tokens":0,"reasoning_reported":0,"cached_tokens":0,"cache_reported":0,"cache_reported_prompt":0}],"rows":[]}` + "\n"
	if code != http.StatusOK || body != want {
		t.Fatalf("usage = %d\n%s", code, body)
	}
	if got := fake.queries[0]; got.FromMS != 1000 || got.ToMS != 2000 || got.Granularity != "day" || got.DeploymentID != 17 || got.APIKeyID != 3 || got.Source != "api" || got.Keyless {
		t.Fatalf("parsed = %+v", got)
	}
	if code, _ := call("/api/v1/analytics/usage?from=1&to=2&key=none", viewer); code != http.StatusOK || !fake.queries[1].Keyless {
		t.Fatalf("keyless = %d %+v", code, fake.queries[1])
	}
	if code, _ := call("/api/v1/analytics/usage?from=1&to=2&source=evaluation", viewer); code != http.StatusOK || fake.queries[2].Source != "evaluation" {
		t.Fatalf("evaluation source = %d %+v", code, fake.queries[2])
	}
	fake.queries = fake.queries[:2]
	if code, _ := call("/api/v1/analytics/gpus?from=1&to=2&group_by=node&metric=temp&model=RTX", viewer); code != http.StatusOK {
		t.Fatalf("gpus = %d", code)
	}
	if got := fake.queries[2]; got.GroupBy != "node" || got.Metric != "temp" || got.NodeUUIDs != nil || got.GPUModel != "RTX" {
		t.Fatalf("gpu params = %+v", got)
	}
	// A label filter reads as the nodes it matches now, written labels and
	// system labels alike; a filter matching none is an empty list, not nil.
	for _, n := range []struct {
		uuid string
		pool string
	}{{"n-a", "inference"}, {"n-b", "inference"}, {"n-c", "training"}} {
		if _, err := st.RegisterNode(n.uuid, store.HashToken("token"), n.uuid); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetNodeLabels(n.uuid, map[string]string{"pool": n.pool}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"label=pool%3Dinference", "[n-a n-b]"},
		{"label=pool%3Dinference&label=pool%3Dtraining", "[n-a n-b n-c]"},
		{"label=gpuworks.ai%2Fnode%3Dn-c", "[n-c]"},
		{"label=pool%3Dnone", "[]"},
	} {
		at := len(fake.queries)
		if code, body := call("/api/v1/analytics/gpus?from=1&to=2&"+tc.query, viewer); code != http.StatusOK {
			t.Fatalf("%s = %d %s", tc.query, code, body)
		}
		got := append([]string(nil), fake.queries[at].NodeUUIDs...)
		sort.Strings(got)
		if fake.queries[at].NodeUUIDs == nil || fmt.Sprint(got) != tc.want {
			t.Fatalf("%s nodes = %#v, want %s", tc.query, fake.queries[at].NodeUUIDs, tc.want)
		}
	}
	before := len(fake.queries)
	if code, body := call("/api/v1/analytics/gpus?from=1&to=2&label=nokey", viewer); code != http.StatusBadRequest || !strings.Contains(body, ErrorListFilterInvalid) {
		t.Fatalf("malformed label = %d %s", code, body)
	}
	if len(fake.queries) != before {
		t.Fatal("a malformed label reached the service")
	}
	// Malformed parameters are refused before the service.
	before = len(fake.queries)
	for _, tc := range []struct{ query, code, field string }{
		{"", "ANALYTICS_RANGE_INVALID", "from"},
		{"from=1", "ANALYTICS_RANGE_INVALID", "to"},
		{"from=x&to=2", "ANALYTICS_RANGE_INVALID", "from"},
		{"from=1&to=2&deployment=abc", "ANALYTICS_VALUE_INVALID", "deployment"},
		{"from=1&to=2&key=0", "ANALYTICS_VALUE_INVALID", "key"},
		{"from=1&to=2&source=cli", "ANALYTICS_VALUE_INVALID", "source"},
	} {
		code, body := call("/api/v1/analytics/quality?"+tc.query, viewer)
		if code != http.StatusBadRequest || !strings.Contains(body, `"code":"`+tc.code+`"`) || !strings.Contains(body, `"field":"`+tc.field+`"`) {
			t.Errorf("%q: %d %s", tc.query, code, body)
		}
	}
	if len(fake.queries) != before {
		t.Fatal("malformed queries reached the service")
	}
	// The service's refusal is a 400 with its code; a failure a 500.
	fake.queryErr = messages.NewProductError(messages.ErrorAnalyticsRangeTooLong, "too long", map[string]any{"field": "granularity", "detail": "7 days"})
	if code, body := call("/api/v1/analytics/gpu-time?from=1&to=2", viewer); code != http.StatusBadRequest || !strings.Contains(body, "ANALYTICS_RANGE_TOO_LONG") {
		t.Fatalf("refused = %d %s", code, body)
	}
	fake.queryErr = errors.New("disk")
	if code, _ := call("/api/v1/analytics/usage?from=1&to=2", viewer); code != http.StatusInternalServerError {
		t.Fatalf("failure = %d", code)
	}
	if code, _ := call("/api/v1/analytics/key-usage", viewer); code != http.StatusInternalServerError {
		t.Fatalf("key usage failure = %d", code)
	}
	fake.queryErr = nil
	if code, body := call("/api/v1/analytics/key-usage", viewer); code != http.StatusOK || !strings.Contains(body, `"items":[{"prefix":"gw-1","today":{"requests":2,`) || !strings.Contains(body, `"time_zone":"UTC"`) {
		t.Fatalf("key usage = %d %s", code, body)
	}
	bare := Router(Deps{Store: st, AuthSigner: signer})
	for _, path := range []string{"/api/v1/analytics/usage?from=1&to=2", "/api/v1/analytics/key-usage"} {
		if rec := telemetryCall(bare, http.MethodGet, path, viewer, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s without the service = %d", path, rec.Code)
		}
	}
}
