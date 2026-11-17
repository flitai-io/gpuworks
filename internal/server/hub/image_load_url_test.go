package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

// archiveURLHub wires a store, an online node with a scripted agent, and an
// archive server, so the whole "ask the node to fetch this URL" path can be
// observed without a real agent process.
type archiveURLHub struct {
	store    *store.Store
	hub      *Hub
	node     *store.Node
	image    *store.Image
	url      string
	size     int64
	requests chan proto.ImageLoadURLParams
}

func newArchiveURLHub(t *testing.T, dsn string, reply func(env proto.Envelope) string) *archiveURLHub {
	t.Helper()
	st, err := store.Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("archive-url-node", store.HashToken("archive-url-token"), "archive-url-node")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{"registry.example/team/runner:v1"}, LayerBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	_, srv := ocifixture.StartArchiveServer(archive)
	t.Cleanup(srv.Close)
	archiveURL := srv.URL + "/image.tar"
	sources, err := json.Marshal([]string{"quay.io/team/runner:v1", archiveURL})
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{ImageRef: "registry.example/team/runner:v1", Engine: "vllm", Enabled: true, RegistrySources: string(sources)})
	if err != nil {
		t.Fatal(err)
	}

	h := New(st, slog.Default())
	h.SetOutboundAllowPrivateHosts([]string{"127.0.0.1"})
	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })
	session := newSession(h, nil, server)
	session.NodeUUID = node.UUID
	h.register(session)
	// Finish registration's store reads before tests install query barriers or
	// fault callbacks; GORM callback registration is not concurrent with use.
	h.waitSessionWork()
	t.Cleanup(func() { h.deregister(session) })
	go func() {
		decoder := json.NewDecoder(server)
		for {
			var env proto.Envelope
			if err := decoder.Decode(&env); err != nil {
				return
			}
			session.handle(env)
		}
	}()

	harness := &archiveURLHub{store: st, hub: h, node: node, image: image, url: archiveURL, size: int64(len(archive)), requests: make(chan proto.ImageLoadURLParams, 4)}
	go func() {
		decoder := json.NewDecoder(client)
		for {
			var env proto.Envelope
			if err := decoder.Decode(&env); err != nil {
				return
			}
			var req struct {
				Method string                   `json:"method"`
				Params proto.ImageLoadURLParams `json:"params"`
			}
			if json.Unmarshal(env.D, &req) == nil && req.Method == proto.RPCImageLoadURL {
				select {
				case harness.requests <- req.Params:
				default:
				}
			}
			body := `{"result":{"state":"syncing"}}`
			if reply != nil {
				body = reply(env)
			}
			line, _ := json.Marshal(proto.Envelope{T: proto.TRPCResp, ID: env.ID, D: json.RawMessage(body)})
			_, _ = client.Write(append(line, '\n'))
		}
	}()
	return harness
}

func (h *archiveURLHub) fetch() imagecatalog.FetchRequest {
	return imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceURL, ArchiveURL: h.url}
}

func TestPullImageFromURLSendsTheArchiveAndRecordsTheURLSource(t *testing.T) {
	h := newArchiveURLHub(t, "file:hub_image_load_url_test?mode=memory&cache=shared", nil)

	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, h.fetch())
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if row.State != store.ImageStateSyncing || row.Source != imagecatalog.FetchSourceURL {
		t.Fatalf("ledger row = %+v", row)
	}
	if row.TransferID == "" {
		t.Fatal("no transfer id was recorded")
	}
	if !strings.Contains(row.Message, imagecatalog.ArchiveURLHost(h.url)) {
		t.Fatalf("message = %q, want the archive host", row.Message)
	}
	select {
	case params := <-h.requests:
		if params.CanonicalRef != h.image.ImageRef || params.ArchiveURL != h.url {
			t.Fatalf("rpc params = %+v", params)
		}
		// The server HEADs the archive so the node can run its own disk
		// admission and show a real progress bar.
		if params.SizeBytes != h.size {
			t.Fatalf("size_bytes = %d, want %d", params.SizeBytes, h.size)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the node was never asked to load the archive")
	}
	stored, err := h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Source != imagecatalog.FetchSourceURL || stored.SizeBytes != h.size {
		t.Fatalf("stored row = %+v", stored)
	}
}

func TestPullImageFromURLMarksTheRowFailedWhenTheAgentRefuses(t *testing.T) {
	h := newArchiveURLHub(t, "file:hub_image_load_url_refused_test?mode=memory&cache=shared", func(proto.Envelope) string {
		return `{"error":"unknown rpc method: image.load_url"}`
	})

	_, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, h.fetch())
	if err == nil || !strings.Contains(err.Error(), "older agent") {
		t.Fatalf("PullImage error = %v, want an upgrade-the-agent message", err)
	}
	stored, storeErr := h.store.ImageNode(h.image.ID, h.node.ID)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if stored.State != store.ImageStateFailed || stored.Source != imagecatalog.FetchSourceURL {
		t.Fatalf("stored row = %+v", stored)
	}
}

func TestPullImageFromURLRefusesUnusableAddresses(t *testing.T) {
	h := newArchiveURLHub(t, "file:hub_image_load_url_invalid_test?mode=memory&cache=shared", nil)

	if _, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID,
		imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceURL, ArchiveURL: "ftp://mirror.example/img.tar"}); err == nil ||
		!strings.Contains(err.Error(), "not http or https") {
		t.Fatalf("scheme error = %v", err)
	}
	select {
	case params := <-h.requests:
		t.Fatalf("a refused request still reached the node: %+v", params)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := h.store.ImageNode(h.image.ID, h.node.ID); err == nil {
		t.Fatal("a refused request must not create a ledger row")
	}
}

func TestPullImageFromURLLeavesAnImageAlreadyPresentAlone(t *testing.T) {
	h := newArchiveURLHub(t, "file:hub_image_load_url_present_test?mode=memory&cache=shared", nil)
	if _, err := h.store.UpsertImageNode(&store.ImageNode{
		ImageVersionID: h.image.ID, ImageID: h.image.ID, NodeID: h.node.ID,
		State: store.ImageStatePresent, Source: imagecatalog.FetchSourceURL,
	}); err != nil {
		t.Fatal(err)
	}
	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, h.fetch())
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if row.State != store.ImageStatePresent {
		t.Fatalf("row = %+v", row)
	}
	select {
	case params := <-h.requests:
		t.Fatalf("an image already present was downloaded again: %+v", params)
	case <-time.After(200 * time.Millisecond):
	}
}

// The whole point of a separate archive path is that a URL never becomes a
// pull. Asserting on the RPC methods is the only place that can be observed.
func TestPullImageFromURLNeverIssuesARegistryPull(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	h := newArchiveURLHub(t, "file:hub_image_load_url_no_pull_test?mode=memory&cache=shared", func(env proto.Envelope) string {
		var req struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(env.D, &req) == nil {
			mu.Lock()
			methods = append(methods, req.Method)
			mu.Unlock()
		}
		return `{"result":{"state":"syncing"}}`
	})

	if _, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, h.fetch()); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	select {
	case <-h.requests:
	case <-time.After(5 * time.Second):
		t.Fatal("the node was never asked to load the archive")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != proto.RPCImageLoadURL {
		t.Fatalf("rpc methods = %v, want only %s", methods, proto.RPCImageLoadURL)
	}
}
