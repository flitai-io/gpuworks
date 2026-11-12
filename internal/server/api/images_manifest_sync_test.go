package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const upstreamManifest = `{"schema_version":3,"catalog_version":"2026-10-09.abcdef12","series":[{"name":"vLLM · CUDA","engine":"vllm","purpose":"inference","framework":"cuda","versions":[` +
	`{"name":"cuda13.0-vllm0.30.0-r4","image_ref":"gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4","engine_version":"0.30.0","framework_version":"13.0","arch":"multi","registry_sources":["quay.io/gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4"]},` +
	`{"name":"cuda13.0-vllm0.31.0-r1","image_ref":"gpuworks/gpuworks-runner:cuda13.0-vllm0.31.0-r1","engine_version":"0.31.0","framework_version":"13.0","arch":"multi"}]}]}`

type imageManifestRun struct {
	t      *testing.T
	store  *store.Store
	router http.Handler
	admin  string
	deps   Deps
}

func newImageManifestRun(t *testing.T) *imageManifestRun {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// A started server: the embedded system catalog is in place.
	if _, err := imagecatalog.SyncSeed(st); err != nil {
		t.Fatal(err)
	}
	signer, admin := roleTestToken(t, st, "admin")
	deps := Deps{Store: st, AuthSigner: signer}
	return &imageManifestRun{t: t, store: st, router: Router(deps), admin: admin, deps: deps}
}

// useChannel points "check for updates" at a test channel.
func (r *imageManifestRun) useChannel(channel *testChannel) {
	r.deps.Outside = channel.sources()
	r.router = Router(r.deps)
}

func (r *imageManifestRun) do(method, path, contentType, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.admin)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	r.router.ServeHTTP(rec, req)
	return rec
}

func (r *imageManifestRun) versionCount() int64 {
	r.t.Helper()
	var n int64
	if err := r.store.DB().Model(&store.Image{}).Count(&n).Error; err != nil {
		r.t.Fatal(err)
	}
	return n
}

func TestCheckForUpdatesReadsTheChannel(t *testing.T) {
	run := newImageManifestRun(t)
	channel := newTestChannel(t)
	channel.publishImageCatalog([]byte(upstreamManifest))
	run.useChannel(channel)
	// A body naming another URL is ignored: the catalog comes from the channel.
	rec := run.do(http.MethodPost, "/api/v1/images/sync", "application/json", `{"url":"http://127.0.0.1:9/evil.json"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if got := channel.requested(); strings.Join(got, ",") != "nightly/index.json,"+channel.imagePath {
		t.Fatalf("requested = %v", got)
	}
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	// The embedded seed has 35 rows; the published catalog keeps one and adds
	// one, so 34 unreferenced system rows are removed.
	if result["catalog_version"] != "2026-10-09.abcdef12" || result["created"] != float64(1) || result["updated"] != float64(1) || result["deleted"] != float64(34) || result["deprecated"] != float64(0) {
		t.Fatalf("result = %v", result)
	}
	if skipped, ok := result["skipped"].([]any); !ok || len(skipped) != 0 {
		t.Fatalf("skipped = %v, want []", result["skipped"])
	}
	series, _ := result["series"].([]any)
	if len(series) != 1 || series[0].(map[string]any)["name"] != "vLLM · CUDA" {
		t.Fatalf("series = %v", result["series"])
	}
	if got := run.versionCount(); got != 2 {
		t.Fatalf("versions after import = %d, want 2", got)
	}
	// The import is what the update view now reports as current.
	if state, err := run.store.OfficialCatalogState(); err != nil || state.ImageCatalogVersion != "2026-10-09.abcdef12" {
		t.Fatalf("recorded state = %+v, %v", state, err)
	}
}

func TestCheckForUpdatesFailuresLeaveCatalogUntouched(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testChannel)
		status  int
		code    string
	}{
		{name: "index unavailable", status: http.StatusBadGateway, code: ErrorUpdateSourcesUnavailable, prepare: func(c *testChannel) {
			c.fail("nightly/index.json", http.StatusInternalServerError)
		}},
		{name: "index too slow", status: http.StatusBadGateway, code: ErrorUpdateSourcesUnavailable, prepare: func(c *testChannel) {
			c.indexTimeout = 200 * time.Millisecond
			c.delay("nightly/index.json", 2*time.Second)
		}},
		{name: "catalog altered on the source", status: http.StatusBadGateway, code: ErrorUpdateDownloadFailed, prepare: func(c *testChannel) {
			c.replace(c.imagePath, []byte(strings.Replace(upstreamManifest, "abcdef12", "abcdef13", 1)))
		}},
		{name: "catalog not published", status: http.StatusNotFound, code: ErrorUpdateNotPublished, prepare: func(c *testChannel) {
			c.publishImageCatalog(nil)
		}},
		{name: "published catalog is not a manifest", status: http.StatusBadRequest, code: ErrorImageManifestInvalid, prepare: func(c *testChannel) {
			c.publishImageCatalog([]byte("<html>maintenance</html>"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newImageManifestRun(t)
			before := run.versionCount()
			channel := newTestChannel(t)
			channel.publishImageCatalog([]byte(upstreamManifest))
			tc.prepare(channel)
			run.useChannel(channel)
			rec := run.do(http.MethodPost, "/api/v1/images/sync", "", "")
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("status = %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
			if after := run.versionCount(); after != before {
				t.Fatalf("versions %d -> %d after a failed check", before, after)
			}
		})
	}
}

func TestUploadManifestIsIdempotentAndRefusesOldFormats(t *testing.T) {
	run := newImageManifestRun(t)
	first := run.do(http.MethodPost, "/api/v1/images/import", "application/json", upstreamManifest)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"created":1`) {
		t.Fatalf("first upload = %d %s", first.Code, first.Body.String())
	}
	second := run.do(http.MethodPost, "/api/v1/images/import", "application/json", upstreamManifest)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"created":0,"updated":2,"deleted":0,"deprecated":0`) {
		t.Fatalf("second upload = %d %s", second.Code, second.Body.String())
	}
	for name, body := range map[string]string{
		"schema 1":  `{"schema_version":1,"version":"1","series":[{"name":"x","versions":[{"image_ref":"x/y:v1"}]}]}`,
		"no series": `{"schema_version":3,"catalog_version":"x","series":[]}`,
		"digest":    `{"schema_version":3,"catalog_version":"x","series":[{"name":"x","versions":[{"image_ref":"x/y@sha256:abc"}]}]}`,
	} {
		rec := run.do(http.MethodPost, "/api/v1/images/import", "application/json", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorImageManifestInvalid) {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// System images do not enter through the community/custom form.
	official := run.do(http.MethodPost, "/api/v1/images/import", "application/json", `{"series":[{"name":"sneaky","engine":"vllm","source":"official","versions":[{"image_ref":"x/sneaky:v1","source":"official"}]}]}`)
	if official.Code != http.StatusBadRequest || !strings.Contains(official.Body.String(), ErrorImageImportUnsupported) {
		t.Fatalf("official through the form = %d %s", official.Code, official.Body.String())
	}
}

func TestUploadManifestConflictIsRefusedWhole(t *testing.T) {
	run := newImageManifestRun(t)
	if _, err := run.store.UpsertImage(&store.Image{ImageRef: "gpuworks/gpuworks-runner:cuda13.0-vllm0.31.0-r1", Engine: "vllm", Framework: "cuda", Source: "custom", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	before := run.versionCount()
	rec := run.do(http.MethodPost, "/api/v1/images/import", "application/json", upstreamManifest)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ErrorImageImportConflict) {
		t.Fatalf("conflict = %d %s", rec.Code, rec.Body.String())
	}
	if after := run.versionCount(); after != before {
		t.Fatalf("a refused import wrote rows: %d -> %d", before, after)
	}
}

// The catalog wire: the version DTO carries reference_count and no longer the
// verification or offline-blob fields; a node row carries the digest the node
// reported and when its row last changed.
func TestImageDTOWire(t *testing.T) {
	run := newImageManifestRun(t)
	image, err := run.store.ImageByRef("gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r4")
	if err != nil {
		t.Fatal(err)
	}
	if err := run.store.DB().Create(&store.Deployment{Name: "pinned", State: "running", Image: image.ImageRef}).Error; err != nil {
		t.Fatal(err)
	}
	withDigest, err := run.store.RegisterNode("node-a", store.HashToken("a"), "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.store.RegisterNode("node-b", store.HashToken("b"), "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := run.store.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, NodeID: withDigest.ID, State: store.ImageStatePresent}); err != nil {
		t.Fatal(err)
	}
	if err := run.store.DB().Model(&store.ImageNode{}).Where("image_version_id = ?", image.ID).Updates(map[string]any{"image_digest_id": "sha256:0123456789abcdef", "updated_at": time.Date(2026, 10, 8, 3, 4, 5, 0, time.UTC)}).Error; err != nil {
		t.Fatal(err)
	}
	// The catalog-wide list carries presence counts, not a row per node.
	rec := run.do(http.MethodGet, "/api/v1/images/versions?source=official", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var versions []map[string]json.RawMessage
	if err := unmarshalList(rec.Body.Bytes(), &versions); err != nil {
		t.Fatal(err)
	}
	var listed map[string]json.RawMessage
	for _, version := range versions {
		if string(version["image_ref"]) == `"`+image.ImageRef+`"` {
			listed = version
		}
	}
	if listed == nil {
		t.Fatalf("%s missing from %s", image.ImageRef, rec.Body.String())
	}
	if string(listed["present_nodes"]) != "1" || string(listed["total_nodes"]) != "2" || listed["nodes"] != nil {
		t.Fatalf("listed version = present %s total %s nodes %s", listed["present_nodes"], listed["total_nodes"], listed["nodes"])
	}
	// The series detail shows one version's nodes.
	detail := run.do(http.MethodGet, fmt.Sprintf("/api/v1/images/%d", image.SeriesID), "", "")
	var series struct {
		Versions []map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &series); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	for _, version := range series.Versions {
		if string(version["image_ref"]) == `"`+image.ImageRef+`"` {
			got = version
		}
	}
	if got == nil {
		t.Fatalf("%s missing from the series detail %s", image.ImageRef, detail.Body.String())
	}
	if string(got["reference_count"]) != "1" {
		t.Fatalf("reference_count = %s", got["reference_count"])
	}
	for _, gone := range []string{"verification_state", "verification_error", "offline_blob_sha256", "offline_blob_bytes", "offline_blob_present"} {
		if _, ok := got[gone]; ok {
			t.Fatalf("version still carries %s", gone)
		}
	}
	var nodes []map[string]any
	if err := json.Unmarshal(got["nodes"], &nodes); err != nil {
		t.Fatal(err)
	}
	byUUID := map[string]map[string]any{}
	for _, node := range nodes {
		byUUID[node["node_uuid"].(string)] = node
	}
	if byUUID["node-a"]["image_digest_id"] != "sha256:0123456789abcdef" || byUUID["node-a"]["updated_at"] != "2026-10-08T03:04:05Z" {
		t.Fatalf("node with a row = %v", byUUID["node-a"])
	}
	if _, ok := byUUID["node-b"]["image_digest_id"]; ok || byUUID["node-b"]["updated_at"] != "" {
		t.Fatalf("node without a row = %v", byUUID["node-b"])
	}

	// Literal round trip of the import result.
	wire, err := json.Marshal(dto.ImageManifestImportResult{CatalogVersion: "v", Created: 1, Skipped: []dto.ImageImportConflict{{Kind: "image", Name: "x/y:1", Reason: "r"}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"catalog_version":"v","created":1,"updated":0,"deleted":0,"deprecated":0,"skipped":[{"kind":"image","name":"x/y:1","reason":"r"}],"series":null}` {
		t.Fatalf("import result wire = %s", wire)
	}
}

func TestRemovedImageRoutes(t *testing.T) {
	run := newImageManifestRun(t)
	for _, path := range []string{"/api/v1/images/offline/import", "/api/v1/images/offline/import-path", "/api/v1/images/offline/cleanup", "/api/v1/images/1/verify"} {
		rec := run.do(http.MethodPost, path, "application/json", "{}")
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestCommunityReposListsBuiltInCards(t *testing.T) {
	run := newImageManifestRun(t)
	rec := run.do(http.MethodGet, "/api/v1/images/community/repos", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var repos []dto.CommunityRepo
	if err := unmarshalList(rec.Body.Bytes(), &repos); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, repo := range repos {
		names = append(names, fmt.Sprintf("%s/%s/%s/%s", repo.Repo, repo.Registry, repo.Engine, repo.Framework))
	}
	want := "vllm/vllm-openai/docker-hub/vllm/cuda,vllm/vllm-omni/docker-hub/vllm-omni/cuda,lmsysorg/sglang/docker-hub/sglang/cuda,quay.io/ascend/vllm-ascend/quay/vllm/cann,quay.io/ascend/sglang/quay/sglang/cann,harbor.sourcefind.cn:5443/dcu/admin/base/vllm/oci/vllm/dtk,quay.io/gpuworks/gpuworks-runner/quay//"
	if strings.Join(names, ",") != want {
		t.Fatalf("repos = %v", names)
	}
	if len(repos[5].Siblings) != 2 || strings.Contains(rec.Body.String(), `"tags"`) {
		t.Fatalf("dcu card = %+v body=%s", repos[5], rec.Body.String())
	}
	if siblings := repos[6].Siblings; len(siblings) != 1 || siblings[0] != "registry.zstack.ai/gpuworks/gpuworks-runner" {
		t.Fatalf("gpuworks runner siblings = %v, want the published ZStack repository", siblings)
	}
}

// "Re-pull" on a rolling tag's node row sends {"force":true} and nothing else:
// it reaches the hub as a forced, unsourced pull: the node's pull source rules
// decide where from.
func TestSyncImageForceReachesThePullUnsourced(t *testing.T) {
	var got imagecatalog.FetchRequest
	h := apiHandlers{d: Deps{ImageFetch: ImageFetchHooks{Pull: func(_ context.Context, imageID uint, nodeUUID string, fetch imagecatalog.FetchRequest) (*store.ImageNode, error) {
		got = fetch
		return &store.ImageNode{State: store.ImageStatePulling, TransferID: "t-1"}, nil
	}}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/7/nodes/node-1/sync", strings.NewReader(`{"force":true}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": "7", "uuid": "node-1"}))
	rec := httptest.NewRecorder()
	h.syncImageToNode(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if !got.Force || got.Source != "" || got.IsURLFetch() {
		t.Fatalf("fetch = %+v", got)
	}
	var wire dto.ImageNodeSyncRequest
	if err := json.Unmarshal([]byte(`{"force":true,"source":"quay"}`), &wire); err != nil || !wire.Force || wire.Source != "quay" {
		t.Fatalf("request wire = %+v, %v", wire, err)
	}
}
