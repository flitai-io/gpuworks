package templateprobe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	defaultMaxMediaBytes    = int64(32 << 20)
	defaultMaxResponseBytes = int64(16 << 20)
)

type HTTPTaskEvaluator struct {
	Client           *http.Client
	APIKey           string
	MaxMediaBytes    int64
	MaxResponseBytes int64
}

func ValidateL3Case(testCase L3Case) error {
	if strings.TrimSpace(testCase.CaseID) == "" {
		return errors.New("L3 case_id is required")
	}
	switch testCase.Task {
	case L3TaskText:
		if strings.TrimSpace(testCase.Prompt) == "" || len(testCase.ExpectedKeywords) == 0 {
			return errors.New("text case requires prompt and expected_keywords")
		}
	case L3TaskEmbedding:
		if len(testCase.EmbeddingInputs) != 3 {
			return errors.New("embedding case requires exactly three inputs: anchor, positive, negative")
		}
		if testCase.ExpectedDimensions < 0 || testCase.MinCosineMargin < 0 {
			return errors.New("embedding dimensions and cosine margin cannot be negative")
		}
	case L3TaskASR:
		if strings.TrimSpace(testCase.AudioPath) == "" || strings.TrimSpace(testCase.ExpectedTranscript) == "" {
			return errors.New("ASR case requires audio_path and expected_transcript")
		}
		if testCase.MaxWER < 0 || testCase.MaxWER > 1 {
			return errors.New("max_wer must be between 0 and 1")
		}
	case L3TaskTTS:
		if strings.TrimSpace(testCase.SpeechText) == "" || strings.TrimSpace(testCase.ExpectedTranscript) == "" ||
			strings.TrimSpace(testCase.ASRBaseURL) == "" || strings.TrimSpace(testCase.ASRModel) == "" {
			return errors.New("TTS case requires speech_text, expected_transcript, asr_base_url, and asr_model")
		}
		if testCase.MaxWER < 0 || testCase.MaxWER > 1 {
			return errors.New("max_wer must be between 0 and 1")
		}
	case L3TaskVisionLanguage:
		if strings.TrimSpace(testCase.ImagePath) == "" || strings.TrimSpace(testCase.Prompt) == "" || len(testCase.ExpectedKeywords) == 0 {
			return errors.New("vision_language case requires image_path, prompt, and expected_keywords")
		}
	default:
		return fmt.Errorf("unsupported L3 task %q", testCase.Task)
	}
	return nil
}

func (e HTTPTaskEvaluator) Evaluate(ctx context.Context, observation DeploymentObservation, testCase L3Case) (TaskEvaluation, error) {
	if err := ValidateL3Case(testCase); err != nil {
		return TaskEvaluation{}, HarnessStageError{Stage: "evaluation_case", Err: err}
	}
	baseURL, err := normalizeHTTPBaseURL(observation.BaseURL)
	if err != nil {
		return TaskEvaluation{}, HarnessStageError{Stage: "evaluation_endpoint", Err: err}
	}
	if strings.TrimSpace(testCase.Model) == "" {
		testCase.Model = observation.Model
	}
	if strings.TrimSpace(testCase.Model) == "" {
		return TaskEvaluation{}, HarnessStageError{Stage: "evaluation_case", Err: errors.New("served model is empty")}
	}
	switch testCase.Task {
	case L3TaskText:
		return e.evaluateText(ctx, baseURL, testCase)
	case L3TaskEmbedding:
		return e.evaluateEmbedding(ctx, baseURL, testCase)
	case L3TaskASR:
		return e.evaluateASR(ctx, baseURL, testCase)
	case L3TaskTTS:
		return e.evaluateTTS(ctx, baseURL, testCase)
	case L3TaskVisionLanguage:
		return e.evaluateVisionLanguage(ctx, baseURL, testCase)
	default:
		return TaskEvaluation{}, HarnessStageError{Stage: "evaluation_case", Err: fmt.Errorf("unsupported task %q", testCase.Task)}
	}
}

func (e HTTPTaskEvaluator) evaluateText(ctx context.Context, baseURL string, testCase L3Case) (TaskEvaluation, error) {
	maxTokens := testCase.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64
	}
	payload := map[string]any{
		"model": testCase.Model, "temperature": 0, "max_tokens": maxTokens,
		"messages": []any{map[string]any{"role": "user", "content": testCase.Prompt}},
	}
	status, body, requestErr := e.postJSON(ctx, baseURL+"/v1/chat/completions", payload)
	result := baseTaskResult("POST", "/v1/chat/completions", status, requestErr)
	if requestErr != "" {
		return result, nil
	}
	content, parseErr := assistantContent(body)
	passed, warning, verification := validateKeywordOutput(content, parseErr, testCase)
	result.SemanticPassed, result.Warning, result.Verification = passed, warning, verification
	return result, nil
}

func (e HTTPTaskEvaluator) evaluateVisionLanguage(ctx context.Context, baseURL string, testCase L3Case) (TaskEvaluation, error) {
	imageBody, mediaType, err := e.readMedia(testCase.ImagePath, testCase.ImageMIME)
	if err != nil {
		return TaskEvaluation{}, HarnessStageError{Stage: "evaluation_fixture", Err: err}
	}
	if !strings.HasPrefix(mediaType, "image/") {
		return TaskEvaluation{}, HarnessStageError{Stage: "evaluation_fixture", Err: fmt.Errorf("vision fixture MIME %q is not an image", mediaType)}
	}
	maxTokens := testCase.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64
	}
	dataURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(imageBody)
	payload := map[string]any{
		"model": testCase.Model, "temperature": 0, "max_tokens": maxTokens,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]string{"url": dataURL}},
			map[string]any{"type": "text", "text": testCase.Prompt},
		}}},
	}
	status, body, requestErr := e.postJSON(ctx, baseURL+"/v1/chat/completions", payload)
	result := baseTaskResult("POST", "/v1/chat/completions", status, requestErr)
	result.SmokeTest["fixture"] = map[string]any{"media_type": mediaType, "bytes": len(imageBody), "transport": "data_url"}
	if requestErr != "" {
		return result, nil
	}
	content, parseErr := assistantContent(body)
	passed, warning, verification := validateKeywordOutput(content, parseErr, testCase)
	result.SemanticPassed, result.Warning, result.Verification = passed, warning, verification
	return result, nil
}

func (e HTTPTaskEvaluator) evaluateEmbedding(ctx context.Context, baseURL string, testCase L3Case) (TaskEvaluation, error) {
	payload := map[string]any{"model": testCase.Model, "input": testCase.EmbeddingInputs, "encoding_format": "float"}
	status, body, requestErr := e.postJSON(ctx, baseURL+"/v1/embeddings", payload)
	result := baseTaskResult("POST", "/v1/embeddings", status, requestErr)
	if requestErr != "" {
		return result, nil
	}
	var response struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		result.Warning = "embedding response is not valid JSON: " + err.Error()
		return result, nil //nolint:nilerr // Invalid embedding output is recorded in the task warning, not a harness error.
	}
	if len(response.Data) != 3 {
		result.Warning = fmt.Sprintf("embedding response has %d vectors, want 3", len(response.Data))
		return result, nil
	}
	vectors := [][]float64{response.Data[0].Embedding, response.Data[1].Embedding, response.Data[2].Embedding}
	dimension := len(vectors[0])
	verification := map[string]any{"embedding_dimensions": dimension}
	result.Verification = verification
	if dimension == 0 || (testCase.ExpectedDimensions > 0 && dimension != testCase.ExpectedDimensions) {
		result.Warning = fmt.Sprintf("embedding dimension is %d, expected %d", dimension, testCase.ExpectedDimensions)
		return result, nil
	}
	for _, vector := range vectors {
		if len(vector) != dimension || !finiteNonzeroVector(vector) {
			result.Warning = "embedding vectors must have identical dimensions and finite nonzero values"
			return result, nil
		}
	}
	positive := cosine(vectors[0], vectors[1])
	negative := cosine(vectors[0], vectors[2])
	margin := positive - negative
	minimum := testCase.MinCosineMargin
	if minimum == 0 {
		minimum = 0.02
	}
	verification["positive_cosine"] = positive
	verification["negative_cosine"] = negative
	verification["cosine_margin"] = margin
	verification["minimum_cosine_margin"] = minimum
	if margin < minimum {
		result.Warning = fmt.Sprintf("embedding cosine margin %.6f is below %.6f", margin, minimum)
		return result, nil
	}
	result.SemanticPassed = true
	return result, nil
}

func (e HTTPTaskEvaluator) postJSON(ctx context.Context, endpoint string, payload any) (int, []byte, string) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, "encode request: " + err.Error()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, "build request: " + err.Error()
	}
	request.Header.Set("Content-Type", "application/json")
	return e.do(request)
}

func (e HTTPTaskEvaluator) do(request *http.Request) (int, []byte, string) {
	if strings.TrimSpace(e.APIKey) != "" {
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(e.APIKey))
	}
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err.Error()
	}
	defer response.Body.Close()
	limit := e.MaxResponseBytes
	if limit <= 0 {
		limit = defaultMaxResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return response.StatusCode, nil, "read response: " + err.Error()
	}
	if int64(len(body)) > limit {
		return response.StatusCode, nil, fmt.Sprintf("response exceeds %d bytes", limit)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, body, fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	return response.StatusCode, body, ""
}

func (e HTTPTaskEvaluator) readMedia(path, explicitMIME string) ([]byte, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	limit := e.MaxMediaBytes
	if limit <= 0 {
		limit = defaultMaxMediaBytes
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > limit {
		return nil, "", fmt.Errorf("media fixture exceeds %d bytes", limit)
	}
	mediaType := strings.TrimSpace(explicitMIME)
	if mediaType == "" {
		mediaType = mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	}
	if mediaType == "" {
		mediaType = http.DetectContentType(body)
	}
	if split, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = split
	}
	return body, mediaType, nil
}

func baseTaskResult(method, path string, status int, requestErr string) TaskEvaluation {
	return TaskEvaluation{
		RequestCompleted: requestErr == "" && status >= 200 && status < 300,
		HTTPStatus:       status, RequestError: requestErr,
		SmokeTest:    map[string]any{"method": method, "path": path, "expected": "HTTP 2xx plus semantic assertion"},
		Verification: map[string]any{},
	}
}

func assistantContent(body []byte) (string, error) {
	var response struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", err
	}
	if len(response.Choices) == 0 {
		return "", errors.New("response has no choices")
	}
	var content string
	if err := json.Unmarshal(response.Choices[0].Message.Content, &content); err != nil {
		return "", errors.New("assistant content is not a string")
	}
	return content, nil
}

func validateKeywordOutput(content string, parseErr error, testCase L3Case) (bool, string, map[string]any) {
	verification := map[string]any{}
	if parseErr != nil {
		return false, "chat response cannot be parsed: " + parseErr.Error(), verification
	}
	trimmed := strings.TrimSpace(content)
	verification["output_nonempty"] = trimmed != ""
	verification["output_runes"] = utf8.RuneCountInString(trimmed)
	if trimmed == "" {
		return false, "assistant output is empty", verification
	}
	maximum := testCase.MaxRepeatedRun
	if maximum <= 0 {
		maximum = 8
	}
	repeated := longestRepeatedRun(trimmed)
	verification["longest_repeated_run"] = repeated
	verification["max_repeated_run"] = maximum
	if repeated > maximum {
		return false, fmt.Sprintf("assistant output contains a repeated run of %d characters", repeated), verification
	}
	normalized := normalizeComparable(trimmed)
	for _, keyword := range testCase.ExpectedKeywords {
		want := normalizeComparable(keyword)
		if want == "" || !strings.Contains(normalized, want) {
			verification["missing_keyword"] = keyword
			return false, fmt.Sprintf("assistant output does not contain expected keyword %q", keyword), verification
		}
	}
	verification["keywords_matched"] = len(testCase.ExpectedKeywords)
	return true, "", verification
}

func longestRepeatedRun(value string) int {
	longest, current := 0, 0
	var previous rune
	for _, r := range value {
		if unicode.IsSpace(r) {
			current = 0
			previous = 0
			continue
		}
		if r == previous {
			current++
		} else {
			previous, current = r, 1
		}
		if current > longest {
			longest = current
		}
	}
	return longest
}

func normalizeComparable(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, value)
}

func finiteNonzeroVector(vector []float64) bool {
	norm := float64(0)
	for _, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
		norm += value * value
	}
	return norm > 0
}

func cosine(left, right []float64) float64 {
	dot, leftNorm, rightNorm := float64(0), float64(0), float64(0)
	for index := range left {
		dot += left[index] * right[index]
		leftNorm += left[index] * left[index]
		rightNorm += right[index] * right[index]
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

func normalizeHTTPBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("invalid HTTP base URL %q", raw)
	}
	if parsed.User != nil {
		return "", errors.New("HTTP base URL must not contain credentials")
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	return strings.TrimRight(parsed.String(), "/"), nil
}
