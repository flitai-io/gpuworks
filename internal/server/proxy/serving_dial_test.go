package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fakeForwarder implements the agent side of proto.ServingForwardMagic: TLS
// with the node-token-derived key, token check, then a splice to upstream.
type fakeForwarder struct {
	ln       net.Listener
	accepted atomic.Int32
}

func startFakeForwarder(t *testing.T, nodeToken, upstream string, known map[string]bool) *fakeForwarder {
	t.Helper()
	cert, err := proto.ServingForwardCertificate(nodeToken)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeForwarder{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.accepted.Add(1)
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				line, _ := r.ReadString('\n')
				fields := strings.Fields(line)
				if len(fields) != 3 || !proto.ValidServingForwardToken(nodeToken, fields[1], fields[2]) {
					io.WriteString(c, "ERR rejected authentication\n")
					return
				}
				if !known[fields[1]] {
					io.WriteString(c, "ERR instance is not running here\n")
					return
				}
				up, err := net.Dial("tcp", upstream)
				if err != nil {
					io.WriteString(c, "ERR instance port\n")
					return
				}
				defer up.Close()
				io.WriteString(c, "OK\n")
				go io.Copy(up, r)
				io.Copy(c, up)
			}()
		}
	}()
	return f
}

type recorded struct {
	mu       sync.Mutex
	statuses []store.ServingForwardStatus
}

func (r *recorded) record(_ string, st store.ServingForwardStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, st)
}

func (r *recorded) modes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.statuses {
		out = append(out, s.Mode+":"+s.Reason)
	}
	return out
}

func testDialer(target string, rec *recorded) (*servingDialer, *atomic.Int32, *time.Time) {
	var dials atomic.Int32
	now := time.Unix(1000, 0)
	d := newServingDialer(slog.New(slog.NewTextHandler(io.Discard, nil)), rec.record)
	d.now = func() time.Time { return now }
	d.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	return d, &dials, &now
}

func TestServingDialerUsesTheForwarderAndReportsChangesOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "engine") }))
	defer upstream.Close()
	fwd := startFakeForwarder(t, "tok-a", strings.TrimPrefix(upstream.URL, "http://"), map[string]bool{"inst-1": true})
	rec := &recorded{}
	d, _, _ := testDialer(fwd.ln.Addr().String(), rec)
	ep := network.ServingEndpoint{IP: "10.40.0.5", NodeToken: "tok-a"}
	for i := 0; i < 2; i++ {
		conn, err := d.open(context.Background(), ep, "node-a", "inst-1")
		if err != nil {
			t.Fatalf("direct path failed: %v", err)
		}
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
		body, _ := io.ReadAll(conn)
		conn.Close()
		if !strings.HasSuffix(string(body), "engine") {
			t.Fatalf("body = %q", body)
		}
	}
	if got := rec.modes(); len(got) != 1 || got[0] != "direct:" {
		t.Fatalf("statuses = %v, want one direct report", got)
	}
	if _, err := d.open(context.Background(), ep, "node-a", "inst-9"); err == nil {
		t.Fatal("unknown instance opened")
	}
	if got := rec.modes(); got[len(got)-1] != "unreachable:forwarder: instance is not running here" {
		t.Fatalf("statuses = %v", got)
	}
}

// A failure is reported once and every request tries the network again:
// there is no tunnel to fall back to, so no reason to stay away from it.
func TestServingDialerReportsFailuresAndKeepsTrying(t *testing.T) {
	fwd := startFakeForwarder(t, "some-other-node", "127.0.0.1:1", nil)
	rec := &recorded{}
	d, dials, _ := testDialer(fwd.ln.Addr().String(), rec)
	ep := network.ServingEndpoint{IP: "10.40.0.5", NodeToken: "tok-a"}
	_, err := d.open(context.Background(), ep, "node-a", "inst-1")
	if err == nil || !strings.Contains(err.Error(), "10.40.0.5 unreachable") {
		t.Fatalf("pin mismatch err = %v", err)
	}
	if got := rec.modes(); len(got) != 1 || !strings.Contains(got[0], "does not match") {
		t.Fatalf("statuses = %v", got)
	}
	if _, err := d.open(context.Background(), ep, "node-a", "inst-1"); err == nil || dials.Load() != 2 {
		t.Fatalf("second attempt err = %v dials = %d", err, dials.Load())
	}
	if len(rec.modes()) != 1 {
		t.Fatalf("an unchanged failure was reported again: %v", rec.modes())
	}
}

type countingHub struct {
	addr    string
	tunnels atomic.Int32
}

func (h *countingHub) OpenTunnel(context.Context, string, string) (net.Conn, error) {
	h.tunnels.Add(1)
	return net.Dial("tcp", h.addr)
}
func (h *countingHub) Online(string) bool { return true }

func TestProxyReachesInstancesOverTheServingNetwork(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)
	st, err := store.Open("file:proxy_serving_e2e?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "m"})
	node, token, err := st.CreateNodePending("gpu-1")
	if err != nil {
		t.Fatal(err)
	}
	inv := proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth1", Addrs: []string{"10.40.0.5/24"}}}}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{Network: &inv}); err != nil {
		t.Fatal(err)
	}
	svc, _ := st.CreateNetwork(&store.Network{Name: "svc", Type: netplan.TypeHostEthernet, CIDR: "10.40.0.0/24", Roles: store.EncodeStringList([]string{netplan.RoleServing})})
	if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: svc.ID, NodeID: node.ID}); err != nil {
		t.Fatal(err)
	}
	dep, _ := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "m", Engine: "vllm", State: "active"})
	if _, err := st.CreateInstance(&store.Instance{UUID: "inst-1", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30000, Ports: "[30000]"}); err != nil {
		t.Fatal(err)
	}
	apiKey, _ := st.CreateAPIKey("k", 0)
	upstreamAddr := strings.TrimPrefix(upstream.URL, "http://")
	fwd := startFakeForwarder(t, token, upstreamAddr, map[string]bool{"inst-1": true})
	hub := &countingHub{addr: upstreamAddr}
	p := New(st, storeNetworks(st), hub, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	t.Cleanup(p.Close)
	var dialed atomic.Value
	p.serving.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed.Store(addr)
		return (&net.Dialer{}).DialContext(ctx, network, fwd.ln.Addr().String())
	}
	call := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.Handler()(rec, req)
		return rec.Code
	}
	if code := call(); code != http.StatusOK {
		t.Fatalf("direct status = %d", code)
	}
	if hub.tunnels.Load() != 0 || fwd.accepted.Load() != 1 || dialed.Load() != "10.40.0.5:19443" {
		t.Fatalf("tunnels=%d forwarded=%d dialed=%v, want the serving network", hub.tunnels.Load(), fwd.accepted.Load(), dialed.Load())
	}
	stored, _ := st.NodeByUUID(node.UUID)
	if s, ok := store.NodeServingForward(stored.ServingForward); !ok || s.Mode != store.ServingForwardDirect {
		t.Fatalf("status = %q", stored.ServingForward)
	}

	// The forwarder going away fails the request: a node on a serving network
	// never reaches its instances over the agent tunnel.
	fwd.ln.Close()
	if code := call(); code < 500 {
		t.Fatalf("status with the serving network down = %d", code)
	}
	if hub.tunnels.Load() != 0 {
		t.Fatalf("fell back to the tunnel %d times", hub.tunnels.Load())
	}
	stored, _ = st.NodeByUUID(node.UUID)
	if s, _ := store.NodeServingForward(stored.ServingForward); s.Mode != store.ServingForwardUnreachable || s.Reason == "" {
		t.Fatalf("status with the network down = %q", stored.ServingForward)
	}
}

// storeNetworks is the network service over every network in st, as the app
// wires it.
func storeNetworks(st *store.Store) Networks {
	return network.New(st, network.StoreCatalog{Store: st})
}
