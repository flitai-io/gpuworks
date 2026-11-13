package modelrepo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResolveURLUsesSourceRevisionSemantics(t *testing.T) {
	hf := ResolveURL(SourceHF, "org/model", "", "config.json", "https://hf.example")
	if want := "https://hf.example/org/model/resolve/main/config.json"; hf != want {
		t.Fatalf("HF URL = %q, want %q", hf, want)
	}
	ms := ResolveURL(SourceMS, "org/model", "v1", "README.md", "https://ms.example/")
	if want := "https://ms.example/models/org/model/resolve/v1/README.md"; ms != want {
		t.Fatalf("ModelScope pinned URL = %q, want %q", ms, want)
	}
	msDefault := ResolveURL(SourceMS, "org/model", "", "weights/model 1.bin", "https://ms.example/")
	if want := "https://ms.example/api/v1/models/org/model/repo?FilePath=weights%2Fmodel+1.bin"; msDefault != want {
		t.Fatalf("ModelScope default URL = %q, want %q", msDefault, want)
	}
}

func TestNormalizeSubfolderAcceptsOnlyCanonicalRelativePaths(t *testing.T) {
	if got, err := NormalizeSubfolder("FL2VA/text_encoder"); err != nil || got != "FL2VA/text_encoder" {
		t.Fatalf("normalized=%q err=%v", got, err)
	}
	for _, value := range []string{"/FL2VA", "../FL2VA", "FL2VA/../Ref2VA", `FL2VA\\weights`} {
		if _, err := NormalizeSubfolder(value); err == nil {
			t.Fatalf("unsafe subfolder %q was accepted", value)
		}
	}
}

func TestListFilesParsesHuggingFaceAndModelScopeResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/api/models/org/model/tree/main":
			if got := r.URL.Query().Get("recursive"); got != "true" {
				t.Errorf("HF recursive = %q, want true", got)
			}
			_, _ = w.Write([]byte(`[{"type":"file","path":"config.json","size":12},{"type":"directory","path":"docs"},{"type":"file","path":"model.safetensors","size":34}]`))
		case "/api/v1/models/org/model/repo/files":
			query := r.URL.Query()
			if got := query.Get("Recursive"); got != "true" {
				t.Errorf("ModelScope Recursive = %q, want true", got)
			}
			if _, ok := query["Revision"]; ok {
				t.Errorf("empty ModelScope revision was sent: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"Code":200,"Data":{"LatestCommitter":{"CommittedDate":20,"ShortId":"def456"},"Files":[{"Path":"config.json","Size":12,"Type":"blob","Revision":"abc123","CommittedDate":10},{"Name":"README.md","Size":8,"Type":"blob","Revision":"def4567890","CommittedDate":20},{"Name":"docs","Type":"tree","Revision":"def4567890","CommittedDate":20}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	hf, err := ListFiles(context.Background(), SourceHF, "org/model", "", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []File{{Path: "config.json", Size: 12}, {Path: "model.safetensors", Size: 34}}; !reflect.DeepEqual(hf, want) {
		t.Fatalf("HF files = %#v, want %#v", hf, want)
	}
	ms, err := ListFiles(context.Background(), SourceMS, "org/model", "", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []File{{Path: "config.json", Size: 12, Revision: "def4567890"}, {Path: "README.md", Size: 8, Revision: "def4567890"}}; !reflect.DeepEqual(ms, want) {
		t.Fatalf("ModelScope files = %#v, want %#v", ms, want)
	}
}

func TestListModelScopeSendsExplicitRevision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("Revision"); got != "release/v1" {
			t.Errorf("Revision = %q, want release/v1", got)
		}
		if got := r.URL.Query().Get("Recursive"); got != "true" {
			t.Errorf("Recursive = %q, want true", got)
		}
		_, _ = w.Write([]byte(`{"Code":200,"Data":{"Files":[{"Path":"config.json","Size":1,"Type":"blob","Revision":"abc123"}]}}`))
	}))
	defer server.Close()

	if _, err := ListFiles(context.Background(), SourceMS, "org/model", "release/v1", server.URL, nil); err != nil {
		t.Fatal(err)
	}
}

func TestListModelScopeRejectsBusinessErrorAndEmptyFiles(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "business error", body: `{"Code":400,"Message":"bad revision","Data":{"Files":null}}`, want: "code=400 message=bad revision"},
		{name: "empty files", body: `{"Code":200,"Data":{"Files":null},"Success":true}`, want: "returned no files"},
		{name: "directories only", body: `{"Code":200,"Data":{"Files":[{"Path":"docs","Type":"tree"}]}}`, want: "returned no files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			_, err := ListFiles(context.Background(), SourceMS, "org/model", "missing", server.URL, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), server.URL+"/api/v1/models/org/model/repo/files") || !strings.Contains(err.Error(), `source=modelscope revision="missing"`) {
				t.Fatalf("error = %v, want %q with URL/source/revision", err, tt.want)
			}
		})
	}
}

func TestListHuggingFaceRejectsEmptyFilesWithRequestContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	_, err := ListFiles(context.Background(), SourceHF, "org/model", "", server.URL, nil)
	if err == nil || !strings.Contains(err.Error(), server.URL+"/api/models/org/model/tree/main?recursive=true") || !strings.Contains(err.Error(), `source=huggingface revision="main"`) {
		t.Fatalf("error = %v, want URL/source/revision", err)
	}
}

func TestListFilesRejectsEmptyHTTPResponsesWithRequestContext(t *testing.T) {
	for _, source := range []string{SourceHF, SourceMS} {
		t.Run(source, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
			}))
			defer server.Close()
			_, err := ListFiles(context.Background(), source, "org/model", "missing", server.URL, nil)
			if err == nil || !strings.Contains(err.Error(), "empty response") ||
				!strings.Contains(err.Error(), server.URL) ||
				!strings.Contains(err.Error(), "source="+source) ||
				!strings.Contains(err.Error(), `revision="missing"`) {
				t.Fatalf("error = %v, want empty response with URL/source/revision", err)
			}
		})
	}
}

// The Hugging Face revision endpoint returns the full model card of the
// requested ref; only "sha" identifies the commit the ref currently points at.
const hfRevisionBody = `{"_id":"621ffdc036468d709f17434d","id":"org/model","modelId":"org/model","author":"org",` +
	`"sha":"2fc0636424b8a1e0c1d1f0d3d1c0a9b8f7e6d5c4","lastModified":"2025-04-28T09:26:23.000Z","private":false,` +
	`"gated":false,"disabled":false,"library_name":"transformers","tags":["transformers","safetensors","qwen3"],` +
	`"downloads":1234,"likes":56,"pipeline_tag":"text-generation",` +
	`"siblings":[{"rfilename":"config.json"},{"rfilename":"model.safetensors"}],"config":{"model_type":"qwen3"}}`

func TestResolveRevisionReadsHuggingFaceDefaultBranchCommit(t *testing.T) {
	var requested atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/models/org/model/revision/main", "/api/models/org/model/revision/refs/pr/3":
			_, _ = w.Write([]byte(hfRevisionBody))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Repository not found"}`))
		}
	}))
	defer server.Close()

	const want = "2fc0636424b8a1e0c1d1f0d3d1c0a9b8f7e6d5c4"
	got, err := ResolveRevision(context.Background(), SourceHF, "org/model", "", server.URL, nil)
	if err != nil || got != want {
		t.Fatalf("default branch revision = %q err=%v, want %q", got, err, want)
	}
	if path := requested.Load(); path != "/api/models/org/model/revision/main" {
		t.Fatalf("empty revision requested %v, want the default branch", path)
	}
	if got, err := ResolveRevision(context.Background(), SourceHF, "org/model", "refs/pr/3", server.URL, nil); err != nil || got != want {
		t.Fatalf("explicit ref revision = %q err=%v", got, err)
	}
	if _, err := ResolveRevision(context.Background(), SourceHF, "org/missing", "", server.URL, nil); err == nil || !strings.Contains(err.Error(), "http 404") {
		t.Fatalf("missing repository error = %v, want http 404", err)
	}
}

func TestResolveRevisionRejectsUnusableHuggingFaceResponses(t *testing.T) {
	for _, tt := range []struct {
		name, body, want string
	}{
		{name: "empty body", body: "", want: "empty response"},
		{name: "no commit", body: `{"id":"org/model","siblings":[]}`, want: "returned no commit"},
		{name: "not json", body: `<html>proxy login</html>`, want: "parse hf revision"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			got, err := ResolveRevision(context.Background(), SourceHF, "org/model", "", server.URL, nil)
			if got != "" || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("revision=%q err=%v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestResolveRevisionReadsModelScopeLatestCommit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/models/org/model/repo/files" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("Recursive"); got != "true" {
			t.Errorf("Recursive = %q, want true", got)
		}
		if _, ok := r.URL.Query()["Revision"]; ok {
			t.Errorf("empty revision was sent as a query parameter: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"Code":200,"Message":"success","RequestId":"a1","Success":true,"Data":{` +
			`"LatestCommitter":{"AuthorName":"org","CommittedDate":1745832383,"ShortId":"4d58a7b5","Message":"update weights"},` +
			`"Files":[` +
			`{"Path":"config.json","Name":"config.json","Size":812,"Type":"blob","Revision":"4d58a7b524cd0a41f80a7fbb6a5b22f2fca1e3c9","CommittedDate":1745832383},` +
			`{"Path":"weights","Name":"weights","Type":"tree","Revision":"0b1e2d3c4f5a6b7c8d9e0f1a2b3c4d5e6f708192","CommittedDate":1740000000},` +
			`{"Path":"weights/model.safetensors","Name":"model.safetensors","Size":1610612736,"Type":"blob","Revision":"0b1e2d3c4f5a6b7c8d9e0f1a2b3c4d5e6f708192","CommittedDate":1740000000}` +
			`]}}`))
	}))
	defer server.Close()

	got, err := ResolveRevision(context.Background(), SourceMS, "org/model", "", server.URL, nil)
	if want := "4d58a7b524cd0a41f80a7fbb6a5b22f2fca1e3c9"; err != nil || got != want {
		t.Fatalf("revision = %q err=%v, want %q", got, err, want)
	}
}

func TestResolveRevisionRejectsUnusableModelScopeResponses(t *testing.T) {
	for _, tt := range []struct {
		name, body, want string
	}{
		{name: "business error", body: `{"Code":404,"Message":"The model can not be found","Success":false,"Data":null}`, want: "code=404 message=The model can not be found"},
		{name: "no commit", body: `{"Code":200,"Success":true,"Data":{"LatestCommitter":{},"Files":[{"Path":"config.json","Type":"blob","Size":3}]}}`, want: "returned no commit"},
		{name: "empty body", body: ``, want: "empty response"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			got, err := ResolveRevision(context.Background(), SourceMS, "org/model", "", server.URL, nil)
			if got != "" || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("revision=%q err=%v, want %q", got, err, tt.want)
			}
			if !strings.Contains(err.Error(), "source="+SourceMS) {
				t.Fatalf("error lost its request context: %v", err)
			}
		})
	}
}

func TestResolveRevisionAppliesRewriteAndInjectedClient(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/models/org/model/revision/main" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(hfRevisionBody))
	}))
	defer server.Close()
	rewrite := func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		base, _ := url.Parse(server.URL)
		parsed.Scheme, parsed.Host = base.Scheme, base.Host
		return parsed.String()
	}
	got, err := ResolveRevision(context.Background(), SourceHF, "org/model", "", "", map[string]string{"Authorization": "Bearer token"},
		RequestOptions{Client: server.Client(), Rewrite: rewrite})
	if err != nil || got != "2fc0636424b8a1e0c1d1f0d3d1c0a9b8f7e6d5c4" || requests.Load() != 1 {
		t.Fatalf("revision=%q err=%v requests=%d", got, err, requests.Load())
	}
}

func TestListFilesAppliesRewriteAndInjectedClient(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/v1/models/org/model/repo/files" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"Code":200,"Data":{"Files":[{"Path":"config.json","Size":1,"Type":"blob","Revision":"abc123"}]}}`))
	}))
	defer server.Close()

	rewrite := func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		base, _ := url.Parse(server.URL)
		parsed.Scheme, parsed.Host = base.Scheme, base.Host
		return parsed.String()
	}
	files, err := ListFiles(context.Background(), SourceMS, "org/model", "", "", nil, RequestOptions{Client: server.Client(), Rewrite: rewrite})
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || len(files) != 1 || files[0].Revision != "abc123" {
		t.Fatalf("requests=%d files=%+v", requests.Load(), files)
	}
}

func TestFilesUnderSubfolderKeepsOnlyThatDirectory(t *testing.T) {
	files := []File{
		{Path: "README.md", Size: 1},
		{Path: "IQ2_XS/a.gguf", Size: 2},
		{Path: "IQ2_XS/nested/b.gguf", Size: 3},
		{Path: "IQ2_XS_big/c.gguf", Size: 4},
		{Path: "IQ2_XS", Size: 5},
		{Path: "IQ3_S/d.gguf", Size: 6},
	}
	got := FilesUnderSubfolder(files, "IQ2_XS")
	if len(got) != 2 || got[0].Path != "IQ2_XS/a.gguf" || got[1].Path != "IQ2_XS/nested/b.gguf" {
		t.Fatalf("FilesUnderSubfolder = %+v, want the two files inside IQ2_XS/ with their repository paths", got)
	}
	if all := FilesUnderSubfolder(files, ""); len(all) != len(files) {
		t.Fatalf("an empty subfolder selected %d of %d files", len(all), len(files))
	}
	// The IQ2_XS directory wins over the file of the same name.
	if none := FilesUnderSubfolder(files, "missing"); len(none) != 0 {
		t.Fatalf("an unknown subfolder selected %+v", none)
	}
}

// A repository that keeps many adapter files at its root (LightX2V publishes
// every Turbo LoRA revision side by side) is reachable one file at a time: a
// subfolder naming a file selects just that file, at its repository path.
func TestFilesUnderSubfolderSelectsOneFileWhenNoDirectoryHasThatName(t *testing.T) {
	files := []File{
		{Path: "README.md", Size: 1},
		{Path: "turbo_4step_v1.3.safetensors", Size: 2},
		{Path: "turbo_4step_v1.3.safetensors.bak", Size: 3},
		{Path: "turbo_8step_v1.1.safetensors", Size: 4},
	}
	got := FilesUnderSubfolder(files, "turbo_4step_v1.3.safetensors")
	if len(got) != 1 || got[0].Path != "turbo_4step_v1.3.safetensors" || got[0].Size != 2 {
		t.Fatalf("FilesUnderSubfolder = %+v, want only the named file", got)
	}
	nested := FilesUnderSubfolder([]File{{Path: `loras\a.safetensors`, Size: 5}, {Path: "loras/b.safetensors", Size: 6}}, "loras/a.safetensors")
	if len(nested) != 1 || nested[0].Path != "loras/a.safetensors" {
		t.Fatalf("nested file selection = %+v", nested)
	}
}

// ".." alone climbs out of the repository as surely as "../x" does, and a
// hub that lists paths with backslashes still has its subtree found, returned
// in the repository's forward-slash form.
func TestSubfolderRefusesTheParentAndReadsBackslashListings(t *testing.T) {
	if _, err := NormalizeSubfolder(".."); err == nil {
		t.Fatal(`".." was accepted as a subfolder`)
	}
	got := FilesUnderSubfolder([]File{{Path: `IQ2_XS\model-00001.gguf`, Size: 1}, {Path: "Q4/model.gguf", Size: 2}}, "IQ2_XS")
	if len(got) != 1 || got[0].Path != "IQ2_XS/model-00001.gguf" {
		t.Fatalf("files = %+v", got)
	}
}
