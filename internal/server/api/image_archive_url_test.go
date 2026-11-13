package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

func archiveFixture(t *testing.T, tags []string, opts ...ocifixture.ArchiveServerOption) (*httptest.Server, int64) {
	t.Helper()
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: tags, LayerBytes: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	_, srv := ocifixture.StartArchiveServer(archive, opts...)
	t.Cleanup(srv.Close)
	return srv, int64(len(archive))
}

func postJSON(t *testing.T, handler func(http.ResponseWriter, *http.Request), path, body string, route map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if route != nil {
		req = req.WithContext(chiRouteContext(req.Context(), route))
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestPreviewImageArchiveURLReportsRealTagAndSize(t *testing.T) {
	srv, size := archiveFixture(t, []string{"gpustack/runner:cuda13.0-vllm0.25.1"})
	rec := postJSON(t, (&apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}).previewImageArchiveURL, "/api/v1/images/preview-archive-url",
		fmt.Sprintf(`{"url":%q}`, srv.URL+"/image.tar#ignored"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var got dto.ImageArchivePreview
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.URL != srv.URL+"/image.tar" {
		t.Errorf("url = %q, want the fragment stripped", got.URL)
	}
	if !got.Previewable || len(got.RepoTags) != 1 || got.RepoTags[0] != "gpustack/runner:cuda13.0-vllm0.25.1" {
		t.Errorf("preview = %+v", got)
	}
	if got.SizeBytes != size {
		t.Errorf("size_bytes = %d, want %d", got.SizeBytes, size)
	}
}

func TestPreviewImageArchiveURLReportsNotPreviewableWithoutRangeSupport(t *testing.T) {
	srv, size := archiveFixture(t, []string{"example/app:v1"}, ocifixture.WithoutRangeSupport())
	rec := postJSON(t, (&apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}).previewImageArchiveURL, "/api/v1/images/preview-archive-url",
		fmt.Sprintf(`{"url":%q}`, srv.URL+"/image.tar"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	// A non-previewable archive is a normal answer: the size is still reported
	// and repo_tags is an empty list rather than a null.
	if body := rec.Body.String(); !strings.Contains(body, `"previewable":false`) || !strings.Contains(body, `"repo_tags":[]`) {
		t.Fatalf("body = %s", body)
	}
	var got dto.ImageArchivePreview
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SizeBytes != size {
		t.Fatalf("size_bytes = %d, want %d", got.SizeBytes, size)
	}
}

func TestPreviewImageArchiveURLRejectsMissingAndUnusableURLs(t *testing.T) {
	for body, want := range map[string]string{
		`{}`:                                  ErrorImageArchiveURLRequired,
		`{"url":"  "}`:                        ErrorImageArchiveURLRequired,
		`not json`:                            ErrorImageArchiveURLRequired,
		`{"url":"ftp://mirror/img.tar"}`:      ErrorImageArchiveURLInvalid,
		`{"url":"https://u:p@m/img.tar"}`:     ErrorImageArchiveURLInvalid,
		`{"url":"quay.io/gpustack/run:v1"}`:   ErrorImageArchiveURLInvalid,
		`{"url":"https://mirror.example"}`:    ErrorImageArchiveURLInvalid,
		`{"url":"file:///srv/local/img.tar"}`: ErrorImageArchiveURLInvalid,
	} {
		rec := postJSON(t, (&apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}).previewImageArchiveURL, "/api/v1/images/preview-archive-url", body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s status = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
			continue
		}
		var got map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got["code"] != want {
			t.Errorf("body %s code = %v, want %s", body, got["code"], want)
		}
	}
}

func TestPreviewImageArchiveURLReportsUpstreamStatus(t *testing.T) {
	srv, _ := archiveFixture(t, []string{"example/app:v1"})
	rec := postJSON(t, (&apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}).previewImageArchiveURL, "/api/v1/images/preview-archive-url",
		fmt.Sprintf(`{"url":%q}`, srv.URL+"/missing.tar?token=private-token"), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	params, _ := got["params"].(map[string]any)
	if got["code"] != ErrorImageArchiveURLUpstreamStatus || params["upstream_status"] != float64(http.StatusNotFound) || params["host"] != strings.TrimPrefix(srv.URL, "http://") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "private-token") || strings.Contains(rec.Body.String(), "missing.tar") {
		t.Fatalf("upstream status leaked the source URL: %s", rec.Body.String())
	}
}

func TestPreviewImageArchiveURLReportsUnreachableArchive(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	rec := postJSON(t, (&apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}).previewImageArchiveURL, "/api/v1/images/preview-archive-url",
		fmt.Sprintf(`{"url":%q}`, "http://"+addr+"/image.tar"), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["code"] != ErrorImageArchiveURLUnreachable {
		t.Fatalf("code = %v body = %s", got["code"], rec.Body.String())
	}
	params, _ := got["params"].(map[string]any)
	if params["host"] != addr || params["upstream_status"] != nil {
		t.Fatalf("params = %+v, want the closed address and no upstream status", params)
	}
}

// imageWithSources stores one catalog image carrying the given registry_sources.
func imageWithSources(t *testing.T, dsn string, sources []string) (*store.Store, *store.Image) {
	t.Helper()
	st, err := store.Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{ImageRef: "registry.example/team/runner:v1", Engine: "vllm", Enabled: true, RegistrySources: string(encoded)})
	if err != nil {
		t.Fatal(err)
	}
	return st, image
}

func TestSyncImageFromURLSendsTheDeclaredArchiveToTheNode(t *testing.T) {
	const archiveURL = "https://containerimages.example/runner--gpu3.tar"
	st, image := imageWithSources(t, "file:api_sync_image_url_test?mode=memory&cache=shared", []string{"quay.io/team/runner:v1", archiveURL})
	var got imagecatalog.FetchRequest
	h := apiHandlers{d: Deps{Store: st, ImageFetch: ImageFetchHooks{Pull: func(_ context.Context, imageID uint, nodeUUID string, fetch imagecatalog.FetchRequest) (*store.ImageNode, error) {
		if imageID != image.ID || nodeUUID != "node-1" {
			t.Fatalf("pull args = %d/%s", imageID, nodeUUID)
		}
		got = fetch
		return &store.ImageNode{State: store.ImageStateSyncing, TransferID: "url-transfer", Message: "downloading image archive from containerimages.example"}, nil
	}}}}
	// A trailing fragment and surrounding space must still match the declared
	// entry: one address has one spelling.
	rec := postJSON(t, h.syncImageToNode, "/api/v1/images/1/nodes/node-1/sync",
		fmt.Sprintf(`{"source":"url","archive_url":"  %s#sha  ","relay":false,"force":true}`, archiveURL),
		map[string]string{"id": fmt.Sprint(image.ID), "uuid": "node-1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if got.Source != "url" || got.ArchiveURL != archiveURL || got.Relay || !got.Force {
		t.Fatalf("fetch request = %+v", got)
	}
	var response dto.ImageNodeSyncResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.State != store.ImageStateSyncing || response.TransferID != "url-transfer" {
		t.Fatalf("response = %+v", response)
	}
}

func TestRetryImageFromURLTakesTheSameRequestShape(t *testing.T) {
	const archiveURL = "https://containerimages.example/runner--gpu3.tar"
	st, image := imageWithSources(t, "file:api_retry_image_url_test?mode=memory&cache=shared", []string{archiveURL})
	called := false
	h := apiHandlers{d: Deps{Store: st, ImageFetch: ImageFetchHooks{Pull: func(_ context.Context, _ uint, _ string, fetch imagecatalog.FetchRequest) (*store.ImageNode, error) {
		called = true
		if fetch.ArchiveURL != archiveURL {
			t.Fatalf("archive url = %q", fetch.ArchiveURL)
		}
		return &store.ImageNode{State: store.ImageStateSyncing, TransferID: "retry-url"}, nil
	}}}}
	rec := postJSON(t, h.retryImageToNode, "/api/v1/images/1/nodes/node-1/sync/retry",
		fmt.Sprintf(`{"source":"url","archive_url":%q}`, archiveURL),
		map[string]string{"id": fmt.Sprint(image.ID), "uuid": "node-1"})
	if rec.Code != http.StatusAccepted || !called {
		t.Fatalf("status = %d called = %v body = %s", rec.Code, called, rec.Body.String())
	}
}

func TestSyncImageFromURLRefusesAnUndeclaredArchive(t *testing.T) {
	st, image := imageWithSources(t, "file:api_sync_image_url_undeclared_test?mode=memory&cache=shared", []string{"https://containerimages.example/runner--gpu3.tar"})
	h := apiHandlers{d: Deps{Store: st, ImageFetch: ImageFetchHooks{Pull: func(context.Context, uint, string, imagecatalog.FetchRequest) (*store.ImageNode, error) {
		t.Fatal("an undeclared archive URL must never reach a node")
		return nil, nil
	}}}}
	for body, want := range map[string]string{
		`{"source":"url"}`:                                                           ErrorImageArchiveURLRequired,
		`{"source":"url","archive_url":""}`:                                          ErrorImageArchiveURLRequired,
		`{"source":"url","archive_url":"ftp://x/img.tar"}`:                           ErrorImageArchiveURLInvalid,
		`{"source":"url","archive_url":"https://evil/x.tar"}`:                        ErrorImageArchiveURLNotDeclared,
		`{"source":"url","archive_url":"https://containerimages.example/other.tar"}`: ErrorImageArchiveURLNotDeclared,
	} {
		rec := postJSON(t, h.syncImageToNode, "/api/v1/images/1/nodes/node-1/sync", body,
			map[string]string{"id": fmt.Sprint(image.ID), "uuid": "node-1"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s status = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
			continue
		}
		var got map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got["code"] != want {
			t.Errorf("body %s code = %v, want %s", body, got["code"], want)
		}
	}
}

func TestSyncImageFromURLRelaysThroughTheManagementServerWhenAsked(t *testing.T) {
	const archiveURL = "https://containerimages.example/runner--gpu3.tar"
	st, image := imageWithSources(t, "file:api_sync_image_url_relay_test?mode=memory&cache=shared", []string{archiveURL})
	var got imagecatalog.FetchRequest
	h := apiHandlers{d: Deps{Store: st, ImageFetch: ImageFetchHooks{Pull: func(_ context.Context, _ uint, _ string, fetch imagecatalog.FetchRequest) (*store.ImageNode, error) {
		got = fetch
		return &store.ImageNode{State: store.ImageStateSyncing, TransferID: "relay-1"}, nil
	}}}}
	rec := postJSON(t, h.syncImageToNode, "/api/v1/images/1/nodes/node-1/sync",
		fmt.Sprintf(`{"source":"url","archive_url":%q,"relay":true}`, archiveURL),
		map[string]string{"id": fmt.Sprint(image.ID), "uuid": "node-1"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !got.Relay || got.ArchiveURL != archiveURL || !got.IsURLFetch() {
		t.Fatalf("fetch = %+v, want the declared archive relayed", got)
	}
}

func TestSyncImageFromURLReportsAnUnknownImage(t *testing.T) {
	st, err := store.Open("file:api_sync_image_url_missing_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, ImageFetch: ImageFetchHooks{Pull: func(context.Context, uint, string, imagecatalog.FetchRequest) (*store.ImageNode, error) {
		t.Fatal("an unknown image must not reach the hub")
		return nil, nil
	}}}}
	rec := postJSON(t, h.syncImageToNode, "/api/v1/images/4242/nodes/node-1/sync",
		`{"source":"url","archive_url":"https://containerimages.example/runner.tar"}`,
		map[string]string{"id": "4242", "uuid": "node-1"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestPatchImageVersionAcceptsAndNormalizesArchiveURLs(t *testing.T) {
	st, image := imageWithSources(t, "file:api_patch_image_url_test?mode=memory&cache=shared", nil)
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/images/versions/1",
		strings.NewReader(`{"registry_sources":["quay.io/team/runner:v1","  https://containerimages.example/runner.tar#sha  ",""]}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(image.ID)}))
	rec := httptest.NewRecorder()
	h.patchImageVersion(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	stored, err := st.GetImage(image.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	if err := json.Unmarshal([]byte(stored.RegistrySources), &sources); err != nil {
		t.Fatal(err)
	}
	want := []string{"quay.io/team/runner:v1", "https://containerimages.example/runner.tar"}
	if fmt.Sprint(sources) != fmt.Sprint(want) {
		t.Fatalf("registry_sources = %v, want %v", sources, want)
	}
}

// An official version is catalog-owned, but the archive address this cluster
// can reach it on is not: without this the node row cannot fetch an official
// image from an HTTP archive at all.
func TestPatchImageVersionAddsAnArchiveAddressToAnOfficialImage(t *testing.T) {
	const archiveURL = "https://containerimages.example/gpustack_runner_cann8.5-910b.tar"
	st, err := store.Open("file:api_patch_official_archive_url_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	series, err := st.UpsertImageSeries(&store.ImageSeries{Name: "vllm-cann", Engine: "vllm", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	official, err := st.UpsertImage(&store.Image{
		SeriesID: series.ID, Name: "cann", Engine: "vllm", Enabled: true,
		ImageRef: "gpustack/runner:cann8.5-910b-vllm0.18.0", Source: "official",
		EngineVersion: "0.18.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/images/versions/1", strings.NewReader(body))
		req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(official.ID)}))
		rec := httptest.NewRecorder()
		h.patchImageVersion(rec, req)
		return rec
	}

	rec := patch(`{"registry_sources":["` + archiveURL + `"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	stored, err := st.GetImage(official.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	if err := json.Unmarshal([]byte(stored.RegistrySources), &sources); err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0] != archiveURL {
		t.Fatalf("registry_sources = %v, want [%s]", sources, archiveURL)
	}
	if stored.EngineVersion != official.EngineVersion || stored.Source != "official" {
		t.Fatalf("adding an address must not touch the catalog's own fields: %+v", stored)
	}

	// A version carries no launch configuration to edit (ADR 0031): a patch
	// still naming one is refused rather than silently ignored.
	if rec := patch(`{"docker_args":["--shm-size=2g"]}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), ErrorImageVersionPatchInvalid) {
		t.Fatalf("a retired launch field must be refused: status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestPatchImageVersionRejectsAnUnusableArchiveURL(t *testing.T) {
	st, image := imageWithSources(t, "file:api_patch_image_url_reject_test?mode=memory&cache=shared", nil)
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/images/versions/1",
		strings.NewReader(`{"registry_sources":["https://user:secret@containerimages.example/runner.tar"]}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(image.ID)}))
	rec := httptest.NewRecorder()
	h.patchImageVersion(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["code"] != ErrorImageArchiveURLInvalid {
		t.Fatalf("code = %v", got["code"])
	}
	stored, err := st.GetImage(image.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.RegistrySources, "secret") {
		t.Fatalf("a rejected patch must not be stored: %q", stored.RegistrySources)
	}
}

func TestImageArchiveDTOsRoundTripTheirWireShape(t *testing.T) {
	const previewWire = `{"url":"https://containerimages.example/runner.tar","size_bytes":9448928256,"repo_tags":["gpustack/runner:cuda13.0-vllm0.25.1"],"previewable":true}`
	var preview dto.ImageArchivePreview
	if err := json.Unmarshal([]byte(previewWire), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.URL != "https://containerimages.example/runner.tar" || preview.SizeBytes != 9448928256 ||
		len(preview.RepoTags) != 1 || preview.RepoTags[0] != "gpustack/runner:cuda13.0-vllm0.25.1" || !preview.Previewable {
		t.Fatalf("decoded preview = %+v", preview)
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != previewWire {
		t.Fatalf("preview round trip = %s, want %s", encoded, previewWire)
	}

	const requestWire = `{"source":"url","archive_url":"https://containerimages.example/runner.tar","relay":true}`
	var request dto.ImageNodeSyncRequest
	if err := json.Unmarshal([]byte(requestWire), &request); err != nil {
		t.Fatal(err)
	}
	if request.Source != "url" || request.ArchiveURL != "https://containerimages.example/runner.tar" || !request.Relay {
		t.Fatalf("decoded request = %+v", request)
	}
	if encoded, err = json.Marshal(request); err != nil || string(encoded) != requestWire {
		t.Fatalf("request round trip = %s err=%v", encoded, err)
	}
	// Every optional field is omitted when unset, so an unsourced request keeps
	// the empty body older clients send.
	if encoded, err = json.Marshal(dto.ImageNodeSyncRequest{}); err != nil || string(encoded) != `{}` {
		t.Fatalf("empty request = %s err=%v", encoded, err)
	}

	const responseWire = `{"state":"syncing","transfer_id":"t-1","message":"downloading image archive from containerimages.example"}`
	var response dto.ImageNodeSyncResponse
	if err := json.Unmarshal([]byte(responseWire), &response); err != nil {
		t.Fatal(err)
	}
	if encoded, err = json.Marshal(response); err != nil || string(encoded) != responseWire {
		t.Fatalf("response round trip = %s err=%v", encoded, err)
	}
	if encoded, err = json.Marshal(dto.ImageNodeSyncResponse{State: "pulling"}); err != nil || string(encoded) != `{"state":"pulling"}` {
		t.Fatalf("minimal response = %s err=%v", encoded, err)
	}
}

func TestPreviewImageArchiveURLRouteRequiresAdministrator(t *testing.T) {
	signer, err := auth.NewTokenSigner("router-archive-url-admin-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open("file:api_archive_url_router_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	r := Router(Deps{AuthSigner: signer, Store: st})

	anonymous := httptest.NewRecorder()
	r.ServeHTTP(anonymous, httptest.NewRequest(http.MethodPost, "/api/v1/images/preview-archive-url", strings.NewReader(`{}`)))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anonymous.Code)
	}
	for _, roleStatus := range []struct {
		role string
		want int
	}{{role: "user", want: http.StatusForbidden}, {role: "admin", want: http.StatusBadRequest}} {
		token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: roleStatus.role, Role: roleStatus.role})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/images/preview-archive-url", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != roleStatus.want {
			t.Fatalf("role %s status = %d, want %d body = %s", roleStatus.role, rec.Code, roleStatus.want, rec.Body.String())
		}
	}

	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	// Only POST is registered on this path. PUT and TRACE reach chi's
	// method-not-allowed handler; GET and DELETE instead match the sibling
	// /images/{id} routes with a non-numeric id and answer 404. Both are
	// asserted so a future route edit cannot silently turn one into the other.
	for method, want := range map[string]int{
		http.MethodPut:    http.StatusMethodNotAllowed,
		http.MethodTrace:  http.StatusMethodNotAllowed,
		http.MethodGet:    http.StatusNotFound,
		http.MethodDelete: http.StatusNotFound,
	} {
		req := httptest.NewRequest(method, "/api/v1/images/preview-archive-url", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%s status = %d, want %d", method, rec.Code, want)
		}
	}
}

// The cancel route ends a transfer that is still running on one node, and it
// reports the row the server ended up with so the client can refresh from it.
func TestCancelImageToNodeStopsThatNodesTransfer(t *testing.T) {
	st, image := imageWithSources(t, "file:api_cancel_image_test?mode=memory&cache=shared", nil)
	var gotImage uint
	var gotNode string
	h := apiHandlers{d: Deps{Store: st, ImageFetch: ImageFetchHooks{Cancel: func(_ context.Context, imageID uint, nodeUUID string) (*store.ImageNode, error) {
		gotImage, gotNode = imageID, nodeUUID
		return &store.ImageNode{State: store.ImageStateFailed, Message: "the transfer was cancelled"}, nil
	}}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/1/nodes/node-1/cancel", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(image.ID), "uuid": "node-1"}))
	rec := httptest.NewRecorder()
	h.cancelImageToNode(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if gotImage != image.ID || gotNode != "node-1" {
		t.Fatalf("cancel called with %d/%s", gotImage, gotNode)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["state"] != store.ImageStateFailed {
		t.Fatalf("body = %v", body)
	}
}

// Cancelling is not offered when the control plane has no way to stop a
// transfer; the client gets a stable product error instead of a silent 200.
func TestCancelImageToNodeWithoutAHookIsUnavailable(t *testing.T) {
	st, image := imageWithSources(t, "file:api_cancel_image_unavailable_test?mode=memory&cache=shared", nil)
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/images/1/nodes/node-1/cancel", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(image.ID), "uuid": "node-1"}))
	rec := httptest.NewRecorder()
	h.cancelImageToNode(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != ErrorImageSyncUnavailable {
		t.Fatalf("code = %v", body["code"])
	}
}

// The delete route hands the decision to the node: the handler reports the row
// the server ended up with, and passes a runtime refusal back to the caller.
func TestRemoveImageFromNodeReportsWhatTheServerDid(t *testing.T) {
	st, image := imageWithSources(t, "file:api_remove_image_test?mode=memory&cache=shared", nil)
	h := apiHandlers{d: Deps{Store: st, ImageFetch: ImageFetchHooks{Remove: func(_ context.Context, imageID uint, nodeUUID string) (*store.ImageNode, error) {
		if imageID != image.ID || nodeUUID != "node-1" {
			t.Fatalf("remove called with %d/%s", imageID, nodeUUID)
		}
		return &store.ImageNode{State: store.ImageStateAbsent, Message: "image deleted from this node"}, nil
	}}}}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/images/1/nodes/node-1", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(image.ID), "uuid": "node-1"}))
	rec := httptest.NewRecorder()
	h.removeImageFromNode(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["state"] != store.ImageStateAbsent {
		t.Fatalf("body = %v", body)
	}

	refusal := "conflict: image is being used by running container"
	h = apiHandlers{d: Deps{Store: st, ImageFetch: ImageFetchHooks{Remove: func(context.Context, uint, string) (*store.ImageNode, error) {
		return nil, errors.New(refusal)
	}}}}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/images/1/nodes/node-1", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(image.ID), "uuid": "node-1"}))
	h.removeImageFromNode(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "running container") {
		t.Fatalf("refusal status = %d body = %s", rec.Code, rec.Body.String())
	}
}
