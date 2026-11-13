package terminal

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/webterm"
)

type terminalHarness struct {
	store     *store.Store
	conn      net.Conn
	signer    *auth.TokenSigner
	online    bool
	caps      []string
	agentDone chan struct{}
}

func newHarness(t *testing.T) (*terminalHarness, *httptest.Server, string) {
	t.Helper()
	st, err := store.Open("file:terminal-"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("terminal-node", store.HashToken("token"), "terminal-host")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("runtime_driver", "docker").Error; err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("terminal-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &terminalHarness{store: st, online: true, caps: []string{proto.CapWebterm}, agentDone: make(chan struct{})}
	mgr := webterm.New(st)
	handler := api.Router(api.Deps{
		Store: st, Bus: st.Bus, AuthSigner: signer,
		Online:   func(string) bool { return h.online },
		NodeCaps: func(string) []string { return h.caps },
		Terminal: mgr,
		OpenTerminal: func(_ string, target, ref string, rows, cols uint16, grace int) (net.Conn, error) {
			if target != proto.TermTargetHost || ref != "" || rows != 40 || cols != 120 || grace <= 0 {
				return nil, context.Canceled
			}
			server, agent := net.Pipe()
			h.conn = server
			go func() {
				defer close(h.agentDone)
				fakeAgent(agent)
			}()
			return server, nil
		},
	})
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		server.Close()
	})
	token := terminalSessionTestToken(t, st, signer, "admin")
	h.signer = signer
	return h, server, node.UUID + "?token=" + token
}

func fakeAgent(c net.Conn) {
	defer c.Close()
	info, _ := json.Marshal(proto.TermSessionInfo{User: "root", UID: 0, Shell: "/bin/sh", Cwd: "/root", Host: "fake-node"})
	if proto.WriteTermFrame(c, proto.TermInfo, info) != nil {
		return
	}
	reader := proto.NewTermFrameReader(c)
	for {
		frame, err := reader.Next()
		if err != nil {
			return
		}
		if frame.Type != proto.TermData {
			continue
		}
		if proto.WriteTermData(c, []byte(strings.ToUpper(string(frame.Payload)))) != nil {
			return
		}
		if strings.Contains(string(frame.Payload), "quit") {
			_ = proto.WriteTermFrame(c, proto.TermExit, proto.EncodeExit(42, "done"))
			return
		}
	}
}

func TestTerminalBridgeTranslatesFramesAndAuditsSession(t *testing.T) {
	h, server, nodeQuery := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parts := strings.SplitN(nodeQuery, "?token=", 2)
	opts := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + parts[1]}}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/nodes/"+parts[0]+"/terminal?rows=40&cols=120", opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("terminal info read type=%v err=%v", typ, err)
	}
	var info map[string]any
	if json.Unmarshal(data, &info) != nil || info["type"] != "info" || info["user"] != "root" {
		t.Fatalf("terminal info=%s", data)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	typ, data, err = c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(data) != "HELLO" {
		t.Fatalf("echo type=%v data=%q err=%v", typ, data, err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, []byte("quit")); err != nil {
		t.Fatal(err)
	}
	for {
		typ, data, err = c.Read(ctx)
		if err != nil {
			break
		}
		if typ == websocket.MessageText && strings.Contains(string(data), `"type":"exit"`) {
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := h.store.TerminalSessions(10)
		if len(rows) > 0 && rows[0].EndedAt != nil {
			if rows[0].BytesIn == 0 || rows[0].BytesOut == 0 || rows[0].CloseReason != webterm.ReasonShellExited {
				t.Fatalf("audit row=%+v", rows[0])
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("terminal audit row was not closed")
}

func TestTerminalAdmissionRejectsMissingCapability(t *testing.T) {
	st, err := store.Open("file:terminal-admission?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("old-node", store.HashToken("token"), "host")
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("terminal-admission", time.Hour)
	token := terminalSessionTestToken(t, st, signer, "admin")
	h := api.Router(api.Deps{Store: st, Bus: st.Bus, AuthSigner: signer, Online: func(string) bool { return true }, NodeCaps: func(string) []string { return nil }, Terminal: webterm.New(st), OpenTerminal: func(string, string, string, uint16, uint16, int) (net.Conn, error) { return nil, context.Canceled }})
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/nodes/"+node.UUID+"/terminal", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("missing capability status=%d", resp.StatusCode)
	}
}

func TestTerminalAdmissionMatrixAndSettingsPreflight(t *testing.T) {
	h, server, nodeQuery := newHarness(t)
	parts := strings.SplitN(nodeQuery, "?token=", 2)
	adminToken := parts[1]
	request := func(path, token string) int {
		req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	base := "/api/v1/nodes/" + parts[0] + "/terminal"
	if got := request(base, ""); got != http.StatusUnauthorized {
		t.Fatalf("unauthenticated terminal status=%d", got)
	}
	userToken := terminalSessionTestToken(t, h.store, h.signer, "user")
	if got := request(base, userToken); got != http.StatusForbidden {
		t.Fatalf("non-admin terminal status=%d", got)
	}
	if got := request(base+"/settings", adminToken); got != http.StatusOK {
		t.Fatalf("terminal settings status=%d", got)
	}
	if err := h.store.SetSetting(store.SettingTerminalPolicy, `{"enabled":false,"idle_timeout_min":10,"grace_sec":5,"max_per_node":2,"max_global":4}`); err != nil {
		t.Fatal(err)
	}
	if got := request(base, adminToken); got != http.StatusForbidden {
		t.Fatalf("disabled terminal status=%d", got)
	}
	if err := h.store.SetSetting(store.SettingTerminalPolicy, `{"enabled":true,"idle_timeout_min":10,"grace_sec":5,"max_per_node":2,"max_global":4}`); err != nil {
		t.Fatal(err)
	}
	h.online = false
	if got := request(base, adminToken); got != http.StatusServiceUnavailable {
		t.Fatalf("offline terminal status=%d", got)
	}
	h.online = true
	h.caps = nil
	if got := request(base, adminToken); got != http.StatusServiceUnavailable {
		t.Fatalf("missing-capability terminal status=%d", got)
	}
	h.caps = []string{proto.CapWebterm}
	if got := request("/api/v1/nodes/does-not-exist/terminal", adminToken); got != http.StatusNotFound {
		t.Fatalf("unknown-node terminal status=%d", got)
	}
	if err := h.store.DB().Model(&store.Node{}).Where("uuid = ?", parts[0]).Update("runtime_driver", "udocker").Error; err != nil {
		t.Fatal(err)
	}
	if got := request(base+"?target=container&ref=container-1", adminToken); got != http.StatusBadRequest {
		t.Fatalf("udocker container terminal status=%d", got)
	}
	// The preflight must identify the runtime-specific capability gate, not just
	// return the generic "container unsupported" boolean. This fake runtime does
	// not implement the controlled-shell extension, so admission remains closed.
	preReq, err := http.NewRequest(http.MethodGet, server.URL+base+"/settings", nil)
	if err != nil {
		t.Fatal(err)
	}
	preReq.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(preReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("udocker preflight status=%d", resp.StatusCode)
	}
	var pre struct {
		ContainerSupported  bool   `json:"container_supported"`
		ContainerReason     string `json:"container_reason"`
		ContainerRuntime    string `json:"container_runtime"`
		ContainerCapability string `json:"container_capability"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pre); err != nil {
		t.Fatal(err)
	}
	if pre.ContainerSupported || pre.ContainerRuntime != "udocker" || pre.ContainerCapability != proto.CapWebtermContainerUdocker || !strings.Contains(pre.ContainerReason, "unavailable") {
		t.Fatalf("udocker preflight = %+v", pre)
	}
}

func TestTerminalClientHangupAuditsClientClosed(t *testing.T) {
	h, server, nodeQuery := newHarness(t)
	parts := strings.SplitN(nodeQuery, "?token=", 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + parts[1]}}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/nodes/"+parts[0]+"/terminal?rows=40&cols=120", opts)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = c.Read(ctx) // info frame
	_ = c.Close(websocket.StatusNormalClosure, "client closed")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := h.store.TerminalSessions(10)
		if len(rows) > 0 && rows[0].EndedAt != nil {
			if rows[0].CloseReason != webterm.ReasonClientClosed {
				t.Fatalf("client hangup audit reason=%q", rows[0].CloseReason)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("client hangup audit row was not closed")
}
