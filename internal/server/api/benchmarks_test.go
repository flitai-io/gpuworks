package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func TestBenchmarkTokenizerImport(t *testing.T) {
	st, err := store.Open("file:benchmark_tokenizer_import_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-tokenizer-import-test", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	dir := t.TempDir()
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer, BenchmarkArtifactDir: dir})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks/tokenizers/import", strings.NewReader(`{"model":{"vocab":{}},"added_tokens":[]}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "tokenizer_ref") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(dir, entries[0].Name())); err != nil {
		t.Fatal(err)
	}
}

func TestBenchmarkMetaDictionarySchema(t *testing.T) {
	st, err := store.Open("file:benchmark_meta_api?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-meta-api", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/benchmarks/meta", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body struct {
		Modes []struct {
			Value string `json:"value"`
		} `json:"modes"`
		ModeDefaults   map[string]json.RawMessage `json:"mode_defaults"`
		SLAConstraints []struct {
			Key string `json:"key"`
		} `json:"sla_constraints"`
		DatasetProfiles map[string]struct {
			SampleCount int `json:"sample_count"`
		} `json:"dataset_profiles"`
		DetailCapMB    struct{ Default, Min, Max int } `json:"detail_cap_mb"`
		AgenticPresets []struct {
			Value string `json:"value"`
		} `json:"agentic_presets"`
		LoadDefaults map[string]json.RawMessage `json:"load_defaults"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(body.Modes) != 3 || len(body.ModeDefaults) != 3 || len(body.SLAConstraints) != 12 || body.DetailCapMB.Default != 20 || body.DetailCapMB.Min != 1 || body.DetailCapMB.Max != 50 {
		t.Fatalf("status=%d body=%s parsed=%+v", w.Code, w.Body.String(), body)
	}
	if body.DatasetProfiles["multiturn"].SampleCount != 3826 || len(body.AgenticPresets) != 8 {
		t.Fatalf("dataset profiles = %+v", body.DatasetProfiles)
	}
	// The legacy per-user throughput constraints are no longer offered; the
	// per-user floor is interactivity.
	keys := map[string]bool{}
	for _, field := range body.SLAConstraints {
		keys[field.Key] = true
	}
	if !keys["min_interactivity"] || !keys["min_session_completion"] || keys["min_per_user_output_tps"] || keys["max_per_user_total_tps"] {
		t.Fatalf("sla constraints = %+v", body.SLAConstraints)
	}
	wantLoad := map[string]string{
		"warmup_requests": "1", "max_warmup_requests": "1000", "burstiness": "1", "point_duration_s": "60",
		"min_point_duration_s": "5", "max_point_duration_s": "3600", "min_open_loop_requests": "20",
		"max_request_rate": "10000", "max_request_rates": "32", "max_burstiness": "100", "max_in_flight": "100000",
		"request_timeout_s": "600", "min_request_timeout_s": "10", "max_request_timeout_s": "7200", "ignore_eos": "true", "ignore_eos_datasets": `["random","agentic"]`,
	}
	if len(body.LoadDefaults) != len(wantLoad) {
		t.Fatalf("load defaults = %s", w.Body.String())
	}
	for key, want := range wantLoad {
		if got := string(body.LoadDefaults[key]); got != want {
			t.Fatalf("load_defaults.%s = %s, want %s", key, got, want)
		}
	}
}

func TestCreateBenchmarkRejectsRetiredAgentTrace(t *testing.T) {
	st, err := store.Open("file:benchmark_retired_dataset?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, _ := auth.NewTokenSigner("benchmark-retired-dataset", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks", strings.NewReader(`{"target_kind":"external","dataset":{"kind":"agenttrace"}}`))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"`+ErrorBenchmarkDatasetRetired+`"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBenchmarkDatasetImportReturnsProfileAndPreservesSampleShape(t *testing.T) {
	st, err := store.Open("file:benchmark_dataset_import_profile?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, _ := auth.NewTokenSigner("benchmark-dataset-import-profile", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	handler := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	body := strings.Join([]string{
		`{"messages":[{"role":"user","content":"first"}],"follow_ups":[{"role":"user","content":"second"}],"input_tokens":40,"output_tokens":20,"wait_millis":15}`,
		`{"messages":[{"role":"user","content":"missing output limit"}]}`,
	}, "\n")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks/datasets/import", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var config dataset.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	if config.Kind != "imported" || config.Profile == nil || config.Profile.SampleCount != 2 || config.Profile.MaxInputTokens != 40 || config.Profile.MaxOutputTokens != dataset.DefaultImportedOutputTokens {
		t.Fatalf("imported profile = %+v", config)
	}
	if len(config.Samples) != 2 || len(config.Samples[0].FollowUps) != 1 || config.Samples[0].InputTokens != 40 || config.Samples[0].OutputTokens != 20 || config.Samples[0].WaitMillis != 15 || config.Samples[1].OutputTokens != dataset.DefaultImportedOutputTokens {
		t.Fatalf("imported samples = %+v", config.Samples)
	}
}

func TestBenchmarkAPIExternalCreateAndConflict(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
		case "/v1/chat/completions":
			startOnce.Do(func() { close(started) })
			<-release
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	defer close(release)
	st, err := store.Open("file:benchmark_api_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-api-test", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	body := `{"name":"x","target_kind":"external","base_url":"` + fixture.URL + `","tokenizer_ref":"` + benchmarkTestTokenizer(t) + `","model":"m","load_origin":"server","num_gpus":4,"dataset":{"kind":"random"}}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var created benchmarkDTO
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.NumGPUs != 4 {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("server benchmark did not start its first request")
	}
	r = httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestManagedBenchmarkKeepsServerOriginAndTargetInstance(t *testing.T) {
	st, err := store.Open("file:benchmark_managed_server_create?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, err := st.RegisterNode("managed-node", store.HashToken("token"), "managed-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "managed", ModelID: benchmarkChatModel(t, st, "managed").ID, ServedName: "managed-model", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&store.Instance{UUID: "managed-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 31000, GPUIndexes: `[0]`})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("managed-server-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("managed-server-test")
	if err != nil {
		t.Fatal(err)
	}
	type gatewayRequest struct {
		method, path, authorization string
		body                        struct {
			Model     string            `json:"model"`
			Stream    bool              `json:"stream"`
			MaxTokens int               `json:"max_tokens"`
			Messages  []json.RawMessage `json:"messages"`
		}
		decodeError error
	}
	observed := make(chan gatewayRequest, 1)
	var requests atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		got := gatewayRequest{method: r.Method, path: r.URL.Path, authorization: r.Header.Get("Authorization")}
		got.decodeError = json.NewDecoder(r.Body).Decode(&got.body)
		select {
		case observed <- got:
		default:
		}
		// Permanent gateway errors must stop warmup; connection refusal is
		// transient and intentionally retried by the production runner.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"synthetic invalid request","type":"invalid_request_error"}}`))
	}))
	defer gateway.Close()
	h := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, AuthSigner: signer, AISecretbox: box, Server: StaticServerSettings{Loopback: gateway.URL}}, benchmarkExecutions: map[uint]*benchmarkExecution{}}
	body := fmt.Sprintf(`{"name":"managed-server","target_kind":"instance","deployment_id":%d,"load_origin":"server","tokenizer_ref":%q,"dataset":{"kind":"random","input_tokens":8,"output_tokens":4},"mode":"grid","search":{"concurrency":[1],"isl":[8],"osl":[4],"requests_per_point_multiplier":1,"warmup_requests":1}}`, dep.ID, benchmarkTestTokenizer(t))
	req := httptest.NewRequest(http.MethodPost, "http://gpuworks.example/api/v1/benchmarks", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.createBenchmark(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created benchmarkDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.LoadOrigin != "server" || created.InstanceUUID != inst.UUID || created.BaseURL != gateway.URL+"/api/v1/playground" {
		t.Fatalf("created=%+v", created)
	}
	persisted, err := st.GetBenchmarkRun(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	internalToken, err := store.DecryptAIKey(box, persisted.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	if claims, err := signer.Verify(internalToken); err != nil || claims.UserID != 1 {
		t.Fatalf("internal gateway token claims=%+v err=%v", claims, err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h.benchmarkMu.Lock()
		running := len(h.benchmarkExecutions)
		h.benchmarkMu.Unlock()
		if running == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	h.benchmarkMu.Lock()
	running := len(h.benchmarkExecutions)
	h.benchmarkMu.Unlock()
	if running != 0 {
		t.Fatal("server benchmark execution did not stop after the permanent internal gateway error")
	}
	select {
	case got := <-observed:
		if got.decodeError != nil || got.method != http.MethodPost || got.path != "/api/v1/playground/v1/chat/completions" || got.body.Model != dep.ServedName || !got.body.Stream || got.body.MaxTokens != 4 || len(got.body.Messages) == 0 {
			t.Fatalf("unexpected gateway request method=%s path=%s body=%+v decodeError=%v", got.method, got.path, got.body, got.decodeError)
		}
		if got.authorization != "Bearer "+internalToken {
			t.Fatal("internal gateway did not receive the persisted platform authorization")
		}
	default:
		t.Fatal("server benchmark never reached the internal gateway")
	}
	if requests.Load() != 1 {
		t.Fatalf("permanent gateway error retried: requests=%d", requests.Load())
	}
	terminal, err := st.GetBenchmarkRun(created.ID)
	if err != nil || terminal.State != "failed" || !strings.Contains(terminal.Error, "synthetic invalid request") {
		t.Fatalf("terminal state=%+v err=%v", terminal, err)
	}
}

func TestCancelServerBenchmarkWaitsForInFlightRequestToStop(t *testing.T) {
	started := make(chan struct{})
	requestCanceled := make(chan struct{})
	var requests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
		case "/v1/chat/completions":
			requests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-started:
			default:
				close(started)
			}
			<-r.Context().Done()
			select {
			case <-requestCanceled:
			default:
				close(requestCanceled)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()

	st, err := store.Open("file:benchmark_cancel_server?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-cancel-server", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	createBody := fmt.Sprintf(`{"name":"cancel-server","target_kind":"external","base_url":%q,"tokenizer_ref":%q,"model":"m","load_origin":"server","dataset":{"kind":"random","input_tokens":8,"output_tokens":4},"mode":"grid","search":{"concurrency":1,"requests_per_point_multiplier":3}}`, fixture.URL, benchmarkTestTokenizer(t))
	create := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks", strings.NewReader(createBody))
	create.Header.Set("Authorization", "Bearer "+token)
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	h.ServeHTTP(created, create)
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var run benchmarkDTO
	if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil || run.ID == 0 {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server benchmark request did not start")
	}

	cancelReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/benchmarks/%d/cancel", run.ID), nil)
	cancelReq.Header.Set("Authorization", "Bearer "+token)
	canceled := httptest.NewRecorder()
	h.ServeHTTP(canceled, cancelReq)
	if canceled.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", canceled.Code, canceled.Body.String())
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("in-flight HTTP request did not observe cancellation after the API response")
	}
	time.Sleep(20 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests=%d, queued requests continued after cancellation", got)
	}
	persisted, err := st.GetBenchmarkRun(run.ID)
	if err != nil || persisted.State != "canceled" {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}

	repeated := httptest.NewRecorder()
	repeatedReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/benchmarks/%d/cancel", run.ID), nil)
	repeatedReq.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(repeated, repeatedReq)
	if repeated.Code != http.StatusOK {
		t.Fatalf("repeated cancel status=%d body=%s", repeated.Code, repeated.Body.String())
	}
}

func TestCancelAgentBenchmarkRequiresExecutionAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name       string
		callErr    error
		wantStatus int
	}{
		{name: "acknowledged", wantStatus: http.StatusOK},
		{name: "agent offline", callErr: errors.New("node not connected: agent-1"), wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:benchmark_cancel_agent_"+strings.ReplaceAll(tc.name, " ", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: tc.name, LoadOrigin: "agent-1", State: "running"})
			if err != nil {
				t.Fatal(err)
			}
			signer, _ := auth.NewTokenSigner("benchmark-cancel-agent-"+tc.name, time.Hour)
			token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
			var calls, syncs atomic.Int32
			h := Router(Deps{
				Store: st, Bus: bus.New(), AuthSigner: signer,
				CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
					calls.Add(1)
					if uuid != "agent-1" || method != proto.RPCBenchCancel || params != (proto.BenchmarkCancelParams{ID: run.ID}) {
						t.Fatalf("call uuid=%q method=%q params=%#v", uuid, method, params)
					}
					if tc.callErr != nil {
						return nil, tc.callErr
					}
					body, _ := json.Marshal(proto.BenchmarkCancelResult{ID: run.ID, State: "canceled", Running: false})
					return body, nil
				},
				NodeSync: nodeSyncCallbacks{full: func(uuid string) error {
					syncs.Add(1)
					if uuid != "agent-1" {
						t.Fatalf("sync uuid=%q", uuid)
					}
					return tc.callErr
				}},
			})
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/benchmarks/%d/cancel", run.ID), nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.callErr != nil && !strings.Contains(w.Body.String(), "marked canceled") {
				t.Fatalf("failure did not explain partial cancellation: %s", w.Body.String())
			}
			if tc.callErr != nil && !strings.Contains(w.Body.String(), `"code":"`+ErrorBenchmarkCancelUnconfirmed+`"`) {
				t.Fatalf("failure did not include stable cancellation error code: %s", w.Body.String())
			}
			if calls.Load() != 1 || syncs.Load() != 1 {
				t.Fatalf("calls=%d syncs=%d", calls.Load(), syncs.Load())
			}
			persisted, err := st.GetBenchmarkRun(run.ID)
			if err != nil || persisted.State != "canceled" {
				t.Fatalf("persisted=%+v err=%v", persisted, err)
			}
		})
	}
}

func TestBenchmarkListIncludesOnlyPersistedBestPointSummary(t *testing.T) {
	st, err := store.Open("file:benchmark_list_summary?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{
		Name: "grid summary", LoadOrigin: "server", State: "done", Mode: "grid",
		Verdict: `{"kind":"grid_done","best_point_seq":2}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBenchmarkPoint(&store.BenchmarkPoint{RunID: run.ID, Seq: 1, Concurrency: 1, OutputTput: 10}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBenchmarkPoint(&store.BenchmarkPoint{RunID: run.ID, Seq: 2, Concurrency: 2, TargetISL: 64, TargetOSL: 16, OutputTput: 25, RequestTput: 1.5, P99TTFT: 120}); err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("benchmark-list-summary", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/benchmarks", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body []benchmarkDTO
	if err := unmarshalList(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(body) != 1 || body[0].BestPoint == nil || body[0].BestPoint.Seq != 2 || body[0].BestPoint.OutputTput != 25 {
		t.Fatalf("status=%d body=%s parsed=%+v", w.Code, w.Body.String(), body)
	}
	if body[0].Points != nil {
		t.Fatalf("list endpoint unexpectedly included all points: %+v", body[0].Points)
	}
}

func TestBenchmarkPointDTOExposesPeakThroughputAndTiming(t *testing.T) {
	started := time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC)
	ended := started.Add(12 * time.Second)
	dto := benchmarkPointDTOFromEntity(store.BenchmarkPoint{MaxOutputTput: 42.5, MaxTotalTput: 84.5, StartedAt: &started, EndedAt: &ended})
	if dto.MaxOutputTput != 42.5 || dto.MaxTotalTput != 84.5 || dto.StartedAt == nil || !dto.StartedAt.Equal(started) || dto.EndedAt == nil || !dto.EndedAt.Equal(ended) {
		t.Fatalf("dto = %+v", dto)
	}
}

func TestInstanceGPUCountUsesPersistedIndexesAndSpecFallback(t *testing.T) {
	if got := instanceGPUCount(store.Instance{GPUIndexes: `[0,2]`}); got != 2 {
		t.Fatalf("persisted GPU count=%d, want 2", got)
	}
	if got := instanceGPUCount(store.Instance{DesiredStateJSON: `{"gpu_indexes":[1,3,5]}`}); got != 3 {
		t.Fatalf("spec GPU count=%d, want 3", got)
	}
}

func TestBenchmarkModelsProbeServerEndpoint(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
	}))
	defer fixture.Close()
	st, err := store.Open("file:benchmark_models_api_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-models-api-test", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks/models", strings.NewReader(`{"node_uuid":"server","url":"`+fixture.URL+`"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "model-a") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBenchmarkModelsProbeNormalizesV1Suffix(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-v1"}]}`))
	}))
	defer fixture.Close()
	st, err := store.Open("file:benchmark_models_v1_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-models-v1-test", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks/models", strings.NewReader(`{"node_uuid":"server","url":"`+fixture.URL+`/v1/"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"normalized_url":"`+fixture.URL+`"`) || strings.Contains(w.Body.String(), `"notice"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBenchmarkModelsProbe404ReturnsSuggestion(t *testing.T) {
	fixture := httptest.NewServer(http.NotFoundHandler())
	defer fixture.Close()
	st, err := store.Open("file:benchmark_models_404_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-models-404-test", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks/models", strings.NewReader(`{"node_uuid":"server","url":"`+fixture.URL+`"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"`+ErrorBenchmarkEndpointNotFound+`"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBenchmarkModelsProbeAutoUsesServer(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"auto-model"}]}`))
	}))
	defer fixture.Close()
	st, err := store.Open("file:benchmark_models_auto_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-models-auto-test", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks/models", strings.NewReader(`{"node_uuid":"auto","url":"`+fixture.URL+`"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "auto-model") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBenchmarkModelsProbeEmptyEndpointFails(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) }))
	defer fixture.Close()
	st, err := store.Open("file:benchmark_models_empty_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-models-empty-test", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks/models", strings.NewReader(`{"node_uuid":"server","url":"`+fixture.URL+`"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBenchmarkModelsProbeHTTPError(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "offline", http.StatusServiceUnavailable) }))
	defer fixture.Close()
	st, err := store.Open("file:benchmark_models_http_error_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-models-http-error-test", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks/models", strings.NewReader(`{"node_uuid":"server","url":"`+fixture.URL+`"}`))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"`+ErrorBenchmarkProbeFailed+`"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

// A probe the node ran: the endpoint answer arrives as the agent's coded
// refusal (or, from an agent older than codes, its sentence), and anything
// else - whatever its wording - is a probe failure.
func TestBenchmarkProbeErrorReadsTheCodeNotTheText(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"agent 404", &proto.RPCError{Code: proto.ErrorBenchEndpointNotFound, Message: "models probe returned 404 Not Found"}, ErrorBenchmarkEndpointNotFound},
		{"server 404 wrapped", fmt.Errorf("external endpoint probe failed: %w", fmt.Errorf("%w: models endpoint returned 404 Not Found: ", loadgen.ErrModelsEndpointNotFound)), ErrorBenchmarkEndpointNotFound},
		{"text that says not found", errors.New("node not found: 404"), ErrorBenchmarkProbeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeBenchmarkProbeError(w, tc.err, "http://10.0.0.1:8000")
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestBenchmarkDatasetImportJSONL(t *testing.T) {
	st, err := store.Open("file:benchmark_dataset_import_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	signer, _ := auth.NewTokenSigner("benchmark-dataset-import-test", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks/datasets/import", strings.NewReader("hello\n[{\"role\":\"user\",\"content\":\"hi\"}]\n"))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"kind":"imported"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDeleteBenchmarkRemovesArtifactAndRejectsActiveRun(t *testing.T) {
	st, err := store.Open("file:benchmark_delete_api?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	artifact := filepath.Join(dir, "detail.jsonl.gz")
	if err := os.WriteFile(artifact, []byte("detail"), 0o600); err != nil {
		t.Fatal(err)
	}
	done, _ := st.CreateBenchmarkRun(&store.BenchmarkRun{LoadOrigin: "server", State: "done"})
	_ = st.UpsertBenchmarkPoint(&store.BenchmarkPoint{RunID: done.ID, Seq: 1, DetailPath: artifact})
	active, _ := st.CreateBenchmarkRun(&store.BenchmarkRun{LoadOrigin: "server", State: "running"})
	signer, _ := auth.NewTokenSigner("benchmark-delete-api", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	h := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer, BenchmarkArtifactDir: dir})
	del := func(id uint) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/benchmarks/%d", id), nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := del(active.ID); w.Code != http.StatusConflict {
		t.Fatalf("active status=%d body=%s", w.Code, w.Body.String())
	}
	if w := del(done.ID); w.Code != http.StatusNoContent {
		t.Fatalf("done status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("artifact should be removed: %v", err)
	}
}
