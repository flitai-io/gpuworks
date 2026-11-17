package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStorageUsageAggregatesManagedBytesAndDiskWatermark(t *testing.T) {
	st, err := store.Open("file:api_storage_usage_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("node-storage", store.HashToken("t"), "storage")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys:       proto.SysInfo{Hostname: "storage", DiskTotalKB: 1024 * 1024},
		PathUsage: []proto.PathUsage{{Kind: "models", Path: "/models", TotalBytes: 1000, AvailableBytes: 400}, {Kind: "docker", Path: "/var/lib/docker", TotalBytes: 1000, AvailableBytes: 400}},
	}); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "local", SourceID: "Qwen2", Revision: "local", Name: "Qwen2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFRegistered, SizeBytes: 250, Downloaded: 250}); err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{ImageRef: "example/vllm:1", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: node.ID, State: store.ImageStatePresent, SizeBytes: 100}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).storageUsage(rec, httptest.NewRequest(http.MethodGet, "/api/v1/storages/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got []dto.StorageNodeUsage
	if err := unmarshalList(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ModelBytes != 250 || got[0].ImageBytes != 100 || got[0].UsedBytes != 600 || got[0].OtherBytes != 250 || got[0].AvailableBytes != 400 {
		t.Fatalf("usage = %+v", got)
	}
	if len(got[0].PathUsage) != 2 {
		t.Fatalf("path usage = %+v", got[0].PathUsage)
	}
}

// The models disk is the models entry an agent reports: a docker data root
// on another filesystem must not stand in for it (it used to overwrite it).
func TestStorageUsageReadsTheModelsDiskNotDocker(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("node-disks", store.HashToken("t"), "disks")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "disks"},
		PathUsage: []proto.PathUsage{{Kind: "models", Path: "/data/models", TotalBytes: 4000, AvailableBytes: 3000},
			{Kind: "docker", Path: "/var/lib/docker", TotalBytes: 100, AvailableBytes: 10}},
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).storageUsage(rec, httptest.NewRequest(http.MethodGet, "/api/v1/storages/usage", nil))
	var got []dto.StorageNodeUsage
	if err := unmarshalList(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TotalBytes != 4000 || got[0].AvailableBytes != 3000 {
		t.Fatalf("usage = %+v, want the models disk", got)
	}
}

// The storage wire: the default local storage a fresh database starts with,
// admin-only writes, validation, the default mark moving, settings, and a
// storage a deployment reads from refusing removal.
func TestStoragesAPI(t *testing.T) {
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
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	seededRow, err := st.StorageByID(1)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := json.Marshal(seededRow.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	seeded := `{"updated_at":` + string(revision) + `,"id":1,"name":"default","type":"local","enabled":true,"is_default":true,"all_nodes":true,"path":"/opt/gpuworks/models","targets":[],"nodes":{"total":0,"ready":0,"problems":[]},"writable":true,"model_count":0,"deployment_ids":[]}`
	if code, _ := call("", http.MethodGet, "/api/v1/storages", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous list = %d", code)
	}
	if code, body := call(member, http.MethodGet, "/api/v1/storages", ""); code != http.StatusOK || body != `{"items":[`+seeded+`],"total":1}` {
		t.Fatalf("list = %d %s", code, body)
	}
	if code, _ := call(member, http.MethodPost, "/api/v1/storages", `{"name":"nvme","type":"local","path":"/nvme/models"}`); code != http.StatusForbidden {
		t.Fatalf("member create = %d, want 403", code)
	}
	for _, bad := range []struct{ body, code string }{
		{`{"name":"NVMe","type":"local"}`, ErrorStorageNameInvalid},
		{`{"name":"nvme","type":"s3"}`, ErrorStorageTypeInvalid},
		{`{"name":"nvme","type":"local","path":"relative/dir"}`, ErrorStoragePathInvalid},
		{`{"name":"nfs1","type":"nfs","export":"no-colon"}`, ErrorStorageExportInvalid},
		{`{"name":"nfs1","type":"nfs","export":"h:/x","mount_options":"rw;reboot"}`, ErrorStorageOptionsInvalid},
		{`{"name":"nvme","type":"local","path":"/nvme","all_nodes":false}`, ErrorStorageTargetsRequired},
		// A local storage states its directory; it never falls back to the agent's.
		{`{"name":"nvme","type":"local"}`, ErrorStoragePathRequired},
		{`{"name":"nvme","type":"local","path":"/data/../models"}`, ErrorStoragePathInvalid},
		{`{"name":"nvme","type":"local","path":"/"}`, ErrorStoragePathInvalid},
		{`{"name":"nvme","type":"local","path":"/etc/models"}`, ErrorStoragePathDenied},
		{`{"name":"cache","type":"nfs_cache","export":"h:/x","cache_max_bytes":-1}`, ErrorStorageCacheLimitNegative},
		{`{"name":"nvme","type":"local","path":"/nvme","targets":[{"node_uuid":"any","cache_max_bytes":-1}]}`, ErrorStorageCacheLimitNegative},
	} {
		code, body := call(admin, http.MethodPost, "/api/v1/storages", bad.body)
		if code != http.StatusBadRequest || !strings.Contains(body, `"code":"`+bad.code+`"`) {
			t.Fatalf("create %s = %d %s, want %s", bad.body, code, body, bad.code)
		}
	}
	if code, body := call(admin, http.MethodDelete, "/api/v1/storages/1", ""); code != http.StatusConflict || !strings.Contains(body, `"code":"`+ErrorStorageIsDefault+`"`) {
		t.Fatalf("delete default = %d %s", code, body)
	}
	if row, err := st.StorageByID(1); err != nil || !row.IsDefault {
		t.Fatalf("default storage after refused delete = %+v err=%v", row, err)
	}
	code, body := call(admin, http.MethodPost, "/api/v1/storages", `{"name":"nvme","type":"local","path":"/nvme/models","is_default":true}`)
	var created dto.Storage
	if err := json.Unmarshal([]byte(body), &created); err != nil || code != http.StatusCreated || !created.IsDefault || !created.AllNodes || created.Path != "/nvme/models" {
		t.Fatalf("create = %d %s", code, body)
	}
	// The default mark moved to the new storage.
	first, err := st.StorageByID(1)
	if err != nil || first.IsDefault {
		t.Fatalf("the previous default kept its mark: %+v err=%v", first, err)
	}
	if code, body := call(admin, http.MethodPatch, "/api/v1/storages/1", `{"enabled":false}`); code != http.StatusOK || !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("disable = %d %s", code, body)
	}
	if code, body := call(admin, http.MethodPatch, "/api/v1/storages/1", `{"type":"nfs"}`); code != http.StatusBadRequest || !strings.Contains(body, ErrorStorageTypeInvalid) {
		t.Fatalf("local to nfs = %d %s", code, body)
	}
	if code, body := call(admin, http.MethodPatch, "/api/v1/storages/9", `{"enabled":true}`); code != http.StatusNotFound {
		t.Fatalf("unknown storage patch = %d %s", code, body)
	}
	// A deployment limited to the previous storage keeps it. That storage is
	// no longer the default, so the refusal names the deployment.
	if _, err := st.CreateDeploymentWithRequirements(&store.Deployment{Name: "d", ServedName: "d", SaveToStorageID: first.ID},
		nodelabel.Requirements{nodelabel.KeyStorage: {strconv.FormatUint(uint64(first.ID), 10)}}); err != nil {
		t.Fatal(err)
	}
	if code, body := call(admin, http.MethodDelete, "/api/v1/storages/1", ""); code != http.StatusConflict || !strings.Contains(body, ErrorStorageInUse) {
		t.Fatalf("delete a storage a deployment is limited to = %d %s", code, body)
	}
	if code, body := call(admin, http.MethodDelete, fmt.Sprintf("/api/v1/storages/%d", created.ID), ""); code != http.StatusConflict || !strings.Contains(body, `"code":"`+ErrorStorageIsDefault+`"`) {
		t.Fatalf("delete current default = %d %s", code, body)
	}
	spareCode, spareBody := call(admin, http.MethodPost, "/api/v1/storages", `{"name":"spare","type":"local","path":"/spare/models"}`)
	var spare dto.Storage
	if err := json.Unmarshal([]byte(spareBody), &spare); err != nil || spareCode != http.StatusCreated || spare.IsDefault {
		t.Fatalf("spare = %d %s", spareCode, spareBody)
	}
	if code, _ := call(admin, http.MethodDelete, fmt.Sprintf("/api/v1/storages/%d", spare.ID), ""); code != http.StatusNoContent {
		t.Fatalf("delete unbound storage = %d", code)
	}
	if code, body := call(member, http.MethodGet, "/api/v1/storages/settings", ""); code != http.StatusOK || !strings.Contains(body, `"control_plane_name":"gpuworks-`) {
		t.Fatalf("settings = %d %s", code, body)
	}
	if code, body := call(admin, http.MethodPatch, "/api/v1/storages/settings", `{"control_plane_name":"team-a","reserve_free_bytes":1073741824}`); code != http.StatusOK || !strings.Contains(body, `"control_plane_name":"team-a"`) || !strings.Contains(body, `"reserve_free_bytes":1073741824`) {
		t.Fatalf("settings patch = %d %s", code, body)
	}
	if code, _ := call(admin, http.MethodPatch, "/api/v1/storages/settings", `{"reserve_free_bytes":-1}`); code != http.StatusBadRequest {
		t.Fatalf("negative reserve = %d", code)
	}
}

// A shared storage is probed before it is saved: a probe that fails refuses
// the create, and one that mounts but cannot write saves it disabled.
func TestCreateSharedStorageProbesFirst(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gpu3", store.HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	result := proto.StorageProbeResult{OK: false, Reason: "missing_client", Message: "mount helper missing"}
	var probed []proto.StorageProbeParams
	h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true },
		CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
			if uuid != node.UUID || method != proto.RPCStorageProbe {
				t.Fatalf("call %s %s", uuid, method)
			}
			probed = append(probed, params.(proto.StorageProbeParams))
			return json.Marshal(result)
		}}}
	create := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		h.createStorage(rec, httptest.NewRequest(http.MethodPost, "/api/v1/storages", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	const body = `{"name":"model-archive","type":"nfs","export":"nfs.example.internal:/export/models"}`
	if code, out := create(body); code != http.StatusConflict || !strings.Contains(out, ErrorStorageProbeFailed) {
		t.Fatalf("failed probe create = %d %s", code, out)
	}
	if len(probed) != 1 || probed[0].Path != "/mnt/gpuworks/model-archive" || probed[0].Export != "nfs.example.internal:/export/models" {
		t.Fatalf("probe params = %+v, want the default mount point", probed)
	}
	result = proto.StorageProbeResult{OK: true, Readable: true, Writable: false, TotalBytes: 100, AvailableBytes: 40}
	code, out := create(body)
	var created dto.Storage
	if err := json.Unmarshal([]byte(out), &created); err != nil || code != http.StatusCreated || created.Enabled || created.TotalBytes != 100 {
		t.Fatalf("read-only create = %d %s, want saved disabled", code, out)
	}
	if code, out := create(`{"name":"other","type":"nfs","export":"NFS.example.internal:/export/models/"}`); code != http.StatusConflict || !strings.Contains(out, ErrorStorageExportTaken) {
		t.Fatalf("same export twice = %d %s", code, out)
	}
	// An nfs_cache storage without a cache directory gets a stated one, never
	// a directory under the agent's state directory.
	result = proto.StorageProbeResult{OK: true, Readable: true, Writable: true}
	code, out = create(`{"name":"team-cache","type":"nfs_cache","export":"nfs.example.internal:/export/cache"}`)
	var cached dto.Storage
	if err := json.Unmarshal([]byte(out), &cached); err != nil || code != http.StatusCreated || cached.CachePath != "/opt/gpuworks/cache/team-cache" {
		t.Fatalf("nfs_cache create = %d %s, want the default cache directory", code, out)
	}
}

// A rescan notices model files someone else deleted from a shared storage (and
// ones that came back), but only marks a file missing after the node confirms
// the directory is gone.
func TestRescanReconcilesSharedModelFiles(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gpu3", store.HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := st.CreateStorage(&store.Storage{Name: "team-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/team-nfs", Export: "nfs.example.internal:/export/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: shared.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/team-nfs", Writable: true},
	}}); err != nil {
		t.Fatal(err)
	}
	files := map[string]uint{}
	for i, c := range []struct{ rel, state string }{
		{"Qwen/Qwen3-8B", proto.MFPresent},           // deleted by another control plane
		{"Qwen/Qwen3-4B", proto.MFPresent},           // not listed, but stat finds it
		{"Qwen/Qwen3-0.6B", proto.MFMissing},         // came back
		{"deepseek-ai/DeepSeek-R1", proto.MFPresent}, // on the next page
	} {
		m, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: c.rel, Name: fmt.Sprintf("m%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		mf, err := st.UpsertModelFile(&store.ModelFile{ModelID: m.ID, StorageID: shared.ID, Path: c.rel, State: c.state, SizeBytes: 10})
		if err != nil {
			t.Fatal(err)
		}
		files[c.rel] = mf.ID
	}
	var stats []string
	h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true },
		CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
			switch method {
			case proto.RPCStorageScan:
				// The first page ends at Qwen/Qwen3-9B; deepseek-ai/* sorts after it.
				return json.Marshal(proto.StorageScanResult{
					Candidates: []proto.StorageScanCandidate{{RelPath: "Qwen/Qwen3-0.6B", SizeBytes: 12}, {RelPath: "Qwen/Qwen3-9B", SizeBytes: 5}},
					NextCursor: "Qwen/Qwen3-9B",
				})
			case proto.RPCFSStat:
				p := params.(proto.FSStatParams).Path
				stats = append(stats, p)
				if p == "/mnt/gpuworks/team-nfs/Qwen/Qwen3-8B" {
					// The agent's coded refusal for a missing path, with the
					// sentence os.Stat writes.
					return nil, &proto.RPCError{Code: proto.ErrorPathNotFound, Message: fmt.Sprintf("stat %s: no such file or directory", p)}
				}
				return json.Marshal(proto.FSStatResult{Path: p, Dir: true})
			}
			t.Fatalf("unexpected call %s", method)
			return nil, nil
		}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/storages/1/scan", strings.NewReader(`{}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fmt.Sprint(shared.ID))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.scanStorage(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rel_path":"Qwen/Qwen3-0.6B"`) || !strings.Contains(rec.Body.String(), `"imported":true`) {
		t.Fatalf("scan = %d %s", rec.Code, rec.Body.String())
	}
	sort.Strings(stats)
	if strings.Join(stats, ",") != "/mnt/gpuworks/team-nfs/Qwen/Qwen3-4B,/mnt/gpuworks/team-nfs/Qwen/Qwen3-8B" {
		t.Fatalf("stat calls = %v", stats)
	}
	for rel, want := range map[string]string{
		"Qwen/Qwen3-8B": proto.MFMissing, "Qwen/Qwen3-4B": proto.MFPresent,
		"Qwen/Qwen3-0.6B": proto.MFPresent, "deepseek-ai/DeepSeek-R1": proto.MFPresent,
	} {
		mf, err := st.ModelFileByID(files[rel])
		if err != nil || mf.State != want {
			t.Errorf("%s = %+v err=%v, want %s", rel, mf, err, want)
		}
	}
	if mf, _ := st.ModelFileByID(files["Qwen/Qwen3-0.6B"]); mf.SizeBytes != 12 {
		t.Errorf("restored size = %d, want the scanned 12", mf.SizeBytes)
	}
}

// A node that is not connected is shown offline, whether it reported the
// storage before (stale) or never did; a connected node keeps what it
// reported.
func TestStorageNodeStatusOfDisconnectedNodes(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	local, err := st.StorageByID(st.DefaultLocalStorageID())
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{"never-reported": store.NodeStateOffline, "went-away": store.NodeStateOffline, "online": store.NodeStateOnline}
	for name, state := range states {
		node, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		if name != "never-reported" {
			if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: local.ID, State: proto.StorageStateReady, Path: "/data/models", Writable: true}}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.SetNodeState(node.UUID, state); err != nil {
			t.Fatal(err)
		}
	}
	h := &apiHandlers{d: Deps{Store: st}}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	out := h.storageDTO(local, nodes, true)
	got := map[string]string{}
	for _, n := range out.Nodes.All {
		got[n.NodeName] = n.State
	}
	want := map[string]string{"never-reported": "offline", "went-away": "offline", "online": proto.StorageStateReady}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %q, want %q (all: %v)", name, got[name], w, got)
		}
	}
	if out.Nodes.Ready != 1 || out.Nodes.Total != 3 {
		t.Errorf("ready %d of %d, want 1 of 3", out.Nodes.Ready, out.Nodes.Total)
	}
}

// The add-storage form probes before it has saved any targets: without a
// named node the probe runs on an online node, and a named one is used as is.
func TestProbeStorageWithoutANamedNode(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	offline, err := st.RegisterNode("gpu2", store.HashToken("a"), "gpu2")
	if err != nil {
		t.Fatal(err)
	}
	online, err := st.RegisterNode("gpu3", store.HashToken("b"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	var probedOn []string
	h := &apiHandlers{d: Deps{Store: st, Online: func(uuid string) bool { return uuid == online.UUID },
		CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
			probedOn = append(probedOn, uuid)
			return json.Marshal(proto.StorageProbeResult{OK: false, Reason: "path_missing", Message: "the export path does not exist on the server"})
		}}}
	probe := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		h.probeStorage(rec, httptest.NewRequest(http.MethodPost, "/api/v1/storages/probe", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	code, out := probe(`{"type":"nfs","path":"/mnt/gpuworks/bad-nfs","export":"nfs.example.internal:/export/does-not-exist"}`)
	if code != http.StatusOK || !strings.Contains(out, `"reason":"path_missing"`) || len(probedOn) != 1 || probedOn[0] != online.UUID {
		t.Fatalf("probe = %d %s on %v, want the failed probe result from the online node", code, out, probedOn)
	}
	if code, out := probe(fmt.Sprintf(`{"type":"nfs","path":"/mnt/x","export":"h:/x","node_uuid":%q}`, offline.UUID)); code != http.StatusConflict || !strings.Contains(out, ErrorStorageProbeFailed) {
		t.Fatalf("probe on an offline node = %d %s", code, out)
	}
}

// A model's file in a shared storage says whether this gpuworks may change
// that storage, so the UI does not offer deleting from a read-only library.
func TestModelStoragePresenceSaysWhetherTheStorageIsWritable(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gpu3", store.HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name, export string, enabled, writable bool) *store.Storage {
		t.Helper()
		row, err := st.CreateStorage(&store.Storage{Name: name, Type: proto.StorageNFS, Enabled: enabled, AllNodes: true,
			Path: "/mnt/gpuworks/" + name, Export: export}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: row.ID, Path: "Qwen/Qwen3-0.6B", State: proto.MFPresent, SizeBytes: 10}); err != nil {
			t.Fatal(err)
		}
		return row
	}
	team := mk("team-nfs", "nfs.example.internal:/export/team", true, true)
	library := mk("library", "nfs.example.internal:/export/library", false, false)
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: team.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/team-nfs", Writable: true},
		{ID: library.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/library", Writable: false},
	}}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	var d dto.Model
	h.fillModelPresence(&d, model.ID)
	got := map[string]bool{}
	for _, p := range d.Storages {
		got[p.StorageName] = p.Writable
	}
	if len(got) != 2 || !got["team-nfs"] || got["library"] {
		t.Fatalf("writable by storage = %v, want team-nfs only", got)
	}
	raw, _ := json.Marshal(d.Storages[0])
	if !strings.Contains(string(raw), `"writable":`) {
		t.Fatalf("wire JSON lacks writable: %s", raw)
	}

	// A node's own files: deletable unless they are in a read-only local
	// storage; a node cache stays deletable whatever its storage's setting.
	cache, err := st.CreateStorage(&store.Storage{Name: "team-cache", Type: proto.StorageNFSCache, Enabled: false, AllNodes: true,
		Path: "/mnt/gpuworks/team-cache", Export: "nfs.example.internal:/export/cache"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	local := st.DefaultLocalStorageID()
	for _, mf := range []store.ModelFile{
		{ModelID: model.ID, StorageID: local, NodeID: node.ID, Path: "/opt/gpuworks/models/q", State: proto.MFPresent},
		{ModelID: model.ID, StorageID: cache.ID, NodeID: node.ID, Path: "/opt/gpuworks/cache/team-cache/q", State: proto.MFPresent},
	} {
		mf := mf
		if _, err := st.UpsertModelFile(&mf); err != nil {
			t.Fatal(err)
		}
	}
	disabled := false
	if _, err := st.UpdateStorage(local, store.StoragePatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	d = dto.Model{}
	h.fillModelPresence(&d, model.ID)
	deletable := map[string]bool{}
	for _, p := range d.Nodes {
		deletable[p.StorageType] = p.Deletable
	}
	if len(deletable) != 2 || deletable[proto.StorageLocal] || !deletable[proto.StorageNFSCache] {
		t.Fatalf("deletable by storage type = %v", deletable)
	}
	for _, p := range d.Nodes {
		raw, _ = json.Marshal(p)
		if has := strings.Contains(string(raw), `"deletable":true`); has != p.Deletable {
			t.Fatalf("wire JSON for deletable=%v: %s", p.Deletable, raw)
		}
	}
}

func TestStorageUsageCapacityKnownWireContract(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		paths                  []proto.PathUsage
		known                  bool
		total, available, used int64
	}{
		{name: "never reported"},
		{name: "unavailable statfs", paths: []proto.PathUsage{{Kind: "models", Path: "/models"}}},
		{name: "empty disk", paths: []proto.PathUsage{{Kind: "models", Path: "/models", TotalBytes: 1000, AvailableBytes: 1000}}, known: true, total: 1000, available: 1000},
		{name: "full disk", paths: []proto.PathUsage{{Kind: "models", Path: "/models", TotalBytes: 1000}}, known: true, total: 1000, used: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node, err := st.RegisterNode("node-capacity", store.HashToken("t"), "storage")
			if err != nil {
				t.Fatal(err)
			}
			if tc.paths != nil {
				if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{Sys: proto.SysInfo{Hostname: "storage"}, PathUsage: tc.paths}); err != nil {
					t.Fatal(err)
				}
			}
			rec := httptest.NewRecorder()
			(&apiHandlers{d: Deps{Store: st}}).storageUsage(rec, httptest.NewRequest(http.MethodGet, "/api/v1/storages/usage", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			var got []map[string]any
			if err := unmarshalList(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"node_uuid": node.UUID, "hostname": "storage", "capacity_known": tc.known, "total_bytes": float64(tc.total), "available_bytes": float64(tc.available), "used_bytes": float64(tc.used), "model_bytes": float64(0), "image_bytes": float64(0), "other_bytes": float64(tc.used)}
			n, err := st.NodeByUUID(node.UUID)
			if err != nil {
				t.Fatal(err)
			}
			want["observed_at"] = nil
			if n.PathUsageObservedAt != nil {
				want["observed_at"] = n.PathUsageObservedAt.Format(time.RFC3339Nano)
			}
			if tc.paths != nil {
				want["path_usage"] = []any{map[string]any{"kind": "models", "path": "/models", "total_bytes": float64(tc.total), "available_bytes": float64(tc.available)}}
			}
			if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Fatalf("usage wire = %s, want %+v", rec.Body.String(), want)
			}
			var decoded dto.List[dto.StorageNodeUsage]
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip []map[string]any
			if err := unmarshalList(raw, &roundTrip); err != nil || !reflect.DeepEqual(roundTrip, got) || decoded.Total != 1 {
				t.Fatalf("DTO round trip = %s, total=%d, err=%v", raw, decoded.Total, err)
			}
		})
	}
}
