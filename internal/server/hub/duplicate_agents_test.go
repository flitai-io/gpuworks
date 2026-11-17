package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// syncBuffer collects log output written from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

// duplicateHub is a hub whose log is captured and whose duplicate-agent
// clock the test moves.
func duplicateHub(t *testing.T) (*Hub, string, *syncBuffer, *time.Time) {
	t.Helper()
	h := newTestHub(t)
	logs := &syncBuffer{}
	h.log = slog.New(slog.NewTextHandler(logs, nil))
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	uuid, _ := newTestNode(t, h.store)
	return h, uuid, logs, &now
}

// connect registers a session for the node as the agent process instance
// would, replacing the node's current one.
func connect(t *testing.T, h *Hub, uuid, instance, host, remote string) *Session {
	t.Helper()
	ctrl, peer := net.Pipe()
	s := newSession(h, nil, ctrl, remote)
	s.NodeUUID = uuid
	s.instanceID, s.hostname = instance, host
	t.Cleanup(func() { s.Close(); _ = peer.Close() })
	h.register(s)
	return s
}

// The issue's flapping: two processes with one node token keep replacing
// each other's session. The second time an evicted process takes the node
// back, the hub reports it once, coded, and not again within the hour.
func TestRegisterFlagsTwoInstancesTradingOneNode(t *testing.T) {
	h, uuid, logs, now := duplicateHub(t)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5000")
	connect(t, h, uuid, "proc-b", "gpu-1", "10.0.0.1:5001")
	if logs.count(duplicateAgentCode) != 0 {
		t.Fatal("flagged a single replacement")
	}
	*now = now.Add(30 * time.Second)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5002")
	if got := logs.count(duplicateAgentCode); got != 1 {
		t.Fatalf("duplicate reports = %d, want 1", got)
	}
	for i := 0; i < 4; i++ {
		*now = now.Add(30 * time.Second)
		connect(t, h, uuid, "proc-b", "gpu-1", "10.0.0.1:5003")
		connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5004")
	}
	if got := logs.count(duplicateAgentCode); got != 1 {
		t.Fatalf("duplicate reports within the hour = %d, want 1", got)
	}
	// Still trading an hour on: reported again.
	for i := 0; i < 12; i++ {
		*now = now.Add(5 * time.Minute)
		connect(t, h, uuid, "proc-b", "gpu-1", "10.0.0.1:5005")
		connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5006")
	}
	if got := logs.count(duplicateAgentCode); got != 2 {
		t.Fatalf("duplicate reports after the hour = %d, want 2", got)
	}
}

// An upgrade or restart replaces the process once; the old one never comes
// back, and one process reconnecting replaces only itself.
func TestRegisterInPlaceRestartIsNotADuplicate(t *testing.T) {
	h, uuid, logs, now := duplicateHub(t)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5000")
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5001")
	connect(t, h, uuid, "proc-b", "gpu-1", "10.0.0.1:5002")
	*now = now.Add(time.Minute)
	connect(t, h, uuid, "proc-c", "gpu-1", "10.0.0.1:5003")
	connect(t, h, uuid, "proc-c", "gpu-1", "10.0.0.1:5004")
	if got := logs.count(duplicateAgentCode); got != 0 {
		t.Fatalf("duplicate reports = %d, want 0", got)
	}
}

// An evicted process that comes back long after is a restart, not a trade.
func TestRegisterForgetsAnEvictionOutsideTheWindow(t *testing.T) {
	h, uuid, logs, now := duplicateHub(t)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5000")
	connect(t, h, uuid, "proc-b", "gpu-1", "10.0.0.1:5001")
	*now = now.Add(11 * time.Minute)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5002")
	if got := logs.count(duplicateAgentCode); got != 0 {
		t.Fatalf("duplicate reports = %d, want 0", got)
	}
}

func TestRegisterIgnoresAgentsWithoutInstanceID(t *testing.T) {
	h, uuid, logs, _ := duplicateHub(t)
	for i := 0; i < 4; i++ {
		connect(t, h, uuid, "", "gpu-1", "10.0.0.1:5000")
	}
	if got := logs.count(duplicateAgentCode); got != 0 {
		t.Fatalf("duplicate reports = %d, want 0", got)
	}
}

// An agent too old to send an instance id cannot be told apart from its own
// reconnects, but one that does send an id is still caught trading with it.
func TestRegisterCatchesAnAgentWithIDTradingWithOneWithout(t *testing.T) {
	h, uuid, logs, now := duplicateHub(t)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5000")
	connect(t, h, uuid, "", "gpu-2", "10.0.0.2:5000")
	*now = now.Add(10 * time.Second)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5001")
	if got := logs.count(duplicateAgentCode); got != 1 {
		t.Fatalf("duplicate reports = %d, want 1", got)
	}
	if !strings.Contains(logs.buf.String(), "other_host=gpu-2") {
		t.Fatalf("report does not name the other host: %s", logs.buf.String())
	}
}

// The hub keeps a trace only while it can still matter: once neither the
// window nor the report period covers it, the node's entry is gone, so a
// node deleted mid-trade, or one upgraded once, leaves nothing behind.
func TestRegisterDropsTracesOnceTheyExpire(t *testing.T) {
	h, uuid, _, now := duplicateHub(t)
	other, _ := newTestNode(t, h.store)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5000")
	connect(t, h, uuid, "proc-b", "gpu-1", "10.0.0.1:5001")
	*now = now.Add(30 * time.Second)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5002") // reported
	if n := h.duplicates.tracked(); n != 1 {
		t.Fatalf("tracked nodes = %d, want 1", n)
	}
	*now = now.Add(duplicateAgentReportEvery + duplicateAgentWindow)
	connect(t, h, other, "proc-x", "gpu-9", "10.0.0.9:5000")
	if n := h.duplicates.tracked(); n != 0 {
		t.Fatalf("tracked nodes after expiry = %d, want 0", n)
	}
}

// Through the real handshake: two agents with the node's token and their
// own instance ids, each taking the connection back, are reported; an
// instance id longer than the hub remembers is treated as none. (The hub's
// real clock: handshake deadlines come from it.)
func TestHandshakeReportsTwoAgentsSharingOneToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    string
		want int
	}{
		{"short ids", "proc-a", 1},
		{"oversized id", strings.Repeat("a", maxInstanceIDLen+1), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHub(t)
			logs := &syncBuffer{}
			h.log = slog.New(slog.NewTextHandler(logs, nil))
			_, tok := newTestNode(t, h.store)
			srv := httptest.NewServer(http.HandlerFunc(h.ServeWS))
			t.Cleanup(srv.Close)
			for _, id := range []string{tc.a, "proc-b", tc.a} {
				dialCtrlLaneWithHello(t, srv, proto.Hello{ProtoMin: 1, ProtoMax: 1, NodeToken: tok, Hostname: "gpu-1", InstanceID: id})
			}
			// register logs before Registered is sent, so the count is final.
			if got := logs.count(duplicateAgentCode); got != tc.want {
				t.Fatalf("duplicate reports = %d, want %d", got, tc.want)
			}
		})
	}
}

// dialCtrlLaneWithHello is dialCtrlLane with the agent's hello given.
func dialCtrlLaneWithHello(t *testing.T, srv *httptest.Server, hello proto.Hello) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/agent/connect?lane=ctrl"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial ctrl: %v", err)
	}
	netConn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	ysess, err := yamux.Client(netConn, laneYamuxConfig(proto.LaneCtrl, io.Discard), nil)
	if err != nil {
		t.Fatalf("yamux: %v", err)
	}
	t.Cleanup(func() { _ = ysess.Close() })
	ctrl, err := ysess.OpenStream(context.Background())
	if err != nil {
		t.Fatalf("open ctrl: %v", err)
	}
	hdr, _ := json.Marshal(proto.StreamHeader{Kind: proto.StreamCtrl})
	_, _ = ctrl.Write(append(hdr, '\n'))
	_ = json.NewEncoder(ctrl).Encode(proto.Envelope{T: proto.THello, D: mustJSON(t, hello)})
	dec := json.NewDecoder(ctrl)
	var reg proto.Registered
	readEnvelopeInto(t, dec, proto.TRegistered, &reg)
}

// When the evicted process comes back to a node the other one has just left,
// the report still names the process that took the node from it.
func TestRegisterNamesTheEvictorWhenItHasGone(t *testing.T) {
	h, uuid, logs, now := duplicateHub(t)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5000")
	b := connect(t, h, uuid, "proc-b", "gpu-2", "10.0.0.2:5000")
	h.deregister(b)
	*now = now.Add(10 * time.Second)
	connect(t, h, uuid, "proc-a", "gpu-1", "10.0.0.1:5001")
	if got := logs.count(duplicateAgentCode); got != 1 {
		t.Fatalf("duplicate reports = %d, want 1", got)
	}
	if out := logs.buf.String(); !strings.Contains(out, "other_host=gpu-2") || !strings.Contains(out, "other_remote=10.0.0.2:5000") {
		t.Fatalf("report does not name the evicting agent: %s", out)
	}
}
