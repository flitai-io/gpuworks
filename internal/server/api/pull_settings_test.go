package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// pullRig serves the real router over a store with one admin and one member
// session, so a route is exercised through its authentication and admin gate.
type pullRig struct {
	t             *testing.T
	st            *store.Store
	router        http.Handler
	admin         string
	member        string
	mu            sync.Mutex
	synced        []string
	networkSynced []string
	online        map[string]bool
	runtime       *fakeNodeRuntime
}

func newPullRig(t *testing.T) *pullRig {
	t.Helper()
	st, err := store.Open("file:pull_rig_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("pull-rig-master-secret")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	signer, err := auth.NewTokenSigner("pull-rig-signer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rig := &pullRig{t: t, st: st, online: map[string]bool{}, runtime: &fakeNodeRuntime{proxies: map[string]*proto.RuntimeProxyStatus{}, afterRestart: map[string]*proto.RuntimeProxyStatus{}}}
	rig.admin, err = managementSessionTestToken(t, st, signer, auth.Claims{Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	rig.member, err = managementSessionTestToken(t, st, signer, auth.Claims{Username: "member", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	rig.router = Router(Deps{
		Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}},
		Store:   st, AuthSigner: signer, NodeRuntime: rig.runtime,
		Online: func(uuid string) bool { rig.mu.Lock(); defer rig.mu.Unlock(); return rig.online[uuid] },
		NodeSync: nodeSyncCallbacks{request: func(uuid string) {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.synced = append(rig.synced, uuid)
		}, network: func(uuid string) {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.networkSynced = append(rig.networkSynced, uuid)
		}},
	})
	return rig
}

// node registers a node, optionally online, with labels.
func (r *pullRig) node(uuid string, online bool, labels map[string]string) *store.Node {
	r.t.Helper()
	node, err := r.st.RegisterNode(uuid, store.HashToken(uuid), uuid)
	if err != nil {
		r.t.Fatal(err)
	}
	if len(labels) > 0 {
		if _, err := r.st.SetNodeLabels(uuid, labels); err != nil {
			r.t.Fatal(err)
		}
	}
	r.mu.Lock()
	r.online[uuid] = online
	r.mu.Unlock()
	return node
}

func (r *pullRig) networkSyncedNodes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.networkSynced...)
}

func (r *pullRig) syncedNodes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.synced...)
}

func (r *pullRig) do(token, method, path, body string) (int, string) {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

// want runs a request as the admin and compares status and the body as JSON
// (key order is not part of the contract). An empty wantBody expects no body.
func (r *pullRig) want(method, path, body string, status int, wantBody string) {
	r.t.Helper()
	got, text := r.do(r.admin, method, path, body)
	if got != status {
		r.t.Fatalf("%s %s = %d %s, want %d", method, path, got, text, status)
	}
	if wantBody == "" {
		if text != "" {
			r.t.Fatalf("%s %s body = %s, want none", method, path, text)
		}
		return
	}
	jsonEqual(r.t, json.RawMessage(text), wantBody)
}

// Every admin route answers 401 without a session and 403 to a member; the
// two reads (rules, sync groups) are open to any signed-in user, the
// credential list is not (it names the accounts nodes log in with).
func TestPullSettingRoutesAuthorization(t *testing.T) {
	rig := newPullRig(t)
	rig.node("n1", true, nil)
	admin := []struct{ method, path, body string }{
		{"POST", "/api/v1/pull/source-rules", `{"match":"docker.io","endpoints":["m.example"],"allow_upstream":true}`},
		{"PUT", "/api/v1/pull/source-rules/order", `{"ids":[]}`},
		{"PUT", "/api/v1/pull/source-rules/1", `{"match":"docker.io","endpoints":["m.example"],"allow_upstream":true}`},
		{"DELETE", "/api/v1/pull/source-rules/1", ``},
		{"POST", "/api/v1/pull/sync-groups", `{"name":"rack"}`},
		{"PUT", "/api/v1/pull/sync-groups/1", `{"name":"rack"}`},
		{"DELETE", "/api/v1/pull/sync-groups/1", ``},
		{"GET", "/api/v1/pull/credentials", ``},
		{"POST", "/api/v1/pull/credentials", `{"host":"ghcr.io","username":"u","secret":"s"}`},
		{"PUT", "/api/v1/pull/credentials/1", `{"host":"ghcr.io","username":"u"}`},
		{"DELETE", "/api/v1/pull/credentials/1", ``},
		{"POST", "/api/v1/pull/credentials/1/test", ``},
		{"POST", "/api/v1/nodes/n1/runtime/restart", ``},
		{"POST", "/api/v1/pull/proxies", `{"http_proxy":"http://p:3128"}`},
		{"PUT", "/api/v1/pull/proxies/order", `{"ids":[]}`},
		{"PUT", "/api/v1/pull/proxies/1", `{"http_proxy":"http://p:3128"}`},
		{"DELETE", "/api/v1/pull/proxies/1", ``},
	}
	for _, route := range admin {
		if code, body := rig.do("", route.method, route.path, route.body); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d %s, want 401", route.method, route.path, code, body)
		}
		code, body := rig.do(rig.member, route.method, route.path, route.body)
		if code != http.StatusForbidden || !strings.Contains(body, `"code":"ADMINISTRATOR_REQUIRED"`) {
			t.Errorf("member %s %s = %d %s, want 403 ADMINISTRATOR_REQUIRED", route.method, route.path, code, body)
		}
	}
	for _, path := range []string{"/api/v1/pull/source-rules", "/api/v1/pull/sync-groups", "/api/v1/pull/proxies", "/api/v1/nodes/n1/pull-proxy"} {
		if code, body := rig.do("", "GET", path, ""); code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d %s", path, code, body)
		}
		if code, body := rig.do(rig.member, "GET", path, ""); code != http.StatusOK {
			t.Errorf("member GET %s = %d %s, want 200", path, code, body)
		}
	}
	// The member's refused writes changed nothing.
	if rules, _ := rig.st.PullSourceRules(); len(rules) != 0 {
		t.Fatalf("a refused write stored rules: %+v", rules)
	}
}

func TestPullSourceRulesCRUDWire(t *testing.T) {
	rig := newPullRig(t)
	rig.want("GET", "/api/v1/pull/source-rules", "", 200, `{"items":[],"total":0}`)

	// allow_upstream is required: leaving it out is not "false".
	rig.want("POST", "/api/v1/pull/source-rules", `{"match":"docker.io","endpoints":["mirror.example/dockerhub"]}`, 400,
		`{"code":"PULL_SETTING_INVALID","error":"allow_upstream is required","params":{"field":"allow_upstream","detail":"allow_upstream is required"}}`)

	rig.want("POST", "/api/v1/pull/source-rules",
		`{"match":" docker.io/ ","endpoints":[" mirror.example/dockerhub ",""],"allow_upstream":true,"note":" mainland "}`, 201,
		`{"id":1,"position":0,"kind":"image","match":"docker.io/","endpoints":["mirror.example/dockerhub"],"allow_upstream":true,"control_plane":false,"note":"mainland","node_requirements":{},"matched_nodes":0}`)
	rig.want("POST", "/api/v1/pull/source-rules",
		`{"match":"ghcr.io","endpoints":["ghcr.mirror.example"],"allow_upstream":false}`, 201,
		`{"id":2,"position":1,"kind":"image","match":"ghcr.io","endpoints":["ghcr.mirror.example"],"allow_upstream":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":0}`)

	// A site rule says so, and may serve the management node as well.
	rig.want("POST", "/api/v1/pull/source-rules",
		`{"match":"HTTPS://HuggingFace.co/","endpoints":["https://hf-mirror.com/"],"allow_upstream":true,"control_plane":true}`, 201,
		`{"id":3,"position":2,"kind":"url","match":"https://huggingface.co","endpoints":["https://hf-mirror.com"],"allow_upstream":true,"control_plane":true,"note":"","node_requirements":{},"matched_nodes":0}`)
	rig.want("DELETE", "/api/v1/pull/source-rules/3", "", 204, "")

	// Update replaces the content and keeps the position.
	rig.want("PUT", "/api/v1/pull/source-rules/1",
		`{"match":"docker.io","endpoints":["a.example/hub","b.example/hub"],"allow_upstream":false}`, 200,
		`{"id":1,"position":0,"kind":"image","match":"docker.io","endpoints":["a.example/hub","b.example/hub"],"allow_upstream":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":0}`)

	// Reorder answers the new list.
	rig.want("PUT", "/api/v1/pull/source-rules/order", `{"ids":[2,1]}`, 200, `{"total":2,"items":[
		{"id":2,"position":0,"kind":"image","match":"ghcr.io","endpoints":["ghcr.mirror.example"],"allow_upstream":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":0},
		{"id":1,"position":1,"kind":"image","match":"docker.io","endpoints":["a.example/hub","b.example/hub"],"allow_upstream":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":0}]}`)
	rig.want("GET", "/api/v1/pull/source-rules", "", 200, `{"total":2,"items":[
		{"id":2,"position":0,"kind":"image","match":"ghcr.io","endpoints":["ghcr.mirror.example"],"allow_upstream":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":0},
		{"id":1,"position":1,"kind":"image","match":"docker.io","endpoints":["a.example/hub","b.example/hub"],"allow_upstream":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":0}]}`)

	rig.want("DELETE", "/api/v1/pull/source-rules/2", "", 204, "")
	rig.want("GET", "/api/v1/pull/source-rules", "", 200, `{"total":1,"items":[
		{"id":1,"position":1,"kind":"image","match":"docker.io","endpoints":["a.example/hub","b.example/hub"],"allow_upstream":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":0}]}`)
}

func TestPullSourceRuleValidationAndMissingRows(t *testing.T) {
	rig := newPullRig(t)
	rig.want("POST", "/api/v1/pull/source-rules", `{"match":"docker.io","endpoints":["m.example"],"allow_upstream":true}`, 201,
		`{"id":1,"position":0,"kind":"image","match":"docker.io","endpoints":["m.example"],"allow_upstream":true,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":0}`)

	for name, tc := range map[string]struct{ body, field string }{
		"a site rule offering a registry":       {`{"match":"https://huggingface.co","endpoints":["m.example"],"allow_upstream":true}`, "endpoints"},
		"a site match with a path":              {`{"match":"https://huggingface.co/api","endpoints":["https://hf-mirror.com"],"allow_upstream":true}`, "match"},
		"a registry rule offering a site":       {`{"match":"docker.io","endpoints":["https://m.example"],"allow_upstream":true}`, "endpoints"},
		"an empty match":                        {`{"match":" ","endpoints":["m.example"],"allow_upstream":true}`, "match"},
		"an endpoint without a host":            {`{"match":"docker.io","endpoints":["justaname"],"allow_upstream":true}`, "endpoints"},
		"the same endpoint twice":               {`{"match":"docker.io","endpoints":["m.example","M.example/"],"allow_upstream":true}`, "endpoints"},
		"upstream allowed with nothing to try":  {`{"match":"docker.io","endpoints":[],"allow_upstream":true}`, "endpoints"},
		"a note that is too long":               {`{"match":"docker.io","endpoints":["m.example"],"allow_upstream":true,"note":"` + strings.Repeat("n", 256) + `"}`, "note"},
		"an image rule for the management node": {`{"match":"docker.io","endpoints":["m.example"],"allow_upstream":true,"control_plane":true}`, "control_plane"},
		"an invalid requirement key":            {`{"match":"docker.io","endpoints":["m.example"],"allow_upstream":true,"node_requirements":{"bad key!":["x"]}}`, "requirements"},
	} {
		code, body := rig.do(rig.admin, "POST", "/api/v1/pull/source-rules", tc.body)
		var got struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%s: %v %s", name, err, body)
		}
		if code != 400 || got.Code != "PULL_SETTING_INVALID" || got.Params["field"] != tc.field || got.Params["detail"] == "" || len(got.Params) != 2 {
			t.Errorf("%s: %d %s, want 400 PULL_SETTING_INVALID on %q", name, code, body, tc.field)
		}
	}
	// The same validation guards an update, and a rejected update changes nothing.
	if code, body := rig.do(rig.admin, "PUT", "/api/v1/pull/source-rules/1", `{"match":"docker.io","endpoints":[],"allow_upstream":true}`); code != 400 || !strings.Contains(body, `"field":"endpoints"`) {
		t.Fatalf("update with no endpoints = %d %s", code, body)
	}
	if rules, _ := rig.st.PullSourceRules(); len(rules) != 1 || rules[0].Endpoints[0] != "m.example" {
		t.Fatalf("a rejected write changed the rules: %+v", rules)
	}

	// Malformed bodies and unknown fields are a 400 too; missing ids a 404.
	for _, body := range []string{`{`, `{"match":"docker.io","allow_upstream":true,"endpoints":["m.example"],"surprise":1}`} {
		if code, text := rig.do(rig.admin, "POST", "/api/v1/pull/source-rules", body); code != 400 {
			t.Errorf("body %s = %d %s", body, code, text)
		}
	}
	for _, route := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/pull/source-rules/99", `{"match":"docker.io","endpoints":["m.example"],"allow_upstream":true}`},
		{"DELETE", "/api/v1/pull/source-rules/99", ``},
		{"DELETE", "/api/v1/pull/sync-groups/99", ``},
		{"PUT", "/api/v1/pull/sync-groups/99", `{"name":"x"}`},
	} {
		if code, body := rig.do(rig.admin, route.method, route.path, route.body); code != 404 {
			t.Errorf("%s %s = %d %s, want 404", route.method, route.path, code, body)
		}
	}
	// A reorder must name every rule exactly once.
	for _, body := range []string{`{"ids":[]}`, `{"ids":[1,1]}`, `{"ids":[2]}`, `{"ids":[1,2]}`} {
		code, text := rig.do(rig.admin, "PUT", "/api/v1/pull/source-rules/order", body)
		if code != 400 || !strings.Contains(text, `"field":"order"`) {
			t.Errorf("order %s = %d %s, want 400 on order", body, code, text)
		}
	}
}

// A rule or group reports how many nodes its node requirements select now.
func TestPullSettingsCountMatchedNodes(t *testing.T) {
	rig := newPullRig(t)
	rig.node("gpu-a", true, map[string]string{"pool": "mainland", "rack": "r1"})
	rig.node("gpu-b", false, map[string]string{"pool": "mainland", "rack": "r2"})
	rig.node("gpu-c", true, map[string]string{"pool": "overseas"})
	rig.node("gpu-d", true, nil)

	rig.want("POST", "/api/v1/pull/source-rules",
		`{"match":"docker.io","endpoints":["m.example"],"allow_upstream":true,"node_requirements":{"pool":["mainland"]}}`, 201,
		`{"id":1,"position":0,"kind":"image","match":"docker.io","endpoints":["m.example"],"allow_upstream":true,"control_plane":false,"note":"","node_requirements":{"pool":["mainland"]},"matched_nodes":2}`)
	rig.want("POST", "/api/v1/pull/source-rules",
		`{"match":"ghcr.io","endpoints":["g.example"],"allow_upstream":true,"node_requirements":{"pool":["mainland","overseas"],"rack":["r1"]}}`, 201,
		`{"id":2,"position":1,"kind":"image","match":"ghcr.io","endpoints":["g.example"],"allow_upstream":true,"control_plane":false,"note":"","node_requirements":{"pool":["mainland","overseas"],"rack":["r1"]},"matched_nodes":1}`)
	rig.want("POST", "/api/v1/pull/source-rules",
		`{"match":"quay.io","endpoints":["q.example"],"allow_upstream":true}`, 201,
		`{"id":3,"position":2,"kind":"image","match":"quay.io","endpoints":["q.example"],"allow_upstream":true,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":4}`)
	code, body := rig.do(rig.admin, "GET", "/api/v1/pull/source-rules", "")
	var list struct {
		Items []struct {
			ID      uint `json:"id"`
			Matched int  `json:"matched_nodes"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil || code != 200 || len(list.Items) != 3 ||
		list.Items[0].Matched != 2 || list.Items[1].Matched != 1 || list.Items[2].Matched != 4 {
		t.Fatalf("list counts = %d %s", code, body)
	}
	// A selector no node meets selects none.
	rig.want("PUT", "/api/v1/pull/source-rules/1",
		`{"match":"docker.io","endpoints":["m.example"],"allow_upstream":true,"node_requirements":{"pool":["nowhere"]}}`, 200,
		`{"id":1,"position":0,"kind":"image","match":"docker.io","endpoints":["m.example"],"allow_upstream":true,"control_plane":false,"note":"","node_requirements":{"pool":["nowhere"]},"matched_nodes":0}`)
}

func TestSyncGroupsWire(t *testing.T) {
	rig := newPullRig(t)
	rig.node("a", true, map[string]string{"rack": "r1"})
	rig.node("b", true, map[string]string{"rack": "r1"})
	rig.node("c", true, map[string]string{"rack": "r2"})

	// A fresh installation has one group: every node, no cap.
	rig.want("GET", "/api/v1/pull/sync-groups", "", 200,
		`{"total":1,"items":[{"id":1,"name":"default-all-nodes","node_requirements":{},"rate_limit_kib_s":0,"matched_nodes":3}]}`)

	rig.want("POST", "/api/v1/pull/sync-groups", `{"name":" rack-1 ","node_requirements":{"rack":["r1"]},"rate_limit_kib_s":51200}`, 201,
		`{"id":2,"name":"rack-1","node_requirements":{"rack":["r1"]},"rate_limit_kib_s":51200,"matched_nodes":2}`)
	// A field the group does not have is refused, not ignored.
	code, body := rig.do(rig.admin, "POST", "/api/v1/pull/sync-groups", `{"name":"x","is_default":true}`)
	if code != 400 {
		t.Fatalf("an unknown field accepted on create: %d %s", code, body)
	}
	rig.want("PUT", "/api/v1/pull/sync-groups/2", `{"name":"rack-2","node_requirements":{"rack":["r2"]},"rate_limit_kib_s":0}`, 200,
		`{"id":2,"name":"rack-2","node_requirements":{"rack":["r2"]},"rate_limit_kib_s":0,"matched_nodes":1}`)
	// The seeded group can be capped and renamed through the same route.
	rig.want("PUT", "/api/v1/pull/sync-groups/1", `{"name":"all","rate_limit_kib_s":1024}`, 200,
		`{"id":1,"name":"all","node_requirements":{},"rate_limit_kib_s":1024,"matched_nodes":3}`)
	rig.want("GET", "/api/v1/pull/sync-groups", "", 200, `{"total":2,"items":[
		{"id":1,"name":"all","node_requirements":{},"rate_limit_kib_s":1024,"matched_nodes":3},
		{"id":2,"name":"rack-2","node_requirements":{"rack":["r2"]},"rate_limit_kib_s":0,"matched_nodes":1}]}`)

	for name, tc := range map[string]struct{ body, field string }{
		"a group needs a name":    {`{"name":"  "}`, "name"},
		"a name that is too long": {`{"name":"` + strings.Repeat("g", 65) + `"}`, "name"},
		"a negative rate limit":   {`{"name":"x","rate_limit_kib_s":-1}`, "rate_limit"},
		"an invalid requirement":  {`{"name":"x","node_requirements":{"bad key!":["v"]}}`, "requirements"},
	} {
		code, text := rig.do(rig.admin, "POST", "/api/v1/pull/sync-groups", tc.body)
		if code != 400 || !strings.Contains(text, `"code":"PULL_SETTING_INVALID"`) || !strings.Contains(text, `"field":"`+tc.field+`"`) {
			t.Errorf("%s: %d %s", name, code, text)
		}
	}
	rig.want("DELETE", "/api/v1/pull/sync-groups/2", "", 204, "")
	if groups, _ := rig.st.SyncGroups(); len(groups) != 1 || groups[0].ID != 1 {
		t.Fatalf("groups after delete = %+v", groups)
	}
}

// The group an installation starts with is an ordinary one: deleting it
// leaves nodes outside any group, which copy from no one automatically, and
// an empty-selector group created later selects every node again.
func TestSeededSyncGroupCanBeDeletedAndRecreated(t *testing.T) {
	rig := newPullRig(t)
	rig.node("n1", true, nil)
	rig.want("DELETE", "/api/v1/pull/sync-groups/1", "", 204, "")
	rig.want("GET", "/api/v1/pull/sync-groups", "", 200, `{"items":[],"total":0}`)
	rig.want("POST", "/api/v1/pull/sync-groups", `{"name":"default-all-nodes"}`, 201,
		`{"id":2,"name":"default-all-nodes","node_requirements":{},"rate_limit_kib_s":0,"matched_nodes":1}`)
}
