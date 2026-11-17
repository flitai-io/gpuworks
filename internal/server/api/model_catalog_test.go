package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCatalogOnlyReadsListingsRevisionAndReadme(t *testing.T) {
	for _, tc := range []struct {
		s, p string
		ok   bool
	}{
		{"huggingface", "/api/models", true},
		{"modelscope", "/api/v1/models/Qwen/Qwen3-8B/commits", true},
		{"modelscope", "/models/Qwen/Qwen3-8B/resolve/master/README.md", true},
		{"huggingface", "/openai-community/gpt2/resolve/main/README.md", true},
		{"huggingface", "//example.com/api/models", false},
		{"huggingface", "/org/repo/resolve/main/weights.bin", false},
		{"modelscope", "/models/../repo/resolve/master/README.md", false},
		{"url", "/api/models", false},
	} {
		if catalogResource(tc.s, tc.p) != tc.ok {
			t.Errorf("catalogResource(%q,%q) want %t", tc.s, tc.p, tc.ok)
		}
	}
}

func TestCatalogProfilesBelongToTheSelectedRepositoryRoot(t *testing.T) {
	model := flatAPIFixture("org/repo", "root")
	model.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"org/repo","revision":"abc"}]`
	other := model.Specs[0]
	other.WeightSources = `[{"provider":"huggingface","repo_id":"org/repo","revision":"abc","subfolder":"gguf"}]`
	model.Specs = append(model.Specs, other)
	for _, tc := range []struct {
		source, id, version string
		n                   int
	}{
		{"huggingface", "org/repo", "abc", 1}, {"modelscope", "org/repo", "abc", 0}, {"huggingface", "org/repo-new", "abc", 0}, {"huggingface", "org/repo", "def", 0}, {"huggingface", "org/repo", "", 0},
	} {
		result := templatesWithWeight([]store.InferenceModel{model}, tc.source, tc.id, tc.version)
		if len(result) != tc.n || (tc.n == 1 && len(result[0].Specs) != 1) {
			t.Fatalf("wrong profiles for %s %s: %+v", tc.source, tc.id, result)
		}
	}
}

func TestCatalogIsAnAuthenticatedReadOnlyRoute(t *testing.T) {
	rig := newPullRig(t)
	for _, tc := range []struct {
		token, method, path string
		status              int
	}{
		{"", "GET", "/api/v1/models/catalog", http.StatusUnauthorized},
		{rig.member, "GET", "/api/v1/models/catalog?source=huggingface&path=/weights.bin", http.StatusBadRequest},
		{rig.member, "POST", "/api/v1/models/catalog", http.StatusMethodNotAllowed},
	} {
		if status, body := rig.do(tc.token, tc.method, tc.path, ""); status != tc.status {
			t.Errorf("%s %s = %d %s, want %d", tc.method, tc.path, status, body, tc.status)
		}
	}
}

func TestCatalogUsesOnlyConfiguredSitesAndPreservesRateLimits(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" || r.URL.Query().Get("limit") != "10" {
			t.Errorf("upstream request = %s", r.URL)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("forwarded management credentials")
		}
		if r.URL.Query().Get("search") == "qwen" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Link", "</api/models?cursor=next>; rel=next")
		_, _ = w.Write([]byte(`[{"id":"org/repo"}]`))
	}))
	defer site.Close()
	st := metadataSourcesStore(t, "catalog_sites")
	_, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "https://huggingface.co", Endpoints: []string{site.URL}, ControlPlane: true})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/models/catalog?source=huggingface&path=/api/models&search=qwen&limit=9999", nil)
	req.Header.Set("Cookie", "session=private")
	h.modelCatalog(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit = %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.modelCatalog(rec, httptest.NewRequest("GET", "/api/v1/models/catalog?source=huggingface&path=/api/models", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `[{"id":"org/repo"}]` || rec.Header().Get("X-Catalog-Link") != "</api/models?cursor=next>; rel=next" {
		t.Fatalf("catalog response = %d %q, link = %q", rec.Code, rec.Body.String(), rec.Header().Get("X-Catalog-Link"))
	}
}
