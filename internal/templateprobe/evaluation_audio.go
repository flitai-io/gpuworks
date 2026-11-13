package templateprobe

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"unicode"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

func (e HTTPTaskEvaluator) evaluateASR(ctx context.Context, baseURL string, testCase L3Case) (TaskEvaluation, error) {
	audio, _, err := e.readMedia(testCase.AudioPath, "audio/wav")
	if err != nil {
		return TaskEvaluation{}, HarnessStageError{Stage: "evaluation_fixture", Err: err}
	}
	wavInfo, err := inspectWAV(audio)
	if err != nil {
		return TaskEvaluation{}, HarnessStageError{Stage: "evaluation_fixture", Err: err}
	}
	status, body, requestErr := e.transcribe(ctx, baseURL, testCase.Model, audio)
	result := baseTaskResult("POST", "/v1/audio/transcriptions", status, requestErr)
	result.SmokeTest["fixture"] = wavInfo
	if requestErr != "" {
		return result, nil
	}
	text, parseErr := transcriptionText(body)
	if parseErr != nil {
		result.Warning = "transcription response cannot be parsed: " + parseErr.Error()
		return result, nil //nolint:nilerr // Invalid transcription output is recorded in the task warning.
	}
	applyWER(&result, testCase.ExpectedTranscript, text, testCase.MaxWER)
	return result, nil
}

func (e HTTPTaskEvaluator) evaluateTTS(ctx context.Context, baseURL string, testCase L3Case) (TaskEvaluation, error) {
	payloadBytes, composeErr := func() (json.RawMessage, error) {
		raw, err := json.Marshal(testCase.RequestOverrides)
		if err != nil {
			return nil, err
		}
		return inferenceapi.ComposeRequest(inferenceapi.Operation{Type: inferenceapi.OperationAudioSpeech}, testCase.Model, map[string]any{"input": testCase.SpeechText}, nil, raw)
	}()
	if composeErr != nil {
		return TaskEvaluation{}, HarnessStageError{Stage: "request_composition", Err: composeErr}
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return TaskEvaluation{}, HarnessStageError{Stage: "request_composition", Err: err}
	}
	status, audio, requestErr := e.postJSON(ctx, baseURL+"/v1/audio/speech", payload)
	result := baseTaskResult("POST", "/v1/audio/speech", status, requestErr)
	if requestErr != "" {
		return result, nil
	}
	wavInfo, err := inspectWAV(audio)
	if err != nil {
		result.Warning = "speech response is not a valid WAV: " + err.Error()
		return result, nil //nolint:nilerr // Invalid WAV output is recorded in the task warning.
	}
	result.Verification["wav"] = wavInfo
	asrBase, err := normalizeHTTPBaseURL(testCase.ASRBaseURL)
	if err != nil {
		return TaskEvaluation{}, HarnessStageError{Stage: "evaluation_endpoint", Err: err}
	}
	asrStatus, asrBody, asrRequestErr := e.transcribe(ctx, asrBase, testCase.ASRModel, audio)
	result.Verification["asr_backread_http_status"] = asrStatus
	if asrRequestErr != "" {
		result.Warning = "ASR backread did not complete: " + asrRequestErr
		return result, nil
	}
	text, parseErr := transcriptionText(asrBody)
	if parseErr != nil {
		result.Warning = "ASR backread response cannot be parsed: " + parseErr.Error()
		return result, nil //nolint:nilerr // Invalid ASR output is recorded in the task warning.
	}
	applyWER(&result, testCase.ExpectedTranscript, text, testCase.MaxWER)
	return result, nil
}

func (e HTTPTaskEvaluator) transcribe(ctx context.Context, baseURL, model string, audio []byte) (int, []byte, string) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	file, err := writer.CreateFormFile("file", "fixture.wav")
	if err != nil {
		return 0, nil, "create multipart file: " + err.Error()
	}
	if _, err := file.Write(audio); err != nil {
		return 0, nil, "write multipart file: " + err.Error()
	}
	if err := writer.WriteField("model", model); err != nil {
		return 0, nil, "write multipart model: " + err.Error()
	}
	if err := writer.Close(); err != nil {
		return 0, nil, "close multipart body: " + err.Error()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/audio/transcriptions", body)
	if err != nil {
		return 0, nil, "build transcription request: " + err.Error()
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return e.do(request)
}

func transcriptionText(body []byte) (string, error) {
	var response struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", err
	}
	if strings.TrimSpace(response.Text) == "" {
		return "", errors.New("transcription text is empty")
	}
	return response.Text, nil
}

func applyWER(result *TaskEvaluation, expected, actual string, maximum float64) {
	wer := wordErrorRate(expected, actual)
	result.Verification["word_error_rate"] = wer
	result.Verification["maximum_word_error_rate"] = maximum
	result.Verification["transcription_nonempty"] = strings.TrimSpace(actual) != ""
	if wer > maximum {
		result.Warning = fmt.Sprintf("word error rate %.6f exceeds %.6f", wer, maximum)
		return
	}
	result.SemanticPassed = true
}

func inspectWAV(body []byte) (map[string]any, error) {
	if len(body) < 44 || string(body[:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		return nil, errors.New("missing RIFF/WAVE header")
	}
	declared := int(binary.LittleEndian.Uint32(body[4:8])) + 8
	if declared > len(body) {
		return nil, fmt.Errorf("declared RIFF size %d exceeds body size %d", declared, len(body))
	}
	var format, channels, bits uint16
	var sampleRate uint32
	dataBytes := 0
	for offset := 12; offset+8 <= len(body); {
		kind := string(body[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(body[offset+4 : offset+8]))
		start, end := offset+8, offset+8+size
		if size < 0 || end > len(body) {
			return nil, fmt.Errorf("invalid WAV chunk %q size", kind)
		}
		switch kind {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("WAV fmt chunk is too short")
			}
			format = binary.LittleEndian.Uint16(body[start : start+2])
			channels = binary.LittleEndian.Uint16(body[start+2 : start+4])
			sampleRate = binary.LittleEndian.Uint32(body[start+4 : start+8])
			bits = binary.LittleEndian.Uint16(body[start+14 : start+16])
		case "data":
			dataBytes += size
		}
		offset = end + size%2
	}
	if format == 0 || channels == 0 || sampleRate == 0 || bits == 0 || dataBytes == 0 {
		return nil, errors.New("WAV is missing a valid fmt or nonempty data chunk")
	}
	return map[string]any{
		"bytes": len(body), "audio_format": int(format), "channels": int(channels),
		"sample_rate": int(sampleRate), "bits_per_sample": int(bits), "data_bytes": dataBytes,
	}, nil
}

func wordErrorRate(expected, actual string) float64 {
	want, got := normalizedWords(expected), normalizedWords(actual)
	if len(want) == 0 {
		if len(got) == 0 {
			return 0
		}
		return 1
	}
	previous := make([]int, len(got)+1)
	for index := range previous {
		previous[index] = index
	}
	for wantIndex, wantWord := range want {
		current := make([]int, len(got)+1)
		current[0] = wantIndex + 1
		for gotIndex, gotWord := range got {
			cost := 1
			if wantWord == gotWord {
				cost = 0
			}
			current[gotIndex+1] = minInt(
				previous[gotIndex+1]+1,
				current[gotIndex]+1,
				previous[gotIndex]+cost,
			)
		}
		previous = current
	}
	return float64(previous[len(got)]) / float64(len(want))
}

func normalizedWords(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func minInt(values ...int) int {
	minimum := values[0]
	for _, value := range values[1:] {
		if value < minimum {
			minimum = value
		}
	}
	return minimum
}
