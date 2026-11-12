package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type switchAPI struct {
	st            *store.Store
	member, admin string
	call          func(token, method, path, body string) (int, string)
	header        func(token, method, path string) http.Header
	cache         *store.Storage
	nodeA, nodeB  *store.Node
	storagePath   string
	online        map[string]bool
}

func newSwitchAPI(t *testing.T) *switchAPI {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, member := roleTestToken(t, st, "member")
	_, admin := roleTestToken(t, st, "admin")
	a := &switchAPI{st: st, member: member, admin: admin, online: map[string]bool{}}
	router := Router(Deps{Store: st, AuthSigner: signer, Online: func(uuid string) bool { return a.online[uuid] },
		CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
			return nil, fmt.Errorf("no node RPC in this test")
		}})
	a.header = func(token, method, path string) http.Header {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Header()
	}
	a.call = func(token, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	a.cache, err = st.CreateStorage(&store.Storage{Name: "nfsc", Type: proto.StorageNFSCache, Export: "nfs.example.internal:/models", Enabled: true, AllNodes: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.storagePath = fmt.Sprintf("/api/v1/storages/%d", a.cache.ID)
	for _, n := range []**store.Node{&a.nodeA, &a.nodeB} {
		node, _, err := st.CreateNodePending(fmt.Sprintf("node-%c", 'a'+len(a.online)))
		if err != nil {
			t.Fatal(err)
		}
		a.online[node.UUID] = false
		*n = node
	}
	return a
}

// The UI's "switch to NFS" sends {"type":"nfs"}. While node caches hold
// entries the switch is refused with a code, counts and the nodes concerned,
// and the storage stays nfs_cache; with the caches cleared it succeeds.
func TestSwitchNFSCacheStorageToNFSRefusesUntilNodeCachesAreCleared(t *testing.T) {
	a := newSwitchAPI(t)
	present := &store.ModelFile{ModelID: 1, StorageID: a.cache.ID, NodeID: a.nodeA.ID, State: proto.MFPresent, Path: a.cache.CachePath + "/m1"}
	copying := &store.ModelFile{ModelID: 2, StorageID: a.cache.ID, NodeID: a.nodeB.ID, State: proto.MFSyncing, Path: "m2", TransferID: "copy"}
	for _, row := range []*store.ModelFile{present, copying} {
		if err := a.st.DB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := a.call(a.member, http.MethodPatch, a.storagePath, `{"type":"nfs"}`); code != http.StatusForbidden {
		t.Fatalf("member switch = %d, want 403", code)
	}
	code, body := a.call(a.admin, http.MethodPatch, a.storagePath, `{"type":"nfs"}`)
	first, second := a.nodeA.UUID, a.nodeB.UUID
	if a.nodeB.ID < a.nodeA.ID {
		first, second = second, first
	}
	want := fmt.Sprintf(`{"code":"STORAGE_CACHE_NOT_EMPTY","error":"node caches still hold entries (entries: 2, nodes: 2, transferring: 1); clear them before switching to nfs","params":{"entries":2,"node_uuids":[%q,%q],"nodes":2,"transferring":1}}`+"\n", first, second)
	if code != http.StatusConflict || body != want {
		t.Fatalf("switch with node caches = %d %s\nwant 409 %s", code, body, want)
	}
	if row, err := a.st.StorageByID(a.cache.ID); err != nil || row.Type != proto.StorageNFSCache {
		t.Fatalf("refused switch changed the storage: %+v %v", row, err)
	}
	for _, row := range []*store.ModelFile{present, copying} {
		if err := a.st.DB().Delete(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if code, body := a.call(a.admin, http.MethodPatch, a.storagePath, `{"type":"nfs"}`); code != http.StatusOK || !strings.Contains(body, `"type":"nfs"`) {
		t.Fatalf("switch with empty caches = %d %s", code, body)
	}
}

// A failed clear answers with a product error, so a client can say why, and
// keeps the counts and per-node failures it always returned. An offline node
// with nothing cached here has nothing to clear and is not a failure.
func TestClearStorageCacheFailureIsAProductErrorAndSkipsEmptyOfflineNodes(t *testing.T) {
	a := newSwitchAPI(t)
	if code, body := a.call(a.admin, http.MethodDelete, a.storagePath+"/cache", ""); code != http.StatusOK || body != `{"failed":[],"freed_bytes":0,"removed":0}`+"\n" {
		t.Fatalf("clear with only empty offline nodes = %d %s", code, body)
	}
	if err := a.st.DB().Create(&store.ModelFile{ModelID: 1, StorageID: a.cache.ID, NodeID: a.nodeA.ID, State: proto.MFPresent, Path: a.cache.CachePath + "/m1"}).Error; err != nil {
		t.Fatal(err)
	}
	code, body := a.call(a.admin, http.MethodDelete, a.storagePath+"/cache", "")
	want := fmt.Sprintf(`{"code":"STORAGE_CACHE_CLEAR_FAILED","error":"cache could not be cleared on 1 node(s); first %s: node is offline","failed":[{"message":"node is offline","node_name":%q,"node_uuid":%q}],"freed_bytes":0,"params":{"failed":1,"node":%q},"removed":0}`+"\n",
		a.nodeA.Name, a.nodeA.Name, a.nodeA.UUID, a.nodeA.Name)
	if code != http.StatusConflict || body != want {
		t.Fatalf("clear with a cached offline node = %d %s\nwant 409 %s", code, body, want)
	}
	// Like any other product error, the code reaches the access log and usage.
	if got := a.header(a.admin, http.MethodDelete, a.storagePath+"/cache").Get(usageErrorHeader); got != "STORAGE_CACHE_CLEAR_FAILED" {
		t.Fatalf("usage error header = %q", got)
	}
}

// The failure list has one entry per file, but the message counts nodes: two
// files failing on one online node is one node that could not be cleared.
func TestClearStorageCacheFailureCountsNodesNotFiles(t *testing.T) {
	a := newSwitchAPI(t)
	a.online[a.nodeA.UUID] = true
	if err := a.st.ApplyStorageStatus(a.nodeA.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: a.cache.ID, State: proto.StorageStateReady, CachePath: a.cache.CachePath}}}); err != nil {
		t.Fatal(err)
	}
	for i, model := range []uint{1, 2} {
		if err := a.st.DB().Create(&store.ModelFile{ModelID: model, StorageID: a.cache.ID, State: proto.MFPresent, Path: fmt.Sprintf("m%d", i)}).Error; err != nil {
			t.Fatal(err)
		}
		if err := a.st.DB().Create(&store.ModelFile{ModelID: model, StorageID: a.cache.ID, NodeID: a.nodeA.ID, State: proto.MFPresent, Path: fmt.Sprintf("%s/m%d", a.cache.CachePath, i)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	code, body := a.call(a.admin, http.MethodDelete, a.storagePath+"/cache", "")
	if code != http.StatusConflict {
		t.Fatalf("clear with failing removals = %d %s", code, body)
	}
	var got struct {
		Code   string              `json:"code"`
		Failed []map[string]string `json:"failed"`
		Params map[string]any      `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != "STORAGE_CACHE_CLEAR_FAILED" || len(got.Failed) != 2 || got.Params["failed"] != float64(1) || got.Params["node"] != a.nodeA.Name {
		t.Fatalf("two failed files on one node = %s", body)
	}
}
