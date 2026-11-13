package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A new deployment's tokenize routes are open unless the request turns them
// off; the answer says which under tokenize_enabled.
func TestCreateDeploymentTokenize(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	modelID := automaticPlacementModel(t, st)
	creates := 0
	create := func(extra string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		creates++
		body := fmt.Sprintf(`{"name":"tok","served_name":"tok-%d","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","quick_params":{}%s}`, creates, modelID, extra)
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("body is not JSON: %s", rec.Body)
		}
		return rec.Code, out
	}
	stored := func(out map[string]any) *store.Deployment {
		t.Helper()
		row, err := st.GetDeployment(uint(out["id"].(float64)))
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	for _, tc := range []struct {
		extra    string
		disabled bool
	}{
		{extra: "", disabled: false},
		{extra: `,"tokenize_enabled":true`, disabled: false},
		{extra: `,"tokenize_enabled":false`, disabled: true},
	} {
		status, out := create(tc.extra)
		if status != http.StatusCreated {
			t.Fatalf("%q: create = %d %v", tc.extra, status, out)
		}
		if row := stored(out); row.TokenizeDisabled != tc.disabled {
			t.Fatalf("%q: stored disabled = %v", tc.extra, row.TokenizeDisabled)
		}
		if out["tokenize_enabled"] != !tc.disabled {
			t.Fatalf("%q: tokenize_enabled = %v", tc.extra, out["tokenize_enabled"])
		}
	}
	if status, out := create(`,"tokenize_enabled":"no"`); status != http.StatusBadRequest {
		t.Fatalf("a non-boolean switch = %d %v", status, out)
	}
	// A clone keeps the original's switch.
	closed, err := st.DeploymentsByServedName("tok-3")
	if err != nil || len(closed) != 1 {
		t.Fatalf("closed = %+v err=%v", closed, err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deployments/x/clone", strings.NewReader(`{"name":"tok-copy"}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", strconv.FormatUint(uint64(closed[0].ID), 10))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	h.cloneDeployment(rec, req)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"tokenize_enabled":false`) {
		t.Fatalf("clone = %d %s", rec.Code, rec.Body)
	}
}

// The request's switch is a pointer: absent stays absent, both values
// survive a round trip under the exact key.
func TestDeployRequestTokenizeWire(t *testing.T) {
	var absent dto.DeployRequest
	if err := json.Unmarshal([]byte(`{"name":"a"}`), &absent); err != nil || absent.TokenizeEnabled != nil {
		t.Fatalf("absent = %+v err=%v", absent.TokenizeEnabled, err)
	}
	wire, err := json.Marshal(absent)
	if err != nil || strings.Contains(string(wire), "tokenize") {
		t.Fatalf("absent marshals as %s", wire)
	}
	for _, value := range []bool{true, false} {
		var req dto.DeployRequest
		raw := fmt.Sprintf(`{"tokenize_enabled":%t}`, value)
		if err := json.Unmarshal([]byte(raw), &req); err != nil || req.TokenizeEnabled == nil || *req.TokenizeEnabled != value {
			t.Fatalf("%s = %+v err=%v", raw, req.TokenizeEnabled, err)
		}
		wire, err := json.Marshal(req)
		if err != nil || !strings.Contains(string(wire), fmt.Sprintf(`"tokenize_enabled":%t`, value)) {
			t.Fatalf("%s marshals as %s", raw, wire)
		}
		var back dto.DeployRequest
		if err := json.Unmarshal(wire, &back); err != nil || !reflect.DeepEqual(back.TokenizeEnabled, req.TokenizeEnabled) {
			t.Fatalf("round trip = %+v", back.TokenizeEnabled)
		}
	}
	var body dto.TokenizeRequest
	if err := json.Unmarshal([]byte(`{}`), &body); err != nil || body.Enabled != nil {
		t.Fatalf("empty PUT body = %+v", body)
	}
	off := false
	if wire, _ := json.Marshal(dto.TokenizeRequest{Enabled: &off}); string(wire) != `{"enabled":false}` {
		t.Fatalf("PUT body = %s", wire)
	}
}

// PUT /deployments/{id}/tokenize opens or closes the gateway's tokenize
// routes; it is authenticated, refuses a body without the switch and answers
// the deployment with tokenize_enabled.
func TestSetTokenizeRoute(t *testing.T) {
	signer, err := auth.NewTokenSigner("tokenize-route-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open("file:tokenize_route?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	router := Router(Deps{AuthSigner: signer, Store: st, Monitor: &fakeMonitorService{}})
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	do := func(method, path, body string, withAuth bool) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if withAuth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "tok", ServedName: "tok", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/deployments/" + strconv.FormatUint(uint64(dep.ID), 10) + "/tokenize"
	sub := st.Bus.Subscribe(8, "deployment")
	defer sub.Close()

	if status, _ := do(http.MethodPut, path, `{"enabled":false}`, false); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", status)
	}
	if status, _ := do(http.MethodPut, "/api/v1/deployments/9999/tokenize", `{"enabled":false}`, true); status != http.StatusNotFound {
		t.Fatalf("unknown deployment = %d", status)
	}
	if status, _ := do(http.MethodPut, path, `{"enabled":`, true); status != http.StatusBadRequest {
		t.Fatalf("malformed body = %d", status)
	}
	if status, _ := do(http.MethodPut, path, `{"enabled":"off"}`, true); status != http.StatusBadRequest {
		t.Fatalf("non-boolean = %d", status)
	}
	status, body := do(http.MethodPut, path, `{}`, true)
	var refused map[string]any
	_ = json.Unmarshal([]byte(body), &refused)
	if status != http.StatusBadRequest || refused["code"] != ErrorFieldInvalid || !strings.Contains(fmt.Sprint(refused["params"]), "enabled") {
		t.Fatalf("missing enabled = %d %s", status, body)
	}
	if status, _ := do(http.MethodGet, path, "", true); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", status)
	}
	if row, _ := st.GetDeployment(dep.ID); row.TokenizeDisabled {
		t.Fatal("a refused request changed the switch")
	}

	status, body = do(http.MethodPut, path, `{"enabled":false}`, true)
	if status != http.StatusOK || !strings.Contains(body, `"tokenize_enabled":false`) {
		t.Fatalf("turn off = %d %s", status, body)
	}
	if row, _ := st.GetDeployment(dep.ID); !row.TokenizeDisabled {
		t.Fatalf("stored = %+v", row)
	}
	select {
	case e := <-sub.C():
		if e.Reason != "tokenize" {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("turning tokenize off published no change for the route table")
	}
	status, body = do(http.MethodPut, path, `{"enabled":true}`, true)
	if status != http.StatusOK || !strings.Contains(body, `"tokenize_enabled":true`) {
		t.Fatalf("turn on = %d %s", status, body)
	}
	if row, _ := st.GetDeployment(dep.ID); row.TokenizeDisabled {
		t.Fatalf("stored = %+v", row)
	}
	status, body = do(http.MethodGet, "/api/v1/deployments/"+strconv.FormatUint(uint64(dep.ID), 10), "", true)
	if status != http.StatusOK || !strings.Contains(body, `"tokenize_enabled":true`) {
		t.Fatalf("detail = %d %s", status, body)
	}
}
