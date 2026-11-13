package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestExplicitProviderRevisionFailureReachesEvaluateAndCreate(t *testing.T) {
	f, _ := templateFailedRevisionFixture(t, true)
	if err := f.h.d.Store.DB().Create(&store.GPU{NodeID: f.node.ID, Index: 0, UUID: "GPU-revision", Name: "NVIDIA H100", Vendor: "nvidia", ComputeCapability: "9.0", ArchID: "sm_90", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	f.h.d.Online = func(uuid string) bool { return uuid == f.node.UUID }
	body := fmt.Sprintf(`{"name":"pinned-revision","model_id":0,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","node_uuid":%q,"model_source":{"provider":%q,"storage_id":%d},"quick_params":{},"placement":{}}`, f.template.ID, f.node.UUID, f.shared.Source, f.nfs.ID)
	status, raw := evaluateForFieldErrors(t, f.h, body)
	errs := fieldErrorsOf(t, raw)
	if status != http.StatusOK || string(raw["ok"]) != "false" || len(errs) != 1 || errs[0].Field != "model_id" || errs[0].Code != ErrorModelSourceUnavailable {
		t.Fatalf("evaluate silently changed the revision: status=%d ok=%s errors=%s", status, raw["ok"], raw["errors"])
	}
	rec := httptest.NewRecorder()
	f.h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	rows, err := f.h.d.Store.ListDeployments()
	if rec.Code != http.StatusBadRequest || err != nil || len(rows) != 0 {
		t.Fatalf("create accepted a different revision: status=%d body=%s deployments=%+v err=%v", rec.Code, rec.Body.String(), rows, err)
	}
}
