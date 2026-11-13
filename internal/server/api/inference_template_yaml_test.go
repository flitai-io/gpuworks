package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/localization"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

const (
	fullTemplateID  = "acme/full"
	fullDraftID     = "acme/full-draft"
	fullAssetPath   = "assets/serve.py"
	fullReadmeZHCN  = "README.zh-CN.md"
	fullReadmeEN    = "README.en.md"
	fullDraftMount  = "/models/draft"
	fullPlacementFn = "def resolve(gpu):\n    if gpu.min_memory_gib < 16:\n        return reject(\"needs 16 GiB\")\n    return {}\n"
)

func boolPtr(value bool) *bool { return &value }

func fullModelConfig() *modelconfig.Config {
	tower := &modelconfig.TowerConfig{
		ModelType: "siglip", HiddenSize: 1152, DModel: 1024, IntermediateSize: 4304, NumHiddenLayers: 27, EncoderLayers: 24,
		NumAttentionHeads: 16, EncoderAttentionHeads: 16, ImageSize: 896, PatchSize: 14, NumChannels: 3,
		MaxPositionEmbeddings: 1024, MaxSourcePositions: 1500, NumMelBins: 128, ProjectionDim: 512, OutputDim: 2048,
		OutHiddenSize: 2048, EmbedDim: 768, Depth: 32, SpatialMergeSize: 2,
	}
	inner := &modelconfig.Config{ModelType: "qwen3_text", HiddenSize: 2048, NumHiddenLayers: 24, Architectures: []string{"Qwen3Text"}}
	return &modelconfig.Config{
		Architectures: []string{"Qwen3ForCausalLM"}, ModelType: "qwen3", NumHiddenLayers: 64, HiddenSize: 5120,
		NumAttentionHeads: 64, NumKeyValueHeads: 8, HeadDim: 128, MaxPositionEmbeddings: 40960, VocabSize: 151936,
		TorchDtype: "bfloat16", RopeScaling: map[string]any{"rope_type": "yarn", "factor": 4.5}, SlidingWindow: 1024,
		UseSlidingWindow: boolPtr(true), KVLoraRank: 512, QKRopeHeadDim: 64, QKNopeHeadDim: 128, VHeadDim: 128,
		LayerTypes: []string{"full_attention", "sliding_attention"}, KVCache: boolPtr(true),
		TextConfig: inner, ThinkerConfig: inner, VisionConfig: tower, AudioConfig: tower,
	}
}

// fullTemplateCatalog is a template that states every fact the flat catalog
// can: each field of every type reachable from FlatCatalog is non-zero in at
// least one place. TestFullTemplateFixtureStatesEveryCatalogField keeps it so.
func fullTemplateCatalog(asset []byte) inferencecatalog.FlatCatalog {
	digest := sha256.Sum256(asset)
	weights := func(repo string) inferencecatalog.Weights {
		return inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{
			{Provider: "huggingface", RepoID: repo, Revision: "0123456789abcdef", Subfolder: "bf16"},
			{Provider: "modelscope", RepoID: repo},
		}}
	}
	gpus := inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0", Name: "H100", ArchID: "sm_90"}
	primary := inferencecatalog.Component{Name: "main", Role: "primary", Weights: weights(fullTemplateID), WeightBytes: 4_000_000_000, MountPath: "/models/main"}
	draft := inferencecatalog.Component{Name: "draft", Role: "speculative_draft", ModelID: fullDraftID, Weights: weights(fullDraftID), WeightBytes: 1_000_000_000, MountPath: fullDraftMount}
	prometheus := &inferenceapi.Metrics{Endpoint: "/stats", Inherit: "sglang", Map: inferenceapi.MetricMap{
		"adapter_ms_total":   {To: "prompt_tokens_total", Scale: 0.001},
		"adapter_requests_x": {To: "requests_running"},
	}}
	chat := inferenceapi.Operation{
		Type: inferenceapi.OperationChat, Endpoint: "/v1/chat/completions", Mode: "stream", RequestEncoding: "json",
		RequestDefaults: map[string]any{"stream": true, "temperature": 0.25, "nested": map[string]any{"top_k": 20}},
		Inputs: []inferenceapi.InputField{
			{Name: "reasoning_effort", Kind: "select", Options: []string{"low", "high"}, Default: "low", Required: true, Label: localization.Text{"en": "Reasoning", "zh-CN": "推理"}},
			{Name: "reference", Kind: "image", Encoding: "data_url"},
		},
	}
	// A fixed-format completion model: its working prompt ends in a newline,
	// which the round trip must keep.
	completion := inferenceapi.Operation{
		Type: inferenceapi.OperationCompletions, Endpoint: "/v1/completions", Mode: "stream", RequestEncoding: "json",
		RequestDefaults: map[string]any{"stop": []any{"<|plamo:op|>"}},
		ExampleInput:    map[string]string{"prompt": "<|plamo:op|>input lang=English\nHello\n<|plamo:op|>output lang=Japanese\n"},
	}
	accelerated := inferencecatalog.InferenceSpec{
		Name: "optimized-h100", DisplayNameZHCN: "H100 · BF16 · 投机解码", DisplayNameEN: "H100 · BF16 · Speculative", Scenario: "latency", Engine: "vllm",
		Image: "gpuworks/vllm:test", ImageSources: []string{"quay.io/gpuworks/vllm:test", "registry.example/gpuworks/vllm:test"},
		Weights: weights(fullTemplateID), WeightBytes: 140_000_000_000, RuntimeWeightBytes: 70_000_000_000,
		GPUs: gpus, Args: []string{"--max-model-len", "32768"}, Env: map[string]string{"VLLM_TEST": "1", "DRAFT_PATH": fullDraftMount},
		PreStartArgv:     []string{"/opt/gpuworks/serve.py", "--prepare"},
		Assets:           []inferencecatalog.Asset{{File: fullAssetPath, SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(asset)), UnpackTo: "/opt/gpuworks/serve.py"}},
		Entrypoint:       []string{"python3", "-m", "vllm.entrypoints.openai.api_server"},
		Placement:        &inferencecatalog.Placement{Language: "starlark", Resolver: fullPlacementFn},
		API:              &inferencecatalog.APIContract{HealthEndpoint: "/health", TokenizerRef: "ms://Acme/Tokenizer@0123456789abcdef0123456789abcdef01234567", Operations: []inferenceapi.Operation{chat, completion}, Metrics: prometheus},
		Components:       []inferencecatalog.Component{primary, draft},
		PDMode:           "vllm-nixl",
		ShmBytes:         429_496_729_600,
		MemlockUnlimited: true,
	}
	scripted := inferencecatalog.InferenceSpec{
		Name: "scripted", Engine: "vllm", Image: "gpuworks/vllm:test", Weights: weights(fullTemplateID),
		GPUs:           inferencecatalog.GPUSelector{Vendor: "ascend"},
		PreStartScript: "set -e\necho preparing\n  indented line\n",
		API: &inferencecatalog.APIContract{Operations: []inferenceapi.Operation{{Type: inferenceapi.OperationChat}, {Type: inferenceapi.OperationImageEdit, MaxReferenceImages: 4}},
			Metrics:      &inferenceapi.Metrics{Format: "json", Map: inferenceapi.MetricMap{"totals.output_tokens": {To: "generation_tokens_total"}}},
			RequestStats: inferenceapi.MetricMap{"timings.prompt_ms": {To: "prefill_seconds", Scale: 0.001}}},
	}
	full := inferencecatalog.InferenceModel{
		ModelID: fullTemplateID, Source: "user", DisplayTitle: "Full", ParameterCount: "70B", Architecture: "Qwen3ForCausalLM",
		InputModalities: []string{"text", "image"}, ModelConfig: fullModelConfig(),
		ReadmeZHCN: fullReadmeZHCN, ReadmeEN: fullReadmeEN,
		KVBytesPerToken: map[string]int64{"vllm": 49152, "sglang": 12284},
		ContextLength:   40960, ReleaseDate: "2026-09-29",
		Unsupported: []inferencecatalog.UnsupportedGPU{{GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: "<8.0"}, Quantization: "fp8", Reason: "no fp8 tensor cores"}},
		Specs:       []inferencecatalog.InferenceSpec{accelerated, scripted},
	}
	return inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{full, {ModelID: fullDraftID, Source: "user", DisplayTitle: "Full draft"}}}
}

// catalogFieldNames lists Type.Field for every field of every struct type
// reachable from t.
func catalogFieldNames(t reflect.Type, names map[string]bool, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for index := 0; index < t.NumField(); index++ {
		field := t.Field(index)
		if !field.IsExported() {
			continue
		}
		names[t.Name()+"."+field.Name] = true
		catalogFieldNames(field.Type, names, seen)
	}
}

// statedFieldNames lists Type.Field for every field that holds a non-zero
// value anywhere inside v.
func statedFieldNames(v reflect.Value, names map[string]bool) {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			statedFieldNames(v.Elem(), names)
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < v.Len(); index++ {
			statedFieldNames(v.Index(index), names)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			statedFieldNames(v.MapIndex(key), names)
		}
	case reflect.Struct:
		for index := 0; index < v.NumField(); index++ {
			if !v.Type().Field(index).IsExported() {
				continue
			}
			if !v.Field(index).IsZero() {
				names[v.Type().Name()+"."+v.Type().Field(index).Name] = true
			}
			statedFieldNames(v.Field(index), names)
		}
	}
}

// The round-trip test below is only as strong as its fixture. This keeps the
// fixture complete: a field added to any catalog type fails here until the
// fixture states it, so the round trip covers it too.
func TestFullTemplateFixtureStatesEveryCatalogField(t *testing.T) {
	all := map[string]bool{}
	catalogFieldNames(reflect.TypeOf(inferencecatalog.FlatCatalog{}), all, map[reflect.Type]bool{})
	stated := map[string]bool{}
	statedFieldNames(reflect.ValueOf(fullTemplateCatalog([]byte("print('ready')\n"))), stated)
	// Retired keys are read on import and dropped, so no round trip can
	// carry them (TestImportInferenceTemplatesAcceptsRetiredDisplayProfileWithWarning).
	retired := map[string]bool{"GPUSelector.RetiredDisplayTarget": true, "InferenceSpec.RetiredDisplayProfile": true}
	var missing []string
	for name := range all {
		if !stated[name] && !retired[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("fullTemplateCatalog leaves these catalog fields empty, so the editor round trip does not cover them: %v", missing)
	}
}

type yamlFixture struct {
	handler  *apiHandlers
	database *store.Store
	packages *packages.Store
	asset    []byte
}

// newFullTemplateFixture imports the full template as a catalog package, the
// only way a template with documentation and assets enters the server.
func newFullTemplateFixture(t *testing.T) yamlFixture {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	packageStore, err := packages.New(filepath.Join(t.TempDir(), "packages"))
	if err != nil {
		t.Fatal(err)
	}
	asset := []byte("print('ready')\n")
	files := map[string][]byte{
		"models/" + fullTemplateID + "/" + fullReadmeZHCN: []byte("# 完整模板\n"),
		"models/" + fullTemplateID + "/" + fullReadmeEN:   []byte("# Full template\n"),
		"models/" + fullTemplateID + "/" + fullAssetPath:  asset,
	}
	packageFS, err := inferencecatalog.BuildPackageFS(time.Now().UTC().Format(time.RFC3339), fullTemplateCatalog(asset), files)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := inferencecatalog.PackPackage(&archive, packageFS); err != nil {
		t.Fatal(err)
	}
	f := yamlFixture{handler: &apiHandlers{d: Deps{Store: database, CatalogPackages: packageStore}}, database: database, packages: packageStore, asset: asset}
	recorder := httptest.NewRecorder()
	f.handler.importInferenceTemplates(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", bytes.NewReader(archive.Bytes())))
	if recorder.Code != http.StatusOK {
		t.Fatalf("package import status/body=%d %s", recorder.Code, recorder.Body.String())
	}
	return f
}

func (f yamlFixture) yaml(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	f.handler.inferenceTemplateYAML(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/yaml?"+query, nil))
	return recorder
}

// withoutIdentity drops what differs between two stores of the same document:
// row ids, timestamps and the digest of the package the files came from.
func withoutIdentity(row store.InferenceModel) store.InferenceModel {
	row.ID, row.CreatedAt, row.UpdatedAt, row.PackageDigest = 0, time.Time{}, time.Time{}, ""
	specs := make([]store.InferenceSpecRow, len(row.Specs))
	for index, spec := range row.Specs {
		spec.ID, spec.ModelID, spec.CreatedAt, spec.UpdatedAt, spec.PackageDigest = 0, 0, time.Time{}, time.Time{}, ""
		specs[index] = spec
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	row.Specs = specs
	return row
}

// The editor's text comes from the server and saving it unchanged must store
// exactly what was there, field by field. The UI used to write its own YAML and
// lost runtime_weight_bytes, pd_mode, shm_bytes, memlock_unlimited, kv_bytes_per_token, model_config and
// unsupported, and invented "unknown" and "balanced" for empty values.
func TestInferenceTemplateYAMLRoundTripsEveryFieldThroughImport(t *testing.T) {
	f := newFullTemplateFixture(t)
	before, err := f.database.InferenceModelByModelID(fullTemplateID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Specs) != 2 || before.PackageDigest == "" {
		t.Fatalf("fixture import stored %+v", before)
	}
	for _, spec := range before.Specs {
		if spec.Source != "user" || spec.PackageDigest != before.PackageDigest {
			t.Fatalf("spec %s not packaged: %+v", spec.Name, spec)
		}
	}

	response := f.yaml(t, "model_id="+url.QueryEscape(fullTemplateID))
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/yaml") {
		t.Fatalf("yaml status=%d type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	text := response.Body.String()
	for _, want := range []string{
		"tokenizer_ref: ms://Acme/Tokenizer@0123456789abcdef0123456789abcdef01234567",
		"runtime_weight_bytes: 70000000000", "pd_mode: vllm-nixl", "shm_bytes: 429496729600", "memlock_unlimited: true", "kv_bytes_per_token:", "model_config:", "unsupported:",
		"arch_id: sm_90", "readme_zh_cn: README.zh-CN.md", "resolver: |", "pre_start_script: |", "request_defaults:", "format: json",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("editor YAML lacks %q:\n%s", want, text)
		}
	}
	for _, invented := range []string{"unknown", "text_config: null", "rope_scaling: {}"} {
		if strings.Contains(text, invented) {
			t.Errorf("editor YAML states %q:\n%s", invented, text)
		}
	}

	save := httptest.NewRecorder()
	f.handler.importInferenceTemplates(save, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import?overwrite=true", strings.NewReader(text)))
	if save.Code != http.StatusOK {
		t.Fatalf("saving the editor text status/body=%d %s\n%s", save.Code, save.Body.String(), text)
	}
	after, err := f.database.InferenceModelByModelID(fullTemplateID)
	if err != nil {
		t.Fatal(err)
	}
	if want, got := withoutIdentity(*before), withoutIdentity(*after); !reflect.DeepEqual(got, want) {
		t.Fatalf("saving the editor text changed the template\n got %+v\nwant %+v", got, want)
	}
	// The package it now points at still holds the documentation and the asset.
	if got, err := f.packages.ReadAsset(after.PackageDigest, fullTemplateID, fullAssetPath); err != nil || !bytes.Equal(got, f.asset) {
		t.Fatalf("asset after save = %q, %v", got, err)
	}
	if got, err := f.packages.ReadDocumentation(after.PackageDigest, fullTemplateID, fullReadmeEN); err != nil || string(got) != "# Full template\n" {
		t.Fatalf("README after save = %q, %v", got, err)
	}
	// The mounted draft model was not rewritten by the edit.
	if draft, err := f.database.InferenceModelByModelID(fullDraftID); err != nil || draft.DisplayTitle != "Full draft" {
		t.Fatalf("draft after save = %+v, %v", draft, err)
	}
	// And the text is stable: asking again after the save gives the same document.
	if again := f.yaml(t, "model_id="+url.QueryEscape(fullTemplateID)); again.Body.String() != text {
		t.Fatalf("editor text changed after saving it:\n%s\n---\n%s", text, again.Body.String())
	}
}

// An upstream template is edited through its user specs only, and its
// documentation stays with the built-in package.
func TestInferenceTemplateYAMLForUpstreamTemplateSelectsSpecSource(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream := flatAPIFixture("acme/overlay", "stock")
	upstream.ReadmeZHCN = fullReadmeZHCN
	if err := database.ReconcileInferenceCatalog([]store.InferenceModel{upstream}); err != nil {
		t.Fatal(err)
	}
	custom := flatAPIFixture("acme/overlay", "mine")
	custom.Specs[0].Image = "private.example/mine:1"
	if err := database.ImportUserInferenceCatalog([]store.InferenceModel{custom}, false); err != nil {
		t.Fatal(err)
	}
	handler := &apiHandlers{d: Deps{Store: database}}
	read := func(query string) string {
		recorder := httptest.NewRecorder()
		handler.inferenceTemplateYAML(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/yaml?"+query, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status/body=%d %s", query, recorder.Code, recorder.Body.String())
		}
		return recorder.Body.String()
	}
	user := read("model_id=acme/overlay&spec_source=user")
	if !strings.Contains(user, "name: mine") || strings.Contains(user, "name: stock") || strings.Contains(user, "readme_zh_cn") || !strings.Contains(user, "source: user") {
		t.Fatalf("user specs of an upstream template:\n%s", user)
	}
	all := read("model_id=acme/overlay")
	if !strings.Contains(all, "name: mine") || !strings.Contains(all, "name: stock") {
		t.Fatalf("every spec:\n%s", all)
	}
}

// Wire contract, with literal output. The document is the model's facts in
// catalog field order with nothing added for what is empty.
func TestInferenceTemplateYAMLWireContract(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	row := store.InferenceModel{ModelID: "acme/wire", Source: "user", Unsupported: `[]`, Specs: []store.InferenceSpecRow{{
		Source: "user", Name: "plain", Engine: "vllm", Image: "vllm:wire", Quantization: "bf16",
		WeightSources: `[{"provider":"huggingface","repo_id":"acme/wire"}]`, GPUs: `{"vendor":"nvidia"}`, Args: `[]`, Env: `{}`, PreStartArgv: `[]`,
		RuntimeWeightBytes: 12884901888, PDMode: "vllm-nixl", ShmBytes: 429496729600, MemlockUnlimited: true,
	}}}
	if err := database.ImportUserInferenceCatalog([]store.InferenceModel{row}, false); err != nil {
		t.Fatal(err)
	}
	handler := &apiHandlers{d: Deps{Store: database}}
	recorder := httptest.NewRecorder()
	handler.inferenceTemplateYAML(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/yaml?model_id=ACME/Wire", nil))
	want := "models:\n" +
		"  - model_id: acme/wire\n" +
		"    source: user\n" +
		"    specs:\n" +
		"      - name: plain\n" +
		"        engine: vllm\n" +
		"        image: vllm:wire\n" +
		"        weights:\n" +
		"          quantization: bf16\n" +
		"          sources:\n" +
		"            - huggingface: acme/wire\n" +
		"        runtime_weight_bytes: 12884901888\n" +
		"        gpus:\n" +
		"          vendor: nvidia\n" +
		"        pd_mode: vllm-nixl\n" +
		"        shm_bytes: 429496729600\n" +
		"        memlock_unlimited: true\n"
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/yaml; charset=utf-8" || recorder.Body.String() != want {
		t.Fatalf("status=%d type=%q body:\n%s\nwant:\n%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String(), want)
	}
}

func TestInferenceTemplateYAMLErrors(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.ReconcileInferenceCatalog([]store.InferenceModel{flatAPIFixture("acme/known", "stock")}); err != nil {
		t.Fatal(err)
	}
	handler := &apiHandlers{d: Deps{Store: database}}
	for _, tc := range []struct {
		name, query string
		status      int
		code        string
	}{
		{"no model", "", http.StatusBadRequest, `"code":"INFERENCE_TEMPLATE_MODEL_REQUIRED"`},
		{"blank model", "model_id=%20", http.StatusBadRequest, `"code":"INFERENCE_TEMPLATE_MODEL_REQUIRED"`},
		{"bad spec source", "model_id=acme/known&spec_source=system", http.StatusBadRequest, `"code":"INFERENCE_TEMPLATE_SPEC_SOURCE_INVALID"`},
		{"unknown model", "model_id=acme/missing", http.StatusNotFound, `"error":"not found"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.inferenceTemplateYAML(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/yaml?"+tc.query, nil))
			if recorder.Code != tc.status || !strings.Contains(recorder.Body.String(), tc.code) {
				t.Fatalf("status/body=%d %s, want %d containing %s", recorder.Code, recorder.Body.String(), tc.status, tc.code)
			}
			if strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/yaml") {
				t.Fatalf("an error answered with a YAML content type")
			}
		})
	}
}

// Through the real router: the route needs a signed-in user (any role reads it),
// answers GET only, and reaches the handler with the query intact.
func TestInferenceTemplateYAMLRoute(t *testing.T) {
	signer, err := auth.NewTokenSigner("template-yaml-route-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{flatAPIFixture("acme/routed", "stock")}); err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{AuthSigner: signer, Store: st})
	target := "/api/v1/inference-templates/yaml?model_id=acme/routed"

	anonymous := httptest.NewRecorder()
	router.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, target, nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s", anonymous.Code, anonymous.Body.String())
	}
	for _, role := range []string{"user", "admin"} {
		token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: role, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "model_id: acme/routed") {
			t.Fatalf("%s status/body=%d %s", role, recorder.Code, recorder.Body.String())
		}
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			request := httptest.NewRequest(method, target, nil)
			request.Header.Set("Authorization", "Bearer "+token)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s status=%d, want 405", role, method, recorder.Code)
			}
		}
	}
}
