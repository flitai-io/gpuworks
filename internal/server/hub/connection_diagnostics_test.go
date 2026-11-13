package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/version"
)

func TestSmokePreservesLiveControlDataAndUpgradeFailure(t *testing.T) {
	h := newTestHub(t)
	uuid, token := newTestNode(t, h.store)
	live, peer, err := upgradeHandshakePeer(t, h, token, version.Version, "original-control-session", false)
	if err != nil {
		t.Fatal(err)
	}
	data, dataClient := yamuxPairData(t)
	live.attachData(data)
	h.setUpgradeError(uuid, "installed binary did not reconnect")
	srv := httptest.NewServer(hServe(h))
	defer srv.Close()
	probe := agentconn.New(agentconn.Credentials{ServerURL: srv.URL, NodeToken: token, Smoke: true}, nil, nil, gpu.NoAccelerator{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := probe.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if h.session(uuid) != live || live.DataLaneState() != "attached" || h.UpgradeErrors()[uuid] != "installed binary did not reconnect" {
		t.Fatal("diagnostic handshake replaced a live session or cleared upgrade failure")
	}
	// Exchange real bytes on the original data lane, not just an attached flag.
	// The server opens and the agent accepts, as every server-side data-plane
	// stream does; the server's own accept loop serves only telemetry.
	stream, err := data.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	defer data.Close()
	accepted, err := dataClient.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	if err := accepted.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := stream.Write([]byte("still-open")); done <- err }()
	got := make([]byte, len("still-open"))
	if _, err := io.ReadFull(accepted, got); err != nil || string(got) != "still-open" {
		t.Fatalf("original data lane failed after Smoke: %q %v", got, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A genuine ctrl RPC round-trip must also work on the original session.
	go func() {
		var env proto.Envelope
		if err := json.NewDecoder(peer).Decode(&env); err != nil {
			done <- err
			return
		}
		done <- json.NewEncoder(peer).Encode(proto.Envelope{T: proto.TRPCResp, ID: env.ID, D: json.RawMessage(`{"result":{"report":"","facts":[{"code":"connection.source","value":"agent_runtime"},{"code":"connection.control","value":"connected"}]}}`)})
	}()
	if raw, err := h.CallNode(ctx, uuid, proto.RPCNodeConnectionDiagnostics, struct{}{}); err != nil || !strings.Contains(string(raw), `"value":"connected"`) {
		t.Fatalf("original ctrl no longer works: %s %v", raw, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConnectionDiagnosticsDiscardsReplyFromReplacedSession(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	s := newSession(h, nil, server)
	s.NodeUUID = uuid
	h.register(s)
	replacementCtrl, replacementPeer := net.Pipe()
	replacement := newSession(h, nil, replacementCtrl)
	replacement.NodeUUID = uuid
	t.Cleanup(func() { replacement.Close(); _ = replacementPeer.Close(); h.deregister(replacement); s.Close() })
	s.sendFn = func(kind string, id uint64, _ any) error {
		if kind != proto.TRPCReq {
			t.Errorf("unexpected envelope %s", kind)
		}
		// The old response and replacement are deliberately both ready when
		// s.Call wakes. The identity check must prevent this stale success.
		s.pendMu.Lock()
		ch := s.pending[id]
		s.pendMu.Unlock()
		ch <- proto.Envelope{T: proto.TRPCResp, ID: id, D: json.RawMessage(`{"result":{"facts":[{"code":"connection.control","value":"connected"}]}}`)}
		h.register(replacement)
		return nil
	}
	if _, err := h.CallNode(context.Background(), uuid, proto.RPCNodeConnectionDiagnostics, struct{}{}); err == nil || !strings.Contains(err.Error(), "connection changed") {
		t.Fatalf("old session success accepted: %v", err)
	}
}
