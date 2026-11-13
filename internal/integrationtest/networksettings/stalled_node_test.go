package networksettings

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// rawControlNode follows the actual agent handshake and reads the initial
// baseline. Its caller owns subsequent reads, including deliberately stalling.
func rawControlNode(t *testing.T, env *testenv.Env, name string) (string, *yamux.Stream, *bufio.Reader) {
	t.Helper()
	node, token, err := env.Store().CreateNodePending(name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(env.URL(), "http")+"/api/v1/agent/connect?lane=ctrl", nil)
	if err != nil {
		t.Fatal(err)
	}
	config := yamux.DefaultConfig()
	config.InitialStreamWindowSize = 256 * 1024
	config.MaxStreamWindowSize = 256 * 1024
	config.LogOutput = io.Discard
	session, err := yamux.Client(websocket.NetConn(context.Background(), ws, websocket.MessageBinary), config, nil)
	if err != nil {
		_ = ws.CloseNow()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	ctrl, err := session.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ctrl.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(ctrl)
	if err := enc.Encode(proto.StreamHeader{Kind: proto.StreamCtrl}); err != nil {
		t.Fatal(err)
	}
	hello, err := json.Marshal(proto.Hello{ProtoMin: 1, ProtoMax: 1, NodeToken: token, Caps: []string{"state.update.network"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(proto.Envelope{T: proto.THello, D: hello}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(ctrl)
	for _, kind := range []string{proto.TRegistered, proto.THello, proto.TStateSync} {
		frame := readFrame(t, reader)
		if frame.T != kind {
			t.Fatalf("handshake frame=%s want=%s", frame.T, kind)
		}
	}
	if err := ctrl.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	return node.UUID, ctrl, reader
}

func readFrame(t *testing.T, reader *bufio.Reader) proto.Envelope {
	t.Helper()
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var frame proto.Envelope
	if err := json.Unmarshal(line, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestNetworkSaveRemainsResponsiveWithStalledControlPeer(t *testing.T) {
	env := testenv.New(t)
	healthy := env.AddNode("healthy", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}))
	env.WaitNodeOnline(t, "healthy")
	stalled, ctrl, reader := rawControlNode(t, env, "stalled")
	_, observer, observed := rawControlNode(t, env, "observer")
	// A body larger than the fixed ctrl receive window makes the server's
	// write block once this peer stops reading. Observing its prefix proves
	// the write has started; this does not depend on a scheduling sleep.
	rpcDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(func() {
		cancel()
		_ = ctrl.Close()
		select {
		case <-rpcDone:
		case <-time.After(5 * time.Second):
			t.Error("stalled RPC did not stop after peer close")
		}
	})
	go func() {
		_, err := env.Hub().CallNode(ctx, stalled, proto.RPCFSStat, proto.FSStatParams{Path: strings.Repeat("x", 1024*1024)})
		rpcDone <- err
	}()
	if err := ctrl.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	prefix, err := reader.Peek(64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(prefix, []byte(`"rpc.req"`)) {
		t.Fatalf("expected blocked RPC prefix: %q", prefix)
	}
	api := env.API(t)
	api.Client.Timeout = 5 * time.Second
	for _, proxy := range []string{"http://replacement.invalid:8080", ""} {
		saveProxy(t, env, api, proxy)
		rpcWitness(t, env, healthy.UUID)
		if err := observer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		frame := readFrame(t, observed)
		if frame.T != proto.TStateUpdate {
			t.Fatalf("proxy save sent %s instead of a network update", frame.T)
		}
		var update map[string]json.RawMessage
		if err := json.Unmarshal(frame.D, &update); err != nil {
			t.Fatal(err)
		}
		for key := range update {
			if key != "network" && key != "image_refs" {
				t.Fatalf("proxy save changed unrelated field %q: %s", key, frame.D)
			}
		}
		if refs := update["image_refs"]; len(refs) > 0 && string(refs) != "null" {
			t.Fatalf("proxy save changed image references: %s", refs)
		}
		var got proto.NetworkConfig
		if err := json.Unmarshal(update["network"], &got); err != nil {
			t.Fatal(err)
		}
		if got.HTTPProxy != proxy || got.HTTPSProxy != proxy {
			t.Fatalf("network update=%s want proxy=%q", frame.D, proxy)
		}
	}
	// The API remained available while the blocked write was still in flight,
	// rather than recovering only because a timeout had evicted that peer.
	select {
	case err := <-rpcDone:
		rpcDone <- err
		t.Fatalf("stalled RPC ended before assertions completed: %v", err)
	default:
	}
	if !env.Hub().Online(stalled) {
		t.Fatal("stalled node disconnected before responsiveness checks")
	}
	requestJSON(t, env, api, http.MethodGet, "/api/v1/healthz", nil)
}
