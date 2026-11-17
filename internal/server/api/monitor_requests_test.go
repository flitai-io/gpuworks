package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

// The base fake answers the request views with nothing; the tests below
// drive requestsMonitor, which records arguments and can fail.
func (f *fakeMonitorService) Requests(context.Context, uint, dto.MonitorRequestsQuery) (dto.MonitorRequests, error) {
	return dto.MonitorRequests{}, nil
}

func (f *fakeMonitorService) LookupRequests(context.Context, dto.MonitorRequestLookupQuery) (dto.MonitorRequestLookup, error) {
	return dto.MonitorRequestLookup{}, nil
}

func (f *fakeMonitorService) Latency(context.Context, uint, time.Duration) (dto.MonitorLatency, error) {
	return dto.MonitorLatency{}, ErrMonitorDeploymentNotFound
}

// Health is the monitor's answer for a deployment with the check on
// (pending until one is set); off reads off, as monitorapi.Service.Health
// does.
func (f *fakeMonitorService) Health(_ uint, enabled bool, thresholdSec int) dto.DeploymentHealth {
	if !enabled {
		return dto.DeploymentHealth{Status: "off", ThresholdSec: thresholdSec, Recent: []dto.HealthProbe{}}
	}
	out := f.health
	if out.Status == "" {
		out = dto.DeploymentHealth{Status: "pending", Recent: []dto.HealthProbe{}}
	}
	out.ThresholdSec = thresholdSec
	return out
}

func (f *fakeMonitorService) Distributions(context.Context, uint, time.Duration) (dto.MonitorDistributions, error) {
	return dto.MonitorDistributions{}, nil
}

type requestsMonitor struct {
	fakeMonitorService
	requests     dto.MonitorRequests
	requestsErr  error
	requestsArgs []struct {
		id uint
		q  dto.MonitorRequestsQuery
	}
	distributions     dto.MonitorDistributions
	distributionsErr  error
	distributionsArgs []struct {
		id  uint
		rng time.Duration
	}
	latency     dto.MonitorLatency
	latencyErr  error
	latencyArgs []struct {
		id  uint
		rng time.Duration
	}
	lookup     dto.MonitorRequestLookup
	lookupErr  error
	lookupArgs []dto.MonitorRequestLookupQuery
}

func (f *requestsMonitor) LookupRequests(_ context.Context, q dto.MonitorRequestLookupQuery) (dto.MonitorRequestLookup, error) {
	f.lookupArgs = append(f.lookupArgs, q)
	return f.lookup, f.lookupErr
}

func (f *requestsMonitor) Latency(_ context.Context, id uint, rng time.Duration) (dto.MonitorLatency, error) {
	f.latencyArgs = append(f.latencyArgs, struct {
		id  uint
		rng time.Duration
	}{id, rng})
	return f.latency, f.latencyErr
}

func (f *requestsMonitor) Requests(_ context.Context, id uint, q dto.MonitorRequestsQuery) (dto.MonitorRequests, error) {
	f.requestsArgs = append(f.requestsArgs, struct {
		id uint
		q  dto.MonitorRequestsQuery
	}{id, q})
	return f.requests, f.requestsErr
}

func (f *requestsMonitor) Distributions(_ context.Context, id uint, rng time.Duration) (dto.MonitorDistributions, error) {
	f.distributionsArgs = append(f.distributionsArgs, struct {
		id  uint
		rng time.Duration
	}{id, rng})
	return f.distributions, f.distributionsErr
}

func monitorGet(t *testing.T, deps Deps, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	Router(deps).ServeHTTP(rec, req)
	return rec
}

func TestMonitorRequestViewsRequireAuthentication(t *testing.T) {
	st, signer, _ := monitorTestToken(t)
	deps := Deps{Store: st, AuthSigner: signer, Monitor: &requestsMonitor{}}
	for _, path := range []string{"/api/v1/monitor/deployments/7/requests", "/api/v1/monitor/distributions", "/api/v1/monitor/requests/lookup?request_id=x"} {
		if rec := monitorGet(t, deps, "", path); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", path, rec.Code)
		}
	}
}

func TestMonitorRequestsWireJSON(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	queue, prompt, status, rangeTotal := int32(12), int32(900), 200, int64(4321)
	pdStage, pdQueue := int32(795), int32(2)
	fake := &requestsMonitor{requests: dto.MonitorRequests{
		View: "history", AsOf: 5000, NextCursor: "c1",
		Requests: []dto.MonitorRequest{{
			ID: "r1", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", DeploymentID: 7, ReceivedMS: 4000, Phase: "done", APIKeyID: 3, APIKeyName: "app", Source: "api",
			Stream: true, BodyBytes: 120, EngineQueueMS: &queue, PromptTokens: &prompt, Status: &status,
			Outcome: "ok", FinishReason: "stop", Result: "completed", Events: 4, Weight: 2,
			Segments: []dto.MonitorRequestSegment{{Kind: "gateway", MS: 2.5, Stage: "first_token"}, {Kind: "queue", MS: 12, Stage: "first_token"}},
			PD:       &dto.MonitorRequestPD{PrefillInstanceUUID: "p-1", PrefillMS: &pdStage, PrefillQueueMS: &pdQueue},
		}},
		FromMS: 1000, ToMS: 5000, RangeTotal: &rangeTotal,
	}}
	rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token,
		"/api/v1/monitor/deployments/7/requests?view=history&range=6h&limit=20&key=3&cursor=c0&from=1000&to=5000&results=length,%20error&min_ms=200&max_ms=8000&min_ttft_ms=50&max_ttft_ms=3000&instance=inst-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	want := `{"view":"history","as_of":5000,"counts":null,"requests":[{"id":"r1","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","deployment_id":7,"received_ms":4000,"phase":"done",` +
		`"age_ms":null,"instance_uuid":"","api_key_id":3,"api_key_name":"app","priority_class":"","user_id":0,"source":"api","stream":true,` +
		`"body_bytes":120,"images":0,"dispatch_ms":null,"first_byte_ms":null,"first_think_ms":null,"first_answer_ms":null,` +
		`"last_chunk_ms":null,"end_ms":null,"events":4,"think_events":0,"max_gap_ms":null,"status":200,"outcome":"ok",` +
		`"finish_reason":"stop","result":"completed","prompt_tokens":900,"cached_tokens":null,"completion_tokens":null,"reasoning_tokens":null,` +
		`"engine_queue_ms":12,"engine_prefill_ms":null,"engine_decode_ms":null,"ttft_ms":null,"tpot_ms":null,"prefill_tok_s":null,"decode_tok_s":null,"weight":2,` +
		`"segments":[{"kind":"gateway","ms":2.5,"stage":"first_token"},{"kind":"queue","ms":12,"stage":"first_token"}],` +
		`"pd":{"prefill_instance_uuid":"p-1","prefill_ms":795,"prefill_queue_ms":2,"prefill_compute_ms":null,"fallback":""}}],"next_cursor":"c1",` +
		`"from_ms":1000,"to_ms":5000,"range_total":4321}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
	var back dto.MonitorRequests
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil || *back.Requests[0].EngineQueueMS != 12 || back.Requests[0].CachedTokens != nil {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if len(fake.requestsArgs) != 1 {
		t.Fatalf("calls = %+v", fake.requestsArgs)
	}
	got := fake.requestsArgs[0]
	wantQ := dto.MonitorRequestsQuery{View: "history", RangeMS: (6 * time.Hour).Milliseconds(), FromMS: 1000, ToMS: 5000, Limit: 20, Cursor: "c0", APIKeyID: 3,
		InstanceUUID: "inst-a", Results: []string{"length", "error"}, MinEndMS: 200, MaxEndMS: 8000, MinTTFTMS: 50, MaxTTFTMS: 3000}
	if got.id != 7 || !reflect.DeepEqual(got.q, wantQ) {
		t.Fatalf("called with %d %+v, want 7 %+v", got.id, got.q, wantQ)
	}
}

func TestMonitorRequestsDefaultsToLive(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	fake := &requestsMonitor{requests: dto.MonitorRequests{View: "live", Counts: &dto.MonitorRequestCounts{Waiting: 2}, Requests: []dto.MonitorRequest{}}}
	rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token, "/api/v1/monitor/deployments/7/requests")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"counts":{"waiting":2,"thinking":0,"answering":0}`) {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if q := fake.requestsArgs[0].q; q.View != "live" || q.RangeMS != time.Hour.Milliseconds() || q.Limit != 0 || q.Results != nil || q.FromMS != 0 {
		t.Fatalf("query = %+v, want live defaults", q)
	}
}

func TestMonitorRequestsRejectsBadQueries(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	cases := map[string]string{
		"/api/v1/monitor/deployments/0/requests":                                              "DEPLOYMENT_ID_INVALID",
		"/api/v1/monitor/deployments/x/requests":                                              "DEPLOYMENT_ID_INVALID",
		"/api/v1/monitor/deployments/7/requests?view=all":                                     "MONITOR_REQUESTS_VIEW_INVALID",
		"/api/v1/monitor/deployments/7/requests?range=3h":                                     "MONITOR_RANGE_INVALID",
		"/api/v1/monitor/deployments/7/requests?limit=0":                                      "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?limit=501":                                    "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?limit=ten":                                    "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?key=0":                                        "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?key=-1":                                       "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?view=history&from=1000":                       "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?view=history&from=5000&to=1000":               "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?view=history&from=x&to=1000":                  "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?view=history&min_ms=0":                        "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?view=history&min_ms=900&max_ms=100":           "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?view=history&max_ms=99999999999":              "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?view=history&min_ttft_ms=0":                   "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/deployments/7/requests?view=history&min_ttft_ms=900&max_ttft_ms=100": "MONITOR_REQUESTS_QUERY_INVALID",
		"/api/v1/monitor/distributions?range=2d":                                              "MONITOR_RANGE_INVALID",
		"/api/v1/monitor/distributions?deployment=abc":                                        "DEPLOYMENT_ID_INVALID",
	}
	for path, code := range cases {
		fake := &requestsMonitor{}
		rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token, path)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
			t.Errorf("%s: status = %d body=%s, want 400 %s", path, rec.Code, rec.Body.String(), code)
		}
		if len(fake.requestsArgs)+len(fake.distributionsArgs) != 0 {
			t.Errorf("%s reached the service", path)
		}
	}
}

func TestMonitorRequestsMapsServiceErrors(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{ErrMonitorDeploymentNotFound, http.StatusNotFound, ""},
		{ErrMonitorRequestsQueryInvalid, http.StatusBadRequest, "MONITOR_REQUESTS_QUERY_INVALID"},
		{errors.New("disk on fire"), http.StatusInternalServerError, ""},
	}
	for _, c := range cases {
		fake := &requestsMonitor{requestsErr: c.err, distributionsErr: c.err}
		rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token, "/api/v1/monitor/deployments/7/requests?view=history&cursor=zz")
		if rec.Code != c.status || (c.code != "" && !strings.Contains(rec.Body.String(), c.code)) {
			t.Errorf("requests %v: status = %d body=%s, want %d", c.err, rec.Code, rec.Body.String(), c.status)
		}
		if c.err == ErrMonitorRequestsQueryInvalid {
			continue
		}
		rec = monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token, "/api/v1/monitor/distributions?deployment=7")
		if rec.Code != c.status {
			t.Errorf("distributions %v: status = %d body=%s, want %d", c.err, rec.Code, rec.Body.String(), c.status)
		}
	}
}

func TestMonitorRequestViewsWithoutMonitorAreNotFound(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	for _, path := range []string{"/api/v1/monitor/deployments/7/requests", "/api/v1/monitor/distributions", "/api/v1/monitor/requests/lookup?request_id=x"} {
		if rec := monitorGet(t, Deps{Store: st, AuthSigner: signer}, token, path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestMonitorDistributionsWireJSON(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	fake := &requestsMonitor{distributions: dto.MonitorDistributions{
		FromMS: 1, ToMS: 2, TokenBuckets: []int64{0, 1}, TTFTBoundsMS: []int32{100},
		InputOutput:  []dto.MonitorGridCell{{X: 3, Y: 1, N: 9}},
		UncachedTTFT: []dto.MonitorGridCell{},
		Coverage:     dto.MonitorDistributionCoverage{Total: 10, WithUsage: 9, WithCache: 4},
	}}
	rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token, "/api/v1/monitor/distributions?range=24h&deployment=7")
	want := `{"from_ms":1,"to_ms":2,"token_buckets":[0,1],"ttft_bounds_ms":[100],"input_output":[{"x":3,"y":1,"n":9}],` +
		`"uncached_ttft":[],"coverage":{"total":10,"with_usage":9,"with_cache":4}}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("status = %d body=%s\nwant %s", rec.Code, rec.Body.String(), want)
	}
	if args := fake.distributionsArgs; len(args) != 1 || args[0].id != 7 || args[0].rng != 24*time.Hour {
		t.Fatalf("called with %+v", args)
	}
	rec = monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token, "/api/v1/monitor/distributions")
	if rec.Code != http.StatusOK || fake.distributionsArgs[1].id != 0 || fake.distributionsArgs[1].rng != time.Hour {
		t.Fatalf("default call: %d %+v", rec.Code, fake.distributionsArgs)
	}
}

// The playground handler learns the signed-in user from the request context,
// so its request records name them.
func TestPlaygroundReceivesTheSignedInUser(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	var got []uint
	playground := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.ClaimsFrom(r.Context())
		if !ok {
			t.Error("playground handler got no claims")
		}
		got = append(got, claims.UserID)
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(Deps{Store: st, AuthSigner: signer, PlaygroundV1: playground}).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || len(got) != 1 || got[0] != 1 {
		t.Fatalf("status = %d, users = %v", rec.Code, got)
	}
}

func TestMonitorLatencyWireJSON(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	prefill := 4000.0
	fake := &requestsMonitor{latency: dto.MonitorLatency{
		FromMS: 1000, ToMS: 5000, Requests: 12,
		EndToEnd: &dto.MonitorLatencyStat{MeanMS: 900, P50MS: 800, P95MS: 1500},
		Segments: []dto.MonitorLatencySegment{{Kind: "prefill", MeanMS: 120, Requests: 12, P50MS: 110, P95MS: 200}},
		Speeds:   dto.MonitorLatencySpeeds{PrefillTokS: &prefill},
	}}
	rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token, "/api/v1/monitor/deployments/7/latency?range=6h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	want := `{"from_ms":1000,"to_ms":5000,"requests":12,"end_to_end":{"mean_ms":900,"p50_ms":800,"p95_ms":1500},` +
		`"segments":[{"kind":"prefill","mean_ms":120,"requests":12,"p50_ms":110,"p95_ms":200}],` +
		`"speeds":{"prefill_tok_s":4000,"thinking_tok_s":null,"answering_tok_s":null,"decode_tok_s":null}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
	if len(fake.latencyArgs) != 1 || fake.latencyArgs[0].id != 7 || fake.latencyArgs[0].rng != 6*time.Hour {
		t.Fatalf("calls = %+v", fake.latencyArgs)
	}
	for path, code := range map[string]int{
		"/api/v1/monitor/deployments/0/latency":          http.StatusBadRequest,
		"/api/v1/monitor/deployments/7/latency?range=2d": http.StatusBadRequest,
	} {
		if rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: &requestsMonitor{}}, token, path); rec.Code != code {
			t.Errorf("%s: status %d, want %d", path, rec.Code, code)
		}
	}
	missing := &requestsMonitor{latencyErr: ErrMonitorDeploymentNotFound}
	if rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: missing}, token, "/api/v1/monitor/deployments/8/latency"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown deployment: status %d", rec.Code)
	}
	if rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, "", "/api/v1/monitor/deployments/7/latency"); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: status %d", rec.Code)
	}
}

func TestMonitorRequestLookupWireJSON(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	status := 200
	fake := &requestsMonitor{lookup: dto.MonitorRequestLookup{
		AsOf: 9000, FromMS: 1000, ToMS: 5000, Window: "requested",
		Requests: []dto.MonitorRequest{{
			ID: "r1", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", DeploymentID: 7, ReceivedMS: 4000, Phase: "done",
			Source: "api", Status: &status, Outcome: "ok", Result: "completed", Weight: 1, Segments: []dto.MonitorRequestSegment{},
		}},
		Deployments: []dto.MonitorLookupDeployment{{ID: 7, Name: "qwen"}, {ID: 8, Name: "org/old", Deleted: true}},
	}}
	rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token,
		"/api/v1/monitor/requests/lookup?trace_id=4BF92F3577B34DA6A3CE929D0E0E4736&from=1000&to=5000&limit=20")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	want := `{"as_of":9000,"from_ms":1000,"to_ms":5000,"window":"requested","requests":[{"id":"r1","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736",` +
		`"deployment_id":7,"received_ms":4000,"phase":"done","age_ms":null,"instance_uuid":"","api_key_id":0,"api_key_name":"","priority_class":"","user_id":0,` +
		`"source":"api","stream":false,"body_bytes":0,"images":0,"dispatch_ms":null,"first_byte_ms":null,"first_think_ms":null,` +
		`"first_answer_ms":null,"last_chunk_ms":null,"end_ms":null,"events":0,"think_events":0,"max_gap_ms":null,"status":200,` +
		`"outcome":"ok","finish_reason":"","result":"completed","prompt_tokens":null,"cached_tokens":null,"completion_tokens":null,` +
		`"reasoning_tokens":null,"engine_queue_ms":null,"engine_prefill_ms":null,"engine_decode_ms":null,"ttft_ms":null,"tpot_ms":null,"prefill_tok_s":null,"decode_tok_s":null,"weight":1,"segments":[],"pd":null}],` +
		`"deployments":[{"id":7,"name":"qwen","deleted":false},{"id":8,"name":"org/old","deleted":true}]}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body =\n%s\nwant\n%s", got, want)
	}
	var back dto.MonitorRequestLookup
	if err := json.Unmarshal(rec.Body.Bytes(), &back); err != nil || back.Requests[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || back.Deployments[0].Name != "qwen" || !back.Deployments[1].Deleted {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	// The trace id goes to the service as typed; it parses it.
	wantQ := dto.MonitorRequestLookupQuery{TraceID: "4BF92F3577B34DA6A3CE929D0E0E4736", FromMS: 1000, ToMS: 5000, Limit: 20}
	if len(fake.lookupArgs) != 1 || !reflect.DeepEqual(fake.lookupArgs[0], wantQ) {
		t.Fatalf("called with %+v, want %+v", fake.lookupArgs, wantQ)
	}

	rec = monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token, "/api/v1/monitor/requests/lookup?request_id=gw-1")
	if rec.Code != http.StatusOK || !reflect.DeepEqual(fake.lookupArgs[1], dto.MonitorRequestLookupQuery{RequestID: "gw-1"}) {
		t.Fatalf("status %d, called with %+v", rec.Code, fake.lookupArgs)
	}
}

func TestMonitorRequestLookupRejectsBadQueries(t *testing.T) {
	st, signer, token := monitorTestToken(t)
	for _, path := range []string{
		"/api/v1/monitor/requests/lookup?request_id=x&from=1000",
		"/api/v1/monitor/requests/lookup?request_id=x&from=5000&to=1000",
		"/api/v1/monitor/requests/lookup?request_id=x&from=a&to=1000",
		"/api/v1/monitor/requests/lookup?request_id=x&limit=0",
		"/api/v1/monitor/requests/lookup?request_id=x&limit=501",
	} {
		fake := &requestsMonitor{}
		rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: fake}, token, path)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"MONITOR_REQUESTS_QUERY_INVALID"`) {
			t.Errorf("%s: status = %d body=%s", path, rec.Code, rec.Body.String())
		}
		if len(fake.lookupArgs) != 0 {
			t.Errorf("%s reached the service", path)
		}
	}
	// What the service refuses is a 400 with its reason; anything else a 500.
	refused := &requestsMonitor{lookupErr: fmt.Errorf("%w: trace_id must be 32 hex digits", ErrMonitorRequestsQueryInvalid)}
	rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: refused}, token, "/api/v1/monitor/requests/lookup?trace_id=nope")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "trace_id must be 32 hex digits") || !strings.Contains(rec.Body.String(), "MONITOR_REQUESTS_QUERY_INVALID") {
		t.Fatalf("refused: %d %s", rec.Code, rec.Body.String())
	}
	broken := &requestsMonitor{lookupErr: errors.New("disk on fire")}
	if rec := monitorGet(t, Deps{Store: st, AuthSigner: signer, Monitor: broken}, token, "/api/v1/monitor/requests/lookup?request_id=x"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("broken: %d", rec.Code)
	}
	if rec := monitorPost(t, Deps{Store: st, AuthSigner: signer, Monitor: &requestsMonitor{}}, token, "/api/v1/monitor/requests/lookup?request_id=x"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}

func monitorPost(t *testing.T, deps Deps, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(deps).ServeHTTP(rec, req)
	return rec
}
