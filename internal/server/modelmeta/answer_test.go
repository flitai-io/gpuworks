package modelmeta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hubAnswers answers a request with the route of the longest prefix its path
// starts with, and 404 without one. The statuses, bodies and error headers
// the tests give it are the ones the real sites send (read 2026-11-25).
func hubAnswers(t *testing.T, routes map[string]func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		longest := ""
		for prefix := range routes {
			if strings.HasPrefix(r.URL.Path, prefix) && len(prefix) > len(longest) {
				longest = prefix
			}
		}
		if longest == "" {
			http.NotFound(w, r)
			return
		}
		routes[longest](w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonAnswer(status int, body string, headers ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const (
	msRepoMissing     = `{"Code":10010205001,"Message":"获取模型信息失败，信息：record not found","RequestId":"29f24b93","Success":false}`
	msFileMissing     = `{"Code":10990101007,"Message":"获取模型文件失败，文件内容为空","RequestId":"ee8a42f4","Success":false}`
	msRevisionMissing = `{"Code":10990101004,"Message":"获取模型目录树失败，信息：unihub GET /repos/models/acme/model/tree: http=404 code=404","RequestId":"30d967e5","Success":false}`
	msRecord          = `{"Code":200,"Data":{"CreatedTime":1735787045,"LastUpdatedTime":1735787045},"Success":true}`
	hfNoToken         = `{"error":"Invalid username or password."}`
	hfRepoMissing     = `{"error":"Repository not found"}`
	hfRevisionMissing = `{"error":"Invalid rev id: deadbeef"}`
)

func TestFetchSaysWhatTheSourceAnsweredWhenItReadNothing(t *testing.T) {
	msMissing := hubAnswers(t, map[string]func(http.ResponseWriter){
		"/api/v1/models/": jsonAnswer(http.StatusNotFound, msRepoMissing),
		"/models/":        jsonAnswer(http.StatusNotFound, msFileMissing),
	})
	msWrongRevision := hubAnswers(t, map[string]func(http.ResponseWriter){
		"/api/v1/models/acme/model/repo/files": jsonAnswer(http.StatusNotFound, msRevisionMissing),
		"/api/v1/models/acme/model":            jsonAnswer(http.StatusOK, msRecord),
		"/models/":                             jsonAnswer(http.StatusNotFound, msFileMissing),
	})
	// Hugging Face without a token: a repository that does not exist and a
	// private one both answer 401.
	hfAnonymous := hubAnswers(t, map[string]func(http.ResponseWriter){
		"/": jsonAnswer(http.StatusUnauthorized, hfNoToken, "X-Error-Message", "Invalid username or password."),
	})
	hfMissing := hubAnswers(t, map[string]func(http.ResponseWriter){
		"/": jsonAnswer(http.StatusNotFound, hfRepoMissing, "X-Error-Code", "RepoNotFound"),
	})
	hfWrongRevision := hubAnswers(t, map[string]func(http.ResponseWriter){
		"/api/models/acme/model/tree/":     jsonAnswer(http.StatusNotFound, hfRevisionMissing, "X-Error-Code", "RevisionNotFound"),
		"/api/models/acme/model/revision/": jsonAnswer(http.StatusNotFound, hfRevisionMissing, "X-Error-Code", "RevisionNotFound"),
		"/api/models/acme/model":           jsonAnswer(http.StatusOK, `{"author":"","createdAt":"2025-01-02T03:04:05Z"}`),
		"/acme/model/resolve/":             jsonAnswer(http.StatusNotFound, hfRevisionMissing, "X-Error-Code", "RevisionNotFound"),
	})
	hfGated := hubAnswers(t, map[string]func(http.ResponseWriter){
		"/api/models/acme/model/tree/": jsonAnswer(http.StatusForbidden, `{"error":"Access to model acme/model is restricted."}`, "X-Error-Code", "GatedRepo"),
		"/api/models/acme/model":       jsonAnswer(http.StatusOK, `{"author":"","createdAt":"2025-01-02T03:04:05Z"}`),
		"/acme/model/resolve/":         jsonAnswer(http.StatusForbidden, `{"error":"Access to model acme/model is restricted."}`, "X-Error-Code", "GatedRepo"),
	})
	overloaded := hubAnswers(t, map[string]func(http.ResponseWriter){
		"/": jsonAnswer(http.StatusServiceUnavailable, `{"error":"Service Unavailable"}`),
	})
	dead := deadAddress(t)

	for name, tc := range map[string]struct {
		source, revision string
		candidates       []string
		want             Answer
	}{
		"modelscope has no such repository":      {"modelscope", "master", []string{msMissing.URL}, Answer{Outcome: OutcomeRepoNotFound, Endpoint: msMissing.URL, Status: 404}},
		"modelscope has no such revision":        {"modelscope", "deadbeef", []string{msWrongRevision.URL}, Answer{Outcome: OutcomeRevisionNotFound, Endpoint: msWrongRevision.URL, Status: 404}},
		"hugging face asked without a token":     {"huggingface", "main", []string{hfAnonymous.URL}, Answer{Outcome: OutcomeDenied, Endpoint: hfAnonymous.URL, Status: 401}},
		"hugging face has no such repository":    {"huggingface", "main", []string{hfMissing.URL}, Answer{Outcome: OutcomeRepoNotFound, Endpoint: hfMissing.URL, Status: 404}},
		"hugging face has no such revision":      {"huggingface", "deadbeef", []string{hfWrongRevision.URL}, Answer{Outcome: OutcomeRevisionNotFound, Endpoint: hfWrongRevision.URL, Status: 404}},
		"hugging face shows a gated repository":  {"huggingface", "main", []string{hfGated.URL}, Answer{Outcome: OutcomeDenied, Endpoint: hfGated.URL, Status: 403}},
		"connection refused":                     {"huggingface", "main", []string{dead}, Answer{Outcome: OutcomeUnreachable, Endpoint: dead, Status: 0}},
		"an error status that says nothing":      {"huggingface", "main", []string{overloaded.URL}, Answer{Outcome: OutcomeUnreachable, Endpoint: overloaded.URL, Status: 503}},
		"a dead mirror before the site's answer": {"huggingface", "main", []string{dead, hfMissing.URL}, Answer{Outcome: OutcomeRepoNotFound, Endpoint: hfMissing.URL, Status: 404, Disputed: true}},
		"a mirror's answer before a dead site":   {"huggingface", "main", []string{hfMissing.URL, dead}, Answer{Outcome: OutcomeRepoNotFound, Endpoint: hfMissing.URL, Status: 404, Disputed: true}},
		"two addresses without the repository":   {"huggingface", "main", []string{hfMissing.URL, hfAnonymous.URL}, Answer{Outcome: OutcomeDenied, Endpoint: hfAnonymous.URL, Status: 401}},
	} {
		meta, err := FetchFrom(context.Background(), tc.source, "acme/model", tc.revision, "", tc.candidates, nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if meta.Answer != tc.want {
			t.Errorf("%s: answer = %+v, want %+v", name, meta.Answer, tc.want)
		}
		if meta.ConfigRaw != "" || meta.README != "" || meta.WeightBytes != 0 {
			t.Errorf("%s: read %+v from a source that served nothing", name, meta)
		}
		if definite := tc.want.Outcome != OutcomeDenied && tc.want.Outcome != OutcomeUnreachable && !tc.want.Disputed; meta.Answer.Definite() != definite {
			t.Errorf("%s: definite = %v, want %v", name, meta.Answer.Definite(), definite)
		}
	}
}

// A site that accepts the connection and never answers is unreachable, with
// no status: the probe's own deadline ends the wait.
func TestFetchReportsATimeoutAsUnreachable(t *testing.T) {
	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); silent.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	meta, err := FetchFrom(ctx, "modelscope", "acme/model", "master", "", []string{silent.URL}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Answer{Outcome: OutcomeUnreachable, Endpoint: silent.URL, Status: 0}); meta.Answer != want {
		t.Fatalf("answer = %+v, want %+v", meta.Answer, want)
	}
}

// A fetch that read the repository's files carries no answer, whatever the
// record request said: a mirror that proxies only the files is such a site.
func TestFetchThatReadTheFilesCarriesNoAnswer(t *testing.T) {
	filesOnly := hubAnswers(t, map[string]func(http.ResponseWriter){
		"/acme/model/resolve/main/config.json": jsonAnswer(http.StatusOK, `{"model_type":"qwen3"}`),
	})
	meta, err := FetchFrom(context.Background(), "huggingface", "acme/model", "main", "", []string{filesOnly.URL}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Answer != (Answer{}) || meta.ConfigRaw == "" {
		t.Fatalf("meta = %+v", meta)
	}
}
