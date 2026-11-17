package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const evaluateCallableHead = `models:
- model_id: Acme/Adapter
  specs:
  - name: serve
    engine: callable
    image: acme/adapter:1
    weights: {quantization: bf16, sources: [{huggingface: acme/adapter}]}
    gpus: {vendor: nvidia}
    args: ["--port", "${API_PORT}"]
`

const evaluateCallableNoAPI = evaluateCallableHead

const evaluateCallableWithBoth = evaluateCallableHead + `    api:
      health_endpoint: /healthz
      operations: [{type: openai.chat}]
      metrics:
        endpoint: /metrics
        inherit: vllm
`

func evaluateRig(t *testing.T) (*apiHandlers, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "evaluate.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &apiHandlers{d: Deps{Store: st}}, st
}

func evaluateRequest(t *testing.T, h *apiHandlers, target string, body []byte) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.evaluateInferenceTemplates(rec, httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body)))
	return rec.Code, rec.Body.String()
}

func TestEvaluateInferenceTemplatesWarnsAboutCallableWithoutMetricsAndWritesNothing(t *testing.T) {
	h, st := evaluateRig(t)
	status, body := evaluateRequest(t, h, "/api/v1/inference-templates/evaluate", []byte(evaluateCallableNoAPI))
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	want := `{"valid":true,"errors":[],"warnings":[` +
		`{"code":"CATALOG_CALLABLE_METRICS_UNDECLARED","params":{"model_id":"acme/adapter","spec":"serve"},"error":"Callable spec serve of acme/adapter declares no api.metrics, so the monitoring center shows no engine metrics for it unless its /metrics happens to use vLLM or SGLang metric names."},` +
		`{"code":"CATALOG_HEALTH_ENDPOINT_UNDECLARED","params":{"model_id":"acme/adapter","spec":"serve"},"error":"Callable spec serve of acme/adapter declares no api.health_endpoint, so its readiness is probed on the default /health."}]}` + "\n"
	if body != want {
		t.Fatalf("wire JSON\n got: %s\nwant: %s", body, want)
	}
	models, err := st.ListInferenceModels()
	if err != nil || len(models) != 0 {
		t.Fatalf("evaluate wrote models=%+v err=%v", models, err)
	}
	// The same document then imports, and reports the same warnings.
	rec := httptest.NewRecorder()
	h.importInferenceTemplates(rec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(evaluateCallableNoAPI)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import status/body=%d %s", rec.Code, rec.Body.String())
	}
	var imported struct {
		Warnings []json.RawMessage `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &imported); err != nil || len(imported.Warnings) != 2 {
		t.Fatalf("import body=%s err=%v", rec.Body.String(), err)
	}
}

func TestImportInferenceTemplatesWireJSONWithAndWithoutWarnings(t *testing.T) {
	h, _ := evaluateRig(t)
	rec := httptest.NewRecorder()
	h.importInferenceTemplates(rec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(evaluateCallableNoAPI)))
	want := `{"models":1,"package_digest":"","warnings":[` +
		`{"code":"CATALOG_CALLABLE_METRICS_UNDECLARED","params":{"model_id":"acme/adapter","spec":"serve"},"error":"Callable spec serve of acme/adapter declares no api.metrics, so the monitoring center shows no engine metrics for it unless its /metrics happens to use vLLM or SGLang metric names."},` +
		`{"code":"CATALOG_HEALTH_ENDPOINT_UNDECLARED","params":{"model_id":"acme/adapter","spec":"serve"},"error":"Callable spec serve of acme/adapter declares no api.health_endpoint, so its readiness is probed on the default /health."}]}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("status=%d\n got: %s\nwant: %s", rec.Code, rec.Body.String(), want)
	}
	// Without warnings the response is byte-identical to the one before the
	// field existed.
	clean := strings.Replace(evaluateCallableWithBoth, "Acme/Adapter", "Acme/Clean", 1)
	rec = httptest.NewRecorder()
	h.importInferenceTemplates(rec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(clean)))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"models":1,"package_digest":""}`+"\n" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestEvaluateInferenceTemplatesDeclaredMetricsAndHealthRaiseNoWarning(t *testing.T) {
	h, _ := evaluateRig(t)
	status, body := evaluateRequest(t, h, "/api/v1/inference-templates/evaluate", []byte(evaluateCallableWithBoth))
	if status != http.StatusOK || body != `{"valid":true,"errors":[],"warnings":[]}`+"\n" {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

func TestEvaluateInferenceTemplatesReportsParseAndValidationErrors(t *testing.T) {
	h, st := evaluateRig(t)
	for _, test := range []struct {
		name, body, code, contains string
	}{
		{name: "yaml that does not parse", body: "models: [", code: "CATALOG_INVALID", contains: "parse inference catalog YAML"},
		{name: "unknown key", body: "models:\n- model_id: a/b\n  bogus: 1\n", code: "CATALOG_INVALID", contains: "bogus"},
		{name: "no models", body: "models: []\n", code: "CATALOG_INVALID", contains: "at least one model"},
		{name: "assets need a package", body: strings.Replace(evaluateCallableHead, "  specs:", "  readme_en: README.en.md\n  specs:", 1), code: "CATALOG_PACKAGE_REQUIRED_FOR_FILES", contains: "require a .gpuworks-catalog.tar.gz"},
		{name: "invalid metrics keep the import's code", body: evaluateCallableHead + "    api:\n      operations: [{type: openai.chat}]\n      metrics: {endpoint: metrics}\n", code: "CATALOG_METRICS_ENDPOINT_INVALID", contains: "endpoint"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, body := evaluateRequest(t, h, "/api/v1/inference-templates/evaluate", []byte(test.body))
			var got struct {
				Valid    bool             `json:"valid"`
				Errors   []map[string]any `json:"errors"`
				Warnings []map[string]any `json:"warnings"`
			}
			if status != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
				t.Fatalf("status=%d body=%s", status, body)
			}
			if got.Valid || len(got.Errors) != 1 || got.Errors[0]["code"] != test.code || !strings.Contains(got.Errors[0]["error"].(string), test.contains) {
				t.Fatalf("body=%s, want one %s error containing %q", body, test.code, test.contains)
			}
			if got.Warnings == nil || len(got.Warnings) != 0 {
				t.Fatalf("warnings must be an empty list: %s", body)
			}
			// The import finds the same code (400 body: {"error","code"}).
			rec := httptest.NewRecorder()
			h.importInferenceTemplates(rec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(test.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("import status=%d body=%s", rec.Code, rec.Body.String())
			}
			var imp map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &imp)
			if code, _ := imp["code"].(string); code != test.code {
				t.Fatalf("import code=%q, evaluate code=%q", code, test.code)
			}
		})
	}
	if models, err := st.ListInferenceModels(); err != nil || len(models) != 0 {
		t.Fatalf("evaluate wrote models=%+v err=%v", models, err)
	}
}

func TestEvaluateInferenceTemplatesOverwriteAllowsFilesLikeImport(t *testing.T) {
	h, _ := evaluateRig(t)
	body := strings.Replace(evaluateCallableWithBoth, "  specs:", "  readme_en: README.en.md\n  specs:", 1)
	status, out := evaluateRequest(t, h, "/api/v1/inference-templates/evaluate?overwrite=true", []byte(body))
	if status != http.StatusOK || !strings.HasPrefix(out, `{"valid":true,`) {
		t.Fatalf("status=%d body=%s", status, out)
	}
}

func TestEvaluateInferenceTemplatePackageWritesNeitherRowsNorPackages(t *testing.T) {
	h, st := evaluateRig(t)
	root := filepath.Join(t.TempDir(), "packages")
	pkgs, err := packages.New(root)
	if err != nil {
		t.Fatal(err)
	}
	h.d.CatalogPackages = pkgs
	model := inferencecatalog.InferenceModel{
		ModelID: "acme/pkg-adapter", Source: "user",
		Specs: []inferencecatalog.InferenceSpec{{
			Name: "serve", Engine: "callable", Image: "acme/adapter:1", Args: []string{"--port", "${API_PORT}"},
			Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "acme/pkg-adapter"}}},
			GPUs:    inferencecatalog.GPUSelector{Vendor: "nvidia"},
			API:     &inferencecatalog.APIContract{Operations: []inferencecatalog.APIOperation{{Type: "openai.chat"}}},
		}},
	}
	modelYAML, err := inferencecatalog.MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, fstest.MapFS{
		"manifest.yaml":                      {Data: []byte("schema_version: 1\npublished_at: 2026-08-31T00:00:00Z\nmodels: 1\nspecs: 1\n")},
		"models/acme/pkg-adapter/model.yaml": {Data: modelYAML},
	}); err != nil {
		t.Fatal(err)
	}
	status, body := evaluateRequest(t, h, "/api/v1/inference-templates/evaluate", archive.Bytes())
	if status != http.StatusOK || !strings.HasPrefix(body, `{"valid":true,"errors":[],"warnings":[{"code":"CATALOG_CALLABLE_METRICS_UNDECLARED"`) || !strings.Contains(body, `"model_id":"acme/pkg-adapter"`) {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if digests, err := st.ReferencedCatalogPackageDigests(); err != nil || len(digests) != 0 {
		t.Fatalf("referenced digests=%v err=%v", digests, err)
	}
	if models, err := st.ListInferenceModels(); err != nil || len(models) != 0 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	entries, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("package storage was written: %v err=%v", entries, err)
	}
	if status, body := evaluateRequest(t, h, "/api/v1/inference-templates/evaluate", []byte("\x1f\x8bnot a gzip stream")); status != http.StatusOK || !strings.HasPrefix(body, `{"valid":false,`) {
		t.Fatalf("corrupt package status=%d body=%s", status, body)
	}
}

func TestEvaluateInferenceTemplatesRouteAuthAndMethods(t *testing.T) {
	signer, err := auth.NewTokenSigner("evaluate-route-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := routerAuthStore(t)
	router := Router(Deps{AuthSigner: signer, Store: st})
	const path = "/api/v1/inference-templates/evaluate"
	call := func(method, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(http.MethodPost, "", evaluateCallableNoAPI); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", rec.Code)
	}
	userToken, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "user", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := call(http.MethodPost, userToken, evaluateCallableNoAPI); rec.Code != http.StatusForbidden {
		t.Fatalf("user status=%d body=%s", rec.Code, rec.Body.String())
	}
	adminToken, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	rec := call(http.MethodPost, adminToken, evaluateCallableNoAPI)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"valid":true`) {
		t.Fatalf("admin status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if rec := call(method, adminToken, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status=%d, want 405", method, rec.Code)
		}
	}
}
