package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRoutingPoliciesWireContract(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&apiHandlers{}).routingPolicies(recorder, httptest.NewRequest("GET", "/api/v1/routing-policies", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var got []dto.RoutingPolicyOption
	if err := unmarshalList(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(deploy.RoutingPolicyValues()) {
		t.Fatalf("options=%d, want %d", len(got), len(deploy.RoutingPolicyValues()))
	}
	for i, value := range deploy.RoutingPolicyValues() {
		if got[i].Value != value || got[i].Label["en"] == "" || got[i].Label["zh"] == "" || got[i].Description["en"] == "" || got[i].Description["zh"] == "" {
			t.Fatalf("option[%d]=%+v, want localized %s", i, got[i], value)
		}
	}
}

func TestRoutingPolicyRoutesUseManagementAuthAndWireErrors(t *testing.T) {
	signer, err := auth.NewTokenSigner("routing-policy-route-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open("file:routing_policy_route_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	router := Router(Deps{AuthSigner: signer, Store: st})
	userToken, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "member", Role: "user"})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/routing-policies", nil)
	request.Header.Set("Authorization", "Bearer "+userToken)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated policy list status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/routing-policies", nil)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated policy list status=%d, want 401", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPut, "/api/v1/deployments/404/routing", strings.NewReader(`{"routing_policy":"unknown"}`))
	request.Header.Set("Authorization", "Bearer "+userToken)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusForbidden {
		t.Fatal("regular management user was incorrectly blocked by admin-only routing middleware")
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "routing-contract", ServedName: "routing-contract", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPut, "/api/v1/deployments/"+strconv.FormatUint(uint64(dep.ID), 10)+"/routing", strings.NewReader(`{"routing_policy":"unknown"}`))
	request.Header.Set("Authorization", "Bearer "+userToken)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var wire map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire["code"] != ErrorRoutingPolicyInvalid {
		t.Fatalf("invalid policy wire=%v, want code %s", wire, ErrorRoutingPolicyInvalid)
	}
}

func TestSetRoutingPolicyHTTPContract(t *testing.T) {
	st, err := store.Open("", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dep, err := st.CreateDeployment(&store.Deployment{Name: "contract", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("routing-contract-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "member", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{AuthSigner: signer, Store: st})
	path := "/api/v1/deployments/" + strconv.FormatUint(uint64(dep.ID), 10) + "/routing"
	for _, tc := range []struct {
		name, method, path, body string
		auth                     bool
		status                   int
		policy                   string
	}{
		{"unauthenticated", "PUT", path, `{}`, false, 401, ""},
		{"missing", "PUT", "/api/v1/deployments/999999/routing", `{}`, true, 404, ""},
		{"method", "POST", path, `{}`, true, 405, ""},
		{"malformed", "PUT", path, `{`, true, 400, ""},
		{"set", "PUT", path, `{"routing_policy":"prefix_affinity"}`, true, 200, "prefix_affinity"},
		{"trim", "PUT", path, `{"routing_policy":" failover "}`, true, 200, "failover"},
		{"default", "PUT", path, `{}`, true, 200, "least_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.auth {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.policy == "" {
				return
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			if string(wire["routing_policy"]) != strconv.Quote(tc.policy) {
				t.Fatalf("wire routing_policy=%s", wire["routing_policy"])
			}
			saved, err := st.GetDeployment(dep.ID)
			if err != nil || saved.RoutingPolicy != tc.policy {
				t.Fatalf("saved=%+v err=%v", saved, err)
			}
		})
	}
}
