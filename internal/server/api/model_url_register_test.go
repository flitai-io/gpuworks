package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// urlDirectoryFixture is a faithful Apache mod_autoindex file server: a
// directory page whose size column is rounded for people, plus HEAD,
// Content-Length, Accept-Ranges and 206 on the files themselves.
type urlDirectoryFixture struct {
	root  string
	files map[string]string
	sizes map[string]string
}

func newURLDirectoryFixture(t *testing.T, root string, files map[string]string, sizes map[string]string) string {
	t.Helper()
	fixture := &urlDirectoryFixture{root: root, files: files, sizes: sizes}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	return server.URL + root
}

func (f *urlDirectoryFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, f.root) {
		http.NotFound(w, r)
		return
	}
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, f.root), "/")
	if rel == "" {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "text/html;charset=UTF-8")
		var out strings.Builder
		out.WriteString("<html><body><table>\n")
		out.WriteString(`<tr><td><a href="?C=N;O=D">Name</a></td></tr>` + "\n")
		out.WriteString(`<tr><td><a href="/">Parent Directory</a></td><td align="right">  - </td></tr>` + "\n")
		for _, name := range slices.Sorted(maps.Keys(f.files)) {
			size := f.sizes[name]
			if size == "" {
				size = fmt.Sprint(len(f.files[name]))
			}
			out.WriteString(`<tr><td><a href="` + name + `">` + name + `</a></td><td align="right">2026-09-01 14:43  </td><td align="right">` + size + `  </td></tr>` + "\n")
		}
		out.WriteString("</table></body></html>\n")
		_, _ = io.WriteString(w, out.String())
		return
	}
	body, ok := f.files[rel]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, rel, time.Unix(1788000000, 0).UTC(), strings.NewReader(body))
}

const urlModelConfig = `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3","hidden_size":1024,` +
	`"num_hidden_layers":28,"max_position_embeddings":40960,"torch_dtype":"bfloat16"}`

func urlModelDirectory(t *testing.T) string {
	t.Helper()
	return newURLDirectoryFixture(t, "/Qwen/Qwen3-0.6B", map[string]string{
		"config.json":       urlModelConfig,
		"model.safetensors": strings.Repeat("w", 9973),
	}, map[string]string{"model.safetensors": "9.7K"})
}

func urlModelStore(t *testing.T, name string) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestPreviewURLModelReturnsTheDirectoryAsAModel(t *testing.T) {
	endpoint := urlModelDirectory(t)
	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: urlModelStore(t, "api_preview_url")}}

	rec := httptest.NewRecorder()
	h.previewURLModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/preview-url",
		strings.NewReader(fmt.Sprintf(`{"url":%q}`, endpoint+"/"))))

	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var wire map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"source_url":       endpoint,
		"source_id":        "Qwen/Qwen3-0.6B",
		"name":             "Qwen3-0.6B",
		"arch":             "Qwen3ForCausalLM",
		"precision":        "bfloat16",
		"context_length":   float64(40960),
		"weight_bytes":     float64(9973),
		"files_count":      float64(2),
		"input_modalities": []any{"text"},
	}
	if len(wire) != len(want) {
		t.Fatalf("preview wire keys = %v, want %v", slices.Sorted(maps.Keys(wire)), slices.Sorted(maps.Keys(want)))
	}
	for key, value := range want {
		if got := fmt.Sprint(wire[key]); got != fmt.Sprint(value) {
			t.Errorf("%s = %v, want %v", key, wire[key], value)
		}
	}
}

func TestPreviewURLModelReportsWhyADirectoryCannotBeUsed(t *testing.T) {
	malformed := newURLDirectoryFixture(t, "/broken", map[string]string{"config.json": "{not json", "model.safetensors": "w"}, nil)
	oversized := map[string]string{"config.json": urlModelConfig}
	for index := 0; index < 5001; index++ {
		oversized[fmt.Sprintf("part-%05d.bin", index)] = "x"
	}
	huge := newURLDirectoryFixture(t, "/huge", oversized, nil)
	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: urlModelStore(t, "api_preview_url_errors")}}

	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{name: "blank url", body: `{"url":"   "}`, status: http.StatusBadRequest, code: ErrorModelSourceURLRequired},
		{name: "config.json that does not parse", body: fmt.Sprintf(`{"url":%q}`, malformed), status: http.StatusBadRequest, code: ErrorHTTPDirectoryNotAModel},
		{name: "larger than a repository", body: fmt.Sprintf(`{"url":%q}`, huge), status: http.StatusBadRequest, code: ErrorHTTPDirectoryTooLarge},
		{name: "host that does not answer", body: `{"url":"http://127.0.0.1:1/models"}`, status: http.StatusBadGateway, code: ErrorHTTPDirectoryUnreachable},
		{name: "wrong scheme", body: `{"url":"ftp://mirror.example.com/models"}`, status: http.StatusBadRequest, code: ErrorModelSourceURLInvalid},
		{name: "malformed request body", body: `{`, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.previewURLModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/preview-url", strings.NewReader(tc.body)))
			if rec.Code != tc.status {
				t.Fatalf("status/body = %d %s, want %d", rec.Code, rec.Body.String(), tc.status)
			}
			if tc.code == "" {
				return
			}
			var wire struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil || wire.Code != tc.code {
				t.Fatalf("body = %s, want code %s", rec.Body.String(), tc.code)
			}
		})
	}
}

func TestRegisterURLModelStoresTheDirectoryAsTheModelsEndpoint(t *testing.T) {
	endpoint := urlModelDirectory(t)
	st := urlModelStore(t, "api_register_url")
	endpoints := []string{}
	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, FetchModelMeta: func(_ context.Context, source, repo, revision, _ string, candidates []string) (*modelmeta.Meta, error) {
		endpoints = append(endpoints, source+"|"+repo+"|"+revision+"|"+strings.Join(candidates, ","))
		return &modelmeta.Meta{Arch: "Qwen3ForCausalLM", WeightBytes: 9973, ConfigRaw: urlModelConfig}, nil
	}}}

	rec := httptest.NewRecorder()
	h.registerURLModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/register-url",
		strings.NewReader(fmt.Sprintf(`{"source_url":%q,"source_id":"Qwen/Qwen3-0.6B-mirror"}`, endpoint+"/"))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}

	models, err := st.ListModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("models = %+v err = %v", models, err)
	}
	model := models[0]
	// The operator's corrected repository id is what the inference catalog and
	// the template matcher key on, so it must survive the preview's default.
	if model.Source != "url" || model.SourceID != "Qwen/Qwen3-0.6B-mirror" || model.SourceURL != endpoint || model.Revision != "" {
		t.Fatalf("model = %+v", model)
	}
	if model.Name != "Qwen3-0.6B" || model.Arch != "Qwen3ForCausalLM" || model.WeightBytes != 9973 {
		t.Fatalf("metadata = %+v", model)
	}
	want := "url|Qwen/Qwen3-0.6B-mirror||" + endpoint
	if !slices.Contains(endpoints, want) {
		t.Fatalf("metadata fetches = %v, want one of %q", endpoints, want)
	}

	// Registering the same directory again is the same identity, not a second
	// model.
	second := httptest.NewRecorder()
	h.registerURLModel(second, httptest.NewRequest(http.MethodPost, "/api/v1/models/register-url",
		strings.NewReader(fmt.Sprintf(`{"source_url":%q,"source_id":"Qwen/Qwen3-0.6B-mirror"}`, endpoint))))
	if second.Code != http.StatusCreated {
		t.Fatalf("second registration status/body = %d %s", second.Code, second.Body.String())
	}
	if models, err := st.ListModels(); err != nil || len(models) != 1 {
		t.Fatalf("re-registration produced %d models (err %v)", len(models), err)
	}
}

func TestRegisterURLModelRefusesADirectoryItCannotRead(t *testing.T) {
	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: urlModelStore(t, "api_register_url_errors")}}
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{name: "no directory", body: `{"source_id":"Qwen/Qwen3-0.6B"}`, status: http.StatusBadRequest, code: ErrorModelSourceURLRequired},
		{name: "host that does not answer", body: `{"source_url":"http://127.0.0.1:1/Qwen/Qwen3-0.6B"}`, status: http.StatusBadGateway, code: ErrorHTTPDirectoryUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.registerURLModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/register-url", strings.NewReader(tc.body)))
			var wire struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &wire)
			if rec.Code != tc.status || wire.Code != tc.code {
				t.Fatalf("status/body = %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
		})
	}
	if models, _ := h.d.Store.ListModels(); len(models) != 0 {
		t.Fatalf("a refused registration stored %d models", len(models))
	}
}

// A repository id names nothing on a plain file server, so the generic
// registration endpoint must send the operator to the one that reads the
// directory rather than storing a model with no endpoint.
func TestCreateModelRefusesADirectorySourceWithoutItsURL(t *testing.T) {
	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: urlModelStore(t, "api_create_url_without_directory")}}
	rec := httptest.NewRecorder()
	h.createModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models",
		strings.NewReader(`{"source":"url","source_id":"Qwen/Qwen3-0.6B"}`)))
	var wire struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &wire)
	if rec.Code != http.StatusBadRequest || wire.Code != ErrorModelSourceURLRequired {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	if models, _ := h.d.Store.ListModels(); len(models) != 0 {
		t.Fatalf("a refused registration stored %d models", len(models))
	}
}

func TestModelDTOCarriesTheDirectoryOnlyForDirectoryModels(t *testing.T) {
	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: urlModelStore(t, "api_model_dto_source_url")}}
	directory, err := h.d.Store.UpsertModel(&store.Model{
		Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://mirror.example.com/Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B",
	})
	if err != nil {
		t.Fatal(err)
	}
	hub, err := h.d.Store.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Revision: "main", Name: "Qwen3-0.6B"})
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(h.modelDTO(directory, false))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["source"] != "url" || wire["source_url"] != "https://mirror.example.com/Qwen/Qwen3-0.6B" {
		t.Fatalf("directory model wire = %s", raw)
	}
	// Round-trip: the field a client reads back is the one it was given.
	var back struct {
		SourceURL string `json:"source_url"`
	}
	if err := json.Unmarshal(raw, &back); err != nil || back.SourceURL != directory.SourceURL {
		t.Fatalf("round-trip source_url = %q err = %v", back.SourceURL, err)
	}

	hubRaw, err := json.Marshal(h.modelDTO(hub, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hubRaw), "source_url") {
		t.Fatalf("a repository model carries a directory: %s", hubRaw)
	}
}

func TestModelURLRoutesRequireAuthentication(t *testing.T) {
	signer, err := auth.NewTokenSigner("model-url-auth-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{AuthSigner: signer, Store: urlModelStore(t, "api_model_url_auth")})
	for _, path := range []string{"/api/v1/models/preview-url", "/api/v1/models/register-url"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"url":"https://mirror.example.com/x"}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated POST %s = %d, want 401", path, rec.Code)
		}
		// The routes exist for POST only; a GET must not fall through to a
		// different handler.
		get := httptest.NewRecorder()
		router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, path, nil))
		if get.Code != http.StatusUnauthorized && get.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 401 or 405", path, get.Code)
		}
	}
}

func TestDownloadToAnOldAgentIsRefusedOnlyForDirectoryModels(t *testing.T) {
	st := urlModelStore(t, "api_download_url_agent_gate")
	directory, err := st.UpsertModel(&store.Model{Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://mirror.example.com/Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B"})
	if err != nil {
		t.Fatal(err)
	}
	hub, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Revision: "main", Name: "Qwen3-0.6B"})
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := st.CreateNodePending("old-agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeAgentInfo(old.UUID, "0.9.0", "127.0.0.1", "agent_dial"); err != nil {
		t.Fatal(err)
	}
	old, err = st.NodeByUUID(old.UUID)
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := st.CreateNodePending("current-agent")
	if err != nil {
		t.Fatal(err)
	}

	if err := deployment.AdmitModelDownloadNode(directory, old); err == nil {
		t.Fatal("an old agent was allowed to download from a directory")
	} else if !strings.Contains(err.Error(), "too old") {
		t.Fatalf("refusal = %v", err)
	}
	// Registration is never gated, and neither is a source an old agent has
	// always understood.
	if err := deployment.AdmitModelDownloadNode(hub, old); err != nil {
		t.Fatalf("a repository download onto an old agent was refused: %v", err)
	}
	// A development build of this tree reports no release version and must
	// stay usable.
	if err := deployment.AdmitModelDownloadNode(directory, current); err != nil {
		t.Fatalf("a development agent was refused: %v", err)
	}

	h := apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st}}
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/models/"+fmt.Sprint(directory.ID)+"/download",
		strings.NewReader(fmt.Sprintf(`{"node_uuid":%q}`, old.UUID)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fmt.Sprint(directory.ID))
	request = request.WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	h.downloadModel(rec, request)
	var wire struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &wire)
	if rec.Code != http.StatusConflict || wire.Code != ErrorModelSourceRequiresNewerAgent {
		t.Fatalf("download status/body = %d %s", rec.Code, rec.Body.String())
	}
	if files, _ := st.ModelFilesForModel(directory.ID); len(files) != 0 {
		t.Fatalf("a refused download created %d model files", len(files))
	}
}
