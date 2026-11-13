package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

var cliBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "changerecord-cli-")
	if err != nil {
		panic(err)
	}
	cliBinary = filepath.Join(dir, "changerecord")
	cmd := exec.Command("go", "build", "-o", cliBinary, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		panic(string(out) + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func gitCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeCLI(t *testing.T, root, name, body string) {
	t.Helper()
	file := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func newCLIRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	gitCLI(t, root, "init", "-b", "master")
	writeCLI(t, root, "scripts/change-record-rules.txt", "decision\tinternal/agent/modelsync/**\ncontract\tinternal/server/api/errors.go\n")
	writeCLI(t, root, "internal/agent/modelsync/payload.go", "original\n")
	writeCLI(t, root, "internal/server/api/errors.go", "original\n")
	writeCLI(t, root, "docs/reference/old.md", "old record\n")
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "base")
	base := gitCLI(t, root, "rev-parse", "HEAD")
	gitCLI(t, root, "switch", "-c", "work/record")
	writeCLI(t, root, "internal/agent/modelsync/payload.go", "changed\n")
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "decision change")
	return root, base
}

func runCLI(t *testing.T, root, base string, extra ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(cliBinary)
	cmd.Dir = root
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		if key == "CI" || strings.HasPrefix(key, "CI_") || strings.HasPrefix(key, "CHANGE_RECORD_") || key == "GITLAB_TOKEN" {
			continue
		}
		cmd.Env = append(cmd.Env, v)
	}
	cmd.Env = append(cmd.Env, "CHANGE_RECORD_BASE="+base, "CHANGE_RECORD_BRANCH=work/record")
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		return string(out), e.ExitCode()
	}
	t.Fatal(err)
	return "", -1
}

func TestCLIDecisionRejectsHistoricalPlan(t *testing.T) {
	root, base := newCLIRepo(t)
	artifacts := filepath.Join(root, "artifacts")
	if err := os.Mkdir(artifacts, 0755); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, artifacts, "init", "-b", "master")
	writeCLI(t, artifacts, "archive/old/plan.md", "---\ndecision: historical choice\nrejected: historical alternative\n---\n")
	gitCLI(t, artifacts, "add", ".")
	gitCLI(t, artifacts, "commit", "-m", "historical record")
	gitCLI(t, artifacts, "switch", "-c", "work/record")
	out, code := runCLI(t, root, base)
	if code == 0 {
		t.Fatalf("historical plan must not satisfy this decision change: %s", out)
	}
}

func TestCLIRenameKeepsControlledSourcePath(t *testing.T) {
	root, base := newCLIRepo(t)
	gitCLI(t, root, "reset", "--hard", base)
	if err := os.MkdirAll(filepath.Join(root, "internal/agent/transport"), 0755); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, root, "mv", "internal/agent/modelsync/payload.go", "internal/agent/transport/payload.go")
	gitCLI(t, root, "commit", "-m", "move transport")
	out, code := runCLI(t, root, base)
	if code == 0 {
		t.Fatalf("moving controlled source still needs a record: %s", out)
	}
}

func addCurrentLocalPlan(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "artifacts")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, dir, "init", "-b", "master")
	writeCLI(t, dir, "README.md", "artifact repository\n")
	gitCLI(t, dir, "add", ".")
	gitCLI(t, dir, "commit", "-m", "base")
	gitCLI(t, dir, "switch", "-c", "work/record")
	writeCLI(t, dir, "work/current/plan.md", "---\ndecision: explicit current choice\nrejected: explicit alternative\n---\n")
	gitCLI(t, dir, "add", ".")
	gitCLI(t, dir, "commit", "-m", "current record")
}

func TestCLICIMustVerifyCompanionMR(t *testing.T) {
	root, base := newCLIRepo(t)
	addCurrentLocalPlan(t, root)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "[]")
	}))
	defer server.Close()
	out, code := runCLI(t, root, base, "CI_PIPELINE_SOURCE=merge_request_event", "CI_MERGE_REQUEST_IID=1024", "CI_API_V4_URL="+server.URL, "GITLAB_TOKEN=fixture-token")
	if code == 0 {
		t.Fatalf("local checkout must not replace CI companion MR verification: %s", out)
	}
}

func TestCLICIDecisionRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		plans  string
		body   string
		wantOK bool
	}{
		{"valid", 200, "work/current/plan.md", "---\ndecision: keep direct sync\nrejected: server relay\n---\n", true},
		{"delete old and add valid", 200, "work/current/plan.md", "---\ndecision: keep direct sync\nrejected: server relay\n---\n", true},
		{"missing fields", 200, "work/current/plan.md", "---\ntitle: incomplete\n---\n", false},
		{"unchanged historical plan", 200, "README.md", "", false},
		{"forbidden", 403, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, base := newCLIRepo(t)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("PRIVATE-TOKEN") != "fixture-token" || r.Header.Get("JOB-TOKEN") != "" {
					http.Error(w, "wrong authentication", 401)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 200 {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, `{"message":"Forbidden"}`)
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/merge_requests"):
					if r.URL.Query().Get("source_branch") != "work/record" || r.URL.Query().Get("state") != "opened" {
						http.Error(w, "wrong MR query", 400)
						return
					}
					fmt.Fprint(w, `[{"iid":7}]`)
				case strings.HasSuffix(r.URL.Path, "/merge_requests/7/changes"):
					changes := []map[string]any{{"new_path": tc.plans}}
					if tc.name == "delete old and add valid" {
						changes = append([]map[string]any{{"new_path": "archive/deleted/plan.md", "deleted_file": true}}, changes...)
					}
					json.NewEncoder(w).Encode(map[string]any{"changes": changes, "diff_refs": map[string]string{"head_sha": "1111111111111111111111111111111111111111"}, "source_project_id": 42})
				case strings.Contains(r.URL.Path, "/repository/files/"):
					if strings.Contains(r.URL.Path, "archive/deleted/plan.md") {
						http.NotFound(w, r)
						return
					}
					if !strings.Contains(r.URL.Path, "/projects/42/") || r.URL.Query().Get("ref") != "1111111111111111111111111111111111111111" {
						http.Error(w, "must read companion source project at verified revision", 400)
						return
					}
					fmt.Fprint(w, tc.body)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			out, code := runCLI(t, root, base, "CI=true", "CI_PIPELINE_SOURCE=merge_request_event", "CI_MERGE_REQUEST_IID=1024", "CI_API_V4_URL="+server.URL, "CHANGE_RECORD_ARTIFACTS_TOKEN=fixture-token")
			if (code == 0) != tc.wantOK {
				t.Fatalf("exit %d, wantOK %v: %s", code, tc.wantOK, out)
			}
			if requests.Load() == 0 {
				t.Fatal("decision verification must contact GitLab")
			}
		})
	}
}

func TestCLILocalCurrentPlanWorksOffline(t *testing.T) {
	root, base := newCLIRepo(t)
	addCurrentLocalPlan(t, root)
	out, code := runCLI(t, root, base)
	if code != 0 {
		t.Fatalf("current local plan should permit offline preflight: %s", out)
	}
}

func TestCLIMergedPipelineDoesNotRequireOpenCompanion(t *testing.T) {
	root, base := newCLIRepo(t)
	for _, source := range []string{"push", "schedule", "web"} {
		out, code := runCLI(t, root, base, "CI=true", "CI_PIPELINE_SOURCE="+source)
		if code != 0 || !strings.Contains(out, "non-MR") {
			t.Fatalf("%s pipeline must not re-require open MR: %d %s", source, code, out)
		}
	}
}

func TestCLIContractNeedsUpdatedSourceDocs(t *testing.T) {
	root, base := newCLIRepo(t)
	gitCLI(t, root, "reset", "--hard", base)
	writeCLI(t, root, "internal/server/api/errors.go", "changed\n")
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "contract change")
	if out, code := runCLI(t, root, base); code == 0 {
		t.Fatalf("missing docs must fail: %s", out)
	}
	writeCLI(t, root, "docs-en/reference/api.md", "translation alone\n")
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "translation")
	if out, code := runCLI(t, root, base); code == 0 {
		t.Fatalf("translation alone must fail: %s", out)
	}
	writeCLI(t, root, "docs/reference/api.md", "new contract\n")
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "source record")
	if out, code := runCLI(t, root, base); code != 0 {
		t.Fatalf("updated source docs must pass: %s", out)
	}
}

func TestCLIDeletingDocsDoesNotRecordContract(t *testing.T) {
	root, base := newCLIRepo(t)
	gitCLI(t, root, "reset", "--hard", base)
	writeCLI(t, root, "internal/server/api/errors.go", "changed\n")
	gitCLI(t, root, "rm", "docs/reference/old.md")
	gitCLI(t, root, "add", ".")
	gitCLI(t, root, "commit", "-m", "delete old doc with contract change")
	if out, code := runCLI(t, root, base); code == 0 {
		t.Fatalf("deleting docs does not describe the contract: %s", out)
	}
}
