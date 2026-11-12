package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/deployment"
)

// Display names allow all languages and punctuation, but not control bytes
// or unbounded text. Required/default semantics remain with each endpoint.
func validateName(w http.ResponseWriter, name string) bool {
	return validateTextField(w, "name", name, deployment.MaxNameRunes)
}

// Node display names retain the store's 255-byte limit after trimming.
// Other resources use the character-based limit in validateName.
func validateNodeName(w http.ResponseWriter, name string) bool {
	detail := ""
	if !utf8.ValidString(name) || containsControl(name) {
		detail = "must contain valid text without control characters"
	} else if len(strings.TrimSpace(name)) > 255 {
		detail = "must contain at most 255 UTF-8 bytes after trimming whitespace"
	}
	if detail != "" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "name "+detail, map[string]any{"field": "name", "detail": detail})
		return false
	}
	return true
}

func validateTextField(w http.ResponseWriter, field, value string, maximum int) bool {
	if err := deployment.TextFieldError(field, value, maximum); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// decodeJSONBody bounds small management writes and rejects concatenated JSON
// values rather than silently applying only the first one.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dest any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dest); err != nil {
		writeJSONBodyError(w, err)
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeJSONBodyError(w, err)
		return false
	}
	return true
}

func writeJSONBodyError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var oversized *http.MaxBytesError
	if errors.As(err, &oversized) {
		status = http.StatusRequestEntityTooLarge
	}
	writeProductError(w, status, ErrorFieldInvalid, "request body must contain one valid JSON object within the size limit", map[string]any{"field": "body", "detail": "request body must contain one valid JSON object within the size limit"})
}
