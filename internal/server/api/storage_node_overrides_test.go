package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStorageNodeOverridesAPI(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	n1, err := st.RegisterNode("override-one", store.HashToken("one"), "one")
	if err != nil {
		t.Fatal(err)
	}
	n2, err := st.RegisterNode("override-two", store.HashToken("two"), "two")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := st.CreateStorage(&store.Storage{Name: "overrides", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true, Export: "nfs:/overrides", CacheMaxBytes: 500}, []store.StorageNode{{NodeID: n1.ID, Path: "/mount", CachePath: "/cache", Existing: true}})
	if err != nil {
		t.Fatal(err)
	}
	signer, member := roleTestToken(t, st, "member")
	_, admin := roleTestToken(t, st, "admin")
	var synced []string
	router := Router(Deps{Store: st, AuthSigner: signer, NodeSync: nodeSyncCallbacks{request: func(uuid string) { synced = append(synced, uuid) }}})
	endpoint := fmt.Sprintf("/api/v1/storages/%d/node-overrides", cache.ID)
	call := func(token, method, url, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	valid := `{"node_uuids":["override-one","override-two"],"cache_max_bytes":0}`
	for _, tc := range []struct {
		token, method, url string
		want               int
	}{
		{"", http.MethodPatch, endpoint, 401}, {member, http.MethodPatch, endpoint, 403}, {admin, http.MethodPost, endpoint, 405},
		{admin, http.MethodPatch, "/api/v1/storages/999/node-overrides", 404},
	} {
		if rec := call(tc.token, tc.method, tc.url, valid); rec.Code != tc.want {
			t.Fatalf("route %s %s: %d %s", tc.method, tc.url, rec.Code, rec.Body.String())
		}
	}
	for _, body := range []string{
		`{}`, `{"node_uuids":[],"cache_max_bytes":0}`, `{"node_uuids":["override-one"]}`,
		`{"node_uuids":["override-one","missing"],"cache_max_bytes":0}`,
		`{"node_uuids":["override-one","override-one"],"cache_max_bytes":0}`,
		`{"node_uuids":[""],"cache_max_bytes":0}`,
		`{"node_uuids":["override-one"],"cache_max_bytes":-1}`,
		`{"node_uuids":["override-one"],"cache_max_bytes":9223372036854775808}`,
		`{"node_uuids":["override-one"],"cache_max_bytes":0.5}`,
		`{"node_uuids":["override-one"],"cache_path":"relative"}`,
		`{"node_uuids":["override-one"],"cache_path":""}`,
		`{"node_uuids":["override-one"],"cache_max_bytes":0,"all_nodes":false}`,
		`{"node_uuids":["override-one"],"cache_max_bytes":0} {}`,
	} {
		if rec := call(admin, http.MethodPatch, endpoint, body); rec.Code != 400 {
			t.Fatalf("bad body %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	rows, _ := st.StorageNodes(cache.ID)
	if len(rows) != 1 || rows[0].CacheMaxBytes != nil {
		t.Fatalf("invalid batch changed rows: %+v", rows)
	}
	rec := call(admin, http.MethodPatch, endpoint, valid)
	if rec.Code != 200 {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	var saved dto.Storage
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.UpdatedAt == nil || !saved.UpdatedAt.After(cache.UpdatedAt) || len(saved.Targets) != 2 || saved.Targets[0].OverrideOnly || !saved.Targets[1].OverrideOnly {
		t.Fatalf("response: %+v", saved)
	}
	for _, target := range saved.Targets {
		if target.CacheMaxBytes == nil || *target.CacheMaxBytes != 0 {
			t.Fatal("zero lost")
		}
	}
	// An old full-form draft must not erase the successful batch.
	oldRevision, _ := json.Marshal(cache.UpdatedAt)
	rec = call(admin, http.MethodPatch, fmt.Sprintf("/api/v1/storages/%d", cache.ID), `{"expected_updated_at":`+string(oldRevision)+`,"targets":[]}`)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), ErrorStorageConfigChanged) {
		t.Fatalf("stale patch: %d %s", rec.Code, rec.Body.String())
	}
	// Path reset and limit reset are independent, and explicit membership remains.
	synced = nil
	rec = call(admin, http.MethodPatch, endpoint, `{"node_uuids":["override-one"],"cache_path":null}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if len(synced) != 1 || synced[0] != n1.UUID {
		t.Fatalf("single-node change synced unrelated nodes: %v", synced)
	}
	rows, _ = st.StorageNodes(cache.ID)
	if rows[0].CachePath != "" || rows[0].CacheMaxBytes == nil || rows[0].Path != "/mount" || !rows[0].Existing {
		t.Fatalf("path-only changed other fields: %+v", rows)
	}
	rec = call(admin, http.MethodPatch, endpoint, `{"node_uuids":["override-one","override-two"],"cache_max_bytes":null}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rows, _ = st.StorageNodes(cache.ID)
	if len(rows) != 1 || rows[0].OverrideOnly || rows[0].CacheMaxBytes != nil {
		t.Fatalf("reset removed membership: %+v", rows)
	}
	// Empty pure override disappears; re-adding it cannot select an excluded node.
	no := false
	targets := []store.StorageNode{{NodeID: n1.ID, Path: "/mount", Existing: true}}
	if _, err := st.UpdateStorage(cache.ID, store.StoragePatch{AllNodes: &no, Targets: &targets}); err != nil {
		t.Fatal(err)
	}
	rec = call(admin, http.MethodPatch, endpoint, `{"node_uuids":["override-two"],"cache_max_bytes":100}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	current, _ := st.StorageByID(cache.ID)
	if _, applies, _ := st.EffectiveStorageForNode(current, n2); applies {
		t.Fatal("batch expanded scope")
	}
}

func TestStorageTargetValidationRejectsNegativeAndOverrideOnlyScope(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("validation-node", store.HashToken("t"), "validation")
	if err != nil {
		t.Fatal(err)
	}
	a := &apiHandlers{d: Deps{Store: st}}
	minus := int64(-1)
	for _, target := range []dto.StorageTarget{
		{NodeUUID: node.UUID, CacheMaxBytes: &minus}, {NodeUUID: node.UUID, LabelKey: "pool"}, {LabelKey: "pool", OverrideOnly: true}, {LabelKey: "pool", CachePath: "/cache"},
	} {
		if _, err := a.storageTargets([]dto.StorageTarget{target}); err == nil {
			t.Fatalf("invalid target accepted: %+v", target)
		}
	}
	no := false
	name, kind, dir := "limited", "local", "/models"
	targets := []dto.StorageTarget{{NodeUUID: node.UUID, OverrideOnly: true}}
	if _, _, err := a.storageFromRequest(dto.StorageRequest{Name: &name, Type: &kind, Path: &dir, AllNodes: &no, Targets: &targets}); err == nil {
		t.Fatal("pure override accepted as scope")
	}

	signer, admin := roleTestToken(t, st, "admin")
	router := Router(Deps{Store: st, AuthSigner: signer})
	for _, tc := range []struct{ method, url, body string }{
		{http.MethodPost, "/api/v1/storages", `{"name":"negative","type":"local","path":"/models","targets":[{"node_uuid":"validation-node","cache_max_bytes":-1}]}`},
		{http.MethodPatch, "/api/v1/storages/1", `{"targets":[{"node_uuid":"validation-node","cache_max_bytes":-1}]}`},
		{http.MethodPatch, "/api/v1/storages/1", `{"all_nodes":false,"targets":[{"node_uuid":"validation-node","override_only":true}]}`},
		{http.MethodPatch, "/api/v1/storages/1", `{"targets":[{"node_uuid":"validation-node","cache_max_bytes":100},{"node_uuid":"validation-node","label_value":"x","cache_max_bytes":200}]}`},
		{http.MethodPatch, "/api/v1/storages/1", `{"targets":[{"node_uuid":"validation-node","cache_max_bytes":100},{"node_uuid":"validation-node","cache_max_bytes":200}]}`},
		{http.MethodPatch, "/api/v1/storages/1", `{"targets":[{"node_uuid":"validation-node","label_value":"x"}]}`},
	} {
		req := httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+admin)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid target wire accepted: %s %d %s", tc.body, rec.Code, rec.Body.String())
		}
	}
}
