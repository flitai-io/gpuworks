package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The model transfer endpoints reach the controller's transfers: each answers
// what the controller says of a node it does not know, not that model sync
// is not configured, as an API left without them would.
func TestTheAPIsModelTransfersAreTheControllers(t *testing.T) {
	_, handler, token := wiredApp(t)
	for _, path := range []string{
		"/api/v1/models/999/nodes/no-such-node/sync",
		"/api/v1/models/999/nodes/no-such-node/sync/probe",
		"/api/v1/models/999/nodes/no-such-node/sync/cancel",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"source_node":"no-such-source"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "not configured") || rec.Code != http.StatusConflict {
			t.Fatalf("POST %s = %d %s; want the controller's conflict", path, rec.Code, rec.Body)
		}
	}
}
