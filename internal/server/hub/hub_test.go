package hub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestAuthNodeTokenUsesPendingNodeIdentity(t *testing.T) {
	st, err := store.Open("file:hub_node_token_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	n, token, err := st.CreateNodePending("managed")
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	got, err := h.authNodeToken(token)
	if err != nil || got.UUID != n.UUID {
		t.Fatalf("authNodeToken = %+v, err=%v", got, err)
	}
	if _, err := h.authNodeToken("global-token"); err == nil {
		t.Fatal("global token unexpectedly authenticated")
	}
}

func TestPushAndPullImageRejectDisconnectedNodeWithStableError(t *testing.T) {
	st, err := store.Open("file:hub_push_disconnected_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("offline-node", store.HashToken("token"), "offline-node")
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{ImageRef: "example/disconnected:1", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	if err := h.PushToNode(node.UUID, proto.StateUpdate{}); err == nil || !strings.Contains(err.Error(), "node not connected: "+node.UUID) {
		t.Fatalf("PushToNode error = %v", err)
	}
	if _, err := h.PullImage(context.Background(), image.ID, node.UUID, imagecatalog.FetchRequest{Source: "quay"}); err == nil || !strings.Contains(err.Error(), "target node is offline") {
		t.Fatalf("PullImage error = %v", err)
	}
}

func TestSessionOmitsOnlyAcknowledgedRuntimeAssetContent(t *testing.T) {
	digest := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	session := &Session{assetDigests: map[string]struct{}{digest: {}}}
	instances := []proto.InstanceDesiredState{{ResolvedLaunch: proto.ResolvedLaunch{RuntimeAssets: []proto.RuntimeAsset{
		{File: "assets/cached.py", SHA256: digest, Content: []byte("cached")},
		{File: "assets/miss.py", SHA256: other, Content: []byte("miss")},
	}}}}
	session.omitCachedAssetContent(instances)
	if instances[0].RuntimeAssets[0].Content != nil {
		t.Fatalf("acknowledged content was retransmitted: %q", instances[0].RuntimeAssets[0].Content)
	}
	if got := string(instances[0].RuntimeAssets[1].Content); got != "miss" {
		t.Fatalf("cache miss content=%q, want miss", got)
	}
}

func TestSessionHydratesOnlyMissingRuntimeAssetsFromPersistedReferences(t *testing.T) {
	content := []byte("print('hydrated')\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	cachedDigest := strings.Repeat("b", 64)
	loads := 0
	h := &Hub{runtimeAssetLoader: func(packageDigest, modelID, file string) ([]byte, error) {
		loads++
		if packageDigest != strings.Repeat("c", 64) || modelID != "gpuworks/wrapper" || file != "assets/serve.py" {
			t.Fatalf("loader reference=%q/%q/%q", packageDigest, modelID, file)
		}
		return append([]byte(nil), content...), nil
	}}
	session := &Session{hub: h, assetDigests: map[string]struct{}{cachedDigest: {}}}
	instances := []proto.InstanceDesiredState{{UUID: "asset-instance", ResolvedLaunch: proto.ResolvedLaunch{RuntimeAssets: []proto.RuntimeAsset{
		{File: "assets/serve.py", SHA256: digest, PackageDigest: strings.Repeat("c", 64), ModelID: "gpuworks/wrapper"},
		{File: "assets/already.py", SHA256: cachedDigest, PackageDigest: strings.Repeat("d", 64), ModelID: "gpuworks/wrapper"},
	}}}}
	if err := session.hydrateRuntimeAssetContent(instances); err != nil {
		t.Fatal(err)
	}
	if loads != 1 || string(instances[0].RuntimeAssets[0].Content) != string(content) {
		t.Fatalf("loads=%d assets=%+v", loads, instances[0].RuntimeAssets)
	}
	if instances[0].RuntimeAssets[1].Content != nil {
		t.Fatalf("cached asset unexpectedly hydrated: %+v", instances[0].RuntimeAssets[1])
	}
}

func TestSessionRejectsMissingOrDamagedRuntimeAssetReference(t *testing.T) {
	content := []byte("wrong")
	h := &Hub{runtimeAssetLoader: func(_, _, _ string) ([]byte, error) { return content, nil }}
	session := &Session{hub: h, assetDigests: map[string]struct{}{}}
	for _, test := range []struct {
		name  string
		asset proto.RuntimeAsset
		want  string
	}{
		{name: "missing reference", asset: proto.RuntimeAsset{File: "assets/serve.py", SHA256: strings.Repeat("a", 64)}, want: "no content loader or persisted package reference"},
		{name: "sha mismatch", asset: proto.RuntimeAsset{File: "assets/serve.py", SHA256: strings.Repeat("a", 64), PackageDigest: strings.Repeat("b", 64), ModelID: "gpuworks/wrapper"}, want: "SHA-256 mismatch"},
		{name: "size mismatch", asset: proto.RuntimeAsset{File: "assets/serve.py", SHA256: fmt.Sprintf("%x", sha256.Sum256(content)), SizeBytes: int64(len(content)) + 1, PackageDigest: strings.Repeat("b", 64), ModelID: "gpuworks/wrapper"}, want: "size mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			instances := []proto.InstanceDesiredState{{UUID: "asset-instance", ResolvedLaunch: proto.ResolvedLaunch{RuntimeAssets: []proto.RuntimeAsset{test.asset}}}}
			if err := session.hydrateRuntimeAssetContent(instances); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestWatchImagePullCandidatesAdvancesAndAuditsFailures(t *testing.T) {
	st, err := store.Open("file:hub_image_pull_watch_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("image-watch-node", store.HashToken("image-watch-token"), "image-watch-node")
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{ImageRef: "example/watch:v1", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.UpsertImageNode(&store.ImageNode{
		ImageVersionID: image.ID,
		ImageID:        image.ID,
		NodeID:         node.ID,
		State:          store.ImageStateFailed,
		Source:         "mirror-a",
		Message:        "first source failed",
		LastTransferID: "watch-attempt",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	session := newSession(h, nil, server)
	session.NodeUUID = node.UUID
	h.register(session)
	defer h.deregister(session)
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

	secondPull := make(chan proto.ImagePullParams, 1)
	fakeDone := make(chan struct{})
	go func() {
		defer close(fakeDone)
		var env proto.Envelope
		if err := json.NewDecoder(client).Decode(&env); err != nil {
			return
		}
		var req struct {
			Method string                `json:"method"`
			Params proto.ImagePullParams `json:"params"`
		}
		if json.Unmarshal(env.D, &req) == nil && req.Method == proto.RPCImagePull {
			secondPull <- req.Params
		}
		resp := proto.Envelope{T: proto.TRPCResp, ID: env.ID, D: json.RawMessage(`{"result":{}}`)}
		line, _ := json.Marshal(resp)
		_, _ = client.Write(append(line, '\n'))
		_ = st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{State: store.ImageStateFailed, TransferID: req.Params.TransferID, Message: "second source failed"})
	}()

	done := make(chan struct{})
	go func() {
		h.watchImagePullCandidates(
			context.Background(),
			image.ID,
			node.ID,
			node.UUID,
			image.ImageRef,
			row.LastTransferID,
			[]imagePullCandidate{
				{ImagePullCandidate: proto.ImagePullCandidate{Ref: "mirror-a/example/watch:v1"}, Source: "mirror-a"},
				{ImagePullCandidate: proto.ImagePullCandidate{Ref: "mirror-b/example/watch:v1"}, Source: "mirror-b"},
			},
			0,
		)
		close(done)
	}()

	select {
	case params := <-secondPull:
		if params.EffectiveRef != "mirror-b/example/watch:v1" || params.CanonicalRef != image.ImageRef {
			t.Fatalf("fallback params = %+v", params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not issue fallback pull")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not finish after final failure")
	}
	<-fakeDone
	final, err := st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != store.ImageStateFailed || !strings.Contains(final.Message, "mirror-a: first source failed") || !strings.Contains(final.Message, "mirror-b: second source failed") {
		t.Fatalf("final row = %+v", final)
	}
}

func TestDialAgentRegistersRawServerDial(t *testing.T) {
	st, err := store.Open("file:hub_server_dial_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	node, nodeToken, err := st.CreateNodePending("server-dial")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registered := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			errc <- err
			return
		}
		c := agentconn.New(agentconn.Credentials{
			NodeToken:   nodeToken,
			ConnectMode: "server_dial",
		}, func(uuid, token string) error {
			registered <- uuid
			return nil
		}, noopHandlers{}, emptyProber{}, slog.Default())
		errc <- c.RunAccepted(ctx, raw)
	}()

	if err := h.DialAgentWithSecret(context.Background(), ln.Addr().String(), nodeToken); err != nil {
		t.Fatalf("DialAgent: %v", err)
	}
	select {
	case uuid := <-registered:
		if uuid == "" {
			t.Fatal("registered uuid is empty")
		}
		if uuid != node.UUID {
			t.Fatalf("registered uuid = %q, want pending node %q", uuid, node.UUID)
		}
		if !h.Online(uuid) {
			t.Fatalf("node %s should be online", uuid)
		}
		node, err := st.NodeByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if node.ServerDialAddress != ln.Addr().String() {
			t.Fatalf("ServerDialAddress = %q, want %q", node.ServerDialAddress, ln.Addr().String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not register")
	}
	cancel()
	select {
	case <-errc:
	case <-time.After(3 * time.Second):
		t.Fatal("agent session did not exit")
	}
}

func TestRunServerDialReconnectUsesRegisteredNodeSecret(t *testing.T) {
	st, err := store.Open("file:hub_server_dial_reconnect_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	const nodeToken = "registered-node-token"
	node, err := st.RegisterNode("server-dial-node", store.HashToken(nodeToken), "reverse-node", "server_dial")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("token", nodeToken).Error; err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := st.SetNodeServerDialAddress(node.UUID, ln.Addr().String()); err != nil {
		t.Fatal(err)
	}

	h := New(st, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errC := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			errC <- err
			return
		}
		c := agentconn.New(agentconn.Credentials{
			NodeUUID:    node.UUID,
			NodeToken:   nodeToken,
			ConnectMode: "server_dial",
		}, nil, noopHandlers{}, emptyProber{}, slog.Default())
		errC <- c.RunAccepted(ctx, raw)
	}()
	go h.RunServerDialReconnect(ctx, 10*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for !h.Online(node.UUID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.Online(node.UUID) {
		t.Fatal("registered server_dial node was not reconnected")
	}
	cancel()
	select {
	case <-errC:
	case <-time.After(3 * time.Second):
		t.Fatal("reconnected agent session did not exit")
	}
}

func TestServerDialReconnectRetriesPendingNodeAfterInitialFailure(t *testing.T) {
	st, err := store.Open("file:hub_server_dial_pending_retry_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, nodeToken, err := st.CreateNodePending("pending-retry", "server_dial")
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeServerDialAddress(node.UUID, addr); err != nil {
		t.Fatal(err)
	}

	h := New(st, slog.Default())
	// The first pass sees a refused endpoint and records the initial 10-second
	// backoff. This is the failure mode that used to strand pending nodes.
	h.reconnectServerDial(context.Background())
	h.reconnectMu.Lock()
	state, ok := h.reconnect[node.UUID]
	h.reconnectMu.Unlock()
	if !ok || state.failures != 1 || time.Until(state.next) < 9*time.Second {
		t.Fatalf("initial reconnect state = %+v, present=%v", state, ok)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("reopen server_dial endpoint: %v", err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errC := make(chan error, 1)
	go func() {
		raw, acceptErr := ln.Accept()
		if acceptErr != nil {
			errC <- acceptErr
			return
		}
		c := agentconn.New(agentconn.Credentials{
			NodeUUID: node.UUID, NodeToken: nodeToken, ConnectMode: "server_dial",
		}, nil, noopHandlers{}, emptyProber{}, slog.Default())
		errC <- c.RunAccepted(ctx, raw)
	}()

	// A production ticker waits for the backoff deadline. Advancing the
	// in-memory deadline keeps this unit test fast while exercising the same
	// reconnect path and success cleanup.
	h.reconnectMu.Lock()
	state.next = time.Now()
	h.reconnect[node.UUID] = state
	h.reconnectMu.Unlock()
	h.reconnectServerDial(ctx)
	if !h.Online(node.UUID) {
		t.Fatal("pending server_dial node was not reconnected")
	}
	h.reconnectMu.Lock()
	_, stillBackedOff := h.reconnect[node.UUID]
	h.reconnectMu.Unlock()
	if stillBackedOff {
		t.Fatal("successful reconnect left stale backoff state")
	}
	cancel()
	select {
	case <-errC:
	case <-time.After(3 * time.Second):
		t.Fatal("reconnected agent session did not exit")
	}
}

func TestServerDialReconnectDelayCapsAtFiveMinutes(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{1, 10 * time.Second},
		{2, 30 * time.Second},
		{3, 90 * time.Second},
		{4, 270 * time.Second},
		{6, 5 * time.Minute},
		{100, 5 * time.Minute},
	}
	for _, tc := range tests {
		if got := serverDialReconnectDelay(tc.failures); got != tc.want {
			t.Fatalf("failures=%d delay=%s, want %s", tc.failures, got, tc.want)
		}
	}
}

type emptyProber struct{}

func (emptyProber) Vendor() string                                 { return "" }
func (emptyProber) Available() bool                                { return false }
func (emptyProber) Probe(context.Context) ([]proto.GPUInfo, error) { return nil, nil }
func (emptyProber) DriverVersion(context.Context) string           { return "" }
func (emptyProber) Topology(context.Context) string                { return "" }
func (emptyProber) Metrics(context.Context) ([]gpu.Metric, error)  { return nil, nil }
func (emptyProber) RuntimeInjection([]int) gpu.RuntimeInjection    { return gpu.RuntimeInjection{} }

type noopHandlers struct{}

func (noopHandlers) Bind(agentconn.SendFunc)                                     {}
func (noopHandlers) Unbind()                                                     {}
func (noopHandlers) OnStateSync(context.Context, proto.DesiredState)             {}
func (noopHandlers) OnStateUpdate(context.Context, proto.StateUpdate) bool       { return false }
func (noopHandlers) OnRPC(context.Context, string, json.RawMessage) (any, error) { return nil, nil }
func (noopHandlers) RuntimeDriverName() string                                   { return "" }
func (noopHandlers) StreamLogs(context.Context, string, bool, io.Writer) error   { return nil }
func (noopHandlers) StreamModelArchive(context.Context, string, io.Writer) error { return nil }
func (noopHandlers) ReceiveModelArchive(context.Context, string, io.Reader) error {
	return nil
}
func (noopHandlers) StreamImage(context.Context, string, string, io.ReadWriter, bool) error {
	return nil
}

func (noopHandlers) StreamLargeFile(context.Context, string, io.Writer) error { return nil }
func (noopHandlers) ReceiveStorageUpload(context.Context, proto.StreamHeader, io.ReadWriter) error {
	return nil
}
func (noopHandlers) SendStorageDownload(context.Context, proto.StreamHeader, io.Writer) error {
	return nil
}
