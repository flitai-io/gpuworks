package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

var accessLogRequestID = regexp.MustCompile(`^r[0-9]+$`)

// TestGatewayRefusalsBeforeForwardingNameTheRequest drives the server's real
// handler chain: every /v1 answer the gateway gives without reaching an
// engine carries the access log's X-Request-ID and a coded error body.
func TestGatewayRefusalsBeforeForwardingNameTheRequest(t *testing.T) {
	application, _ := newTestApp(t, "gpuworks-test-password-2026")
	key, err := application.store.CreateAPIKey("gateway-errors", 0)
	if err != nil {
		t.Fatal(err)
	}
	type errorBody struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	for _, tc := range []struct {
		name, method, path, key, body string
		status                        int
		code, typ                     string
		functions                     []string
	}{
		{name: "no key", method: http.MethodPost, path: "/v1/chat/completions", body: `{"model":"m"}`, status: 401, code: "INVALID_API_KEY", typ: "authentication_error"},
		{name: "unknown key", method: http.MethodPost, path: "/v1/chat/completions", key: "gw-not-a-key", body: `{"model":"m"}`, status: 401, code: "INVALID_API_KEY", typ: "authentication_error"},
		{name: "models without key", method: http.MethodGet, path: "/v1/models", status: 401, code: "INVALID_API_KEY", typ: "authentication_error"},
		{name: "no model", method: http.MethodPost, path: "/v1/chat/completions", key: key, body: `{}`, status: 400, code: "MODEL_REQUIRED", typ: "invalid_request_error"},
		{name: "unknown model", method: http.MethodPost, path: "/v1/chat/completions", key: key, body: `{"model":"nope"}`, status: 404, code: "MODEL_NOT_FOUND", typ: "not_found_error"},
		{name: "tokenize method", method: http.MethodGet, path: "/v1/tokenize", key: key, status: 405, code: "METHOD_NOT_ALLOWED", typ: "invalid_request_error"},
		{name: "batches without key", method: http.MethodGet, path: "/v1/batches", status: 401, code: "invalid_api_key", typ: "invalid_request_error"},
		{name: "gateway path on a console-only address", method: http.MethodPost, path: "/v1/chat/completions", key: key, body: `{"model":"m"}`, status: 404, code: "ROUTE_NOT_FOUND", typ: "not_found_error", functions: []string{serverconfig.FunctionConsole}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.key != "" {
				req.Header.Set("Authorization", "Bearer "+tc.key)
			}
			if tc.functions != nil {
				req = req.WithContext(context.WithValue(req.Context(), listenerFunctionsKey{}, tc.functions))
			}
			rec := httptest.NewRecorder()
			application.http.Handler.ServeHTTP(rec, req)
			var body errorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
			if rec.Code != tc.status || body.Error.Code != tc.code || body.Error.Type != tc.typ || body.Error.Message == "" {
				t.Fatalf("= %d %s, want %d %s %s", rec.Code, rec.Body.String(), tc.status, tc.code, tc.typ)
			}
			if id := rec.Header().Get("X-Request-ID"); !accessLogRequestID.MatchString(id) {
				t.Fatalf("X-Request-ID = %q, want the access log's id", id)
			}
		})
	}
}

// TestManagementErrorsOutsideHandlersHaveCodes: the router's 404/405 and an
// API path on an address that does not serve the console answer in the
// management API's coded shape.
func TestManagementErrorsOutsideHandlersHaveCodes(t *testing.T) {
	application, _ := newTestApp(t, "gpuworks-test-password-2026")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req = req.WithContext(context.WithValue(req.Context(), listenerFunctionsKey{}, []string{serverconfig.FunctionGateway}))
	rec := httptest.NewRecorder()
	application.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"code":"NOT_FOUND","error":"/api/v1/nodes is not served on this address"}`+"\n" {
		t.Fatalf("console path on a gateway address = %d %s", rec.Code, rec.Body.String())
	}
	// A download path keeps its plain-text 404.
	req = httptest.NewRequest(http.MethodGet, "/dl/gpuworks-agent-linux-amd64", nil)
	req = req.WithContext(context.WithValue(req.Context(), listenerFunctionsKey{}, []string{serverconfig.FunctionGateway}))
	rec = httptest.NewRecorder()
	application.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || strings.HasPrefix(rec.Body.String(), "{") {
		t.Fatalf("download path on a gateway address = %d %s", rec.Code, rec.Body.String())
	}
}
