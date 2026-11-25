package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

const loadURLCanonical = "registry.example/team/runner:v1"

// archiveLoadHarness wires a NodeRuntime to a recording runtime and a faithful
// archive server, and collects every image-node status the agent reports.
type archiveLoadHarness struct {
	node     *NodeRuntime
	driver   *recording.Driver
	server   *httptest.Server
	fixture  *ocifixture.ArchiveServer
	statuses chan proto.ImageNodeStatus
	size     int64
}

func newArchiveLoadHarness(t *testing.T, opts ...ocifixture.ArchiveServerOption) *archiveLoadHarness {
	t.Helper()
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{loadURLCanonical}, LayerBytes: 128 << 10})
	if err != nil {
		t.Fatal(err)
	}
	fixture, srv := ocifixture.StartArchiveServer(archive, opts...)
	t.Cleanup(srv.Close)

	driver := recording.New(t.TempDir() + "/spec.json")
	driver.InjectInspection(runtime.ImageInspection{ID: "sha256:loaded", Entrypoint: []string{"/entrypoint.sh"}, Cmd: []string{"--serve"}})
	node := NewNodeRuntime(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), driver, t.TempDir()+"/models")
	h := &archiveLoadHarness{node: node, driver: driver, server: srv, fixture: fixture, statuses: make(chan proto.ImageNodeStatus, 64), size: int64(len(archive))}
	node.send = func(kind string, data any) error {
		if kind == proto.TStatusImageNode {
			select {
			case h.statuses <- data.(proto.ImageNodeStatus):
			default:
			}
		}
		return nil
	}
	return h
}

func (h *archiveLoadHarness) url() string { return h.server.URL + "/image.tar" }

func (h *archiveLoadHarness) params(sizeBytes int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"canonical_ref":%q,"archive_url":%q,"size_bytes":%d}`, loadURLCanonical, h.url(), sizeBytes))
}

// awaitTerminal collects statuses until present or failed, or fails the test.
func (h *archiveLoadHarness) awaitTerminal(t *testing.T) proto.ImageNodeStatus {
	t.Helper()
	deadline := time.After(20 * time.Second)
	var seen []string
	for {
		select {
		case status := <-h.statuses:
			seen = append(seen, status.State)
			if status.State == "present" || status.State == "failed" {
				return status
			}
		case <-deadline:
			t.Fatalf("no terminal image status; saw %v", seen)
		}
	}
}

// archiveDir is where the agent stages downloads for this node.
func (h *archiveLoadHarness) archiveDir(t *testing.T) string {
	t.Helper()
	dir, err := h.node.imageWork().imageArchiveDir()
	if err != nil {
		t.Fatalf("image archive dir: %v", err)
	}
	return dir
}

// assertNoStagedArchive proves the staged tar and its resume sidecars are gone.
func (h *archiveLoadHarness) assertNoStagedArchive(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(h.archiveDir(t))
	if err != nil {
		t.Fatalf("read archive dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("staged archive files left behind: %v", names)
	}
}

func TestRPCImageLoadURLRejectsUnusableRequests(t *testing.T) {
	h := newArchiveLoadHarness(t)
	for _, raw := range []string{
		`{}`,
		`{"canonical_ref":"registry.example/team/runner:v1"}`,
		`{"archive_url":"https://mirror.example/img.tar"}`,
		`{"canonical_ref":"registry.example/team/runner@sha256:abc","archive_url":"https://mirror.example/img.tar"}`,
		`{"canonical_ref":"registry.example/team/runner:v1","archive_url":"ftp://mirror.example/img.tar"}`,
		`{"canonical_ref":"registry.example/team/runner:v1","archive_url":"https://user:pass@mirror.example/img.tar"}`,
		`{"canonical_ref":"registry.example/team/runner:v1","archive_url":"not a url at all"}`,
	} {
		if _, err := h.node.imageWork().rpcImageLoadURL(context.Background(), json.RawMessage(raw)); err == nil {
			t.Errorf("rpcImageLoadURL(%s) unexpectedly succeeded", raw)
		}
	}
	if calls := h.driver.Calls(); len(calls) != 0 {
		t.Fatalf("a rejected request touched the runtime: %v", calls)
	}
	if requests := h.fixture.Requests(); len(requests) != 0 {
		t.Fatalf("a rejected request touched the network: %v", requests)
	}
}

func TestRPCImageLoadURLDownloadsLoadsAndHydrates(t *testing.T) {
	h := newArchiveLoadHarness(t)
	result, err := h.node.imageWork().rpcImageLoadURL(context.Background(), h.params(h.size))
	if err != nil {
		t.Fatal(err)
	}
	// The RPC acknowledges before the transfer runs, exactly like image.pull.
	if got := result.(proto.ImageLoadURLResult); got.State != "syncing" || got.CanonicalRef != loadURLCanonical {
		t.Fatalf("rpc result = %+v", got)
	}
	status := h.awaitTerminal(t)
	if status.State != "present" {
		t.Fatalf("terminal status = %+v", status)
	}
	if status.ImageID != "sha256:loaded" {
		t.Fatalf("the node's image id was not taken from the inspect: %+v", status)
	}
	calls := h.driver.Calls()
	want := []string{
		"LoadImageFile(" + loadURLCanonical + ")",
		"ImageExists(" + loadURLCanonical + ")",
		"InspectImage(" + loadURLCanonical + ")",
	}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("runtime calls = %v, want %v", calls, want)
	}
	// The archive is loaded from the file that was downloaded: no second copy.
	paths := h.driver.LoadedPaths()
	if len(paths) != 1 || !strings.HasPrefix(paths[0], h.archiveDir(t)) || !strings.HasSuffix(paths[0], ".tar") {
		t.Fatalf("loaded paths = %v, want one archive under %s", paths, h.archiveDir(t))
	}
	h.assertNoStagedArchive(t)
}

func TestRPCImageLoadURLTagsAnArchiveThatCarriedAnotherReference(t *testing.T) {
	h := newArchiveLoadHarness(t)
	const realRef = "gpustack/runner:cuda13.0-vllm0.25.1"
	h.driver.InjectMissingImage(loadURLCanonical)
	h.driver.InjectLoadedRef(realRef)

	if _, err := h.node.imageWork().rpcImageLoadURL(context.Background(), h.params(h.size)); err != nil {
		t.Fatal(err)
	}
	status := h.awaitTerminal(t)
	if status.State != "present" {
		t.Fatalf("terminal status = %+v", status)
	}
	if !strings.Contains(status.Message, realRef) || !strings.Contains(status.Message, loadURLCanonical) {
		t.Fatalf("message = %q, want both references recorded", status.Message)
	}
	calls := h.driver.Calls()
	want := []string{
		"LoadImageFile(" + loadURLCanonical + ")",
		"ImageExists(" + loadURLCanonical + ")",
		"TagImage(" + realRef + "->" + loadURLCanonical + ")",
		"ImageExists(" + loadURLCanonical + ")",
		"InspectImage(" + loadURLCanonical + ")",
	}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("runtime calls = %v, want %v", calls, want)
	}
	h.assertNoStagedArchive(t)
}

func TestRPCImageLoadURLFailsWhenTheArchiveReferenceIsUnknown(t *testing.T) {
	h := newArchiveLoadHarness(t)
	// The archive carried some other reference and the runtime did not print
	// which one, so there is nothing to tag.
	h.driver.InjectMissingImage(loadURLCanonical)
	h.driver.InjectNoLoadedRef()

	if _, err := h.node.imageWork().rpcImageLoadURL(context.Background(), h.params(h.size)); err != nil {
		t.Fatal(err)
	}
	status := h.awaitTerminal(t)
	if status.State != "failed" || !strings.Contains(status.Message, loadURLCanonical) {
		t.Fatalf("terminal status = %+v", status)
	}
	if !strings.Contains(status.Message, "did not report the reference") {
		t.Fatalf("message = %q", status.Message)
	}
	for _, call := range h.driver.Calls() {
		if strings.HasPrefix(call, "TagImage(") {
			t.Fatalf("tagged without knowing the real reference: %v", h.driver.Calls())
		}
	}
	h.assertNoStagedArchive(t)
}

func TestRPCImageLoadURLReportsRuntimeFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		inject     func(*recording.Driver)
		wantState  string
		wantDetail string
	}{
		"load failure": {
			inject:     func(d *recording.Driver) { d.InjectLoadFailure(errors.New("archive is corrupt")) },
			wantState:  "failed",
			wantDetail: "archive is corrupt",
		},
		"tag failure": {
			inject: func(d *recording.Driver) {
				d.InjectMissingImage(loadURLCanonical)
				d.InjectLoadedRef("gpustack/runner:v1")
				d.InjectTagFailure(errors.New("tag denied"))
			},
			wantState:  "failed",
			wantDetail: "tag denied",
		},
		"inspect failure is not fatal": {
			inject:     func(d *recording.Driver) { d.InjectInspectFailure(errors.New("inspect unavailable")) },
			wantState:  "present",
			wantDetail: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newArchiveLoadHarness(t)
			tc.inject(h.driver)
			if _, err := h.node.imageWork().rpcImageLoadURL(context.Background(), h.params(h.size)); err != nil {
				t.Fatal(err)
			}
			status := h.awaitTerminal(t)
			if status.State != tc.wantState {
				t.Fatalf("terminal status = %+v, want %s", status, tc.wantState)
			}
			if tc.wantDetail != "" && !strings.Contains(status.Message, tc.wantDetail) {
				t.Fatalf("message = %q, want it to carry %q", status.Message, tc.wantDetail)
			}
			// Whatever happened, the multi-gigabyte staging file is gone.
			h.assertNoStagedArchive(t)
		})
	}
}

func TestRPCImageLoadURLRefusesBeforeTouchingTheNetworkWhenTheDiskIsTooSmall(t *testing.T) {
	h := newArchiveLoadHarness(t)
	// A size no test machine can hold: admission must refuse it outright.
	if _, err := h.node.imageWork().rpcImageLoadURL(context.Background(), h.params(1<<60)); err != nil {
		t.Fatal(err)
	}
	status := h.awaitTerminal(t)
	if status.State != "failed" || !strings.Contains(status.Message, "insufficient disk space") {
		t.Fatalf("terminal status = %+v", status)
	}
	if requests := h.fixture.Requests(); len(requests) != 0 {
		t.Fatalf("admission spent bandwidth before refusing: %v", requests)
	}
	if calls := h.driver.Calls(); len(calls) != 0 {
		t.Fatalf("admission touched the runtime: %v", calls)
	}
}

func TestRPCImageLoadURLReportsAMirrorThatDropsTheTransfer(t *testing.T) {
	h := newArchiveLoadHarness(t, ocifixture.WithMidStreamBreak(4096))
	if _, err := h.node.imageWork().rpcImageLoadURL(context.Background(), h.params(h.size)); err != nil {
		t.Fatal(err)
	}
	status := h.awaitTerminal(t)
	if status.State != "failed" {
		t.Fatalf("terminal status = %+v", status)
	}
	if !strings.Contains(status.Message, "download") || !strings.Contains(status.Message, h.server.Listener.Addr().String()) {
		t.Fatalf("message = %q, want the failing host named", status.Message)
	}
	for _, call := range h.driver.Calls() {
		if strings.HasPrefix(call, "LoadImageFile(") {
			t.Fatal("a truncated download must never reach the runtime")
		}
	}
	h.assertNoStagedArchive(t)
}

func TestRPCImageLoadURLFallsBackToOneConnectionWithoutRangeSupport(t *testing.T) {
	h := newArchiveLoadHarness(t, ocifixture.WithoutRangeSupport())
	// A server that ignores Range also cannot report a trustworthy size ahead
	// of time, so the request carries none: admission is skipped and the
	// download still has to work.
	if _, err := h.node.imageWork().rpcImageLoadURL(context.Background(), h.params(0)); err != nil {
		t.Fatal(err)
	}
	if status := h.awaitTerminal(t); status.State != "present" {
		t.Fatalf("terminal status = %+v", status)
	}
	h.assertNoStagedArchive(t)
}

func TestRPCImageLoadURLSerializesWithOtherImageWorkAndSaysSo(t *testing.T) {
	h := newArchiveLoadHarness(t)
	// The image slot is what keeps two large image operations off one disk at the
	// same time. Hold it and prove the load waits — and that waiting is
	// reported, because a queued transfer that says nothing is indistinguish-
	// able from a stalled one on the progress column.
	h.node.imageWork().slot.Lock()
	if _, err := h.node.imageWork().rpcImageLoadURL(context.Background(), h.params(h.size)); err != nil {
		h.node.imageWork().slot.Unlock()
		t.Fatal(err)
	}
	select {
	case status := <-h.statuses:
		if !strings.Contains(status.Message, "waiting for another image operation") {
			h.node.imageWork().slot.Unlock()
			t.Fatalf("a queued load reported %+v, want the waiting notice", status)
		}
	case <-time.After(3 * time.Second):
		h.node.imageWork().slot.Unlock()
		t.Fatal("a queued load must say it is waiting")
	}
	if requests := h.fixture.Requests(); len(requests) != 0 {
		h.node.imageWork().slot.Unlock()
		t.Fatalf("load downloaded while the image lock was held: %v", requests)
	}
	h.node.imageWork().slot.Unlock()
	if status := h.awaitTerminal(t); status.State != "present" {
		t.Fatalf("terminal status = %+v", status)
	}
}

// A cancel names one attempt. The same image can have a transfer wedged in its
// runtime and a retry queued behind it, so the transfer id — not the reference —
// is what makes the stop land on the intended one.
func TestRPCImageCancelStopsTheNamedQueuedTransfer(t *testing.T) {
	h := newArchiveLoadHarness(t)
	params := json.RawMessage(fmt.Sprintf(`{"canonical_ref":%q,"archive_url":%q,"size_bytes":%d,"transfer_id":"t-1"}`,
		loadURLCanonical, h.url(), h.size))
	// Hold the image slot so the transfer is still queued when the cancel lands.
	h.node.imageWork().slot.Lock()
	if _, err := h.node.imageWork().rpcImageLoadURL(context.Background(), params); err != nil {
		h.node.imageWork().slot.Unlock()
		t.Fatal(err)
	}
	select {
	case status := <-h.statuses:
		if !strings.Contains(status.Message, "waiting for another image operation") {
			h.node.imageWork().slot.Unlock()
			t.Fatalf("queued transfer reported %+v", status)
		}
	case <-time.After(3 * time.Second):
		h.node.imageWork().slot.Unlock()
		t.Fatal("the queued transfer never said it was waiting")
	}

	cancelParams := json.RawMessage(fmt.Sprintf(`{"canonical_ref":%q,"transfer_id":"t-1"}`, loadURLCanonical))
	raw, err := h.node.imageWork().rpcImageCancel(context.Background(), cancelParams)
	if err != nil {
		h.node.imageWork().slot.Unlock()
		t.Fatal(err)
	}
	if result, ok := raw.(proto.ImageCancelResult); !ok || !result.Cancelled {
		h.node.imageWork().slot.Unlock()
		t.Fatalf("cancel result = %#v", raw)
	}
	// Cancelling the same attempt twice, or one that never existed, is a no-op
	// rather than an error: the server may race the cancel against the terminal
	// status.
	unknown := json.RawMessage(fmt.Sprintf(`{"canonical_ref":%q,"transfer_id":"t-2"}`, loadURLCanonical))
	if raw, err = h.node.imageWork().rpcImageCancel(context.Background(), unknown); err != nil {
		h.node.imageWork().slot.Unlock()
		t.Fatal(err)
	}
	if result, ok := raw.(proto.ImageCancelResult); !ok || result.Cancelled {
		h.node.imageWork().slot.Unlock()
		t.Fatalf("unknown transfer cancel = %#v", raw)
	}

	h.node.imageWork().slot.Unlock()
	if status := h.awaitTerminal(t); status.State != "failed" || !strings.Contains(status.Message, "cancelled") {
		t.Fatalf("terminal status after cancel = %+v", status)
	}
	h.assertNoStagedArchive(t)
}

func TestRPCImageLoadURLReportsProgressAgainstTheArchiveSize(t *testing.T) {
	h := newArchiveLoadHarness(t)
	if _, err := h.node.imageWork().rpcImageLoadURL(context.Background(), h.params(h.size)); err != nil {
		t.Fatal(err)
	}
	var syncing []proto.ImageNodeStatus
	deadline := time.After(20 * time.Second)
	for {
		var status proto.ImageNodeStatus
		select {
		case status = <-h.statuses:
		case <-deadline:
			t.Fatal("no terminal image status")
		}
		if status.State == "present" {
			if status.SizeBytes != h.size {
				t.Fatalf("final size = %d, want the archive size %d", status.SizeBytes, h.size)
			}
			break
		}
		if status.State == "failed" {
			t.Fatalf("unexpected failure: %+v", status)
		}
		syncing = append(syncing, status)
	}
	if len(syncing) == 0 {
		t.Fatal("no syncing status was reported")
	}
	for _, status := range syncing {
		if status.State != "syncing" {
			t.Fatalf("intermediate status = %+v, want syncing", status)
		}
		if status.TransferredBytes > status.SizeBytes && status.SizeBytes > 0 {
			t.Fatalf("progress exceeded the total: %+v", status)
		}
	}
}

func TestRPCImageLoadURLFailsWhenTheRuntimeCannotLoadAFile(t *testing.T) {
	// A runtime without ImageFileLoader must be refused with a clear message
	// rather than silently falling back to a reader-based load.
	node := NewNodeRuntime(slog.Default(), &imagePullTestDriver{tagImage: true}, t.TempDir()+"/models")
	statuses := make(chan proto.ImageNodeStatus, 8)
	node.send = func(kind string, data any) error {
		if kind == proto.TStatusImageNode {
			statuses <- data.(proto.ImageNodeStatus)
		}
		return nil
	}
	raw := json.RawMessage(fmt.Sprintf(`{"canonical_ref":%q,"archive_url":"https://mirror.example/img.tar"}`, loadURLCanonical))
	if _, err := node.imageWork().rpcImageLoadURL(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	select {
	case status := <-statuses:
		if status.State != "failed" || !strings.Contains(status.Message, "cannot load an image archive from a file") {
			t.Fatalf("status = %+v", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no status reported")
	}
}

func TestCheckImageArchiveSpaceRequiresRoomForTheExpansion(t *testing.T) {
	dir := t.TempDir()
	// An unknown size is not a reason to refuse: nothing can be checked.
	if err := checkImageArchiveSpace(dir, 0); err != nil {
		t.Fatalf("unknown size = %v, want no refusal", err)
	}
	if err := checkImageArchiveSpace(dir, -1); err != nil {
		t.Fatalf("negative size = %v, want no refusal", err)
	}
	// A small archive fits on any machine that can run the test suite.
	if err := checkImageArchiveSpace(dir, 1<<20); err != nil {
		t.Fatalf("1 MiB archive refused: %v", err)
	}
	err := checkImageArchiveSpace(dir, 1<<60)
	if err == nil || !strings.Contains(err.Error(), "insufficient disk space") {
		t.Fatalf("1 EiB archive = %v, want a refusal", err)
	}
	// The message has to explain the 2.2x headroom, not just the archive size.
	if !strings.Contains(err.Error(), "expansion_and_margin") {
		t.Fatalf("refusal message = %v", err)
	}
}

func TestImageArchiveClientBoundsRedirectsAndRechecksTheTarget(t *testing.T) {
	var loop *httptest.Server
	loop = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, loop.URL+r.URL.Path, http.StatusFound)
	}))
	defer loop.Close()
	client := imageArchiveClient(nil)
	if _, err := client.Get(loop.URL + "/image.tar"); err == nil || !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Fatalf("redirect loop = %v, want the 5-hop bound", err)
	}

	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "ftp://elsewhere.example/img.tar", http.StatusFound)
	}))
	defer elsewhere.Close()
	if _, err := client.Get(elsewhere.URL + "/image.tar"); err == nil || !strings.Contains(err.Error(), "not http or https") {
		t.Fatalf("scheme redirect = %v, want the scheme rule re-applied", err)
	}
}

func TestRuntimeImageStoreRootFollowsTheRuntime(t *testing.T) {
	for driver, want := range map[string]string{
		"docker":    "/var/lib/docker",
		"nerdctl":   "/var/lib/containerd",
		"udocker":   "/srv/agent/udocker",
		"recording": "",
	} {
		if got := runtimeImageStoreRoot(driver, "/srv/agent/models"); got != want {
			t.Errorf("runtimeImageStoreRoot(%q) = %q, want %q", driver, got, want)
		}
	}
}

// A stand-in named after a real runtime does not own that runtime's image
// store, so its archives are never staged in the host's /var/lib/docker; a
// real driver of that name does own it.
func TestAStandInDriverOwnsNoImageStore(t *testing.T) {
	h := newArchiveLoadHarness(t)
	if h.driver.Name() != runtime.DriverDocker {
		t.Fatalf("the recording driver is named %q, want it to stand in for docker", h.driver.Name())
	}
	if got := h.node.imageWork().imageStoreRoot(); got != "" {
		t.Fatalf("a stand-in for docker claims the image store %s", got)
	}
	real := NewNodeRuntime(slog.Default(), dockerdrv.New(), t.TempDir()+"/models")
	if got := real.imageWork().imageStoreRoot(); got != "/var/lib/docker" {
		t.Fatalf("docker's image store = %q, want /var/lib/docker", got)
	}
	// Nor does the stand-in report the host's Docker data root as its
	// runtime disk, which the scheduler would hold image sizes against.
	reportsDockerDisk := func(n *NodeRuntime) bool {
		for _, usage := range n.pathUsage() {
			if usage.Kind == runtime.DriverDocker {
				return true
			}
		}
		return false
	}
	if goruntime.GOOS == "darwin" {
		return
	}
	if reportsDockerDisk(h.node) {
		t.Fatal("a stand-in for docker reports the host's Docker data root")
	}
	if !reportsDockerDisk(real) {
		t.Fatal("a docker node does not report its data root")
	}
}
