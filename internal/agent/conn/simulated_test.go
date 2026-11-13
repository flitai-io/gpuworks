package conn

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/hostsim"
	"gpuworks.ai/gpuworks/internal/proto"
)

// simulatedHost stands for another machine whose only dialable local port
// is its engine's, and whose other engine ports are the odd ones.
func simulatedHost() *hostsim.Host {
	return &hostsim.Host{
		Dir:     "/nonexistent/models",
		MayDial: func(addr string) bool { return addr == "127.0.0.1:30001" },
		Describe: func(sys proto.SysInfo) proto.SysInfo {
			sys.Hostname = "sim-01"
			return sys
		},
		Listeners: func(r proto.PortRange) []int {
			var out []int
			for p := r.Start; p <= r.End; p++ {
				if p%2 == 1 {
					out = append(out, p)
				}
			}
			return out
		},
	}
}

// countingRPCHandlers records which RPCs reached the handler.
type countingRPCHandlers struct {
	inboundHandlers
	mu      sync.Mutex
	methods []string
}

func (h *countingRPCHandlers) OnRPC(_ context.Context, method string, _ json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	h.mu.Unlock()
	return "ok", nil
}

// TestSimulatedNodeRefusesRPCsAtTheDoor: every RPC passes handleRPC, which
// refuses for a simulated node before any handler (or the upgrade path)
// runs; a real node is unaffected.
func TestSimulatedNodeRefusesRPCsAtTheDoor(t *testing.T) {
	ctrl := &nopRWC{}
	h := &countingRPCHandlers{}
	c := &Conn{handlers: h, ctrl: ctrl, log: slog.Default(), creds: Credentials{Simulated: simulatedHost()}}
	for i, method := range []string{proto.RPCNodeDoctor, proto.RPCAgentUpgrade, proto.RPCImageExists} {
		raw, _ := json.Marshal(map[string]string{"method": method})
		c.handleRPC(context.Background(), proto.Envelope{T: proto.TRPCReq, ID: uint64(i + 1), D: raw})
	}
	if len(h.methods) != 1 || h.methods[0] != proto.RPCImageExists {
		t.Fatalf("handler ran %v, want only the allowed RPC", h.methods)
	}
	if got := strings.Count(string(ctrl.BytesCopy()), hostsim.ErrRefused.Error()); got != 2 {
		t.Fatalf("refusals = %d in %q", got, ctrl.BytesCopy())
	}

	real := &countingRPCHandlers{}
	c = &Conn{handlers: real, ctrl: &nopRWC{}, log: slog.Default()}
	c.handleRPC(context.Background(), proto.Envelope{T: proto.TRPCReq, ID: 1, D: json.RawMessage(`{"method":"node.doctor"}`)})
	if len(real.methods) != 1 {
		t.Fatalf("real node handler ran %v", real.methods)
	}
}

// TestSimulatedNodeRefusesStreamsAtTheDoor: a refused stream is closed
// before any handler sees it.
func TestSimulatedNodeRefusesStreamsAtTheDoor(t *testing.T) {
	clientConn, serverConn := pipe(t)
	handlers := &streamTestHandlers{}
	c := &Conn{handlers: handlers, log: slog.Default(), creds: Credentials{Simulated: simulatedHost()}}
	for _, hdr := range []proto.StreamHeader{
		{Kind: proto.StreamModelRead, Dst: "/"},
		{Kind: proto.StreamImageRead, Ref: "x"},
		{Kind: proto.StreamTunnel, Dst: "127.0.0.1:22"},
		{Kind: "future"},
	} {
		stream, err := clientConn.OpenStream(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			if accepted, err := serverConn.AcceptStream(); err == nil {
				c.handleStream(context.Background(), accepted)
			}
			close(done)
		}()
		header, _ := json.Marshal(hdr)
		if _, err := stream.Write(append(header, '\n')); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s stream not closed", hdr.Kind)
		}
		_ = stream.Close()
	}
	handlers.mu.Lock()
	defer handlers.mu.Unlock()
	if len(handlers.modelReads) != 0 || len(handlers.imageReads) != 0 {
		t.Fatalf("a refused stream reached its handler: %+v", handlers)
	}
}

func pipe(t *testing.T) (client, server *yamux.Session) {
	t.Helper()
	a, b := net.Pipe()
	client, err := yamux.Client(a, yamux.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err = yamux.Server(b, yamux.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

func TestSimulatedInventoryCarriesTheOtherNodesEnginePorts(t *testing.T) {
	ctrl := &nopRWC{}
	c := New(Credentials{NodeUUID: "node-1", Simulated: simulatedHost()}, nil, &inventoryHandlers{}, &gpu.FixedProber{VendorName: "nvidia"}, slog.Default())
	c.ctrl = ctrl
	c.enginePorts.Store(&proto.PortRange{Start: 30000, End: 30005})
	c.reportStatus(context.Background())
	statuses := sentStatuses(t, ctrl)
	if len(statuses) != 1 || statuses[0].Network == nil {
		t.Fatalf("statuses = %+v", statuses)
	}
	if got := statuses[0].Network.EnginePortListeners; len(got) != 3 || got[0] != 30001 || got[2] != 30005 {
		t.Fatalf("listeners = %v, want the simulated host's", got)
	}
	if statuses[0].Network.Interfaces[0].Name != "sim0" {
		t.Fatalf("simulated inventory replaced: %+v", statuses[0].Network)
	}
	if statuses[0].Sys.Hostname != "sim-01" {
		t.Fatalf("hostname = %q, want the simulated machine's", statuses[0].Sys.Hostname)
	}

	// A hermetic agent that is not a simulated node keeps reporting none.
	ctrl = &nopRWC{}
	c = New(Credentials{NodeUUID: "node-1"}, nil, &inventoryHandlers{}, &gpu.FixedProber{VendorName: "test"}, slog.Default())
	c.ctrl = ctrl
	c.enginePorts.Store(&proto.PortRange{Start: 30000, End: 30005})
	c.reportStatus(context.Background())
	if got := sentStatuses(t, ctrl)[0].Network.EnginePortListeners; got != nil {
		t.Fatalf("hermetic listeners = %v, want none", got)
	}
}
