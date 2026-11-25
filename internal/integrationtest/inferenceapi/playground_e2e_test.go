package inferenceapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestPlaygroundOperationsThroughRealControlPlane is the backend acceptance
// path for executable API contracts. It deliberately crosses the real router,
// JWT middleware, deployment snapshot lookup, proxy tunnel, agent and fake
// runtime HTTP server. No handler is invoked directly.
func TestPlaygroundOperationsThroughRealControlPlane(t *testing.T) {
	env := testenv.New(t)
	node := env.AddNode("inference-api-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, node.Name)
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "gpuworks/all-operations", Revision: "main", Name: "all-operations",
		Arch: "FakeForCausalLM", WeightBytes: 1 << 20,
		Meta: `{"model_type":"fake","architectures":["FakeForCausalLM"],"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := inferenceapi.NormalizeContract(inferenceapi.Contract{HealthEndpoint: "/health", Operations: []inferenceapi.Operation{
		{Type: inferenceapi.OperationChat, Mode: "stream"},
		{Type: inferenceapi.OperationEmbeddings},
		{Type: inferenceapi.OperationRerank},
		{Type: inferenceapi.OperationAudioTranscribe},
		{Type: inferenceapi.OperationAudioSpeech},
		{Type: inferenceapi.OperationImageGenerate},
		{Type: inferenceapi.OperationVideoGenerate},
	}})
	if err != nil {
		t.Fatal(err)
	}
	operationsJSON, err := json.Marshal(contract.Operations)
	if err != nil {
		t.Fatal(err)
	}
	modalitiesJSON, err := json.Marshal([]string{"text", "image", "audio", "video"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().SetModelInferenceConfig(model.ID, string(operationsJSON), string(modalitiesJSON)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "all-operations")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	createBody, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "node_uuid": nodes[0].UUID, "served_name": "all-operations", "engine": "vllm",
		"image": "gpuworks/fake-engine:test", "quick_params": map[string]any{"max_model_len": 128},
	})
	created, err := api.PostJSON("/api/v1/deployments", createBody)
	if err != nil {
		t.Fatal(err)
	}
	if created.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(created.Body)
		created.Body.Close()
		t.Fatalf("create deployment status=%d body=%s", created.StatusCode, body)
	}
	created.Body.Close()

	env.Eventually(t, 30*time.Second, func() bool {
		instances, listErr := env.Store().ListInstances()
		if listErr != nil || len(instances) != 1 || instances[0].State != proto.InstRunning || instances[0].APIPort == 0 {
			return false
		}
		return true
	})

	catalogResponse, err := api.Get("/api/v1/playground/models")
	if err != nil {
		t.Fatal(err)
	}
	var catalog dto.PlaygroundCatalog
	if err := json.NewDecoder(catalogResponse.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	catalogResponse.Body.Close()
	if catalogResponse.StatusCode != http.StatusOK || len(catalog.Models) != 1 || len(catalog.Models[0].Operations) != 7 {
		t.Fatalf("catalog status=%d models=%+v", catalogResponse.StatusCode, catalog.Models)
	}

	t.Run("discovery through JWT router and agent tunnel", func(t *testing.T) {
		for _, route := range []string{"/v1/audio/voices", "/openapi.json", "/v1/models"} {
			response, err := api.Get("/api/v1/playground" + route + "?model=all-operations")
			if err != nil {
				t.Fatal(err)
			}
			body := readBody(t, response)
			if response.StatusCode != http.StatusOK || !json.Valid(body) {
				t.Fatalf("%s status=%d body=%s", route, response.StatusCode, body)
			}
			if route == "/v1/audio/voices" && !bytes.Contains(body, []byte(`"voices":[]`)) {
				t.Fatalf("voices=%s", body)
			}
			if route == "/openapi.json" && !bytes.Contains(body, []byte(`OpenAICreateSpeechRequest`)) {
				t.Fatal("schema missing speech request")
			}
		}
	})

	t.Run("chat SSE", func(t *testing.T) {
		response := doJSON(t, api, http.MethodPost, "/api/v1/playground/v1/chat/completions", `{"model":"all-operations","messages":[{"role":"user","content":"hello"}],"stream":true}`)
		body := readBody(t, response)
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" || !bytes.Contains(body, []byte("data: [DONE]")) {
			t.Fatalf("status=%d type=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), body)
		}
	})

	t.Run("embeddings JSON", func(t *testing.T) {
		response := doJSON(t, api, http.MethodPost, "/api/v1/playground/v1/embeddings", `{"model":"all-operations","input":"hello"}`)
		body := readBody(t, response)
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"embedding":[0.125,-0.25,0.5]`)) {
			t.Fatalf("status=%d body=%s", response.StatusCode, body)
		}
	})

	t.Run("rerank JSON", func(t *testing.T) {
		response := doJSON(t, api, http.MethodPost, "/api/v1/playground/v1/rerank", `{"model":"all-operations","query":"hello","documents":["one","two"]}`)
		body := readBody(t, response)
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"relevance_score":1`)) {
			t.Fatalf("status=%d body=%s", response.StatusCode, body)
		}
	})

	t.Run("audio multipart", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("model", "all-operations")
		file, createErr := writer.CreateFormFile("file", "audio.wav")
		if createErr != nil {
			t.Fatal(createErr)
		}
		_, _ = file.Write([]byte("RIFF-audio"))
		_ = writer.Close()
		request, _ := http.NewRequest(http.MethodPost, env.URL()+"/api/v1/playground/v1/audio/transcriptions", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		response, requestErr := api.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		responseBody := readBody(t, response)
		if response.StatusCode != http.StatusOK || !bytes.Contains(responseBody, []byte(`"text":"hello from fake-engine"`)) {
			t.Fatalf("status=%d body=%s", response.StatusCode, responseBody)
		}
	})

	t.Run("audio binary", func(t *testing.T) {
		response := doJSON(t, api, http.MethodPost, "/api/v1/playground/v1/audio/speech", `{"model":"all-operations","input":"hello","voice":"alloy","response_format":"wav"}`)
		body := readBody(t, response)
		if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "audio/wav" || !bytes.HasPrefix(body, []byte("RIFF")) {
			t.Fatalf("status=%d type=%q body=%q", response.StatusCode, response.Header.Get("Content-Type"), body)
		}
	})

	t.Run("image JSON", func(t *testing.T) {
		response := doJSON(t, api, http.MethodPost, "/api/v1/playground/v1/images/generations", `{"model":"all-operations","prompt":"sunrise","response_format":"b64_json"}`)
		body := readBody(t, response)
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"b64_json"`)) {
			t.Fatalf("status=%d body=%s", response.StatusCode, body)
		}
	})

	t.Run("video async create poll content cancel", func(t *testing.T) {
		createdVideo := doJSON(t, api, http.MethodPost, "/api/v1/playground/v1/videos", `{"model":"all-operations","prompt":"sunrise","seconds":4}`)
		if createdVideo.StatusCode != http.StatusAccepted {
			body := readBody(t, createdVideo)
			t.Fatalf("create status=%d body=%s", createdVideo.StatusCode, body)
		}
		var job struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.NewDecoder(createdVideo.Body).Decode(&job); err != nil {
			t.Fatal(err)
		}
		createdVideo.Body.Close()
		if job.ID == "" || job.Status != "queued" {
			t.Fatalf("created job=%+v", job)
		}

		poll := do(t, api, http.MethodGet, fmt.Sprintf("/api/v1/playground/v1/videos/%s?model=all-operations", job.ID), nil, "")
		if err := json.NewDecoder(poll.Body).Decode(&job); err != nil {
			t.Fatal(err)
		}
		poll.Body.Close()
		if poll.StatusCode != http.StatusOK || job.Status != "completed" {
			t.Fatalf("poll status=%d job=%+v", poll.StatusCode, job)
		}

		content := do(t, api, http.MethodGet, fmt.Sprintf("/api/v1/playground/v1/videos/%s/content?model=all-operations", job.ID), nil, "")
		video := readBody(t, content)
		if content.StatusCode != http.StatusOK || content.Header.Get("Content-Type") != "video/mp4" || !bytes.Contains(video, []byte("ftypisom")) {
			t.Fatalf("content status=%d type=%q body=%q", content.StatusCode, content.Header.Get("Content-Type"), video)
		}

		cancelled := do(t, api, http.MethodDelete, fmt.Sprintf("/api/v1/playground/v1/videos/%s?model=all-operations", job.ID), nil, "")
		if err := json.NewDecoder(cancelled.Body).Decode(&job); err != nil {
			t.Fatal(err)
		}
		cancelled.Body.Close()
		if cancelled.StatusCode != http.StatusOK || job.Status != "deleted" {
			t.Fatalf("cancel status=%d job=%+v", cancelled.StatusCode, job)
		}
	})

	t.Run("contract and upstream validation reject invalid requests", func(t *testing.T) {
		wrongRoute := doJSON(t, api, http.MethodPost, "/api/v1/playground/v1/audio/translations", `{"model":"all-operations"}`)
		wrongRouteBody := readBody(t, wrongRoute)
		if wrongRoute.StatusCode != http.StatusNotFound || !bytes.Contains(wrongRouteBody, []byte("does not expose")) {
			t.Fatalf("wrong route status=%d body=%s", wrongRoute.StatusCode, wrongRouteBody)
		}
		invalidPayload := doJSON(t, api, http.MethodPost, "/api/v1/playground/v1/embeddings", `{"model":"all-operations"}`)
		invalidBody := readBody(t, invalidPayload)
		if invalidPayload.StatusCode != http.StatusBadRequest || !bytes.Contains(invalidBody, []byte("input is required")) {
			t.Fatalf("invalid payload status=%d body=%s", invalidPayload.StatusCode, invalidBody)
		}
		unauthenticated, requestErr := http.Get(env.URL() + "/api/v1/playground/models")
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		unauthenticated.Body.Close()
		if unauthenticated.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated status=%d", unauthenticated.StatusCode)
		}
	})
}

func doJSON(t *testing.T, api *testenv.APIClient, method, path, body string) *http.Response {
	t.Helper()
	return do(t, api, method, path, strings.NewReader(body), "application/json")
}

func do(t *testing.T, api *testenv.APIClient, method, path string, body io.Reader, contentType string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, api.BaseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := api.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
