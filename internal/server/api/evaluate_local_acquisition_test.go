package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The form first evaluates a registered model without a spec. Offering "local"
// as a public provider locks that value into the form; the next evaluation,
// with the chosen spec, then rejects its own previously offered candidate.
func TestEvaluateLocalModelThenTemplateAcquisition(t *testing.T) {
	h, templateID := evaluateFixtureWorld(t)
	st := h.d.Store
	model, err := st.UpsertModel(&store.Model{
		Source: "local", SourceID: "Acme/Deploy-FP8", Revision: "local",
		Name: "Registered weights", WeightBytes: 16 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.NodeByUUID("fixture-node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: node.ID, Path: "/models/registered",
		State: proto.MFPresent, SizeBytes: 16 << 30,
	}); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("local-acquisition-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer})
	evaluate := func(body string, want int) dto.EvaluateResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("evaluate: status=%d body=%s", rec.Code, rec.Body.String())
		}
		var out dto.EvaluateResponse
		if want == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		} else if !strings.Contains(rec.Body.String(), ErrorAcquisitionCandidateStale) {
			t.Fatalf("expected stale source rejection: %s", rec.Body.String())
		}
		return out
	}
	before := evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"fixture-node"}`, model.ID), http.StatusOK)
	if before.Acquisition == nil || len(before.Acquisition.Providers) != 0 {
		t.Fatalf("local registration is not a public download provider: %+v", before.Acquisition)
	}
	if len(before.Acquisition.Present) != 1 || !before.Acquisition.Present[0].OnNode {
		t.Fatalf("registered files must remain available: %+v", before.Acquisition)
	}
	body := fmt.Sprintf(`{"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","node_uuid":"fixture-node"`, model.ID, templateID)
	// Switching to manual placement evaluates once before a node is selected.
	// There are no Present candidates then, so the browser would select the
	// first public provider if the API mistakenly returned "local".
	manual := fmt.Sprintf(`{"model_id":%d,"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","placement_mode":"manual"}`, model.ID, templateID)
	noNode := evaluate(manual, http.StatusOK)
	if noNode.Acquisition == nil || len(noNode.Acquisition.Providers) != 0 {
		t.Fatalf("manual draft offered providers: %+v", noNode.Acquisition)
	}
	after := evaluate(body+"}", http.StatusOK)
	if !after.OK || after.Acquisition == nil || len(after.Acquisition.Providers) != 0 ||
		len(after.Acquisition.Present) != 1 || !after.Acquisition.Present[0].OnNode {
		t.Fatalf("template selection must reuse local files without offering a local download provider: %+v", after)
	}
	// Fix the candidates, not the validation: an actually stale explicit
	// public provider must still fail instead of silently selecting another.
	evaluate(body+`,"model_source":{"provider":"unknown"}}`, http.StatusBadRequest)
}
