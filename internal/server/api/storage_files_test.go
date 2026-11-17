package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Every storage-files route is admin only, a read token cannot change files,
// and a cross-site browser request cannot either.
func TestStorageFilesRoutesAuthentication(t *testing.T) {
	rig := newStorageFilesRig(t)
	rec := rig.do(rig.admin, http.MethodPost, "/api/v1/platform-tokens", strings.NewReader(`{"name":"ro","scope":"read"}`))
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil || minted.Token == "" {
		t.Fatalf("mint read token: %d %s", rec.Code, rec.Body)
	}
	type route struct {
		method, path, body string
		write              bool
	}
	routes := []route{
		{http.MethodGet, rig.url(rig.shared, "?path=files"), "", false},
		{http.MethodGet, rig.url(rig.shared, "/download?path=files/a"), "", false},
		{http.MethodPut, rig.url(rig.shared, "/upload?path=files/a"), "data", true},
		{http.MethodPost, rig.url(rig.shared, "/mkdir"), `{"path":"files/d"}`, true},
		{http.MethodPost, rig.url(rig.shared, "/rename"), `{"from":"files/a","to":"files/b"}`, true},
		{http.MethodDelete, rig.url(rig.shared, "?path=files/a"), "", true},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			if rec := rig.do("", rt.method, rt.path, strings.NewReader(rt.body)); rec.Code != http.StatusUnauthorized {
				t.Fatalf("anonymous = %d", rec.Code)
			}
			expectError(t, rig.do(rig.member, rt.method, rt.path, strings.NewReader(rt.body)), http.StatusForbidden, ErrorAdministratorRequired)
			if rt.write {
				expectError(t, rig.do(minted.Token, rt.method, rt.path, strings.NewReader(rt.body)), http.StatusForbidden, ErrorPlatformTokenReadOnly)
				expectError(t, rig.do(rig.admin, rt.method, rt.path, strings.NewReader(rt.body), "Origin", "https://evil.example"), http.StatusForbidden, ErrorManagementOriginRejected)
				expectError(t, rig.do(rig.admin, rt.method, rt.path, strings.NewReader(rt.body), "Sec-Fetch-Site", "cross-site"), http.StatusForbidden, ErrorManagementOriginRejected)
			} else if rec := rig.do(minted.Token, rt.method, rt.path, nil); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
				t.Fatalf("an admin's read token cannot read: %d %s", rec.Code, rec.Body)
			}
		})
	}
	// Only the read token's two reads reached the node.
	if got := rig.callCount() + rig.streams.opened(); got != 2 {
		t.Fatalf("%d calls/streams reached the node, want only the read token's 2", got)
	}
}

func TestStorageFilesRoutesUnknownPathAndMethod(t *testing.T) {
	rig := newStorageFilesRig(t)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPatch, rig.url(rig.shared, ""), http.StatusMethodNotAllowed},
		{http.MethodGet, rig.url(rig.shared, "/mkdir"), http.StatusMethodNotAllowed},
		{http.MethodPost, rig.url(rig.shared, "/upload"), http.StatusMethodNotAllowed},
		{http.MethodPut, rig.url(rig.shared, "/download"), http.StatusMethodNotAllowed},
		{http.MethodGet, rig.url(rig.shared, "/elsewhere"), http.StatusNotFound},
	} {
		if rec := rig.do(rig.admin, tc.method, tc.path, nil); rec.Code != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.status)
		}
	}
	if rec := rig.do(rig.admin, http.MethodGet, "/api/v1/storages/999/files?node_uuid="+rig.n1.UUID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown storage = %d %s", rec.Code, rec.Body)
	}
}

// A listing names each entry's path, flags what can be renamed or deleted and
// whether the directory takes new entries, and passes paging to the agent.
func TestListStorageFilesWireAndWritableFlags(t *testing.T) {
	rig := newStorageFilesRig(t)
	rig.setAnswer(func(method string, params json.RawMessage) (json.RawMessage, error) {
		var p proto.StorageFilesListParams
		_ = json.Unmarshal(params, &p)
		if p.Path == "files" {
			return json.RawMessage(`{"entries":[{"name":"a.txt","dir":false,"size":12,"mtime":"2026-10-21T08:00:00Z"},{"name":"ln","dir":false,"symlink":true,"size":4,"mtime":"2026-10-21T09:00:00+01:00"}],"next_cursor":"ln"}`), nil
		}
		return json.RawMessage(`{"entries":[{"name":"files","dir":true,"size":0,"mtime":"2026-10-21T08:00:00Z"}]}`), nil
	})
	rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "?node_uuid="+rig.n2.UUID+"&path=/files/&cursor=a&limit=2"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	want := `{"path":"files","node_uuid":"` + rig.n2.UUID + `","writable":true,"items":[` +
		`{"name":"a.txt","path":"files/a.txt","dir":false,"symlink":false,"size":12,"mtime":"2026-10-21T08:00:00Z","writable":true},` +
		`{"name":"ln","path":"files/ln","dir":false,"symlink":true,"size":4,"mtime":"2026-10-21T08:00:00Z","writable":true}],"next_cursor":"ln"}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("wire = %s\nwant   %s", got, want)
	}
	call := rig.lastCall()
	var params proto.StorageFilesListParams
	if err := json.Unmarshal(call.params, &params); err != nil {
		t.Fatal(err)
	}
	if call.node != rig.n2.UUID || call.method != proto.RPCStorageFilesList || params != (proto.StorageFilesListParams{StorageID: rig.local.ID, Path: "files", Cursor: "a", Limit: 2}) {
		t.Fatalf("call = %s %s %+v", call.node, call.method, params)
	}

	// The root takes no new entries and the region directory itself cannot
	// be renamed or deleted.
	rec = rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "?node_uuid="+rig.n2.UUID), nil)
	if got := strings.TrimSpace(rec.Body.String()); !strings.Contains(got, `"path":"","node_uuid"`) || !strings.Contains(got, `"writable":false,"items":[{"name":"files","path":"files","dir":true,"symlink":false,"size":0,"mtime":"2026-10-21T08:00:00Z","writable":false}],"next_cursor":""`) {
		t.Fatalf("root = %s", got)
	}

	for path, writable := range map[string]bool{".gpuworks": false, ".gpuworks/apps": true, ".gpuworks/apps/x": true, "Qwen/Qwen3-8B": false} {
		rig.setAnswer(nil)
		rec = rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path="+path), nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"writable":`+map[bool]string{true: "true", false: "false"}[writable]+`,"items":[]`) {
			t.Fatalf("%s = %d %s, want writable %v and items []", path, rec.Code, rec.Body, writable)
		}
	}
}

func TestListStorageFilesValidation(t *testing.T) {
	rig := newStorageFilesRig(t)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files/../models"), nil), http.StatusBadRequest, ErrorStorageFilePathInvalid)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files%5Cx"), nil), http.StatusBadRequest, ErrorStorageFilePathInvalid)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?limit=-1"), nil), http.StatusBadRequest, ErrorFieldInvalid)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?limit=many"), nil), http.StatusBadRequest, ErrorFieldInvalid)
	if rig.callCount() != 0 {
		t.Fatal("an invalid request reached the node")
	}
}

// A disabled storage can still be browsed; nothing in it is writable.
func TestDisabledStorageBrowsesReadOnlyAndRefusesChanges(t *testing.T) {
	rig := newStorageFilesRig(t)
	rig.disable()
	rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files"), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"writable":false`) {
		t.Fatalf("list on disabled = %d %s", rec.Code, rec.Body)
	}
	calls := rig.callCount()
	expectError(t, rig.do(rig.admin, http.MethodPost, rig.url(rig.shared, "/mkdir"), strings.NewReader(`{"path":"files/d"}`)), http.StatusConflict, ErrorStorageDisabled)
	expectError(t, rig.do(rig.admin, http.MethodDelete, rig.url(rig.shared, "?path=files/a"), nil), http.StatusConflict, ErrorStorageDisabled)
	expectError(t, rig.do(rig.admin, http.MethodPost, rig.url(rig.shared, "/rename"), strings.NewReader(`{"from":"files/a","to":"files/b"}`)), http.StatusConflict, ErrorStorageDisabled)
	if rig.callCount() != calls {
		t.Fatal("a change to a disabled storage reached the node")
	}
}

func TestStorageFilesMutationsSendTheRPCAndAnswer(t *testing.T) {
	rig := newStorageFilesRig(t)
	rec := rig.do(rig.admin, http.MethodPost, rig.url(rig.shared, "/mkdir"), strings.NewReader(`{"path":"/files/new//d/"}`))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"path":"files/new/d"}` {
		t.Fatalf("mkdir = %d %s", rec.Code, rec.Body)
	}
	if call := rig.lastCall(); call.method != proto.RPCStorageFilesMkdir || string(call.params) != `{"storage_id":`+itoa(rig.shared.ID)+`,"path":"files/new/d"}` {
		t.Fatalf("mkdir call = %s %s", call.method, call.params)
	}
	rec = rig.do(rig.admin, http.MethodPost, rig.url(rig.local, "/rename"), strings.NewReader(`{"node_uuid":"`+rig.n1.UUID+`","from":"files/a","to":".gpuworks/apps/x/a"}`))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"path":".gpuworks/apps/x/a"}` {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body)
	}
	if call := rig.lastCall(); call.node != rig.n1.UUID || call.method != proto.RPCStorageFilesRename || string(call.params) != `{"storage_id":`+itoa(rig.local.ID)+`,"from":"files/a","to":".gpuworks/apps/x/a"}` {
		t.Fatalf("rename call = %s %s %s", call.node, call.method, call.params)
	}
	rec = rig.do(rig.admin, http.MethodDelete, rig.url(rig.shared, "?path=files/a"), nil)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d %q", rec.Code, rec.Body)
	}
	if call := rig.lastCall(); call.method != proto.RPCStorageFilesDelete || string(call.params) != `{"storage_id":`+itoa(rig.shared.ID)+`,"path":"files/a"}` {
		t.Fatalf("delete call = %s %s", call.method, call.params)
	}
}

func TestStorageFilesMutationValidation(t *testing.T) {
	rig := newStorageFilesRig(t)
	post := func(suffix, body string) *httptest.ResponseRecorder {
		return rig.do(rig.admin, http.MethodPost, rig.url(rig.shared, suffix), strings.NewReader(body))
	}
	expectError(t, post("/mkdir", `{"path":`), http.StatusBadRequest, ErrorFieldInvalid)
	expectError(t, post("/mkdir", `{"path":"files/d"} {}`), http.StatusBadRequest, ErrorFieldInvalid)
	expectError(t, post("/mkdir", `{"path":"files/../x"}`), http.StatusBadRequest, ErrorStorageFilePathInvalid)
	expectError(t, post("/mkdir", `{"path":"models/x"}`), http.StatusForbidden, ErrorStorageFileRegionReadonly)
	expectError(t, post("/mkdir", `{"path":"files"}`), http.StatusForbidden, ErrorStorageFileRegionReadonly)
	expectError(t, post("/mkdir", `{"path":".gpuworks/trash/x"}`), http.StatusForbidden, ErrorStorageFileRegionReadonly)
	expectError(t, post("/mkdir", `{"path":""}`), http.StatusForbidden, ErrorStorageFileRegionReadonly)
	expectError(t, post("/rename", `{"from":"files","to":"files/b"}`), http.StatusForbidden, ErrorStorageFileRegionReadonly)
	expectError(t, post("/rename", `{"from":"files/a","to":".gpuworks/apps"}`), http.StatusForbidden, ErrorStorageFileRegionReadonly)
	expectError(t, post("/rename", `{"from":"files/a","to":"files/a"}`), http.StatusBadRequest, ErrorStorageFilePathInvalid)
	expectError(t, post("/rename", `{"from":"files/a","to":"files/a/b"}`), http.StatusBadRequest, ErrorStorageFilePathInvalid)
	expectError(t, rig.do(rig.admin, http.MethodDelete, rig.url(rig.shared, "?path=.gpuworks/apps"), nil), http.StatusForbidden, ErrorStorageFileRegionReadonly)
	expectError(t, rig.do(rig.admin, http.MethodDelete, rig.url(rig.shared, "?path=../files/a"), nil), http.StatusBadRequest, ErrorStorageFilePathInvalid)
	if rig.callCount() != 0 {
		t.Fatal("an invalid request reached the node")
	}
}

// The agent's refusals keep their codes; anything else is an operation that
// failed on the node.
func TestStorageFilesAgentRefusalsMapToProductErrors(t *testing.T) {
	rig := newStorageFilesRig(t)
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{proto.NewStorageFilesError(proto.StorageFilesErrPathInvalid, "resolves outside the root"), http.StatusBadRequest, ErrorStorageFilePathInvalid},
		{proto.NewStorageFilesError(proto.StorageFilesErrRegionReadonly, "outside the regions"), http.StatusForbidden, ErrorStorageFileRegionReadonly},
		{proto.NewStorageFilesError(proto.StorageFilesErrNotFound, "files/a"), http.StatusNotFound, ErrorStorageFileNotFound},
		{proto.NewStorageFilesError(proto.StorageFilesErrExists, "files/b"), http.StatusConflict, ErrorStorageFileExists},
		{proto.NewStorageFilesError(proto.StorageFilesErrNotWritable, "read-only mount"), http.StatusConflict, ErrorStorageNotWritable},
		{proto.NewStorageFilesError(proto.StorageFilesErrExtractFailed, "bad archive"), http.StatusUnprocessableEntity, ErrorStorageFileExtractFailed},
		{proto.NewStorageFilesError(proto.StorageFilesErrFailed, "EIO"), http.StatusBadGateway, ErrorStorageFileOperationFailed},
		{fmt.Errorf("rpc: %w", proto.NewStorageFilesError(proto.StorageFilesErrExists, "x")), http.StatusConflict, ErrorStorageFileExists},
		// The hub turns an agent's refusal (coded or, from an older agent,
		// worded) into the typed error before the API sees it; text alone is
		// an operation that failed.
		{errors.New("rpc: " + proto.NewStorageFilesError(proto.StorageFilesErrExists, "x").Error()), http.StatusBadGateway, ErrorStorageFileOperationFailed},
		{errors.New("context deadline exceeded"), http.StatusBadGateway, ErrorStorageFileOperationFailed},
	} {
		rig.setAnswer(func(string, json.RawMessage) (json.RawMessage, error) { return nil, tc.err })
		expectError(t, rig.do(rig.admin, http.MethodPost, rig.url(rig.shared, "/rename"), strings.NewReader(`{"from":"files/a","to":"files/b"}`)), tc.status, tc.code)
		expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files"), nil), tc.status, tc.code)
	}
	rig.setAnswer(func(string, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`[`), nil })
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files"), nil), http.StatusBadGateway, ErrorStorageFileOperationFailed)
}

// A shared storage picks a node itself: online, ready, a storage-files agent
// and an attached data lane. When none fits, the reason is the answer.
func TestSharedStorageFilesNodeSelection(t *testing.T) {
	rig := newStorageFilesRig(t)
	rig.streams.setAttached(rig.n1.UUID, false)
	if rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files"), nil); rec.Code != http.StatusOK || rig.lastCall().node != rig.n2.UUID {
		t.Fatalf("picked %s (%d %s), want the node with a data lane", rig.lastCall().node, rec.Code, rec.Body)
	}
	// A read prefers a node that can also write.
	rig.streams.setAttached(rig.n1.UUID, true)
	rig.setStorageState(rig.n1, true, false)
	if rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files"), nil); rec.Code != http.StatusOK || rig.lastCall().node != rig.n2.UUID || !strings.Contains(rec.Body.String(), `"writable":true`) {
		t.Fatalf("read pick = %s %s", rig.lastCall().node, rec.Body)
	}
	// Writes need a writable node; with none left the storage takes none.
	rig.setStorageState(rig.n2, true, false)
	expectError(t, rig.do(rig.admin, http.MethodPost, rig.url(rig.shared, "/mkdir"), strings.NewReader(`{"path":"files/d"}`)), http.StatusConflict, ErrorStorageNotWritable)
	rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files"), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"writable":false`) {
		t.Fatalf("read-only list = %d %s", rec.Code, rec.Body)
	}
	rig.setStorageState(rig.n1, true, true)
	rig.setStorageState(rig.n2, true, true)

	rig.streams.setAttached(rig.n1.UUID, false)
	rig.streams.setAttached(rig.n2.UUID, false)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files"), nil), http.StatusServiceUnavailable, ErrorStorageFileDataLaneUnavailable)
	rig.streams.setAttached(rig.n1.UUID, true)
	rig.streams.setAttached(rig.n2.UUID, true)

	rig.setCaps(rig.n1.UUID)
	rig.setCaps(rig.n2.UUID, proto.CapWebterm)
	rec = rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files"), nil)
	expectError(t, rec, http.StatusConflict, ErrorStorageFileAgentUnsupported)
	if !strings.Contains(rec.Body.String(), `"params":{"node":"gpu1"}`) {
		t.Fatalf("unsupported params = %s", rec.Body)
	}
	rig.setCaps(rig.n1.UUID, proto.CapStorageFiles)
	rig.setCaps(rig.n2.UUID, proto.CapStorageFiles)

	rig.setOnline(rig.n1.UUID, false)
	rig.setOnline(rig.n2.UUID, false)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files"), nil), http.StatusConflict, ErrorStorageUnavailableOnNode)
	expectError(t, rig.do(rig.admin, http.MethodPost, rig.url(rig.shared, "/mkdir"), strings.NewReader(`{"path":"files/d"}`)), http.StatusConflict, ErrorStorageNotWritable)
}

// A named node must be online with the storage ready; a local storage needs
// one named.
func TestNamedStorageFilesNode(t *testing.T) {
	rig := newStorageFilesRig(t)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "?path=files"), nil), http.StatusBadRequest, ErrorStorageFileNodeRequired)
	expectError(t, rig.do(rig.admin, http.MethodPost, rig.url(rig.local, "/mkdir"), strings.NewReader(`{"path":"files/d"}`)), http.StatusBadRequest, ErrorStorageFileNodeRequired)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "?node_uuid=nope"), nil), http.StatusConflict, ErrorStorageUnavailableOnNode)

	rig.setOnline(rig.n1.UUID, false)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "?node_uuid="+rig.n1.UUID), nil), http.StatusConflict, ErrorNodeOffline)
	rig.setOnline(rig.n1.UUID, true)

	rig.setStorageState(rig.n1, false, false)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?node_uuid="+rig.n1.UUID), nil), http.StatusConflict, ErrorStorageUnavailableOnNode)
	rig.setStorageState(rig.n1, true, false)
	expectError(t, rig.do(rig.admin, http.MethodPost, rig.url(rig.local, "/mkdir"), strings.NewReader(`{"node_uuid":"`+rig.n1.UUID+`","path":"files/d"}`)), http.StatusConflict, ErrorStorageNotWritable)
	if rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "?node_uuid="+rig.n1.UUID+"&path=files"), nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"writable":false`) {
		t.Fatalf("read-only node list = %d %s", rec.Code, rec.Body)
	}
	rig.setStorageState(rig.n1, true, true)

	// A metadata RPC on a named node does not need the data lane.
	rig.streams.setAttached(rig.n1.UUID, false)
	if rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "?node_uuid="+rig.n1.UUID), nil); rec.Code != http.StatusOK {
		t.Fatalf("list without lane = %d %s", rec.Code, rec.Body)
	}
	rig.setCaps(rig.n1.UUID)
	expectError(t, rig.do(rig.admin, http.MethodGet, rig.url(rig.local, "?node_uuid="+rig.n1.UUID), nil), http.StatusConflict, ErrorStorageFileAgentUnsupported)
}

func itoa(id uint) string {
	b, _ := json.Marshal(id)
	return string(b)
}

// An extraction or operation failure carries the agent's reason as the detail
// parameter: the localized text has no other way to say which entry or which
// system call failed.
func TestStorageFilesFailureDetailParam(t *testing.T) {
	rig := newStorageFilesRig(t)
	for _, tc := range []struct {
		err  error
		code string
	}{
		{proto.NewStorageFilesError(proto.StorageFilesErrExtractFailed, "archive entry ../x escapes"), ErrorStorageFileExtractFailed},
		{proto.NewStorageFilesError(proto.StorageFilesErrFailed, "EIO"), ErrorStorageFileOperationFailed},
	} {
		rig.setAnswer(func(string, json.RawMessage) (json.RawMessage, error) { return nil, tc.err })
		rec := rig.do(rig.admin, http.MethodGet, rig.url(rig.shared, "?path=files"), nil)
		var body struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		want := tc.err.(*proto.StorageFilesError).Detail
		if body.Code != tc.code || body.Params["detail"] != want {
			t.Fatalf("body = %s, want code %s with detail %q", rec.Body, tc.code, want)
		}
	}
}
