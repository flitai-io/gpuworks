package community

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRegistries serves the three registry APIs the community tab lists tags
// from, keyed by the host the production code addressed. The JSON shapes and
// pagination fields are the registries' own: Docker Hub's results/next,
// quay's tags/has_additional, and the distribution API's tags with a Link
// header and a Bearer token challenge.
type fakeRegistries struct {
	mu       sync.Mutex
	requests []string
	handler  func(w http.ResponseWriter, r *http.Request, host string)
	browser  *Browser
}

// install starts the fake and returns a Browser whose requests all land on it.
func (f *fakeRegistries) install(t *testing.T) *Browser {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Header.Get("X-Test-Host")
		f.mu.Lock()
		f.requests = append(f.requests, "https://"+host+r.URL.RequestURI())
		f.mu.Unlock()
		f.handler(w, r, host)
	}))
	t.Cleanup(server.Close)
	f.browser = NewBrowser(routingClient{base: server.URL})
	return f.browser
}

func (f *fakeRegistries) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// routingClient sends every request to the fake server, keeping the host the
// caller addressed in a header so the fake can answer per registry.
type routingClient struct{ base string }

func (c routingClient) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("community request must use https: %s", req.URL)
	}
	if _, ok := req.Context().Deadline(); !ok {
		return nil, errors.New("community request has no deadline")
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("X-Test-Host", req.URL.Host)
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(c.base, "http://")
	clone.Host = ""
	return http.DefaultClient.Do(clone)
}

func tagNames(row Repo) []string {
	out := make([]string, 0, len(row.Tags))
	for _, tag := range row.Tags {
		out = append(out, tag.Tag)
	}
	return out
}

func findTag(t *testing.T, row Repo, name string) Tag {
	t.Helper()
	for _, tag := range row.Tags {
		if tag.Tag == name {
			return tag
		}
	}
	t.Fatalf("tag %q missing from %v", name, tagNames(row))
	return Tag{}
}

func TestBrowseDockerHubListsEveryTagAcrossPages(t *testing.T) {
	fake := &fakeRegistries{handler: func(w http.ResponseWriter, r *http.Request, host string) {
		if host != "hub.docker.com" || r.URL.Path != "/v2/repositories/vllm/vllm-openai/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = io.WriteString(w, `{"count":4,"next":null,"previous":"https://hub.docker.com/v2/repositories/vllm/vllm-openai/tags?page_size=100","results":[{"name":"v0.11.0","last_updated":"2026-09-01T00:00:00.000000Z","digest":"sha256:c"},{"name":"latest","last_updated":"2026-10-07T00:00:00.000000Z","digest":"sha256:d"}]}`)
			return
		}
		if r.URL.Query().Get("page_size") != "100" {
			t.Errorf("first page query = %q", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"count":4,"next":"https://hub.docker.com/v2/repositories/vllm/vllm-openai/tags?page=2&page_size=100","previous":null,"results":[{"name":"qwen38","last_updated":"2026-10-05T12:00:00.000000Z","digest":"sha256:a"},{"name":"nightly","last_updated":"2026-10-08T03:00:00.000000Z","digest":"sha256:b"}]}`)
	}}
	browser := fake.install(t)
	row, err := browser.Browse(context.Background(), "vllm/vllm-openai", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tagNames(row), ","); got != "qwen38,nightly,v0.11.0,latest" {
		t.Fatalf("tags = %s (no tag may be filtered out)", got)
	}
	if row.Engine != "vllm" || row.Framework != "cuda" || row.Registry != RegistryDockerHub {
		t.Fatalf("repo = %+v", row)
	}
	if tag := findTag(t, row, "qwen38"); tag.RollingTag || tag.LastUpdated != "2026-10-05T12:00:00.000000Z" {
		t.Fatalf("qwen38 = %+v", tag)
	}
	for _, rolling := range []string{"nightly", "latest"} {
		if !findTag(t, row, rolling).RollingTag {
			t.Fatalf("%s must be a rolling tag", rolling)
		}
	}
	if tag := findTag(t, row, "v0.11.0"); tag.EngineVersion != "0.11.0" || tag.RollingTag {
		t.Fatalf("v0.11.0 = %+v", tag)
	}

	// Served from the hour-long cache: the registry is not asked again.
	before := fake.count()
	again, err := browser.Browse(context.Background(), "vllm/vllm-openai", nil)
	if err != nil || len(again.Tags) != 4 {
		t.Fatalf("cached browse = %+v, %v", again, err)
	}
	if fake.count() != before {
		t.Fatalf("cached browse hit the registry: %d requests, want %d", fake.count(), before)
	}
}

func TestBrowseDockerHubStopsAtPageLimit(t *testing.T) {
	fake := &fakeRegistries{handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		_, _ = fmt.Fprintf(w, `{"next":"https://hub.docker.com/v2/repositories/acme/huge/tags?page=%s1","results":[{"name":"t%s"}]}`, page, page)
	}}
	browser := fake.install(t)
	row, err := browser.Browse(context.Background(), "acme/huge", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fake.count() != maxPages || len(row.Tags) != maxPages {
		t.Fatalf("requests = %d, tags = %d, want %d of each", fake.count(), len(row.Tags), maxPages)
	}
	// A repository nobody configured is listed too, with no engine to offer.
	if row.Engine != "" || row.Registry != RegistryDockerHub {
		t.Fatalf("unconfigured repo = %+v", row)
	}
}

func TestBrowseQuayFollowsHasAdditional(t *testing.T) {
	fake := &fakeRegistries{handler: func(w http.ResponseWriter, r *http.Request, host string) {
		if host != "quay.io" || r.URL.Path != "/api/v1/repository/ascend/vllm-ascend/tag/" {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		if query.Get("limit") != "100" || query.Get("onlyActiveTags") != "true" {
			t.Errorf("quay query = %q", r.URL.RawQuery)
		}
		switch query.Get("page") {
		case "1":
			_, _ = io.WriteString(w, `{"tags":[{"name":"v0.27.1rc1","reversion":false,"start_ts":1759000000,"manifest_digest":"sha256:a","is_manifest_list":true,"size":null,"last_modified":"Mon, 29 Sep 2026 10:00:00 -0000"},{"name":"main","manifest_digest":"sha256:b","last_modified":"Wed, 08 Oct 2026 01:00:00 -0000"}],"page":1,"has_additional":true}`)
		case "2":
			_, _ = io.WriteString(w, `{"tags":[{"name":"v0.26.0rc1-a3","manifest_digest":"sha256:c","last_modified":"Mon, 01 Sep 2026 10:00:00 -0000"}],"page":2,"has_additional":false}`)
		default:
			t.Errorf("unexpected page %q", query.Get("page"))
		}
	}}
	browser := fake.install(t)
	row, err := browser.Browse(context.Background(), "quay.io/ascend/vllm-ascend", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tagNames(row), ","); got != "v0.27.1rc1,main,v0.26.0rc1-a3" {
		t.Fatalf("tags = %s", got)
	}
	if row.Engine != "vllm" || row.Framework != "cann" || row.Registry != RegistryQuay {
		t.Fatalf("repo = %+v", row)
	}
	if tag := findTag(t, row, "main"); !tag.RollingTag || tag.LastUpdated != "Wed, 08 Oct 2026 01:00:00 -0000" {
		t.Fatalf("main = %+v", tag)
	}
	if fake.count() != 2 {
		t.Fatalf("requests = %d, want 2", fake.count())
	}
}

const harborRepo = "harbor.sourcefind.cn:5443/dcu/admin/base/sglang"

func harborHandler(t *testing.T, tokenOK bool) func(w http.ResponseWriter, r *http.Request, host string) {
	return func(w http.ResponseWriter, r *http.Request, host string) {
		switch {
		case host == "harbor.sourcefind.cn:5443" && r.URL.Path == "/service/token":
			if r.URL.Query().Get("service") != "harbor-registry" || r.URL.Query().Get("scope") != "repository:dcu/admin/base/sglang:pull" {
				t.Errorf("token query = %q", r.URL.RawQuery)
			}
			if !tokenOK {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"unauthorized to access repository"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"token":"anon-token","access_token":"","expires_in":1800}`)
		case host == "harbor.sourcefind.cn:5443" && r.URL.Path == "/v2/dcu/admin/base/sglang/tags/list":
			if r.Header.Get("Authorization") != "Bearer anon-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="https://harbor.sourcefind.cn:5443/service/token",service="harbor-registry",scope="repository:dcu/admin/base/sglang:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"unauthorized"}]}`)
				return
			}
			if r.URL.Query().Get("last") == "" {
				if r.URL.Query().Get("n") != "1000" {
					t.Errorf("tags/list query = %q", r.URL.RawQuery)
				}
				w.Header().Set("Link", `</v2/dcu/admin/base/sglang/tags/list?last=0.5.12-ubuntu22.04-dtk2604-py3.10&n=1000>; rel="next"`)
				_, _ = io.WriteString(w, `{"name":"dcu/admin/base/sglang","tags":["0.5.10-ubuntu22.04-dtk2604-py3.10","0.5.12-ubuntu22.04-dtk2604-py3.10"]}`)
				return
			}
			_, _ = io.WriteString(w, `{"name":"dcu/admin/base/sglang","tags":["latest"]}`)
		default:
			http.NotFound(w, r)
		}
	}
}

// A sibling path on the DCU card is browsed on its own and inherits the
// card's engine and framework. Harbor refuses the first anonymous request
// with a Bearer challenge; an anonymous token is fetched and the list follows
// the Link header to its second page.
func TestBrowseOCIAnonymousTokenAndLinkPagination(t *testing.T) {
	fake := &fakeRegistries{handler: harborHandler(t, true)}
	browser := fake.install(t)
	row, err := browser.Browse(context.Background(), harborRepo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tagNames(row), ","); got != "0.5.10-ubuntu22.04-dtk2604-py3.10,0.5.12-ubuntu22.04-dtk2604-py3.10,latest" {
		t.Fatalf("tags = %s", got)
	}
	if row.Engine != "vllm" || row.Framework != "dtk" || row.Registry != RegistryOCI {
		t.Fatalf("sibling must inherit the card: %+v", row)
	}
	if tag := findTag(t, row, "0.5.12-ubuntu22.04-dtk2604-py3.10"); tag.LastUpdated != "" || tag.EngineVersion != "0.5.12" {
		t.Fatalf("oci tag = %+v", tag)
	}
	if !findTag(t, row, "latest").RollingTag {
		t.Fatal("latest must be rolling")
	}
}

func TestBrowseOCIRefusedTokenIsReadableError(t *testing.T) {
	fake := &fakeRegistries{handler: harborHandler(t, false)}
	browser := fake.install(t)
	_, err := browser.Browse(context.Background(), harborRepo, nil)
	if err == nil || !strings.Contains(err.Error(), "community registry requires credentials") {
		t.Fatalf("err = %v", err)
	}
}

// An unreachable registry is an error, not a list of placeholder tags. A tag
// that was listed before and is gone from the next successful list stays
// visible, marked, once the cache expires.
func TestBrowseUpstreamFailureAndGoneTags(t *testing.T) {
	fail := true
	fake := &fakeRegistries{handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Query().Get("page") == "" && len(r.URL.Query()) == 1 {
			_, _ = io.WriteString(w, `{"next":null,"results":[{"name":"v0.2.0"}]}`)
		}
	}}
	browser := fake.install(t)
	row, err := browser.Browse(context.Background(), "lmsysorg/sglang", nil)
	if err == nil || len(row.Tags) != 0 {
		t.Fatalf("failed browse = %+v, %v", row, err)
	}
	fail = false
	// An expired entry for the same configuration: its tags are what the
	// refreshed list is merged against.
	browser.mu.Lock()
	browser.entries["lmsysorg/sglang"] = cacheEntry{fingerprint: fingerprint(Repo{Repo: "lmsysorg/sglang", Registry: RegistryDockerHub}), tags: []Tag{{Tag: "v0.1.0", EngineVersion: "0.1.0"}}}
	browser.mu.Unlock()
	row, err = browser.Browse(context.Background(), "lmsysorg/sglang", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Tags) != 2 || row.Tags[0].Tag != "v0.2.0" || row.Tags[0].UpstreamGone || row.Tags[1].Tag != "v0.1.0" || !row.Tags[1].UpstreamGone {
		t.Fatalf("merged = %+v", row.Tags)
	}
}

// Changing where a repository is listed from is a different list: the cached
// tags of the old configuration are neither served nor merged.
func TestBrowseCacheFollowsConfiguration(t *testing.T) {
	fake := &fakeRegistries{handler: func(w http.ResponseWriter, r *http.Request, host string) {
		if host == "mirror.example" {
			_, _ = io.WriteString(w, `{"next":null,"results":[{"name":"from-mirror"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"next":null,"results":[{"name":"from-hub"}]}`)
	}}
	browser := fake.install(t)
	first, err := browser.Browse(context.Background(), "acme/runner", []Repo{{Repo: "acme/runner", Engine: "vllm"}})
	if err != nil || len(first.Tags) != 1 || first.Tags[0].Tag != "from-hub" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	moved, err := browser.Browse(context.Background(), "acme/runner", []Repo{{Repo: "acme/runner", Engine: "vllm", UpstreamAPI: "https://mirror.example/v2/repositories/acme/runner/tags"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(moved.Tags) != 1 || moved.Tags[0].Tag != "from-mirror" {
		t.Fatalf("after reconfiguring = %+v, want only the new registry's tags", moved.Tags)
	}
}

func TestCommunityReposOverlayAndValidation(t *testing.T) {
	repos, err := Repos([]Repo{
		{Repo: "vllm/vllm-openai", Engine: "vllm", Framework: "rocm"},
		{Repo: "registry.example.com/team/runner", Engine: "callable", Framework: "cuda"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, row := range repos {
		names = append(names, row.Repo+"="+row.Registry)
	}
	want := "vllm/vllm-openai=docker-hub,vllm/vllm-omni=docker-hub,lmsysorg/sglang=docker-hub,quay.io/ascend/vllm-ascend=quay,quay.io/ascend/sglang=quay,harbor.sourcefind.cn:5443/dcu/admin/base/vllm=oci,quay.io/gpuworks/gpuworks-runner=quay,registry.example.com/team/runner=oci"
	if strings.Join(names, ",") != want {
		t.Fatalf("repos = %v", names)
	}
	if repos[0].Framework != "rocm" {
		t.Fatalf("configured entry must replace the built-in one: %+v", repos[0])
	}
	if len(repos[5].Siblings) != 2 {
		t.Fatalf("dcu card siblings = %+v", repos[5])
	}
	for _, bad := range []Repo{
		{Repo: "bad", Engine: "vllm", UpstreamAPI: "file:///etc/passwd"},
		{Repo: "bad", Engine: "vllm", Registry: "ftp"},
		{Repo: ""},
	} {
		if err := ValidateRepos([]Repo{bad}); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestIsRollingTagDetectsMutableNamesOnly(t *testing.T) {
	for _, value := range []string{"latest", "repo:main", "registry.example/team/image:stable", "master", "nightly", "vllm/vllm-openai:NIGHTLY", "dev", "edge"} {
		if !IsRollingTag(value) {
			t.Fatalf("%q was not detected as rolling", value)
		}
	}
	for _, value := range []string{"v0.8.5", "release-candidate", "sha-abcdef123456", "qwen38", "nightly-2026-10-08", "localhost:5000/latest"} {
		if IsRollingTag(value) {
			t.Fatalf("%q was incorrectly detected as rolling", value)
		}
	}
}

func TestInferEngineVersionFromCommunityTags(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  string
	}{
		{value: "v0.9.0", want: "0.9.0"},
		{value: "cuda13.0-vllm0.29.0-amd64-slot01", want: "0.29.0"},
		{value: "cann9.0-sglang0.5.15.post1-arm64-diffusion-slot16", want: "0.5.15.post1"},
		{value: "cuda13.0-vllm-omni0.30.0-r3", want: "0.30.0"},
		{value: "xre-vllm0.1.dev3-amd64-slot25", want: "0.1.dev3"},
		{value: "release-0.8.5", want: "0.8.5"},
		{value: "registry.example/vllm:v0.10", want: "0.10"},
		{value: "latest", want: ""},
		{value: "sha-abcdef123456", want: ""},
	} {
		if got := InferEngineVersion(tt.value); got != tt.want {
			t.Errorf("InferEngineVersion(%q) = %q, want %q", tt.value, got, tt.want)
		}
	}
}

func TestMixedRepositoryDoesNotInventAnEngine(t *testing.T) {
	rows, err := Repos([]Repo{{Repo: "registry.example/team/mixed"}})
	if err != nil {
		t.Fatal(err)
	}
	row := lookup("registry.example/team/mixed", rows)
	if row.Engine != "" || row.Framework != "" {
		t.Fatal("mixed repository received invented engine defaults")
	}
}
