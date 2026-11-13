package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestWriteErrorPreservesAdmissionDiagnosticsAndCause(t *testing.T) {
	cause := errors.New("source test cause")
	report := &dto.DiagnosticReport{Checks: []dto.DiagnosticCheck{{
		ID: "source:7", Status: "failed", Message: dto.ProductMessage{
			Code: "MODEL_SOURCE_UNAVAILABLE", Params: map[string]any{"node_id": 7, "reason": "model_not_on_node"}, Message: "No complete model record on node",
		},
	}}}
	err := deployment.WithDiagnostics(messages.WrapProductError("MODEL_SOURCE_UNAVAILABLE", "Model unavailable", map[string]any{"model_id": 42}, cause), report)
	err = fmt.Errorf("admission: %w", err)
	if !errors.Is(err, cause) {
		t.Fatal("diagnostics replaced the underlying cause")
	}
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusBadRequest, err)
	var body struct {
		Code        string                `json:"code"`
		Params      map[string]any        `json:"params"`
		Diagnostics *dto.DiagnosticReport `json:"diagnostics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 400 || body.Code != "MODEL_SOURCE_UNAVAILABLE" || body.Params["model_id"] != float64(42) {
		t.Fatalf("legacy error contract changed: %d %s", rec.Code, rec.Body.String())
	}
	if body.Diagnostics == nil || len(body.Diagnostics.Checks) != 1 || body.Diagnostics.Checks[0].ID != "source:7" {
		t.Fatalf("admission explanation lost: %s", rec.Body.String())
	}
}

type evaluationLogRecorder struct {
	*httptest.ResponseRecorder
	code    string
	params  map[string]any
	ok      *bool
	skipped bool
}

func (w *evaluationLogRecorder) NoteProductError(code string, params map[string]any) {
	w.code, w.params = code, params
}

func (w *evaluationLogRecorder) NoteEvaluationOutcome(ok, skipped bool) {
	w.ok, w.skipped = &ok, skipped
}

func TestEvaluateFailureCarriesRequestIDAndRecordsBusinessReason(t *testing.T) {
	w := &evaluationLogRecorder{ResponseRecorder: httptest.NewRecorder()}
	w.Header().Set("X-Request-ID", "r123")
	resp := &dto.EvaluateResponse{OK: false, Errors: []dto.FieldError{
		{Field: "model_id", Code: "MODEL_SOURCE_UNAVAILABLE", Params: map[string]any{"model_id": 42}},
		{Field: "model_source", Code: "STORAGE_UNAVAILABLE_ON_NODE", Params: map[string]any{"storage_id": 3}},
	}}
	writeJSON(w, http.StatusOK, resp)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || body["ok"] != false || body["request_id"] != "r123" {
		t.Fatalf("evaluation correlation missing: %d %s", w.Code, w.Body.String())
	}
	if w.code != "MODEL_SOURCE_UNAVAILABLE" || w.params["error_count"] != 2 {
		t.Fatalf("business failure not recorded: code=%q params=%v", w.code, w.params)
	}
	if w.ok == nil || *w.ok || w.skipped {
		t.Fatalf("evaluation outcome missing: ok=%v skipped=%v", w.ok, w.skipped)
	}
}
