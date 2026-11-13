package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type accessDoctorStub struct {
	allowed bool
	err     error
	calls   int
}

func (d *accessDoctorStub) Diagnose(_ context.Context, _ string, allowed bool) (diagnostics.DoctorReport, error) {
	d.allowed = allowed
	d.calls++
	return diagnostics.DoctorReport{DoctorReport: proto.DoctorReport{Report: "partial"}, CheckedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), Summary: "unavailable", Checks: []diagnostics.DoctorCheck{{Stage: "authentication", Status: "unavailable", Reason: "authentication_not_checked", Vantage: "node"}}}, d.err
}

func TestNodeDoctorAccessReportWithoutOnlineRPC(t *testing.T) {
	d := &accessDoctorStub{}
	h := apiHandlers{d: Deps{NodeDoctor: d}}
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", "node")
	w := httptest.NewRecorder()
	h.nodeDoctor(w, req)
	if w.Code != 200 || d.calls != 1 || d.allowed {
		t.Fatalf("status=%d calls=%d allowed=%v", w.Code, d.calls, d.allowed)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 4 || body["access"] != nil || body["checks"] == nil || string(body["report"]) != `"partial"` {
		t.Fatalf("wire=%s", w.Body)
	}
	var report diagnostics.DoctorReport
	_ = json.Unmarshal(w.Body.Bytes(), &report)
	if report.Checks[0].Status != "unavailable" || report.Checks[0].ObservedAt != nil {
		t.Fatal("missing evidence acquired a verdict or timestamp")
	}
}

func TestNodeDoctorAccessMapsRequestErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{store.ErrNotFound, 404}, {diagnostics.ErrDoctorResponse, 502}, {errors.New("RPC failure"), 409}} {
		h := apiHandlers{d: Deps{NodeDoctor: &accessDoctorStub{err: tc.err}}}
		req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", "node")
		w := httptest.NewRecorder()
		h.nodeDoctor(w, req)
		if w.Code != tc.status {
			t.Fatalf("%v: status %d", tc.err, w.Code)
		}
	}
}

func TestNodeConnectionDiagnosticsDoesNotInvokeExtendedDoctor(t *testing.T) {
	st, _ := platformKeyAPIStore(t, true)
	node, _, err := st.CreateNodePending("compat", "server_dial")
	if err != nil {
		t.Fatal(err)
	}
	d := &accessDoctorStub{}
	h := apiHandlers{d: Deps{Store: st, NodeDoctor: d}}
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", node.UUID)
	w := httptest.NewRecorder()
	h.nodeConnectionDiagnostics(w, req)
	if w.Code != 200 || d.calls != 0 {
		t.Fatalf("status=%d extended calls=%d", w.Code, d.calls)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["access"] != nil {
		t.Fatal("legacy endpoint gained extended report")
	}
}
