package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func flatAPIFixture(modelID, specName string) store.InferenceModel {
	return store.InferenceModel{ModelID: modelID, DisplayTitle: modelID, ReleaseDate: "2025-04-03", Source: "upstream", Unsupported: `[]`, Specs: []store.InferenceSpecRow{{
		Name: specName, DisplayNameZHCN: "Turing 及以上 · BF16", DisplayNameEN: "Turing+ · BF16", Scenario: "balanced", Engine: "vllm", Image: "vllm/vllm-openai:latest", Quantization: "bf16",
		WeightSources: `[{"provider":"huggingface","repo_id":"` + modelID + `"}]`, GPUs: `{"vendor":"nvidia","cc":">=7.5","display_target":"nvidia"}`, Args: `[]`, Env: `{}`, PreStartArgv: `[]`,
	}}}
}

func TestInferenceTemplateIndexIncludesProfileGPUContracts(t *testing.T) {
	st, err := store.Open("file:template_index_profiles?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model := flatAPIFixture("acme/index", "amd395")
	model.Specs[0].GPUs = `{"vendor":"amd","arch_id":"gfx1151","display_target":"amd395"}`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).listInferenceTemplateIndex(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/index", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"profiles"`) || !strings.Contains(rec.Body.String(), `"source":"upstream","name":"amd395"`) || !strings.Contains(rec.Body.String(), `"vendor":"amd"`) || !strings.Contains(rec.Body.String(), `"arch_id":"gfx1151"`) || !strings.Contains(rec.Body.String(), `"weight_repo_ids":["acme/index"]`) {
		t.Fatalf("status/body=%d %s", rec.Code, rec.Body.String())
	}
}

func TestInferenceTemplateIndexPreservesSameNameSources(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	upstream := flatAPIFixture("acme/same", "general")
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{upstream}); err != nil {
		t.Fatal(err)
	}
	custom := flatAPIFixture("acme/same", "general")
	custom.Source = "user"
	custom.Specs[0].Source = "user"
	custom.Specs[0].Image = "private.example/optimized:mine"
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{custom}, false); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).listInferenceTemplateIndex(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/index", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var wire struct {
		Items []struct {
			Profiles []struct {
				Source string `json:"source"`
				Name   string `json:"name"`
				Image  string `json:"image"`
			} `json:"profiles"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Items) != 1 || len(wire.Items[0].Profiles) != 2 {
		t.Fatalf("index merged distinct schemes: %s", rec.Body)
	}
	got := map[string]string{}
	for _, profile := range wire.Items[0].Profiles {
		got[profile.Source+":"+profile.Name] = profile.Image
	}
	want := map[string]string{"upstream:general": "vllm/vllm-openai:latest", "user:general": "private.example/optimized:mine"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identity=%v want=%v", got, want)
	}
}

func TestInferenceTemplateListPaginatesFiltersAndSupportsETag(t *testing.T) {
	st, err := store.Open("file:flat_api_list?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{flatAPIFixture("acme/a", "a"), flatAPIFixture("acme/b", "b")}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates?page=1&page_size=1&gpu=GeForce%20RTX%202080%20Ti&vendor=nvidia&cc=7.5", nil)
	rec := httptest.NewRecorder()
	h.listInferenceTemplates(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == "" {
		t.Fatalf("status=%d etag=%q body=%s", rec.Code, rec.Header().Get("ETag"), rec.Body.String())
	}
	var page dto.InferenceTemplatePage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Total != 2 || page.Items[0].ReleaseDate != "2025-04-03" || page.Items[0].Specs[0].DisplayNameZHCN != "Turing 及以上 · BF16" || page.Items[0].Specs[0].DisplayNameEN != "Turing+ · BF16" {
		t.Fatalf("page=%+v", page)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates?page=1&page_size=1&gpu=GeForce%20RTX%202080%20Ti&vendor=nvidia&cc=7.5", nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	notModified := httptest.NewRecorder()
	h.listInferenceTemplates(notModified, req)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("status=%d", notModified.Code)
	}
}

func TestInferenceTemplateListFiltersByArchID(t *testing.T) {
	st, err := store.Open("file:flat_api_arch_filter?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	older := flatAPIFixture("acme/gfx1150", "gfx1150")
	older.Specs[0].GPUs = `{"vendor":"amd","name":"Radeon 8060S","arch_id":"gfx1150"}`
	newer := flatAPIFixture("acme/gfx1151", "gfx1151")
	newer.Specs[0].GPUs = `{"vendor":"amd","name":"Radeon 8060S","arch_id":"gfx1151"}`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{older, newer}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates?gpu=Radeon%208060S&vendor=amd&arch_id=gfx1151", nil)
	(&apiHandlers{d: Deps{Store: st}}).listInferenceTemplates(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var page dto.InferenceTemplatePage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ModelID != "acme/gfx1151" {
		t.Fatalf("arch_id filter returned %+v", page)
	}
}

func TestInferenceTemplateHardwareFilterValidatesComputeCapability(t *testing.T) {
	st, err := store.Open("file:flat_api_filter?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &apiHandlers{d: Deps{Store: st}}
	for _, target := range []string{"/api/v1/inference-templates?cc=sm89&gpu=H100", "/api/v1/inference-templates?cc=0&gpu=H100", "/api/v1/inference-templates?cc=8.9", "/api/v1/inference-templates?arch_id=gfx1151"} {
		rec := httptest.NewRecorder()
		h.listInferenceTemplates(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status/body=%d %s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestListTemplateChipsUsesStaticPlanningCatalog(t *testing.T) {
	rec := httptest.NewRecorder()
	(&apiHandlers{}).listTemplateChips(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/chips", nil))
	var rows []dto.TemplateChip
	if err := unmarshalList(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	foundB300, found910B, foundAMD := false, false, false
	for _, row := range rows {
		if row.Vendor == "amd" && row.ArchID == "gfx1151" && row.CC == 0 {
			foundAMD = true
		}
		if row.Name == "NVIDIA B300" && row.Architecture == "Blackwell" && row.Category == "data-center" && row.CC == 10.3 {
			foundB300 = true
		}
		if row.Name == "Ascend 910B" && row.Vendor == "ascend" && row.Category == "domestic" {
			found910B = true
		}
	}
	if !foundB300 || !found910B || !foundAMD {
		t.Fatalf("static catalog missing locked entries")
	}
}

func TestInferenceTemplateImportUsesFlatYAMLAndRejectsLegacyShape(t *testing.T) {
	st, err := store.Open("file:flat_api_import?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &apiHandlers{d: Deps{Store: st}}
	legacy := httptest.NewRecorder()
	h.importInferenceTemplates(legacy, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(`{"templates":[]}`)))
	if legacy.Code != http.StatusBadRequest {
		t.Fatalf("legacy status/body=%d %s", legacy.Code, legacy.Body.String())
	}
	yaml := `models:
- model_id: Acme/Flat
  display_title: Flat
  specs:
  - name: fp8-balanced
    display_name_zh_cn: Ada 及以上 · FP8
    display_name_en: Ada+ · FP8
    scenario: balanced
    engine: vllm
    image: vllm/vllm-openai:latest
    weights:
      quantization: fp8
      sources:
      - modelscope: Acme/Flat-FP8
      - huggingface: Acme/Flat-FP8
    gpus: {vendor: nvidia, cc: ">=8.9"}
    args: ["--kv-cache-dtype", "fp8"]
    env: {CATALOG: "1"}
    pre_start_argv: [prepare]
`
	rec := httptest.NewRecorder()
	h.importInferenceTemplates(rec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(yaml)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body=%d %s", rec.Code, rec.Body.String())
	}
	model, err := st.InferenceModelByModelID("acme/flat")
	if err != nil || model.Source != "user" || len(model.Specs) != 1 || model.Specs[0].Name != "fp8-balanced" || model.Specs[0].DisplayNameZHCN != "Ada 及以上 · FP8" || model.Specs[0].DisplayNameEN != "Ada+ · FP8" || !strings.Contains(model.Specs[0].WeightSources, "modelscope") {
		t.Fatalf("model=%+v err=%v", model, err)
	}
	conflict := httptest.NewRecorder()
	h.importInferenceTemplates(conflict, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(yaml)))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("duplicate import status/body=%d %s", conflict.Code, conflict.Body.String())
	}
	overwrite := httptest.NewRecorder()
	h.importInferenceTemplates(overwrite, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import?overwrite=true", strings.NewReader(yaml)))
	if overwrite.Code != http.StatusOK {
		t.Fatalf("overwrite status/body=%d %s", overwrite.Code, overwrite.Body.String())
	}
}

func TestInferenceTemplateImportAddsSameNamedUserSpecBesideBuiltIn(t *testing.T) {
	st, err := store.Open("file:flat_api_source_import?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{flatAPIFixture("acme/builtin", "general")}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	yaml := `models:
- model_id: acme/builtin
  specs:
  - name: general
    engine: vllm
    image: custom/image:latest
    weights: {quantization: bf16, sources: [{huggingface: acme/builtin}]}
    gpus: {vendor: nvidia}
`
	recorder := httptest.NewRecorder()
	h.importInferenceTemplates(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(yaml)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body=%d %s", recorder.Code, recorder.Body.String())
	}
	model, err := st.InferenceModelByModelID("acme/builtin")
	if err != nil || model.Source != "upstream" || len(model.Specs) != 2 {
		t.Fatalf("built-in model=%+v err=%v", model, err)
	}
	sources := map[string]bool{}
	for _, spec := range model.Specs {
		sources[spec.Source+"/"+spec.Name] = true
	}
	if !sources["upstream/general"] || !sources["user/general"] {
		t.Fatalf("spec identities=%v", sources)
	}
	view := inferenceTemplateView(*model)
	if len(view.Specs) != 2 || view.Specs[0].Source == "" || view.Specs[1].Source == "" {
		t.Fatalf("wire spec sources=%+v", view.Specs)
	}
	for _, source := range []string{"upstream", "user"} {
		filtered, err := filterInferenceModels([]store.InferenceModel{*model}, httptest.NewRequest(http.MethodGet, "/?source="+source, nil))
		if err != nil || len(filtered) != 1 || len(filtered[0].Specs) != 1 || filtered[0].Specs[0].Source != source {
			t.Fatalf("source %s filter=%+v err=%v", source, filtered, err)
		}
	}
}

func TestInferenceTemplateModelIDSplatCloneAndDetectedChips(t *testing.T) {
	st, err := store.Open("file:flat_api_custom?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{flatAPIFixture("acme/org/model", "balanced")}); err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("template-chip-node", store.HashToken("token"), "template-chip-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "gpu-custom", Vendor: "NVIDIA", Name: "NVIDIA H100", ComputeCapability: "9.0"}).Error; err != nil {
		t.Fatal(err)
	}
	for index, archID := range []string{"gfx1150", "gfx1151"} {
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: index + 1, UUID: "gpu-amd-" + archID, Vendor: "amd", Name: "AMD GPU 0x1586", ArchID: archID}).Error; err != nil {
			t.Fatal(err)
		}
	}
	h := &apiHandlers{d: Deps{Store: st}}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("*", "acme/org/model")
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/model/acme/org/model", nil).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
	getRec := httptest.NewRecorder()
	h.getInferenceTemplate(getRec, getReq)
	if getRec.Code != http.StatusOK || !strings.Contains(getRec.Body.String(), `"model_id":"acme/org/model"`) {
		t.Fatalf("splat get status/body=%d %s", getRec.Code, getRec.Body.String())
	}
	cloneRec := httptest.NewRecorder()
	h.cloneInferenceTemplate(cloneRec, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/clone", strings.NewReader(`{"source_model_id":"acme/org/model","model_id":"me/editable"}`)))
	if cloneRec.Code != http.StatusCreated {
		t.Fatalf("clone status/body=%d %s", cloneRec.Code, cloneRec.Body.String())
	}
	clone, err := st.InferenceModelByModelID("me/editable")
	if err != nil || clone.Source != "user" || len(clone.Specs) != 1 {
		t.Fatalf("clone=%+v err=%v", clone, err)
	}
	chipsRec := httptest.NewRecorder()
	h.listDetectedTemplateChips(chipsRec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/my-chips", nil))
	if chipsRec.Code != http.StatusOK || !strings.Contains(chipsRec.Body.String(), `"name":"NVIDIA H100"`) || !strings.Contains(chipsRec.Body.String(), `"cc":"9.0"`) ||
		!strings.Contains(chipsRec.Body.String(), `"arch_id":"gfx1150"`) || !strings.Contains(chipsRec.Body.String(), `"arch_id":"gfx1151"`) {
		t.Fatalf("my chips must preserve distinct arch_id values for same-name GPUs; status/body=%d %s", chipsRec.Code, chipsRec.Body.String())
	}
}

func TestInferenceTemplateImportBackfillsWeightBytesUsingDeclaredSourceOrder(t *testing.T) {
	st, err := store.Open("file:flat_api_weight_backfill?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var calls []string
	h := &apiHandlers{d: Deps{Store: st, FetchModelMeta: func(_ context.Context, source, _ string, _, _ string, _ []string) (*modelmeta.Meta, error) {
		calls = append(calls, source)
		if source == "modelscope" {
			return nil, errors.New("modelscope unavailable")
		}
		return &modelmeta.Meta{WeightBytes: 12345}, nil
	}}}
	yaml := `models:
- model_id: acme/backfill
  specs:
  - name: default
    engine: vllm
    image: vllm:latest
    weights:
      quantization: bf16
      sources:
      - huggingface: acme/backfill
      - modelscope: acme/backfill
    gpus: {vendor: nvidia}
`
	recorder := httptest.NewRecorder()
	h.importInferenceTemplates(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(yaml)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body=%d %s", recorder.Code, recorder.Body.String())
	}
	model, err := st.InferenceModelByModelID("acme/backfill")
	if err != nil || len(model.Specs) != 1 || model.Specs[0].WeightBytes != 12345 {
		t.Fatalf("model=%+v err=%v", model, err)
	}
	if !reflect.DeepEqual(calls, []string{"huggingface"}) {
		t.Fatalf("metadata source order = %#v", calls)
	}
}

func TestInferenceTemplateDeleteUserSpecScenarios(t *testing.T) {
	st, err := store.Open("file:flat_api_delete_spec?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{
		flatAPIFixture("acme/builtin", "general"),
	}); err != nil {
		t.Fatal(err)
	}
	userSpec := func(name string) store.InferenceSpecRow {
		return store.InferenceSpecRow{
			Name: name, Engine: "sglang", Image: "custom/image:latest", Quantization: "fp8",
			WeightSources: `[{"provider":"huggingface","repo_id":"acme/useronly"}]`, GPUs: `{"vendor":"nvidia"}`,
		}
	}
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/useronly", Source: "user", Specs: []store.InferenceSpecRow{userSpec("solo"), userSpec("extra")},
	}}, true); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	call := func(modelID, spec string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/inference-templates/spec?model_id="+url.QueryEscape(modelID)+"&spec="+url.QueryEscape(spec), nil)
		rec := httptest.NewRecorder()
		h.deleteInferenceTemplateSpec(rec, req)
		return rec
	}
	// validation: missing params
	if rec := call("", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing params status=%d body=%s", rec.Code, rec.Body.String())
	}
	// unknown model and unknown spec both 404
	if rec := call("acme/missing", "general"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := call("acme/builtin", "nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown spec status=%d body=%s", rec.Code, rec.Body.String())
	}
	// upstream specs are not deletable through the user-spec path
	if rec := call("acme/builtin", "general"); rec.Code != http.StatusNotFound {
		t.Fatalf("upstream spec status=%d body=%s", rec.Code, rec.Body.String())
	}
	// deleting one user spec beside another leaves the model with the survivor
	if rec := call("acme/useronly", "extra"); rec.Code != http.StatusOK {
		t.Fatalf("delete user spec status=%d body=%s", rec.Code, rec.Body.String())
	}
	model, err := st.InferenceModelByModelID("acme/useronly")
	if err != nil || len(model.Specs) != 1 || model.Specs[0].Name != "solo" {
		t.Fatalf("after delete model=%+v err=%v", model, err)
	}
	// deleting the last user spec drops the whole user model
	if rec := call("acme/useronly", "solo"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model_deleted":true`) {
		t.Fatalf("last spec delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.InferenceModelByModelID("acme/useronly"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("model row survived last spec delete: err=%v", err)
	}
}

func TestInferenceTemplateDeleteUserSpecRefusesWhileReferenced(t *testing.T) {
	st, err := store.Open("file:flat_api_delete_ref?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{flatAPIFixture("acme/base", "general")}); err != nil {
		t.Fatal(err)
	}
	userSpecs := func(name string) []store.InferenceSpecRow {
		return []store.InferenceSpecRow{{
			Name: name, Engine: "sglang", Image: "custom/image:latest", Quantization: "bf16",
			WeightSources: `[{"provider":"huggingface","repo_id":"acme/base"}]`, GPUs: `{"vendor":"nvidia"}`,
		}}
	}
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{{ModelID: "acme/base", Source: "user", Specs: userSpecs("tuned")}}, true); err != nil {
		t.Fatal(err)
	}
	base, err := st.InferenceModelByModelID("acme/base")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/combo", Source: "upstream", Specs: []store.InferenceSpecRow{{
			Name: "combo", Engine: "vllm", Image: "vllm/vllm-openai:latest", Quantization: "bf16",
			WeightSources: `[{"provider":"huggingface","repo_id":"acme/combo"}]`, GPUs: `{"vendor":"nvidia"}`,
			Components: `[{"name":"rerank","role":"component","model_id":"acme/useronly","mount_path":"/models/rerank"}]`,
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{{ModelID: "acme/useronly", Source: "user", Specs: userSpecs("only")}}, true); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	// a deployment pinning the spec blocks deletion with 409
	pinner, err := st.CreateDeployment(&store.Deployment{Name: "pinned", TemplateID: base.ID, TemplateSpecSource: "user", TemplateSpecName: "tuned", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/inference-templates/spec?model_id=acme%2Fbase&spec=tuned", nil)
	rec := httptest.NewRecorder()
	h.deleteInferenceTemplateSpec(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "pinned") {
		t.Fatalf("in-use status=%d body=%s", rec.Code, rec.Body.String())
	}
	// a deleting deployment no longer pins
	if err := st.SetDeploymentState(pinner.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.deleteInferenceTemplateSpec(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("after deleting deployment status=%d body=%s", rec.Code, rec.Body.String())
	}
	// dropping the last spec of a model referenced as a component is refused
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/inference-templates/spec?model_id=acme%2Fuseronly&spec=only", nil)
	rec = httptest.NewRecorder()
	h.deleteInferenceTemplateSpec(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "acme/combo") {
		t.Fatalf("component reference status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.InferenceModelByModelID("acme/useronly"); err != nil {
		t.Fatalf("referenced model must survive: %v", err)
	}
}

func TestInferenceTemplateSchemaServesEditorDictionary(t *testing.T) {
	st, err := store.Open("file:flat_api_schema?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).inferenceTemplateSchema(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/schema", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if _, ok := schema.Properties["models"]; !ok {
		t.Fatalf("schema lacks the models root: %s", rec.Body.String()[:200])
	}
	if !strings.Contains(rec.Body.String(), "openai.chat") {
		t.Fatal("schema does not embed the code-owned operation vocabulary")
	}
	if !strings.Contains(rec.Body.String(), `"https://json-schema.org/draft/2020-12/schema"`) {
		t.Fatal("schema does not declare its draft")
	}
}

func TestCloneInferenceTemplateRejectsInvalidModelID(t *testing.T) {
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{flatAPIFixture("acme/source", "default")}); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("clone-model-id", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer})
	request := func(body, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/clone", strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := request(`{"source_model_id":"acme/source","model_id":"me/copy"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated=%d %s", rec.Code, rec.Body.String())
	}
	for _, modelID := range []string{"bad model", "a//b", "../escape", "a/./b", "a/../b", `a\b`, "/absolute", "a/"} {
		t.Run(modelID, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"source_model_id": "acme/source", "model_id": modelID})
			rec := request(string(body), token)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_id") {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	rows, err := st.ListInferenceModels()
	if err != nil || len(rows) != 1 {
		t.Fatalf("invalid clone changed catalog=%+v err=%v", rows, err)
	}
	for _, body := range []string{
		`{"source_model_id":"missing/model","model_id":"me/nowhere"}`,
		`{"model_id":"me/nowhere"}`,
	} {
		rec := request(body, token)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("missing source status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if rows, err = st.ListInferenceModels(); err != nil || len(rows) != 1 {
		t.Fatalf("missing source created a template=%+v err=%v", rows, err)
	}
	rec := request(`{"source_model_id":"acme/source","model_id":" ME/Valid "}`, token)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"model_id":"me/valid"`) {
		t.Fatalf("valid clone=%d %s", rec.Code, rec.Body.String())
	}
}
