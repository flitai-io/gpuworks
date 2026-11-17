package inferenceapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ExampleRequest is the machine-readable request behind the rendered curl and
// Python snippets. Tests and UI can compare this object with the actual request
// instead of treating example text as another source of protocol truth.
type ExampleRequest struct {
	Method      string            `json:"method"`
	Endpoint    string            `json:"endpoint"`
	ContentType string            `json:"content_type,omitempty"`
	Body        json.RawMessage   `json:"body,omitempty"`
	Form        map[string]string `json:"form,omitempty"`
}

type Example struct {
	Request   ExampleRequest   `json:"request"`
	FollowUps []ExampleRequest `json:"follow_ups,omitempty"`
	Curl      string           `json:"curl"`
	Python    string           `json:"python"`
}

// BuildExample generates human-readable snippets from the same structured
// request that drives usage UI. The model may be a concrete served name or the
// catalog placeholder used before deployment.
func BuildExample(operation Operation, model string) (Example, error) {
	contract, err := NormalizeContract(Contract{Operations: []Operation{operation}})
	if err != nil {
		return Example{}, err
	}
	operation = contract.Operations[0]
	if strings.TrimSpace(model) == "" {
		model = "<served-model>"
	}
	request := ExampleRequest{Method: "POST", Endpoint: operation.Endpoint}
	switch operation.Type {
	case OperationChat:
		request.ContentType = "application/json"
		request.Body = mustJSON(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "Hello"}}, "stream": operation.Mode == "stream"})
	case OperationCompletions:
		request.ContentType = "application/json"
		body, composeErr := ComposeRequest(operation, model, map[string]any{"prompt": exampleText(operation, "prompt", "The capital of France is"), "max_tokens": 256, "stream": operation.Mode == "stream"}, exampleInputs(operation), nil)
		if composeErr != nil {
			return Example{}, composeErr
		}
		request.Body = body
	case OperationEmbeddings:
		request.ContentType = "application/json"
		request.Body = mustJSON(map[string]any{"model": model, "input": exampleText(operation, "input", "GPU inference")})
	case OperationRerank:
		request.ContentType = "application/json"
		request.Body = mustJSON(map[string]any{"model": model, "query": exampleText(operation, "query", "GPU inference"), "documents": []string{"Document one", "Document two"}})
	case OperationAudioTranscribe:
		request.ContentType = "multipart/form-data"
		request.Form = map[string]string{"model": model, "file": "@audio.wav"}
	case OperationAudioSpeech:
		request.ContentType = "application/json"
		body, composeErr := ComposeRequest(operation, model, map[string]any{"input": exampleText(operation, "input", "Hello")}, exampleInputs(operation), nil)
		if composeErr != nil {
			return Example{}, composeErr
		}
		request.Body = body
	case OperationImageGenerate:
		request.ContentType = "application/json"
		body, composeErr := ComposeRequest(operation, model, map[string]any{"prompt": exampleText(operation, "prompt", "A sunrise over mountains")}, exampleInputs(operation), nil)
		if composeErr != nil {
			return Example{}, composeErr
		}
		request.Body = body
	case OperationImageEdit:
		request.ContentType = "multipart/form-data"
		request.Form = map[string]string{
			"model":  model,
			"prompt": exampleText(operation, "prompt", "Make the background blue"),
			"image":  "@input.png",
		}
	case OperationVideoGenerate:
		body, composeErr := ComposeRequest(operation, model, map[string]any{"prompt": exampleText(operation, "prompt", "A sunrise over mountains")}, exampleInputs(operation), nil)
		if composeErr != nil {
			return Example{}, composeErr
		}
		if operation.RequestEncoding == EncodingMultipart {
			request.ContentType = "multipart/form-data"
			request.Form = formExample(body)
		} else {
			request.ContentType = "application/json"
			request.Body = body
		}
	case OperationRealtime:
		request.Method = "GET"
	case OperationDecide:
		request.ContentType = "application/json"
		request.Body = mustJSON(map[string]any{
			"model": model,
			"state": "Customer message: my order arrived broken and I want my money back today.",
			"questions": map[string]any{
				"refund": map[string]any{"type": "noul", "instructions": "Does the customer ask for a refund?"},
				"topic": map[string]any{"type": "choice", "instructions": "What is the message about?", "criteria": map[string]any{
					"billing": "Payments, refunds or invoices", "shipping": "Delivery or damaged goods", "account": "Login or profile", "other": "Anything else",
				}},
				"urgency": map[string]any{"type": "score", "instructions": "How urgent is the message?", "criteria": []string{"Not urgent", "Somewhat urgent", "Very urgent"}},
			},
		})
	default:
		return Example{}, fmt.Errorf("unsupported operation %q", operation.Type)
	}
	request.Body = readableJSON(request.Body)
	example := Example{Request: request}
	definition, _ := Lookup(operation.Type)
	if definition.Async != nil {
		example.FollowUps = []ExampleRequest{
			{Method: definition.Async.PollMethod, Endpoint: expandLifecycleEndpoint(definition.Async.PollEndpoint, operation.Endpoint)},
			{Method: definition.Async.ContentMethod, Endpoint: expandLifecycleEndpoint(definition.Async.ContentEndpoint, operation.Endpoint)},
			{Method: definition.Async.CancelMethod, Endpoint: expandLifecycleEndpoint(definition.Async.CancelEndpoint, operation.Endpoint)},
		}
	}
	example.Curl = renderCurl(example, model)
	example.Python = renderPython(example, model, definition)
	return example, nil
}

func exampleInputs(operation Operation) map[string]any {
	values := map[string]any{}
	for _, field := range operation.Inputs {
		if field.Default != nil {
			values[field.Name] = field.Default
			continue
		}
		if !field.Required {
			continue
		}
		switch field.Kind {
		case "text":
			values[field.Name] = "Reference text"
		case "number":
			values[field.Name] = float64(1)
		case "select":
			if len(field.Options) > 0 {
				values[field.Name] = field.Options[0]
			}
		case "audio":
			values[field.Name] = "data:audio/wav;base64,AA=="
		case "image":
			values[field.Name] = "data:image/png;base64,AA=="
		case "file":
			values[field.Name] = "@fixture.bin"
		case "json":
			values[field.Name] = map[string]any{}
		}
	}
	return values
}

func formExample(body json.RawMessage) map[string]string {
	values := map[string]any{}
	_ = json.Unmarshal(body, &values)
	form := make(map[string]string, len(values))
	for key, value := range values {
		if text, ok := value.(string); ok {
			form[key] = text
			continue
		}
		form[key] = fmt.Sprint(value)
	}
	return form
}

// readableJSON re-encodes a body without HTML escaping: a snippet meant to
// be read and edited shows "<|plamo:op|>", not "\u003c|plamo:op|\u003e".
func readableJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return raw
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return raw
	}
	return json.RawMessage(bytes.TrimRight(out.Bytes(), "\n"))
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func expandLifecycleEndpoint(template, endpoint string) string {
	return strings.ReplaceAll(strings.ReplaceAll(template, "{endpoint}", endpoint), "{id}", "{id}")
}

func renderCurl(example Example, model string) string {
	request := example.Request
	if request.ContentType == "" {
		return fmt.Sprintf("websocat %s -H=%s", shellToken("$GPUWORKS_WS_URL"+request.Endpoint+"?model="+model), shellToken("Authorization: Bearer $GPUWORKS_API_KEY"))
	}
	lines := []string{fmt.Sprintf("curl -X %s \"$GPUWORKS_URL%s\"", request.Method, request.Endpoint), "  -H \"Authorization: Bearer $GPUWORKS_API_KEY\""}
	if request.ContentType == "application/json" {
		lines = append(lines, "  -H \"Content-Type: application/json\"", "  --data '"+strings.ReplaceAll(string(request.Body), "'", "'\\''")+"'")
	} else {
		keys := make([]string, 0, len(request.Form))
		for key := range request.Form {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lines = append(lines, fmt.Sprintf("  -F %s=%s", shellToken(key), shellToken(request.Form[key])))
		}
	}
	result := strings.Join(lines, " \\\n")
	if len(example.FollowUps) > 0 {
		followUps := []string{"# Use the returned id for status, content, or cancellation:"}
		for _, followUp := range example.FollowUps {
			followUps = append(followUps, fmt.Sprintf("curl -X %s \"$GPUWORKS_URL%s?model=%s\" -H \"Authorization: Bearer $GPUWORKS_API_KEY\"", followUp.Method, followUp.Endpoint, model))
		}
		result += "\n\n" + strings.Join(followUps, "\n")
	}
	return result
}

func renderPython(example Example, model string, definition Definition) string {
	request := example.Request
	if request.ContentType == "" {
		return strings.Join([]string{
			"import os", "import websocket", "",
			fmt.Sprintf("url = os.environ[\"GPUWORKS_WS_URL\"].rstrip(\"/\") + %s", strconv.Quote(request.Endpoint+"?model="+model)),
			"ws = websocket.create_connection(url, header=[f\"Authorization: Bearer {os.environ['GPUWORKS_API_KEY']}\"])",
			"print(ws.recv())", "ws.close()",
		}, "\n")
	}
	lines := []string{"import os", "import requests", "", "base_url = os.environ[\"GPUWORKS_URL\"].rstrip(\"/\")", "headers = {\"Authorization\": f\"Bearer {os.environ['GPUWORKS_API_KEY']}\"}"}
	if request.ContentType == "application/json" {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, request.Body, "", "    "); err != nil {
			pretty.Write(request.Body)
		}
		lines = append(lines, "payload = "+pretty.String(), fmt.Sprintf("response = requests.%s(base_url + %s, headers=headers, json=payload)", strings.ToLower(request.Method), strconv.Quote(request.Endpoint)))
	} else {
		lines = append(lines, fmt.Sprintf("data = {\"model\": %s}", strconv.Quote(model)), "with open(\"audio.wav\", \"rb\") as audio:", fmt.Sprintf("    response = requests.%s(base_url + %s, headers=headers, data=data, files={\"file\": audio})", strings.ToLower(request.Method), strconv.Quote(request.Endpoint)))
	}
	lines = append(lines, "response.raise_for_status()")
	if len(example.FollowUps) == 0 {
		switch definition.Response {
		case ResponseSSE:
			lines = append(lines, "for line in response.iter_lines():", "    if line:", "        print(line.decode())")
		case ResponseAudio:
			lines = append(lines, "open(\"result.wav\", \"wb\").write(response.content)")
		default:
			lines = append(lines, "print(response.json())")
		}
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "job = response.json()", "job_id = job[\"id\"]")
	poll := strings.ReplaceAll(example.FollowUps[0].Endpoint, "{id}", "{job_id}")
	content := strings.ReplaceAll(example.FollowUps[1].Endpoint, "{id}", "{job_id}")
	lines = append(lines,
		fmt.Sprintf("status = requests.get(base_url + f%s, headers=headers, params={\"model\": %s})", strconv.Quote(poll), strconv.Quote(model)),
		"status.raise_for_status()", "print(status.json())",
		fmt.Sprintf("content = requests.get(base_url + f%s, headers=headers, params={\"model\": %s})", strconv.Quote(content), strconv.Quote(model)),
		"content.raise_for_status()", "open(\"result.mp4\", \"wb\").write(content.content)",
	)
	return strings.Join(lines, "\n")
}

func shellToken(value string) string {
	if strings.HasPrefix(value, "@") || strings.IndexFunc(value, func(r rune) bool { return r == ' ' || r == '\t' || r == '\'' || r == '"' }) >= 0 {
		return strconv.Quote(value)
	}
	return value
}
