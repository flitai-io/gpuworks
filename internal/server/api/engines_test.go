package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// Every /v1 path, the model list and the batch files included, goes to the one
// data-plane handler, which authenticates by API key itself.
func TestRouterHandsEveryV1PathToTheDataPlane(t *testing.T) {
	r := Router(Deps{
		ProxyV1: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Seen", r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}),
	})
	for _, path := range []string{"/v1/models", "/v1/chat/completions", "/v1/files", "/v1/files/file-1/content", "/v1/batches", "/v1/batches/batch_1/cancel"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNoContent || rec.Header().Get("X-Seen") != path {
			t.Errorf("%s = %d seen %q", path, rec.Code, rec.Header().Get("X-Seen"))
		}
	}
}

func TestEnginesEndpointReturnsProfiles(t *testing.T) {
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	signer, err := auth.NewTokenSigner("engines-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	r := Router(Deps{Store: st, Log: slog.Default(), AuthSigner: signer})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/engines", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var profiles []deploy.Profile
	if err := unmarshalList(rec.Body.Bytes(), &profiles); err != nil {
		t.Fatal(err)
	}
	byName := map[string]deploy.Profile{}
	for _, p := range profiles {
		if !reflect.DeepEqual(p.OperationDefinitions, inferenceapi.Definitions()) {
			t.Fatalf("%s operation definitions drifted", p.Name)
		}
		byName[p.Name] = p
	}
	for _, name := range []string{deploy.EngineVLLM, deploy.EngineSGLang} {
		if !slices.Contains(byName[name].QuickFormKeys, "data_parallel_size") {
			t.Fatalf("%s hides the data-parallel field from the deploy form", name)
		}
	}
	vllm, ok := byName["vllm"]
	if !ok {
		t.Fatalf("vllm profile missing: %+v", profiles)
	}
	if vllm.DefaultImage != "example/vllm:test" {
		t.Fatalf("vllm image = %q", vllm.DefaultImage)
	}
	if vllm.HealthPath != "/health" || vllm.MetricPath != "/metrics" || !vllm.Sleep.Supported {
		t.Fatalf("unexpected vllm profile: %+v", vllm)
	}
	if vllm.ParamDict["max_model_len"] != "--max-model-len" || vllm.MetricMap["vllm:num_requests_running"] != "gpuworks_infer_requests_running" {
		t.Fatalf("vllm profile maps incomplete: %+v", vllm)
	}
	if vllm.MetricMap["vllm:prefix_cache_queries_total"] != "gpuworks_infer_kv_queries_total" ||
		vllm.MetricMap["vllm:prefix_cache_hits_total"] != "gpuworks_infer_kv_hits_total" ||
		vllm.MetricMap["vllm:spec_decode_num_draft_tokens_total"] != "gpuworks_infer_spec_draft_tokens_total" ||
		vllm.MetricMap["vllm:spec_decode_num_accepted_tokens_total"] != "gpuworks_infer_spec_accepted_tokens_total" {
		t.Fatalf("vllm inference monitoring maps incomplete: %+v", vllm.MetricMap)
	}
	foundPerformanceMetadata := false
	for _, spec := range vllm.ParamDefinitions {
		if spec.Key == "gpu_memory_utilization" && spec.Group == "performance" && spec.Control == "slider" && spec.Default == float64(0.9) {
			foundPerformanceMetadata = true
		}
	}
	if !foundPerformanceMetadata {
		t.Fatalf("/engines omitted structured performance metadata: %#v", vllm.ParamDefinitions)
	}
	if len(vllm.QuickFormKeys) == 0 || vllm.QuickFormKeys[0] != "max_model_len" {
		t.Fatalf("/engines omitted the server-owned quick form whitelist: %#v", vllm.QuickFormKeys)
	}
	sglang, ok := byName["sglang"]
	if !ok {
		t.Fatalf("sglang profile missing: %+v", profiles)
	}
	if sglang.Sleep.Supported {
		t.Fatalf("sglang should not advertise sleep support: %+v", sglang.Sleep)
	}
	if sglang.ParamDict["max_model_len"] != "--context-length" || sglang.MetricMap["sglang:num_running_reqs"] != "gpuworks_infer_requests_running" {
		t.Fatalf("sglang profile maps incomplete: %+v", sglang)
	}
	if sglang.MetricMap["sglang:cache_hit_rate"] != "gpuworks_infer_kv_hit_rate_instant" ||
		sglang.MetricMap["sglang:spec_accept_rate"] != "gpuworks_infer_spec_accept_rate" ||
		sglang.MetricMap["sglang:spec_accept_length"] != "gpuworks_infer_spec_accept_length" {
		t.Fatalf("sglang inference monitoring maps incomplete: %+v", sglang.MetricMap)
	}
	callable, ok := byName["callable"]
	if !ok {
		t.Fatalf("callable profile missing: %+v", profiles)
	}
	if len(callable.ParamDefinitions) != 0 || len(callable.QuickFormKeys) != 0 {
		t.Fatalf("catalog-owned callable must not expose synthetic quick parameters: %+v", callable)
	}
}

// The KV disk tier's kernel floor is a fact only the server states; the node
// detail view reads it off this wire key rather than hard-coding a release
// number, so the key name and value are pinned literally here.
func TestEnginesEndpointPublishesKVDiskKernelFloor(t *testing.T) {
	signer, err := auth.NewTokenSigner("engines-kernel-floor-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/engines", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(Deps{Store: st, Log: slog.Default(), AuthSigner: signer}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var raw []map[string]any
	if err := unmarshalList(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, p := range raw {
		name, _ := p["name"].(string)
		byName[name] = p
	}
	vllm, ok := byName["vllm"]
	if !ok {
		t.Fatalf("vllm profile missing: %v", byName)
	}
	if got := vllm["kv_disk_min_kernel"]; got != "5.14" {
		t.Fatalf("vllm kv_disk_min_kernel = %#v, want \"5.14\"", got)
	}
	// Engines without a disk tier state no floor at all, so a client cannot
	// mistake "no tier" for "any kernel is too old".
	for _, name := range []string{"sglang", "callable"} {
		p, ok := byName[name]
		if !ok {
			t.Fatalf("%s profile missing: %v", name, byName)
		}
		if _, present := p["kv_disk_min_kernel"]; present {
			t.Fatalf("%s must not state a KV disk kernel floor: %#v", name, p["kv_disk_min_kernel"])
		}
	}
}
