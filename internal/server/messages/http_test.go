package messages

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestGenericErrorCodeByStatus(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusBadRequest:                   "BAD_REQUEST",
		http.StatusUnauthorized:                 "UNAUTHORIZED",
		http.StatusForbidden:                    "FORBIDDEN",
		http.StatusNotFound:                     "NOT_FOUND",
		http.StatusMethodNotAllowed:             "METHOD_NOT_ALLOWED",
		http.StatusRequestTimeout:               "REQUEST_TIMEOUT",
		http.StatusConflict:                     "CONFLICT",
		http.StatusGone:                         "GONE",
		http.StatusRequestEntityTooLarge:        "REQUEST_TOO_LARGE",
		http.StatusUnsupportedMediaType:         "UNSUPPORTED_MEDIA_TYPE",
		http.StatusRequestedRangeNotSatisfiable: "RANGE_NOT_SATISFIABLE",
		http.StatusUnprocessableEntity:          "UNPROCESSABLE",
		http.StatusFailedDependency:             "FAILED_DEPENDENCY",
		http.StatusTooManyRequests:              "TOO_MANY_REQUESTS",
		http.StatusInternalServerError:          "INTERNAL_ERROR",
		http.StatusNotImplemented:               "NOT_IMPLEMENTED",
		http.StatusBadGateway:                   "BAD_GATEWAY",
		http.StatusServiceUnavailable:           "SERVICE_UNAVAILABLE",
		http.StatusGatewayTimeout:               "GATEWAY_TIMEOUT",
		http.StatusInsufficientStorage:          "INSUFFICIENT_STORAGE",
		// A status without a code of its own falls into its class.
		http.StatusTeapot:                  "BAD_REQUEST",
		http.StatusPreconditionFailed:      "BAD_REQUEST",
		http.StatusHTTPVersionNotSupported: "INTERNAL_ERROR",
	} {
		if got := GenericErrorCode(status); got != want {
			t.Errorf("GenericErrorCode(%d) = %q, want %q", status, got, want)
		}
	}
	if len(GenericErrorCodes()) != 20 {
		t.Fatalf("GenericErrorCodes() = %v, want the 20 status codes", GenericErrorCodes())
	}
	if !IsGenericErrorCode("NOT_FOUND") || IsGenericErrorCode("MOUNT_TARGET_DUPLICATE") {
		t.Fatal("IsGenericErrorCode does not tell generic from specific codes")
	}
}

func TestHTTPErrorCodeKeepsAProductErrorsCode(t *testing.T) {
	product := NewProductError("MOUNT_TARGET_DUPLICATE", "two mounts", map[string]any{"path": "/data"})
	code, params := HTTPErrorCode(http.StatusBadRequest, fmt.Errorf("create: %w", product))
	if code != "MOUNT_TARGET_DUPLICATE" || params["path"] != "/data" {
		t.Fatalf("wrapped product error = %q %v", code, params)
	}
	code, params = HTTPErrorCode(http.StatusNotFound, errors.New("deployment 7 not found"))
	if code != "NOT_FOUND" || params != nil {
		t.Fatalf("plain error = %q %v, want NOT_FOUND without params", code, params)
	}
	// A product error with an empty code is no code at all.
	code, _ = HTTPErrorCode(http.StatusConflict, NewProductError("", "x", nil))
	if code != "CONFLICT" {
		t.Fatalf("empty product code = %q, want CONFLICT", code)
	}
}

func TestWriteHTTPErrorWireShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteHTTPError(rec, http.StatusBadRequest, errors.New("unknown lane"))
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/json" ||
		rec.Body.String() != `{"code":"BAD_REQUEST","error":"unknown lane"}`+"\n" {
		t.Fatalf("plain error = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	rec = httptest.NewRecorder()
	WriteHTTPError(rec, http.StatusConflict, NewProductError("PEER_RELAY_BUSY", "busy", map[string]any{"n": 2}))
	if rec.Body.String() != `{"code":"PEER_RELAY_BUSY","error":"busy","params":{"n":2}}`+"\n" {
		t.Fatalf("product error = %q", rec.Body.String())
	}
}

// TestGenericErrorCodesHaveClientText: the console renders a generic code as
// its category in the reader's language beside the English detail, so every
// generic code needs en and zh text and the console's list of generic codes
// must be this one.
func TestGenericErrorCodesHaveClientText(t *testing.T) {
	root := filepath.Join("..", "..", "..", "ui", "src")
	source, err := os.ReadFile(filepath.Join(root, "apiError.ts"))
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)GENERIC_API_ERROR_CODES = \[(.*?)\]`).FindSubmatch(source)
	if block == nil {
		t.Fatal("ui/src/apiError.ts declares no GENERIC_API_ERROR_CODES")
	}
	var ui []string
	for _, m := range regexp.MustCompile(`"([A-Z_]+)"`).FindAllSubmatch(block[1], -1) {
		ui = append(ui, string(m[1]))
	}
	server := GenericErrorCodes()
	slices.Sort(ui)
	slices.Sort(server)
	if !slices.Equal(ui, server) {
		t.Fatalf("console generic codes %v, server %v", ui, server)
	}
	for _, locale := range []string{"en", "zh"} {
		raw, err := os.ReadFile(filepath.Join(root, "i18n", "locales", locale, "common.json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			APIError       map[string]string `json:"apiError"`
			APIErrorDetail string            `json:"apiErrorDetail"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(doc.APIErrorDetail, "{{category}}") || !strings.Contains(doc.APIErrorDetail, "{{detail}}") {
			t.Errorf("%s apiErrorDetail = %q, want {{category}} and {{detail}}", locale, doc.APIErrorDetail)
		}
		for _, code := range server {
			if strings.TrimSpace(doc.APIError[code]) == "" {
				t.Errorf("%s common.json has no apiError.%s", locale, code)
			}
		}
	}
}
