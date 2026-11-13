package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNodeLabelsAPIContract(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("labels", store.HashToken("labels"), "worker"); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	for _, tc := range []struct {
		name, uuid, body string
		status           int
	}{
		{"write", "labels", `{"labels":{"pool":"inference","resource_group":"gpu"}}`, 200},
		{"invalid key", "labels", `{"labels":{"a b":"c"}}`, 400},
		{"reserved key", "labels", `{"labels":{"gpuworks.ai/pool":"a"}}`, 400},
		{"reserved subdomain", "labels", `{"labels":{"rack.gpuworks.ai/row":"7"}}`, 400},
		{"wrong value", "labels", `{"labels":{"a":3}}`, 400},
		{"missing labels", "labels", `{}`, 400},
		{"null labels", "labels", `{"labels":null}`, 400},
		{"missing node", "absent", `{"labels":{}}`, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/v1/nodes/"+tc.uuid+"/labels", bytes.NewBufferString(tc.body))
			req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"uuid": tc.uuid}))
			rec := httptest.NewRecorder()
			h.updateNodeLabels(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if tc.status == 200 {
				var got map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				var labels map[string]string
				_ = json.Unmarshal(got["labels"], &labels)
				if !reflect.DeepEqual(labels, map[string]string{"pool": "inference", "resource_group": "gpu"}) {
					t.Fatalf("labels=%v", labels)
				}
				if _, ok := got["resource_group"]; ok {
					t.Fatal("resource_group leaked as a top-level field")
				}
				if _, ok := got["token"]; ok {
					t.Fatal("node token leaked")
				}
			}
		})
	}
	// A node inside the requirements is accepted: evaluate reports no
	// requirements error for it.
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", bytes.NewBufferString(`{"node_uuid":"labels","placement_mode":"manual","node_requirements":{"pool":["inference"]}}`)))
	var accepted struct {
		Errors []dto.FieldError `json:"errors"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &accepted) != nil {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
	}
	for _, fieldErr := range accepted.Errors {
		if fieldErr.Field == "node_requirements" {
			t.Fatalf("a node inside the requirements was refused: %+v", fieldErr)
		}
	}
	// Create keeps the 400 for a node the requirements exclude; evaluate
	// reports the same judgement as a field error with ok=false, so the form
	// keeps the draft while it shows what is wrong.
	rec = httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", bytes.NewBufferString(`{"node_uuid":"labels","node_requirements":{"pool":["wrong"]}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/v1/evaluate bypassed the requirements: %d %s", rec.Code, rec.Body)
	}
	var evaluated struct {
		OK     bool             `json:"ok"`
		Errors []dto.FieldError `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &evaluated); err != nil {
		t.Fatal(err)
	}
	if evaluated.OK || len(evaluated.Errors) != 1 || evaluated.Errors[0].Field != "node_requirements" || evaluated.Errors[0].Code != ErrorNodeRequirementsUnmet {
		t.Fatalf("evaluate requirement mismatch = %+v", evaluated)
	}
	rec = httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", bytes.NewBufferString(`{"node_uuid":"labels","node_requirements":{"pool":["wrong"]}}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), ErrorNodeRequirementsUnmet) {
		t.Fatalf("/api/v1/deployments bypassed the requirements: %d %s", rec.Code, rec.Body)
	}
	signer, admin := roleTestToken(t, st, "admin")
	router := Router(Deps{Store: st, AuthSigner: signer})
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/nodes/labels/labels", bytes.NewBufferString(`{"labels":{}}`)))
	if rec.Code != 401 {
		t.Fatalf("unauthenticated status=%d body=%s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodTrace, "/api/v1/nodes/labels/labels", nil))
	if rec.Code != 401 {
		t.Fatalf("unauthenticated method status=%d", rec.Code)
	}
	reqMethod := httptest.NewRequest(http.MethodTrace, "/api/v1/nodes/labels/labels", nil)
	reqMethod.Header.Set("Authorization", "Bearer "+admin)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, reqMethod)
	if rec.Code != 405 {
		t.Fatalf("authenticated method status=%d", rec.Code)
	}
}
