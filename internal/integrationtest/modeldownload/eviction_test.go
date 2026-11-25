package modeldownload

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// stallingHub fronts the fake model hub and can hold one request in place, the
// way a slow public registry keeps a model transfer open for minutes. A held
// request either ends because the agent abandons it, or is released by the
// test. A released weight transfer can be cut off mid-body, which is what a
// registry or proxy does to a long transfer (the agent's own request timeout
// ends it the same way).
type stallingHub struct {
	*httptest.Server
	upstream *url.URL

	mu    sync.Mutex
	armed *stall
}

type stall struct {
	match     func(*http.Request) bool
	cut       bool
	held      chan struct{}
	release   chan struct{}
	abandoned chan struct{}
}

func newStallingHub(t *testing.T, upstream string) *stallingHub {
	t.Helper()
	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	hub := &stallingHub{upstream: target}
	hub.Server = httptest.NewServer(http.HandlerFunc(hub.serve))
	t.Cleanup(hub.Server.Close)
	return hub
}

// arm holds the next request that match accepts. Only one request is held per
// arm, so a retry or a new download after it passes straight through. cut
// drops the released request's connection halfway through its body.
func (h *stallingHub) arm(match func(*http.Request) bool, cut bool) *stall {
	s := &stall{match: match, cut: cut, held: make(chan struct{}), release: make(chan struct{}), abandoned: make(chan struct{})}
	h.mu.Lock()
	h.armed = s
	h.mu.Unlock()
	return s
}

func (h *stallingHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	s := h.armed
	if s != nil && s.match(r) {
		h.armed = nil
	} else {
		s = nil
	}
	h.mu.Unlock()
	if s == nil {
		h.proxy(w, r, false)
		return
	}
	close(s.held)
	select {
	case <-r.Context().Done():
		close(s.abandoned)
		return
	case <-s.release:
	}
	h.proxy(w, r, s.cut)
}

// proxy forwards r to the fake hub. cut sends only the first half of the body
// and then drops the connection.
func (h *stallingHub) proxy(w http.ResponseWriter, r *http.Request, cut bool) {
	forwarded := *r.URL
	forwarded.Scheme, forwarded.Host = h.upstream.Scheme, h.upstream.Host
	request, err := http.NewRequestWithContext(r.Context(), r.Method, forwarded.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	request.Header = r.Header.Clone()
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if !cut {
		_, _ = io.Copy(w, response.Body)
		return
	}
	_, _ = io.CopyN(w, response.Body, response.ContentLength/2)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	panic(http.ErrAbortHandler)
}

func isWeightTransfer(r *http.Request) bool {
	return strings.Contains(r.URL.Path, "/resolve/") && strings.HasSuffix(r.URL.Path, ".safetensors") &&
		r.Header.Get("Range") != "bytes=0-0"
}

func isFileListing(r *http.Request) bool {
	return strings.Contains(r.URL.Path, "/tree/")
}

type evictionFixture struct {
	env   *testenv.Env
	api   *testenv.APIClient
	hub   *stallingHub
	model *store.Model
	node  store.Node
}

func newEvictionFixture(t *testing.T, name string) *evictionFixture {
	t.Helper()
	upstream := testenv.FakeModelHub(t)
	hub := newStallingHub(t, upstream.Endpoint)
	env := testenv.NewWithModelEndpoint(t, hub.URL)
	env.AddNode(name+"-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, name+"-node")
	rows, err := env.Store().ListNodes()
	if err != nil || len(rows) != 1 {
		t.Fatalf("nodes=%+v err=%v", rows, err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: upstream.Repo, Revision: upstream.Revision, Name: name + "-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &evictionFixture{env: env, api: env.API(t), hub: hub, model: model, node: rows[0]}
}

func (f *evictionFixture) startDownload(t *testing.T) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"node_uuid": f.node.UUID})
	resp, err := f.api.PostJSON(fmt.Sprintf("/api/v1/models/%d/download", f.model.ID), body)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start download status=%v err=%v", responseStatus(resp), err)
	}
	resp.Body.Close()
}

func (f *evictionFixture) evict(t *testing.T) {
	t.Helper()
	resp, err := f.api.Delete(fmt.Sprintf("/api/v1/models/%d/nodes/%s", f.model.ID, f.node.UUID))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("evict status=%v err=%v", responseStatus(resp), err)
	}
	resp.Body.Close()
}

// downloadingPath waits until the agent has reported the download and returns
// the absolute directory it is writing.
func (f *evictionFixture) downloadingPath(t *testing.T) string {
	t.Helper()
	var path string
	f.env.Eventually(t, 30*time.Second, func() bool {
		mf, err := f.env.Store().ModelFileByModelNode(f.model.ID, f.node.ID)
		if err != nil || mf.State != proto.MFDownloading || !filepath.IsAbs(mf.Path) {
			return false
		}
		path = mf.Path
		return true
	})
	return path
}

func (f *evictionFixture) waitTerminal(t *testing.T) store.ModelFile {
	t.Helper()
	var mf store.ModelFile
	f.env.Eventually(t, 60*time.Second, func() bool {
		got, err := f.env.Store().ModelFileByModelNode(f.model.ID, f.node.ID)
		if err != nil {
			return false
		}
		mf = *got
		return mf.State == proto.MFPresent || mf.State == proto.MFFailed || mf.State == proto.MFInsufficientDisk
	})
	return mf
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Deleting a node copy while its download is still running, then asking for
// the download again straight away, must start a fresh download that succeeds
// on the first request. The interrupted transfer must not survive the delete
// and fail the new download later.
func TestEvictDuringDownloadThenDownloadAgainSucceedsFirstTime(t *testing.T) {
	f := newEvictionFixture(t, "evict-redownload")
	for round := 1; round <= 3; round++ {
		stalled := f.hub.arm(isWeightTransfer, true)
		f.startDownload(t)
		waitClosed(t, stalled.held, "the weight transfer to start")
		target := f.downloadingPath(t)

		f.evict(t)
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("round %d: model directory after evict: %v", round, err)
		}
		f.startDownload(t)
		// Let a transfer the delete left running carry on and be cut off, as a
		// slow registry transfer eventually is.
		close(stalled.release)

		mf := f.waitTerminal(t)
		if mf.State != proto.MFPresent {
			t.Fatalf("round %d: download after evict = %s: %s", round, mf.State, mf.Message)
		}
		select {
		case <-stalled.abandoned:
		default:
			t.Fatalf("round %d: the delete left the interrupted transfer running", round)
		}
		if _, err := os.Stat(filepath.Join(target, ".gpuworks-model-complete.json")); err != nil {
			t.Fatalf("round %d: present model has no completion marker: %v", round, err)
		}
		f.evict(t)
	}
}

// A delete that lands before the download has written anything must still end
// the download: the abandoned request must not carry on once the delete
// returned, nothing may recreate the directory, and the model has no row and
// no files on the node afterwards.
func TestEvictDuringFileListingLeavesNothingBehind(t *testing.T) {
	f := newEvictionFixture(t, "evict-listing")
	stalled := f.hub.arm(isFileListing, false)
	f.startDownload(t)
	waitClosed(t, stalled.held, "the file listing to start")
	target := f.downloadingPath(t)

	f.evict(t)
	// A listing the delete left running gets its answer now and would go on
	// to download the whole repository.
	close(stalled.release)
	// The fixture repository downloads in well under a second, so a writer
	// that outlived the delete would have recreated the directory by now.
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("model directory was recreated after evict (stat err=%v)", err)
	}
	select {
	case <-stalled.abandoned:
	default:
		t.Fatal("the delete left the download running")
	}
	if mf, err := f.env.Store().ModelFileByModelNode(f.model.ID, f.node.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("model file row after evict = %+v, err=%v", mf, err)
	}
}
