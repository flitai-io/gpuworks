package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/priority"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// getKey reads /v1/key as a client does; key "" sends none.
func getKey(t *testing.T, p *Proxy, method, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/key", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	p.KeyHandler()(rec, req)
	return rec
}

// newKeyRig is newPriorityRig with "v" given a second replica launched
// without priority scheduling and "s" a multi-node follower without it, which
// is part of its leader's replica and counts for nothing.
func newKeyRig(t *testing.T) *priorityRig {
	t.Helper()
	rig := newPriorityRig(t)
	node, err := rig.st.RegisterNode("node-prio-2", store.HashToken("node-2"), "node-prio-2")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []store.Instance{
		{UUID: "v-instance-2", DeploymentID: rig.deployments["v"], NodeID: node.ID, State: proto.InstRunning, APIPort: 31000, Ports: "[31000]", DesiredStateJSON: `{"argv":["vllm","serve"]}`},
		{UUID: "s-follower", DeploymentID: rig.deployments["s"], NodeID: node.ID, State: proto.InstRunning, Headless: true, DesiredStateJSON: `{"argv":["python","-m","sglang.launch_server"]}`},
	} {
		if _, err := rig.st.CreateInstance(&in); err != nil {
			t.Fatal(err)
		}
	}
	rig.p.routes.rebuild()
	return rig
}

// The answer is the wire shape a gateway in front of GPUWorks reads, field
// for field: the key's classes highest first, and every model it may call in
// name order with how its running replicas schedule.
func TestKeyInfoWireShape(t *testing.T) {
	rig := newKeyRig(t)
	key := rig.key(t, store.APIKeyPriority{Class: priority.Normal, Ceiling: priority.Critical})
	rec := getKey(t, rig.p, http.MethodGet, key)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("GET /v1/key = %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	want := `{"object":"key","features":["priority_scheduling_header","busy_reject"],` +
		`"priority":{"own":{"name":"normal","value":0},"ceiling":{"name":"critical","value":40},` +
		`"reachable":[{"name":"critical","value":40},{"name":"high","value":20},{"name":"normal","value":0},{"name":"low","value":-20},{"name":"batch","value":-40}]},` +
		`"models":[{"id":"off","priority_scheduling":"none","busy_reject":false},` +
		`{"id":"s","priority_scheduling":"all","busy_reject":false},` +
		`{"id":"unlaunched","priority_scheduling":"none","busy_reject":false},` +
		`{"id":"v","priority_scheduling":"some","busy_reject":false}]}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("GET /v1/key =\n%s\nwant\n%s", got, want)
	}
	var decoded KeyInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(decoded)
	if err != nil || string(again) != want {
		t.Fatalf("round trip = %s %v", again, err)
	}
}

// The priority block follows the key's binding by the rules a request is
// resolved by (priority.KeyReach): no ceiling is the key's own class, fixed
// reaches only it, down only what is not above it, and a custom class counts
// like a built-in one.
func TestKeyInfoFollowsTheBinding(t *testing.T) {
	rig := newKeyRig(t)
	if err := rig.st.CreatePriorityClass(&store.PriorityClass{Name: "gold", Value: 30}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		binding store.APIKeyPriority
		want    string
	}{
		{"no ceiling", store.APIKeyPriority{Class: priority.High},
			`{"own":{"name":"high","value":20},"ceiling":{"name":"high","value":20},"reachable":[{"name":"high","value":20},{"name":"normal","value":0},{"name":"low","value":-20},{"name":"batch","value":-40}]}`},
		{"unbound", store.APIKeyPriority{},
			`{"own":{"name":"normal","value":0},"ceiling":{"name":"normal","value":0},"reachable":[{"name":"normal","value":0},{"name":"low","value":-20},{"name":"batch","value":-40}]}`},
		{"a custom ceiling", store.APIKeyPriority{Class: priority.Normal, Ceiling: "gold"},
			`{"own":{"name":"normal","value":0},"ceiling":{"name":"gold","value":30},"reachable":[{"name":"gold","value":30},{"name":"high","value":20},{"name":"normal","value":0},{"name":"low","value":-20},{"name":"batch","value":-40}]}`},
		{"fixed", store.APIKeyPriority{Class: priority.Low, Policy: priority.PolicyFixed},
			`{"own":{"name":"low","value":-20},"ceiling":{"name":"low","value":-20},"reachable":[{"name":"low","value":-20}]}`},
		{"down only", store.APIKeyPriority{Class: "gold", Policy: priority.PolicyDownOnly},
			`{"own":{"name":"gold","value":30},"ceiling":{"name":"gold","value":30},"reachable":[{"name":"gold","value":30},{"name":"high","value":20},{"name":"normal","value":0},{"name":"low","value":-20},{"name":"batch","value":-40}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := getKey(t, rig.p, http.MethodGet, rig.key(t, tc.binding))
			var body struct {
				Priority json.RawMessage `json:"priority"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("GET /v1/key = %d %s %v", rec.Code, rec.Body, err)
			}
			if string(body.Priority) != tc.want {
				t.Fatalf("priority = %s, want %s", body.Priority, tc.want)
			}
		})
	}
}

// A scoped key sees only the models it may call; a request without a key
// under open access has no binding to report.
func TestKeyInfoScopeAndAnonymousAccess(t *testing.T) {
	rig := newKeyRig(t)
	scoped, err := rig.st.CreateScopedAPIKey("scoped", 0, store.APIKeyScope{Restricted: true, DeploymentIDs: []uint{rig.deployments["s"], rig.deployments["off"]}})
	if err != nil {
		t.Fatal(err)
	}
	models := func(t *testing.T, rec *httptest.ResponseRecorder) (string, string) {
		t.Helper()
		var body struct {
			Priority json.RawMessage `json:"priority"`
			Models   json.RawMessage `json:"models"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/key = %d %s %v", rec.Code, rec.Body, err)
		}
		return string(body.Priority), string(body.Models)
	}
	if _, got := models(t, getKey(t, rig.p, http.MethodGet, scoped)); got != `[{"id":"off","priority_scheduling":"none","busy_reject":false},{"id":"s","priority_scheduling":"all","busy_reject":false}]` {
		t.Fatalf("scoped key's models = %s", got)
	}

	if rec := getKey(t, rig.p, http.MethodGet, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key, open access off = %d %s, want 401", rec.Code, rec.Body)
	}
	if rec := getKey(t, rig.p, http.MethodGet, "gpw_bogus_secret"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a forged key = %d %s, want 401", rec.Code, rec.Body)
	}
	// A fresh server: the closed switch this one just read is cached a while.
	t.Run("open access", func(t *testing.T) {
		open := newKeyRig(t)
		if err := open.st.SetSecurityPolicy(store.SecurityPolicy{InferenceOpenAccess: true}); err != nil {
			t.Fatal(err)
		}
		prio, got := models(t, getKey(t, open.p, http.MethodGet, ""))
		if prio != "null" || !strings.Contains(got, `"id":"v"`) || !strings.Contains(got, `"id":"unlaunched"`) {
			t.Fatalf("anonymous = priority %s models %s, want null and every model", prio, got)
		}
	})
}

// Only GET: another method is a 405 naming it, in the gateway's error shape.
func TestKeyInfoRefusesOtherMethods(t *testing.T) {
	rig := newKeyRig(t)
	key := rig.key(t, store.APIKeyPriority{})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		rec := getKey(t, rig.p, method, key)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
			t.Fatalf("%s /v1/key = %d Allow %q", method, rec.Code, rec.Header().Get("Allow"))
		}
		if method == http.MethodHead {
			continue
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != ErrorMethodNotAllowed || body.Error.Type != typeInvalidRequest {
			t.Fatalf("%s /v1/key body = %s %v", method, rec.Body, err)
		}
	}
	// The key is checked first: a caller without one learns nothing more.
	if rec := getKey(t, rig.p, http.MethodPost, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST without a key = %d", rec.Code)
	}
}

// /v1/key is not traffic: polling it accounts nothing, records nothing and
// reads no deployment rows.
func TestKeyInfoIsNotTraffic(t *testing.T) {
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, http.NotFoundHandler())
	defer closeUpstream()
	var records []uxrecord.Request
	p.SetRequestSink(func(r uxrecord.Request) { records = append(records, r) })
	var deploymentReads atomic.Int64
	db := storeOf(p.store).DB()
	name := "test:key_info_deployment_reads"
	if err := db.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "deployments" {
			deploymentReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(name) })
	for range 3 {
		if rec := getKey(t, p, http.MethodGet, apiKey); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"omni"`) {
			t.Fatalf("GET /v1/key = %d %s", rec.Code, rec.Body)
		}
	}
	if n := deploymentReads.Load(); n != 0 {
		t.Errorf("GET /v1/key read the deployments table %d times, want 0", n)
	}
	assertUnaccounted(t, p, acct, depID, records)
}

// Every answer the gateway routes says whether the engine scheduled the
// request by its class: on only for an instance launched with priority
// scheduling, on a generating path.
func TestAnswerSaysWhetherTheEngineScheduledByPriority(t *testing.T) {
	rig := newPriorityRig(t)
	key := rig.key(t, store.APIKeyPriority{Class: priority.High})
	for model, want := range map[string]string{"v": "on", "s": "on", "off": "off", "unlaunched": "off"} {
		rec, _, _ := rig.send(t, key, model, "", "")
		if rec.Code != http.StatusOK || rec.Header().Get(priorityScheduling) != want {
			t.Fatalf("%s = %d %s %q, want %q", model, rec.Code, priorityScheduling, rec.Header().Get(priorityScheduling), want)
		}
	}
	// A class at the engine's neutral value is still scheduled by.
	if rec, _, _ := rig.send(t, rig.key(t, store.APIKeyPriority{}), "v", "", ""); rec.Header().Get(priorityScheduling) != "on" {
		t.Fatalf("normal on v = %q", rec.Header().Get(priorityScheduling))
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/tokenize", strings.NewReader(`{"model":"v","prompt":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	rig.p.Handler()(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get(priorityScheduling) != "off" {
		t.Fatalf("tokenize = %d %q, want off", rec.Code, rec.Header().Get(priorityScheduling))
	}
}
