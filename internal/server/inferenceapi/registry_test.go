package inferenceapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRegistryCoversElevenExecutableOperations(t *testing.T) {
	definitions := Definitions()
	if len(definitions) != 11 {
		t.Fatalf("definitions=%d, want 11", len(definitions))
	}
	for _, definition := range definitions {
		if definition.Type == "" || definition.DefaultEndpoint == "" || len(definition.AllowedEndpoints) == 0 || len(definition.RequestEncodings) == 0 || len(definition.Modes) == 0 || definition.Renderer == "" || definition.Response == "" {
			t.Fatalf("incomplete definition: %+v", definition)
		}
	}
	video, ok := Lookup(OperationVideoGenerate)
	if !ok || video.Async == nil || video.Async.PollEndpoint != "{endpoint}/{id}" || video.Async.ContentEndpoint != "{endpoint}/{id}/content" || video.Async.CancelMethod != "DELETE" {
		t.Fatalf("video async lifecycle = %+v", video.Async)
	}
}

// A System One decision is one JSON request to /v1/systemone and one JSON
// reply, rendered by the Playground's decision page.
func TestSystemOneDecisionIsASynchronousJSONOperation(t *testing.T) {
	decide, ok := Lookup(OperationDecide)
	if !ok {
		t.Fatal("systemone.decide is not registered")
	}
	if decide.DefaultEndpoint != "/v1/systemone" || strings.Join(decide.AllowedEndpoints, ",") != "/v1/systemone" ||
		strings.Join(decide.Modes, ",") != "sync" || strings.Join(decide.RequestEncodings, ",") != EncodingJSON ||
		decide.Renderer != "decision" || decide.Response != ResponseJSON || strings.Join(decide.PrimaryInputs, ",") != "state,questions" {
		t.Fatalf("definition = %+v", decide)
	}
	contract, err := NormalizeContract(Contract{Operations: []Operation{{Type: OperationDecide}}})
	if err != nil || contract.Operations[0].Endpoint != "/v1/systemone" || contract.Operations[0].Mode != "sync" {
		t.Fatalf("normalized = %+v, %v", contract, err)
	}
	if !AllowsRequest(contract.Operations[0], "POST", "/v1/systemone") || AllowsRequest(contract.Operations[0], "POST", "/v1/chat/completions") {
		t.Fatal("a decision deployment must serve exactly its own route")
	}
	if _, err := NormalizeContract(Contract{Operations: []Operation{{Type: OperationDecide, Endpoint: "/v1/decisions"}}}); err == nil {
		t.Fatal("an endpoint outside the registered one must be rejected")
	}
	if _, err := NormalizeContract(Contract{Operations: []Operation{{Type: OperationDecide, Inputs: []InputField{{Name: "questions", Kind: "json"}}}}}); err == nil {
		t.Fatal("a declared input may not shadow the questions primary input")
	}
}

func TestRealtimeWebSocketRouteIsAuthorized(t *testing.T) {
	operation := Operation{Type: OperationRealtime}
	if !AllowsRequest(operation, "GET", "/v1/realtime") {
		t.Fatal("realtime WebSocket handshake route should be allowed")
	}
	for _, request := range [][2]string{{"POST", "/v1/realtime"}, {"GET", "/v1/chat/completions"}} {
		if AllowsRequest(operation, request[0], request[1]) {
			t.Fatalf("accepted %s %s", request[0], request[1])
		}
	}
	example, err := BuildExample(operation, "voxtral")
	if err != nil {
		t.Fatal(err)
	}
	if example.Request.Method != "GET" || !strings.Contains(example.Curl, "websocat") || !strings.Contains(example.Python, "websocket.create_connection") {
		t.Fatalf("realtime example=%+v", example)
	}
}

func TestNormalizeContractUsesRegisteredDefaultsAndRejectsDrift(t *testing.T) {
	contract, err := NormalizeContract(Contract{Operations: []Operation{{Type: " OPENAI.VIDEO.GENERATE "}}})
	if err != nil {
		t.Fatal(err)
	}
	operation := contract.Operations[0]
	if operation.Endpoint != "/v1/videos" || operation.Mode != "async" {
		t.Fatalf("operation=%+v", operation)
	}
	for _, input := range []Contract{
		{Operations: []Operation{{Type: OperationVideoGenerate, Endpoint: "/custom/video"}}},
		{Operations: []Operation{{Type: OperationEmbeddings, Mode: "stream"}}},
		{Operations: []Operation{{Type: "custom.magic"}}},
	} {
		if _, err := NormalizeContract(input); err == nil {
			t.Fatalf("accepted invalid contract %+v", input)
		}
	}
}

func TestVideoAsyncRoutesAreAuthorizedButUnrelatedRoutesAreNot(t *testing.T) {
	operation := Operation{Type: OperationVideoGenerate}
	allowed := [][2]string{{"POST", "/v1/videos"}, {"GET", "/v1/videos/job-1"}, {"DELETE", "/v1/videos/job-1"}, {"GET", "/v1/videos/job-1/content"}}
	for _, request := range allowed {
		if !AllowsRequest(operation, request[0], request[1]) {
			t.Fatalf("rejected %s %s", request[0], request[1])
		}
	}
	for _, request := range [][2]string{{"GET", "/v1/videos"}, {"POST", "/v1/videos/job-1"}, {"GET", "/v1/videos/job-1/secret"}, {"POST", "/v1/chat/completions"}} {
		if AllowsRequest(operation, request[0], request[1]) {
			t.Fatalf("accepted %s %s", request[0], request[1])
		}
	}
}

func TestSpeechOperationAcceptsAudioGenerationRoute(t *testing.T) {
	contract, err := NormalizeContract(Contract{Operations: []Operation{{
		Type: OperationAudioSpeech, Endpoint: "/v1/audio/generate",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	operation := contract.Operations[0]
	if operation.Endpoint != "/v1/audio/generate" || operation.Mode != "sync" || operation.RequestEncoding != EncodingJSON {
		t.Fatalf("operation=%+v", operation)
	}
	if !AllowsRequest(operation, "POST", "/v1/audio/generate") {
		t.Fatal("declared audio generation route should be allowed")
	}
	for _, request := range [][2]string{{"POST", "/v1/audio/speech"}, {"GET", "/v1/audio/generate"}} {
		if AllowsRequest(operation, request[0], request[1]) {
			t.Fatalf("accepted %s %s", request[0], request[1])
		}
	}
	if _, err := NormalizeContract(Contract{Operations: []Operation{{Type: OperationAudioSpeech, Endpoint: "/v1/audio/music"}}}); err == nil {
		t.Fatal("accepted an unregistered audio route")
	}
}

func TestImageEditRequiresItsMultipartRoute(t *testing.T) {
	contract, err := NormalizeContract(Contract{Operations: []Operation{{Type: OperationImageEdit}}})
	if err != nil {
		t.Fatal(err)
	}
	operation := contract.Operations[0]
	if operation.Endpoint != "/v1/images/edits" || operation.RequestEncoding != EncodingMultipart {
		t.Fatalf("operation=%+v", operation)
	}
	if !AllowsRequest(operation, "POST", "/v1/images/edits") {
		t.Fatal("image edit route should be allowed")
	}
	for _, request := range [][2]string{{"POST", "/v1/images/generations"}, {"GET", "/v1/images/edits"}} {
		if AllowsRequest(operation, request[0], request[1]) {
			t.Fatalf("accepted %s %s", request[0], request[1])
		}
	}
}

func TestEffectiveContractWithoutDeclarationHasNoOperations(t *testing.T) {
	contract, err := EffectiveContract(nil, DefaultsInput{Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(contract.Operations) != 0 {
		t.Fatalf("operations=%+v", contract.Operations)
	}
}

func TestSynchronousVideoEndpointHasNoAsyncLifecycleRoutes(t *testing.T) {
	contract, err := NormalizeContract(Contract{Operations: []Operation{{
		Type: OperationVideoGenerate, Endpoint: "/v1/videos/sync", Mode: "sync",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	operation := contract.Operations[0]
	if !AllowsRequest(operation, "POST", "/v1/videos/sync") {
		t.Fatal("sync video creation route should be allowed")
	}
	if AllowsRequest(operation, "GET", "/v1/videos/sync/job-1") {
		t.Fatal("sync video operation must not expose async polling routes")
	}
}

func TestRequestEncodingIsNormalizedAndControlsVideoExample(t *testing.T) {
	contract, err := NormalizeContract(Contract{Operations: []Operation{{
		Type: OperationVideoGenerate, Endpoint: "/v1/videos/sync", Mode: "sync", RequestEncoding: EncodingMultipart,
		Inputs: []InputField{{Name: "size", Kind: "select", Options: []string{"640x360"}, Default: "640x360"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := contract.Operations[0].RequestEncoding; got != EncodingMultipart {
		t.Fatalf("request encoding=%q", got)
	}
	example, err := BuildExample(contract.Operations[0], "served-model")
	if err != nil {
		t.Fatal(err)
	}
	if example.Request.ContentType != "multipart/form-data" || example.Request.Form["size"] != "640x360" {
		t.Fatalf("video example=%+v", example.Request)
	}
	if _, err := NormalizeContract(Contract{Operations: []Operation{{Type: OperationVideoGenerate, RequestEncoding: "xml"}}}); err == nil {
		t.Fatal("unsupported request encoding was accepted")
	}
}

func TestExamplesAreGeneratedFromStructuredRequestsForEveryOperation(t *testing.T) {
	for _, definition := range Definitions() {
		example, err := BuildExample(Operation{Type: definition.Type}, "served-model")
		if err != nil {
			t.Fatalf("%s: %v", definition.Type, err)
		}
		wantMethod := "POST"
		if definition.Type == OperationRealtime {
			wantMethod = "GET"
		}
		if example.Request.Method != wantMethod || example.Request.Endpoint != definition.DefaultEndpoint || example.Curl == "" || example.Python == "" {
			t.Fatalf("%s example=%+v", definition.Type, example)
		}
		if example.Request.ContentType == "application/json" {
			var body map[string]any
			if err := json.Unmarshal(example.Request.Body, &body); err != nil || body["model"] != "served-model" {
				t.Fatalf("%s body=%s err=%v", definition.Type, example.Request.Body, err)
			}
			if !strings.Contains(example.Curl, string(example.Request.Body)) {
				t.Fatalf("%s curl is not rendered from body: %s", definition.Type, example.Curl)
			}
		} else if definition.Type == OperationRealtime {
			if example.Request.ContentType != "" || example.Request.Body != nil || example.Request.Form != nil {
				t.Fatalf("%s request=%+v, want bodyless WebSocket handshake", definition.Type, example.Request)
			}
		} else if example.Request.Form["model"] != "served-model" || (definition.Type == OperationAudioTranscribe && example.Request.Form["file"] != "@audio.wav") || (definition.Type == OperationImageEdit && example.Request.Form["image"] != "@input.png") {
			t.Fatalf("%s form=%v", definition.Type, example.Request.Form)
		}
	}
}

// Text completion is its own operation on /v1/completions: a deployment
// that declares it serves that route and no chat route, and one that
// declares chat only does not serve it.
func TestTextCompletionRouteBelongsToItsOperation(t *testing.T) {
	completion := Operation{Type: OperationCompletions}
	if !AllowsRequest(completion, "POST", "/v1/completions") || AllowsRequest(completion, "POST", "/v1/chat/completions") {
		t.Fatal("text completion routes")
	}
	if AllowsRequest(Operation{Type: OperationChat}, "POST", "/v1/completions") {
		t.Fatal("chat must not expose /v1/completions")
	}
	definition, _ := Lookup(OperationCompletions)
	if definition.Renderer != "completion" || definition.Response != ResponseSSE || definition.DefaultEndpoint != "/v1/completions" {
		t.Fatalf("definition = %+v", definition)
	}
	usage, ok := UsageForPath("/v1/completions")
	if !ok || usage.Prompt != "prompt_tokens" || usage.Completion != "completion_tokens" || !usage.StreamTextInChoice || len(usage.StreamText) != 1 || usage.StreamText[0] != "text" {
		t.Fatalf("usage = %+v", usage)
	}
	if chat, _ := UsageForPath("/v1/chat/completions"); chat.StreamTextInChoice {
		t.Fatal("chat text is in the delta")
	}
	example, err := BuildExample(Operation{Type: OperationCompletions, Mode: "stream", RequestDefaults: map[string]any{"stop": []any{"<|plamo:op|>"}}}, "plamo")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(example.Request.Body, &body); err != nil {
		t.Fatal(err)
	}
	if example.Request.Endpoint != "/v1/completions" || body["model"] != "plamo" || body["prompt"] == nil || body["stream"] != true || body["max_tokens"] != float64(256) {
		t.Fatalf("example body = %s", example.Request.Body)
	}
	if stop, _ := body["stop"].([]any); len(stop) != 1 || stop[0] != "<|plamo:op|>" {
		t.Fatalf("request defaults not in the example: %s", example.Request.Body)
	}
	op, ok := Contract{Operations: []Operation{{Type: OperationEmbeddings}, {Type: OperationCompletions, Endpoint: "/v1/completions"}}}.CheckOperation()
	if !ok || op.Type != OperationCompletions {
		t.Fatalf("health check operation = %+v", op)
	}
}

// example_input gives a text primary input a working value that only the
// usage example and the Playground use; anything else is refused.
func TestExampleInputIsAWorkingValueForATextPrimaryInput(t *testing.T) {
	prompt := "<|plamo:op|>dataset\ntranslation\n<|plamo:op|>input lang=English\nHello\n<|plamo:op|>output lang=Japanese\n"
	contract, err := NormalizeContract(Contract{Operations: []Operation{{Type: OperationCompletions, ExampleInput: map[string]string{"prompt": prompt}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := contract.Operations[0].ExampleInput["prompt"]; got != prompt {
		t.Fatalf("example prompt = %q, want it kept as written", got)
	}
	example, err := BuildExample(contract.Operations[0], "plamo")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(example.Request.Body, &body)
	if body["prompt"] != prompt {
		t.Fatalf("example body = %s", example.Request.Body)
	}
	for name, op := range map[string]Operation{
		"not a primary input": {Type: OperationCompletions, ExampleInput: map[string]string{"stop": "x"}},
		"not a text input":    {Type: OperationChat, ExampleInput: map[string]string{"messages": "hi"}},
		"another op's input":  {Type: OperationEmbeddings, ExampleInput: map[string]string{"prompt": "hi"}},
		"empty value":         {Type: OperationCompletions, ExampleInput: map[string]string{"prompt": " \n"}},
	} {
		if _, err := NormalizeContract(Contract{Operations: []Operation{op}}); err == nil || !strings.Contains(err.Error(), "example_input") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Records written before the field existed read the same.
	var old []Operation
	if err := json.Unmarshal([]byte(`[{"type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]`), &old); err != nil || old[0].ExampleInput != nil {
		t.Fatalf("old record = %+v, %v", old, err)
	}
	wire, _ := json.Marshal(contract.Operations[0])
	var back Operation
	if err := json.Unmarshal(wire, &back); err != nil || back.ExampleInput["prompt"] != prompt || !strings.Contains(string(wire), `"example_input":{"prompt":`) {
		t.Fatalf("round trip = %s", wire)
	}
}

// The decision example is the request the Playground starts from and the
// template page shows: it must be a complete System One request with one
// question of each type, and the curl snippet must send exactly that body.
func TestDecisionExampleIsACompleteSystemOneRequest(t *testing.T) {
	example, err := BuildExample(Operation{Type: OperationDecide}, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if example.Request.Method != "POST" || example.Request.Endpoint != "/v1/systemone" || example.Request.ContentType != "application/json" {
		t.Fatalf("request = %+v", example.Request)
	}
	var body struct {
		Model     string `json:"model"`
		State     string `json:"state"`
		Questions map[string]struct {
			Type         string          `json:"type"`
			Instructions string          `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(example.Request.Body, &body); err != nil {
		t.Fatal(err)
	}
	types := map[string]bool{}
	for name, question := range body.Questions {
		types[question.Type] = true
		if question.Instructions == "" {
			t.Errorf("%s has no instructions", name)
		}
		if question.Type != "noul" && len(question.Criteria) == 0 {
			t.Errorf("%s (%s) has no criteria", name, question.Type)
		}
	}
	if body.Model != "svc" || body.State == "" || !types["noul"] || !types["choice"] || !types["score"] || len(types) != 3 {
		t.Fatalf("body = %+v", body)
	}
	if !strings.Contains(example.Curl, "/v1/systemone") || !strings.Contains(example.Curl, `"questions"`) {
		t.Fatalf("curl = %s", example.Curl)
	}
}
