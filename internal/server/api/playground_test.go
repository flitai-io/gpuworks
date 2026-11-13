package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/localization"
)

func TestPlaygroundCatalogIncludesDeploymentsWithoutAssistantConfigs(t *testing.T) {
	st, err := store.Open("file:playground_catalog_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "acme/asr", Revision: "main", Name: "ASR",
		Meta: `{"architectures":["Qwen3ASRForConditionalGeneration"],"audio_config":{"model_type":"whisper"}}`, ContextLength: 32768,
		Operations: `[{"type":"openai.audio.transcribe"}]`, InputModalities: `["audio"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	node, _ := st.RegisterNode("playground-node", store.HashToken("token"), "playground-node")
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "qwen-asr", State: "active", Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "asr-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, SleepLevel: 1, APIPort: 30000}); err != nil {
		t.Fatal(err)
	}
	emptyModel, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/empty", Revision: "main", Name: "empty", Operations: `[]`, InputModalities: `[]`})
	emptyDep, _ := st.CreateDeployment(&store.Deployment{ModelID: emptyModel.ID, NodeID: node.ID, ServedName: "empty", State: "active", Engine: "vllm"})
	if _, err := st.CreateInstance(&store.Instance{UUID: "empty-instance", DeploymentID: emptyDep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30001}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAIConfig(&store.AIConfig{Name: "Poe", BaseURL: "https://api.poe.com/v1", Model: "GPT-5"}); err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("playground-catalog-secret", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	router := Router(Deps{Store: st, AuthSigner: signer})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playground/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status=%d body=%s", rec.Code, rec.Body.String())
	}
	var catalog dto.PlaygroundCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Parameters) != 7 || len(catalog.Models) != 1 {
		t.Fatalf("catalog = %+v", catalog)
	}
	for _, parameter := range catalog.Parameters {
		if parameter.Label[localization.English] == "" || parameter.Label[localization.Chinese] == "" {
			t.Fatalf("parameter %q has incomplete labels: %#v", parameter.Name, parameter.Label)
		}
	}
	// Presets travel with the model, because whether a preset can be honored
	// depends on the deployment.
	presets := catalog.Models[0].ExtraParameterPresets
	if len(presets) != 2 {
		t.Fatalf("presets = %+v", presets)
	}
	for _, preset := range presets {
		if preset.Label[localization.English] == "" || preset.Label[localization.Chinese] == "" {
			t.Fatalf("preset has incomplete labels: %#v", preset.Label)
		}
	}
	// The stream_options the client adds are part of the dictionary, on by
	// default, so the panel can show and switch them off.
	if !strings.Contains(rec.Body.String(), `"stream_options":[{"name":"include_usage","label":{"en":"Report usage (include_usage)","zh":"返回用量（include_usage）"},`) ||
		!strings.Contains(rec.Body.String(), `"default":true},{"name":"continuous_usage_stats",`) || !strings.Contains(rec.Body.String(), `"default":true,"requires":"include_usage"}]`) {
		t.Fatalf("stream options wire = %s", rec.Body.String())
	}
	if len(catalog.StreamOptions) != 2 || catalog.StreamOptions[1].Description[localization.Chinese] == "" {
		t.Fatalf("stream options = %+v", catalog.StreamOptions)
	}
	if catalog.Parameters[0].Name != "temperature" || catalog.Parameters[0].Default != float64(0.7) || presets[0].Value["reasoning_effort"] != "low" {
		t.Fatalf("playground semantic defaults changed: %+v / %+v", catalog.Parameters[0], presets[0])
	}
	if len(catalog.Models[0].Operations) != 1 || catalog.Models[0].Operations[0].Type != inferenceapi.OperationAudioTranscribe || !catalog.Models[0].Sleeping || catalog.Models[0].ID != "qwen-asr" || !catalog.Models[0].PublicDataPlane {
		t.Fatalf("deployment model = %+v", catalog.Models[0])
	}
	if len(catalog.Models[0].Operations[0].Discovery) != 2 || len(catalog.Models[0].Operations[0].PrimaryInputs) != 1 || catalog.Models[0].Operations[0].PrimaryInputs[0] != "file" {
		t.Fatalf("sleeping model lost discovery/inputs: %+v", catalog.Models[0].Operations)
	}
	if len(catalog.Models[0].Operations) != 1 || catalog.Models[0].Operations[0].Type != "openai.audio.transcribe" || catalog.Models[0].Operations[0].Endpoint != "/v1/audio/transcriptions" || catalog.Models[0].Operations[0].Renderer != "audio" {
		t.Fatalf("deployment operations = %+v", catalog.Models[0].Operations)
	}
	if strings.Contains(rec.Body.String(), "external:") || strings.Contains(rec.Body.String(), "GPT-5") {
		t.Fatalf("assistant configuration leaked into Playground: %s", rec.Body)
	}
}

func TestPlaygroundCatalogUsesCurrentModelOperations(t *testing.T) {
	st, err := store.Open("file:playground_snapshot_contract_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/video", Revision: "main", Name: "video", Meta: `{"architectures":["CausalLM"]}`, Operations: `[{"type":"openai.video.generate"}]`, InputModalities: `["text"]`})
	node, _ := st.RegisterNode("snapshot-node", store.HashToken("token"), "snapshot-node")
	dep, _ := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "video", State: "active", Engine: "sglang"})
	contract := json.RawMessage(`{"health_endpoint":"/health","operations":[{"type":"openai.embeddings"}]}`)
	desired, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{APIContract: contract}})
	_, _ = st.CreateInstance(&store.Instance{UUID: "snapshot-instance", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, DesiredStateJSON: string(desired)})
	signer, _ := auth.NewTokenSigner("snapshot-contract-secret", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playground/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(Deps{Store: st, AuthSigner: signer}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var catalog dto.PlaygroundCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 || len(catalog.Models[0].Operations) != 1 || catalog.Models[0].Operations[0].Type != "openai.video.generate" {
		t.Fatalf("catalog=%+v", catalog.Models)
	}
}

func TestPlaygroundCatalogDeduplicatesRolloutsAndPrefersServingDeployment(t *testing.T) {
	st, err := store.Open("file:playground_rollout_dedupe_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/video", Revision: "main", Name: "video", Operations: `[{"type":"openai.video.generate"}]`, InputModalities: `["text"]`})
	node, _ := st.RegisterNode("rollout-node", store.HashToken("token"), "rollout-node")
	serving, _ := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "stable-name", State: "active", Engine: "sglang"})
	contract := json.RawMessage(`{"health_endpoint":"/health","operations":[{"type":"openai.embeddings"}]}`)
	desired, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: proto.ResolvedLaunch{APIContract: contract}})
	_, _ = st.CreateInstance(&store.Instance{UUID: "serving-instance", DeploymentID: serving.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, DesiredStateJSON: string(desired)})
	// Written as a row from before served names were unique: a create now
	// refuses the name the serving deployment holds.
	pending := &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "stable-name", State: "active", Engine: "vllm"}
	if err := st.DB().Create(pending).Error; err != nil {
		t.Fatal(err)
	}
	_, _ = st.CreateInstance(&store.Instance{UUID: "pending-instance", DeploymentID: pending.ID, NodeID: node.ID, State: proto.InstStarting})

	signer, _ := auth.NewTokenSigner("rollout-dedupe-secret", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playground/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(Deps{Store: st, AuthSigner: signer}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var catalog dto.PlaygroundCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].DeploymentID != serving.ID {
		t.Fatalf("catalog models=%+v, want one serving rollout", catalog.Models)
	}
	if len(catalog.Models[0].Operations) != 1 || catalog.Models[0].Operations[0].Type != "openai.video.generate" {
		t.Fatalf("catalog contract came from pending rollout: %+v", catalog.Models[0].Operations)
	}
}

func TestPlaygroundCatalogDropsTranscriptionTheEngineDoesNotServe(t *testing.T) {
	st, err := store.Open("file:playground_openapi_paths?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("openapi-node", store.HashToken("token"), "openapi-node")
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, operations string, served *bool) {
		t.Helper()
		model, err := st.UpsertModel(&store.Model{
			Source: "huggingface", SourceID: "acme/" + name, Revision: "main", Name: name,
			Operations: operations, InputModalities: `["audio","text"]`,
		})
		if err != nil {
			t.Fatal(err)
		}
		dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: name, State: "active", Engine: "vllm"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInstance(&store.Instance{
			UUID: name, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, TranscriptionServed: served,
		}); err != nil {
			t.Fatal(err)
		}
	}
	served, absent := true, false
	add("asr-only", `[{"type":"openai.audio.transcribe"}]`, &absent)
	add("asr-and-chat", `[{"type":"openai.chat"},{"type":"openai.audio.transcribe"}]`, &absent)
	add("asr-served", `[{"type":"openai.audio.transcribe"}]`, &served)
	add("asr-unknown", `[{"type":"openai.audio.transcribe"}]`, nil)

	signer, _ := auth.NewTokenSigner("openapi-paths-secret", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playground/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(Deps{Store: st, AuthSigner: signer}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var catalog dto.PlaygroundCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, model := range catalog.Models {
		for _, operation := range model.Operations {
			got[model.ID] = append(got[model.ID], operation.Type)
		}
	}
	if _, ok := got["asr-only"]; ok {
		t.Fatalf("transcription-only model stayed listed: %+v", got)
	}
	if strings.Join(got["asr-and-chat"], ",") != inferenceapi.OperationChat {
		t.Fatalf("chat model = %v", got["asr-and-chat"])
	}
	if strings.Join(got["asr-served"], ",") != inferenceapi.OperationAudioTranscribe {
		t.Fatalf("served asr = %v", got["asr-served"])
	}
	if strings.Join(got["asr-unknown"], ",") != inferenceapi.OperationAudioTranscribe {
		t.Fatalf("unknown observation = %v", got["asr-unknown"])
	}
}

func TestPlaygroundCatalogOmitsDeploymentsThatCannotServe(t *testing.T) {
	st, err := store.Open("file:playground_unservable_catalog_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	ops := `[{"type":"openai.embeddings"}]`
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/embed", Revision: "main", Name: "embed", Operations: ops, InputModalities: `["text"]`})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("catalog-node", store.HashToken("token"), "catalog-node")
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, uuid, state string, sleep, port int) *store.Deployment {
		t.Helper()
		dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: name, State: "active", Engine: "vllm"})
		if err != nil {
			t.Fatal(err)
		}
		// A sleep order is written into the instance's desired state, so the
		// fixture carries one: without it the order has nowhere to land.
		desired := fmt.Sprintf(`{"uuid":%q,"state":"running","sleepable":true}`, uuid)
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: state, SleepLevel: sleep, APIPort: port, DesiredStateJSON: desired}); err != nil {
			t.Fatal(err)
		}
		return dep
	}
	running := add("running-model", "running-instance", proto.InstRunning, 0, 30000)
	// Asleep by the platform's choice: a request wakes it.
	platform := add("sleeping-model", "sleeping-instance", proto.InstRunning, 1, 30001)
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", platform.ID).Update("sleepable", true).Error; err != nil {
		t.Fatal(err)
	}
	// Ordered to sleep by an operator, engine not there yet: the gateway
	// already refuses this deployment, so the page must offer the wake instead
	// of hiding it behind an instance that still reports level 0.
	ordered := add("ordered-model", "ordered-instance", proto.InstRunning, 0, 30002)
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", ordered.ID).Update("sleepable", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleepHeld(ordered.ID, []string{"ordered-instance"}, 1, "wait", true); err != nil {
		t.Fatal(err)
	}
	// One replica held asleep by an operator, the other asleep by the
	// platform's choice: a request wakes the latter.
	mixed := add("mixed-model", "mixed-held", proto.InstRunning, 2, 30003)
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", mixed.ID).Update("sleepable", true).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "mixed-free", DeploymentID: mixed.ID, NodeID: node.ID, State: proto.InstRunning, SleepLevel: 1, APIPort: 30004,
		DesiredStateJSON: `{"uuid":"mixed-free","state":"running","sleepable":true}`}); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleepHeld(mixed.ID, []string{"mixed-held"}, 2, "wait", true); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleep(mixed.ID, []string{"mixed-free"}, 1, "wait"); err != nil {
		t.Fatal(err)
	}
	add("failed-model", "failed-instance", proto.InstFailed, 0, 0)
	add("starting-model", "starting-instance", proto.InstStarting, 0, 0)
	// A newer failed rollout of a name that is already serving must not
	// replace that serving deployment, and must not add a second entry.
	failedRollout := &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "running-model", State: "active", Engine: "vllm"}
	if err := st.DB().Create(failedRollout).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "failed-rollout", DeploymentID: failedRollout.ID, NodeID: node.ID, State: proto.InstFailed}); err != nil {
		t.Fatal(err)
	}

	signer, _ := auth.NewTokenSigner("unservable-catalog-secret", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playground/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(Deps{Store: st, AuthSigner: signer}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var catalog dto.PlaygroundCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	got := map[string]dto.PlaygroundModel{}
	for _, model := range catalog.Models {
		got[model.ID] = model
	}
	if len(got) != 4 {
		t.Fatalf("models=%v, want running, sleeping, ordered and mixed only", got)
	}
	if !got["mixed-model"].Sleeping || !got["mixed-model"].WakesOnRequest {
		t.Fatalf("mixed model=%+v, want asleep and woken by a request through its platform sleeper", got["mixed-model"])
	}
	if got["running-model"].DeploymentID != running.ID || got["running-model"].Sleeping {
		t.Fatalf("running model=%+v, want serving deployment %d", got["running-model"], running.ID)
	}
	if !got["sleeping-model"].Sleeping || !got["sleeping-model"].WakesOnRequest {
		t.Fatalf("sleeping model=%+v, want asleep and woken by a request", got["sleeping-model"])
	}
	if !got["ordered-model"].Sleeping || got["ordered-model"].WakesOnRequest {
		t.Fatalf("sleep-ordered model=%+v, want the wake offered before its engine reports a level, and no request to wake it", got["ordered-model"])
	}
	if got["running-model"].WakesOnRequest {
		t.Fatal("an awake model says a request wakes it")
	}
	if _, ok := got["failed-model"]; ok {
		t.Fatal("failed deployment was listed as available")
	}
	if _, ok := got["starting-model"]; ok {
		t.Fatal("starting deployment was listed as available")
	}
}

// The page learns how many reference images an edit takes from the server:
// the declared count, one when the stored contract predates the field, and no
// key at all for an operation that takes none.
func TestPlaygroundCatalogResolvesReferenceImageLimits(t *testing.T) {
	st, err := store.Open("file:playground_reference_images_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, _ := st.RegisterNode("reference-node", store.HashToken("token"), "reference-node")
	for index, item := range []struct{ served, operations string }{
		{"multi-edit", `[{"type":"openai.image.generate"},{"type":"openai.image.edit","endpoint":"/v1/images/edits","mode":"sync","request_encoding":"multipart","max_reference_images":9}]`},
		{"single-edit", `[{"type":"openai.image.edit","endpoint":"/v1/images/edits","mode":"sync","request_encoding":"multipart","inputs":[]}]`},
	} {
		model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/" + item.served, Revision: "main", Name: item.served, Operations: item.operations, InputModalities: `["text","image"]`})
		if err != nil {
			t.Fatal(err)
		}
		dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: item.served, State: "active", Engine: "vllm"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInstance(&store.Instance{UUID: item.served, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000 + index}); err != nil {
			t.Fatal(err)
		}
	}
	signer, _ := auth.NewTokenSigner("reference-images-secret", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playground/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(Deps{Store: st, AuthSigner: signer}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var wire struct {
		Models []struct {
			ID         string                       `json:"id"`
			Operations []map[string]json.RawMessage `json:"operations"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, model := range wire.Models {
		for _, operation := range model.Operations {
			var kind string
			_ = json.Unmarshal(operation["type"], &kind)
			got[model.ID+" "+kind] = string(operation["max_reference_images"])
		}
	}
	want := map[string]string{"multi-edit openai.image.generate": "", "multi-edit openai.image.edit": "9", "single-edit openai.image.edit": "1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("max_reference_images on the wire = %v, want %v", got, want)
	}
}
