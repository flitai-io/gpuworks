package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The UI's "switch to NFS" sends {"type":"nfs"}. While node caches hold
// entries the switch is refused with a code and counts the console can show,
// and the storage stays nfs_cache; with the caches cleared it succeeds.
func TestSwitchNFSCacheStorageToNFSRefusesUntilNodeCachesAreCleared(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, member := roleTestToken(t, st, "member")
	_, admin := roleTestToken(t, st, "admin")
	router := Router(Deps{Store: st, AuthSigner: signer})
	call := func(token, method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	cache, err := st.CreateStorage(&store.Storage{Name: "nfsc", Type: proto.StorageNFSCache, Export: "nfs.example.internal:/models", Enabled: true, AllNodes: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/storages/%d", cache.ID)
	present := &store.ModelFile{ModelID: 1, StorageID: cache.ID, NodeID: 7, State: proto.MFPresent, Path: "m1"}
	copying := &store.ModelFile{ModelID: 2, StorageID: cache.ID, NodeID: 8, State: proto.MFSyncing, Path: "m2", TransferID: "copy"}
	for _, row := range []*store.ModelFile{present, copying} {
		if err := st.DB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}

	if code, _ := call(member, http.MethodPatch, path, `{"type":"nfs"}`); code != http.StatusForbidden {
		t.Fatalf("member switch = %d, want 403", code)
	}
	code, body := call(admin, http.MethodPatch, path, `{"type":"nfs"}`)
	want := `{"code":"STORAGE_CACHE_NOT_EMPTY","error":"2 node cache entries on 2 nodes (1 still transferring) must be cleared before switching to nfs","params":{"entries":2,"nodes":2,"transferring":1}}` + "\n"
	if code != http.StatusConflict || body != want {
		t.Fatalf("switch with node caches = %d %s\nwant 409 %s", code, body, want)
	}
	if row, err := st.StorageByID(cache.ID); err != nil || row.Type != proto.StorageNFSCache {
		t.Fatalf("refused switch changed the storage: %+v %v", row, err)
	}

	for _, row := range []*store.ModelFile{present, copying} {
		if err := st.DB().Delete(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if code, body := call(admin, http.MethodPatch, path, `{"type":"nfs"}`); code != http.StatusOK || !strings.Contains(body, `"type":"nfs"`) {
		t.Fatalf("switch with empty caches = %d %s", code, body)
	}
}
