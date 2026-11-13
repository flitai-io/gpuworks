package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/community"
)

func TestCommunityBrowseDistinguishesMissingRepository(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("registry method = %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/repositories/acme/missing/tags":
			if r.URL.Query().Get("page_size") != "100" {
				t.Error("missing Docker Hub page size")
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"object not found"}`))
		case "/api/v1/repository/acme/missing/tag/":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error_type":"not_found","error_message":"Not found"}`))
		case "/v2/acme/missing/tags/list":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"NAME_UNKNOWN","message":"repository name not known to registry"}]}`))
		case "/v2/acme/broken-auth/tags/list":
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://auth.example/token",service="registry.example",scope="repository:acme/broken-auth:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`))
		case "/token":
			if r.URL.Query().Get("service") != "registry.example" || r.URL.Query().Get("scope") != "repository:acme/broken-auth:pull" || r.Header.Get("Authorization") != "" {
				t.Errorf("invalid anonymous token request: %s", r.URL)
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"token endpoint not found"}`))
		case "/v2/repositories/acme/available/tags", "/v2/repositories/library/alpine/tags":
			_, _ = w.Write([]byte(`{"count":1,"next":null,"previous":null,"results":[{"name":"v1.0","last_updated":"2026-10-16T00:00:00Z"}]}`))
		case "/v2/acme/model__variant/tags/list", "/v2/acme/nested/model--variant/tags/list":
			_, _ = w.Write([]byte(`{"name":"acme/available","tags":["v1.0"]}`))
		case "/v2/repositories/acme/unavailable/tags":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"message":"unavailable"}`))
		default:
			t.Errorf("unexpected registry request: %s", r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" {
			t.Errorf("upstream scheme = %s", r.URL.Scheme)
		}
		forwarded := r.Clone(r.Context())
		forwarded.URL.Scheme, forwarded.URL.Host = target.Scheme, target.Host
		return upstream.Client().Transport.RoundTrip(forwarded)
	})}
	run := newImageManifestRun(t)
	run.deps.Outside.Community = community.NewBrowser(client)
	run.router = Router(run.deps)
	for _, tc := range []struct {
		repo      string
		status    int
		errorText string
	}{
		{"acme/missing", 404, "registry returned 404 Not Found"},
		{"quay.io/acme/missing", 404, "registry returned 404 Not Found"},
		{"registry.example/acme/missing", 404, "registry returned 404 Not Found"},
		{"registry.example/acme/broken-auth", 400, "anonymous registry token"},
		{"", 400, "repo is required"},
		{"acme/unavailable", 400, "registry returned 503 Service Unavailable"},
		{"acme/available", 200, ""},
		{"alpine", 200, ""},
		{"docker.io/library/alpine", 200, ""},
		{"registry.example:5000/acme/model__variant", 200, ""},
		{"localhost:5000/acme/nested/model--variant", 200, ""},
	} {
		t.Run(tc.repo, func(t *testing.T) {
			rec := run.do(http.MethodGet, "/api/v1/images/community/browse?repo="+url.QueryEscape(tc.repo), "", "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if tc.errorText != "" {
				message, _ := body["error"].(string)
				if tc.repo == "registry.example/acme/broken-auth" && strings.Contains(message, "community repository not found") {
					t.Fatalf("token service failure misclassified as missing repository: %s", message)
				}
				if !strings.Contains(message, tc.errorText) {
					t.Fatalf("error envelope = %v", body)
				}
			} else if body["repo"] != tc.repo || len(body["tags"].([]any)) != 1 || body["tags"].([]any)[0].(map[string]any)["tag"] != "v1.0" {
				t.Fatalf("repository tags = %v", body)
			}
		})
	}
	for _, tc := range []struct {
		method, token string
		status        int
	}{
		{http.MethodGet, "", 401}, {http.MethodPatch, run.admin, 405},
	} {
		req := httptest.NewRequest(tc.method, "/api/v1/images/community/browse?repo=acme/available", nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		run.router.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("%s status = %d, want %d", tc.method, rec.Code, tc.status)
		}
	}
}

func TestCommunityBrowseRejectsMalformedRepositoryBeforeRegistryLookup(t *testing.T) {
	run := newImageManifestRun(t)
	run.deps.Outside.Community = community.NewBrowser(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("malformed repository reached registry: %s", r.URL)
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: http.NoBody, Request: r}, nil
	})})
	run.router = Router(run.deps)
	for _, repo := range []string{"acme/invalid repo", "https://docker.io/acme/model", "acme/model:v1", "acme/model@sha256:abcd", "acme//model", "acme/../model", "acme/model?tag=x", "acme/model#tag", "acme/"} {
		t.Run(repo, func(t *testing.T) {
			rec := run.do(http.MethodGet, "/api/v1/images/community/browse?repo="+url.QueryEscape(repo), "", "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if message, _ := body["error"].(string); !strings.Contains(message, "invalid repository") {
				t.Fatalf("error envelope=%v", body)
			}
		})
	}
}

func TestCommunityBrowseReturnsPerTagMetadata(t *testing.T) {
	run := newImageManifestRun(t)
	run.deps.Outside.Community = community.NewBrowser(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"results":[{"name":"cann9.0-sglang0.5.15.post1-arm64-diffusion-slot16"},{"name":"latest"}],"next":null}`)), Request: r}, nil
	})})
	run.router = Router(run.deps)
	// Even a default CUDA/vLLM card can carry a tag naming another stack.
	rec := run.do(http.MethodGet, "/api/v1/images/community/browse?repo=vllm/vllm-openai", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Tags []map[string]any `json:"tags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"tag": "cann9.0-sglang0.5.15.post1-arm64-diffusion-slot16", "engine": "sglang", "engine_version": "0.5.15.post1", "framework": "cann", "framework_version": "9.0", "arch": "arm64"}
	if len(body.Tags) != 2 || !reflect.DeepEqual(body.Tags[0], want) || !reflect.DeepEqual(body.Tags[1], map[string]any{"tag": "latest", "rolling_tag": true}) {
		t.Fatalf("tag contract=%v", body.Tags)
	}
}
