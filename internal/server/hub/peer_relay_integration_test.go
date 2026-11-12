package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentapp "gpuworks.ai/gpuworks/internal/agent/app"
	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Keep each fixture's temporary model root while using the production RPC,
// stream dispatch, TLS protocol, archive generation and receiver implementation.
type relayIntegrationHandlers struct{ *agentapp.NodeRuntime }

func (*relayIntegrationHandlers) OnStateSync(context.Context, proto.DesiredState)       {}
func (*relayIntegrationHandlers) OnStateUpdate(context.Context, proto.StateUpdate) bool { return false }

type peerRelayRig struct {
	h                            *Hub
	source, target               *Session
	sourceRoot, targetRoot       string
	sourceRuntime, targetRuntime *agentapp.NodeRuntime
	sourceDriver                 runtime.Driver
	targetDriver                 *recording.Driver
}

func newPeerRelayRig(t *testing.T, sourceDriver runtime.Driver, topology ...string) *peerRelayRig {
	t.Helper()
	return newPeerRelayRigOnHub(t, newTestHub(t), sourceDriver, topology...)
}

func newPeerRelayRigOnHub(t *testing.T, h *Hub, sourceDriver runtime.Driver, topology ...string) *peerRelayRig {
	t.Helper()
	return newPeerRelayRigWithTarget(t, h, sourceDriver, nil, topology...)
}

// newPeerRelayRigWithTarget lets a test wrap the target's runtime driver, for
// example to make the receiver slow after it has consumed the archive.
func newPeerRelayRigWithTarget(t *testing.T, h *Hub, sourceDriver runtime.Driver, wrapTarget func(*recording.Driver) runtime.Driver, topology ...string) *peerRelayRig {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(http.HandlerFunc(h.ServeWS))
	t.Cleanup(server.Close)
	r := &peerRelayRig{h: h, sourceRoot: filepath.Join(t.TempDir(), "models"), targetRoot: filepath.Join(t.TempDir(), "models")}
	if sourceDriver == nil {
		sourceDriver = recording.New(filepath.Join(t.TempDir(), "source.json"))
	}
	r.sourceDriver = sourceDriver
	r.targetDriver = recording.New(filepath.Join(t.TempDir(), "target.json"))
	r.sourceRuntime = agentapp.NewNodeRuntime(log, sourceDriver, r.sourceRoot)
	var targetDriver runtime.Driver = r.targetDriver
	if wrapTarget != nil {
		targetDriver = wrapTarget(r.targetDriver)
	}
	r.targetRuntime = agentapp.NewNodeRuntime(log, targetDriver, r.targetRoot)
	for _, entry := range []struct {
		name    string
		node    *agentapp.NodeRuntime
		session **Session
	}{{"source", r.sourceRuntime, &r.source}, {"target", r.targetRuntime, &r.target}} {
		node, token, err := h.store.CreateNodePending(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		if len(topology) > 0 && (topology[0] == "server_dial" || topology[0] == "mixed" && entry.name == "target") {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				done <- agentconn.ServeServerDial(ctx, listener, func() *agentconn.Conn {
					return agentconn.New(agentconn.Credentials{NodeToken: token, NodeUUID: node.UUID, ConnectMode: "server_dial", DataLane: true}, nil, &relayIntegrationHandlers{entry.node}, emptyProber{}, log)
				}, nil)
			}()
			if err := h.DialAgentWithSecret(ctx, listener.Addr().String(), token); err != nil {
				cancel()
				t.Fatal(err)
			}
		} else {
			c := agentconn.New(agentconn.Credentials{ServerURL: server.URL, NodeToken: token, NodeUUID: node.UUID, DataLane: true}, nil, &relayIntegrationHandlers{entry.node}, emptyProber{}, log)
			go func() { done <- c.Run(ctx) }()
		}
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("peer integration agent leaked")
			}
		})
		waitFor(t, 5*time.Second, func() bool { s := h.session(node.UUID); return s != nil && s.DataLaneState() == "attached" }, entry.name+" dual lane")
		*entry.session = h.session(node.UUID)
	}
	return r
}

func (r *peerRelayRig) prepare(t *testing.T, kind, id string) (string, any) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if kind == "model" {
		dir := filepath.Join(r.sourceRoot, "source")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{"config.json": []byte(`{}`), "model.safetensors": bytes.Repeat([]byte("weights"), 4096)} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
				t.Fatal(err)
			}
		}
		wire, err := r.target.call(ctx, proto.RPCModelSyncListen, proto.ModelSyncListenParams{TransferID: id, Key: id, TargetPath: id, Token: "once", BindIP: "127.0.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		var ready proto.ModelSyncListenResult
		if err := json.Unmarshal(wire, &ready); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = r.target.call(ctx, proto.RPCModelSyncStop, proto.ModelSyncStopParams{TransferID: id, Key: id})
		})
		return proto.RPCModelSyncSend, proto.ModelSyncSendParams{TransferID: id, SourcePath: "source", Token: "once", Endpoints: []string{unreachable}, Fingerprint: ready.Fingerprint, AllowRelay: true}
	}
	wire, err := r.target.call(ctx, proto.RPCImageSyncListen, proto.ImageSyncListenParams{TransferID: id, ImageRef: "example/relay:" + id, Token: "once", BindIP: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	var ready proto.ImageSyncListenResult
	if err := json.Unmarshal(wire, &ready); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = r.target.call(ctx, proto.RPCImageSyncStop, proto.ImageSyncStopParams{TransferID: id})
	})
	return proto.RPCImageSyncSend, proto.ImageSyncSendParams{TransferID: id, ImageRef: "example/relay:" + id, Token: "once", Endpoints: []string{unreachable}, Fingerprint: ready.Fingerprint, AllowRelay: true}
}

func TestPeerRelayIntegrationDirectFailureProbeAndConfirmedArchive(t *testing.T) {
	for _, mode := range []string{"agent_dial", "mixed", "server_dial"} {
		t.Run(mode, func(t *testing.T) {
			r := newPeerRelayRig(t, nil, mode)
			for _, kind := range []string{"model", "image"} {
				t.Run(kind, func(t *testing.T) {
					method, params := r.prepare(t, kind, "verified-"+kind)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					raw, err := r.source.call(ctx, method, params)
					if err != nil {
						t.Fatal(err)
					}
					var direct struct {
						DirectDialError string `json:"direct_dial_error"`
					}
					if err := json.Unmarshal(raw, &direct); err != nil || direct.DirectDialError == "" {
						t.Fatalf("direct result=%s err=%v", raw, err)
					}
					probe := proto.RPCModelSyncProbe
					id := "verified-" + kind
					if kind == "image" {
						probe = proto.RPCImageSyncProbe
					}
					relay := r.h.archiveRelayFor(r.source.NodeUUID, r.target.NodeUUID, kind)
					if _, err := relay(ctx, probe, id, params); err != nil {
						t.Fatalf("relay probe: %v", err)
					}
					raw, err = relay(ctx, method, id, params)
					if err != nil {
						t.Fatal(err)
					}
					var result struct {
						Bytes  int64  `json:"bytes"`
						SHA256 string `json:"sha256"`
					}
					if err := json.Unmarshal(raw, &result); err != nil || result.Bytes == 0 || len(result.SHA256) != 64 {
						t.Fatalf("unconfirmed result=%s err=%v", raw, err)
					}
					if kind == "model" {
						data, err := os.ReadFile(filepath.Join(r.targetRoot, id, "model.safetensors"))
						if err != nil || !bytes.Equal(data, bytes.Repeat([]byte("weights"), 4096)) {
							t.Fatalf("model target differs: bytes=%d err=%v", len(data), err)
						}
					} else {
						calls := strings.Join(r.targetDriver.Calls(), "\n")
						if !strings.Contains(calls, "LoadImage(example/relay:"+id+")") {
							t.Fatalf("image target not loaded: %s", calls)
						}
					}
					if r.source.CtrlDataPlaneStreamCount() != 0 || r.target.CtrlDataPlaneStreamCount() != 0 {
						t.Fatal("bulk relay used ctrl")
					}
				})
			}
		})
	}
}

// A blocked producer lets failure happen during an archive, rather than only
// before TCP/TLS. It can finish only when its producer context is canceled.
type relayGateDriver struct {
	*recording.Driver
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func (d *relayGateDriver) SaveImage(ctx context.Context, _ string, w io.Writer) error {
	if _, err := w.Write(bytes.Repeat([]byte("first"), 16384)); err != nil {
		return err
	}
	d.once.Do(func() { close(d.started) })
	<-ctx.Done()
	close(d.canceled)
	return ctx.Err()
}

func TestPeerRelayIntegrationFailureNeverReturnsSuccess(t *testing.T) {
	for _, failure := range []string{"cancel", "data-disconnect", "target-data-disconnect", "fingerprint"} {
		t.Run(failure, func(t *testing.T) {
			driver := &relayGateDriver{Driver: recording.New(filepath.Join(t.TempDir(), "source.json")), started: make(chan struct{}), canceled: make(chan struct{})}
			r := newPeerRelayRig(t, driver)
			method, params := r.prepare(t, "image", "failed")
			if failure == "fingerprint" {
				p := params.(proto.ImageSyncSendParams)
				p.Fingerprint = strings.Repeat("0", 64)
				params = p
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := r.h.archiveRelayFor(r.source.NodeUUID, r.target.NodeUUID, "image")(ctx, method, "failed", params)
				result <- err
			}()
			if failure != "fingerprint" {
				select {
				case <-driver.started:
				case err := <-result:
					t.Fatalf("transfer stopped before producer: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if failure == "cancel" {
					cancel()
				} else {
					session := r.source
					if failure == "target-data-disconnect" {
						session = r.target
					}
					session.dataMu.Lock()
					data := session.data
					session.dataMu.Unlock()
					data.Close()
				}
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("failed relay returned success")
				}
				if failure == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("failed relay did not terminate")
			}
			if failure != "fingerprint" {
				select {
				case <-driver.canceled:
				case <-time.After(2 * time.Second):
					t.Fatal("producer did not observe data/stream cancellation")
				}
			}
			aliveCtx, aliveCancel := context.WithTimeout(context.Background(), time.Second)
			defer aliveCancel()
			if _, err := r.source.call(aliveCtx, proto.RPCImageSyncStop, proto.ImageSyncStopParams{TransferID: "failed"}); err != nil {
				t.Fatalf("relay failure broke ctrl: %v", err)
			}
			if _, err := r.target.call(aliveCtx, proto.RPCImageSyncStop, proto.ImageSyncStopParams{TransferID: "failed"}); err != nil {
				t.Fatalf("relay failure broke target ctrl: %v", err)
			}
			r.source.pendMu.Lock()
			pending := len(r.source.pending)
			r.source.pendMu.Unlock()
			r.target.pendMu.Lock()
			pending += len(r.target.pending)
			r.target.pendMu.Unlock()
			relays := r.h.PeerTransferSnapshot().ActiveCount
			if pending != 0 || relays != 0 {
				t.Fatalf("relay resources leaked: pending=%d slots=%d", pending, relays)
			}
		})
	}
}
