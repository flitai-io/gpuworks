package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A use case's refusal is answered with the status its kind maps to and the
// wire body writeErr gives any error: a served name in use is a 409 with its
// code, a store failure a 500.
func TestUseCaseRefusalsMapOntoStatuses(t *testing.T) {
	inUse := deployment.CreateRowError(&store.ServedNameInUseError{ServedName: "taken", HolderID: 3, HolderName: "first"})
	for _, tc := range []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{"conflict", inUse, http.StatusConflict, `"code":"SERVED_NAME_IN_USE"`},
		{"store failure", deployment.CreateRowError(errors.New("disk I/O error")), http.StatusInternalServerError, `"error":"disk I/O error"`},
		{"no kind", errors.New("plain"), http.StatusInternalServerError, `"error":"plain"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeUseCaseErr(rec, tc.err)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("%d %s, want %d with %s", rec.Code, rec.Body, tc.status, tc.body)
			}
		})
	}
}
