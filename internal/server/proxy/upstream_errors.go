package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxUpstreamErrorBytes = 16 << 10
const maxUpstreamErrorText = 2048

var echoedInput = regexp.MustCompile(`(?i)\binput_value\s*=|\binput\s*:|["']input["']\s*:`)
var encodedPayload = regexp.MustCompile(`data:[^\s"'<>]*|[A-Za-z0-9+/=_-]{128,}`)

// Bound upstream failures before ReverseProxy copies them to the client. A
// successful SSE/binary response remains untouched. Error envelopes retain the
// HTTP status and useful message, never Pydantic's input/ctx payloads.
func boundUpstreamError(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	var raw []byte
	var err error
	if resp.ContentLength <= maxUpstreamErrorBytes && resp.Header.Get("Content-Encoding") == "" {
		raw, err = io.ReadAll(io.LimitReader(resp.Body, maxUpstreamErrorBytes+1))
	}
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	text := "The upstream request failed. Check the input and selected model."
	// Ordinary compact errors keep their original wire shape and useful codes.
	if len(raw) <= maxUpstreamErrorText && len(raw) > 0 && json.Valid(raw) && safeCompactError(raw) {
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	if len(raw) > maxUpstreamErrorBytes || resp.ContentLength > maxUpstreamErrorBytes {
		text = "The upstream error was too large to display. Check the input and selected model."
	} else if len(raw) > 0 {
		var payload struct {
			Error   json.RawMessage `json:"error"`
			Message string          `json:"message"`
			Detail  json.RawMessage `json:"detail"`
		}
		if json.Unmarshal(raw, &payload) == nil {
			message := payload.Message
			var nested struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(payload.Error, &nested) == nil && nested.Message != "" {
				message = nested.Message
			} else {
				var value string
				if json.Unmarshal(payload.Error, &value) == nil {
					message = value
				}
			}
			if message == "" {
				var value string
				if json.Unmarshal(payload.Detail, &value) == nil {
					message = value
				} else {
					var details []struct {
						Message string `json:"msg"`
					}
					if json.Unmarshal(payload.Detail, &details) == nil {
						for i, detail := range details {
							if i == 4 {
								break
							}
							if detail.Message != "" {
								if message != "" {
									message += "; "
								}
								message += detail.Message
							}
						}
					}
				}
			}
			if message != "" {
				text = boundedDiagnostic(message)
			}
		} else if utf8.Valid(raw) && !bytes.ContainsRune(raw, 0) && !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "html") && !strings.HasPrefix(strings.TrimSpace(string(raw)), "<") {
			text = boundedDiagnostic(string(raw))
		}
	}
	// The engine's own text, bounded, under the gateway's code for an
	// upstream refusal; its status stays the engine's.
	encoded := apiErrorBody(ErrorUpstreamRejected, map[string]any{"detail": text}, resp.StatusCode)
	if len(encoded) > 4096 {
		encoded = apiErrorBody(ErrorUpstreamRejected, map[string]any{"detail": "The upstream error was too large to display."}, resp.StatusCode)
	}
	resp.Body = io.NopCloser(bytes.NewReader(encoded))
	resp.ContentLength = int64(len(encoded))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
	for _, key := range []string{"Content-Encoding", "Transfer-Encoding", "Content-MD5", "ETag", "Trailer"} {
		resp.Header.Del(key)
	}
	resp.TransferEncoding = nil
	resp.Trailer = nil
	return nil
}

func boundedDiagnostic(raw string) string {
	if len(raw) > maxUpstreamErrorBytes {
		raw = raw[:maxUpstreamErrorBytes]
	}
	if location := echoedInput.FindStringIndex(raw); location != nil {
		raw = raw[:location[0]]
	}
	raw = encodedPayload.ReplaceAllString(raw, "…")
	if len(raw) > maxUpstreamErrorText {
		raw = raw[:maxUpstreamErrorText-3]
		for !utf8.ValidString(raw) {
			raw = raw[:len(raw)-1]
		}
		raw += "…"
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "The upstream rejected the input."
	}
	return raw
}

func safeCompactError(raw []byte) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var safe func(any) bool
	safe = func(value any) bool {
		switch typed := value.(type) {
		case string:
			return boundedDiagnostic(typed) == typed
		case []any:
			for _, item := range typed {
				if !safe(item) {
					return false
				}
			}
		case map[string]any:
			for key, item := range typed {
				switch key {
				case "input", "input_value", "ctx", "input_audio", "image_url":
					return false
				}
				if !safe(item) {
					return false
				}
			}
		}
		return true
	}
	return safe(value)
}
