package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/testenv/testport"
)

func freePortFor(t *testing.T, ip string) string {
	t.Helper()
	// An address this machine does not have is a reason to skip, not a
	// finding about ports.
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot bind %s: %v", ip, err)
	}
	_ = ln.Close()
	return strconv.Itoa(testport.Port(t, ip))
}

// listenApp starts an App bound per function on distinct loopback addresses
// (127.0.0.2 and 127.0.0.3 are ordinary addresses; only 127.0.0.1 serves
// everything).
func listenApp(t *testing.T, listen serverconfig.Listen, configPath string, selfRestart bool) (*App, chan error, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	host := serverconfig.Host{Interfaces: func() ([]serverconfig.Interface, error) {
		return []serverconfig.Interface{{Name: "lo", Up: true, Addrs: []net.IP{
			net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.2"), net.ParseIP("127.0.0.3"), net.ParseIP("10.0.0.9"),
		}}}, nil
	}}
	a, err := New(Config{
		Listen: listen, ConfigPath: configPath, Host: host, SelfRestart: selfRestart,
		DataDir: dir, DBPath: filepath.Join(dir, "gpuworks.db"), AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	// Run opens the database and migrates it before it binds, which a loaded
	// runner stretches well past a few seconds; wait on the event (the
	// listeners up, or Run returning with the reason it could not bind)
	// instead of guessing a short deadline.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for {
		a.lnMu.RLock()
		up := a.lns != nil
		a.lnMu.RUnlock()
		if up {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("server stopped before it bound: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not bind")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return a, done, cancel
}

func status(t *testing.T, method, url, token string, body []byte) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func login(t *testing.T, base string) string {
	t.Helper()
	code, body := status(t, http.MethodPost, base+"/api/v1/auth/login", "", []byte(`{"username":"admin","password":"gpuworks-test-password-2026"}`))
	if code != http.StatusOK {
		t.Fatalf("login %d %s", code, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &out)
	return out.Token
}

func TestListenersServeOnlyTheirFunctions(t *testing.T) {
	consolePort, gatewayPort := freePortFor(t, "127.0.0.2"), freePortFor(t, "127.0.0.3")
	console := "http://127.0.0.2:" + consolePort
	gateway := "http://127.0.0.3:" + gatewayPort
	loopback := "http://127.0.0.1:" + consolePort
	listenApp(t, serverconfig.Listen{Console: []string{"127.0.0.2:" + consolePort}, Gateway: []string{"127.0.0.3:" + gatewayPort}}, "", false)

	for _, c := range []struct {
		url  string
		want int
	}{
		{console + "/healthz", 200},
		{console + "/api/v1/nodes", 401}, // console: the route exists
		{console + "/install.sh", 200},   // agent inherits console
		{console + "/v1/models", 404},    // gateway lives elsewhere
		{gateway + "/healthz", 200},      // every listener answers health
		{gateway + "/v1/models", 401},    // the gateway asks for an API key
		{gateway + "/api/v1/nodes", 404}, // no management API here
		{gateway + "/api/v1/agent/connect", 404},
		{gateway + "/", 404},           // nor the Web UI
		{loopback + "/v1/models", 401}, // 127.0.0.1 serves everything
		{loopback + "/api/v1/nodes", 401},
	} {
		if code, body := status(t, http.MethodGet, c.url, "", nil); code != c.want {
			t.Errorf("GET %s = %d, want %d (%s)", c.url, code, c.want, body)
		}
	}
	// The offline package is an agent download: public on the console like
	// /dl/ (200, or 404 in a build without embedded agents), absent from the
	// gateway.
	if code, body := status(t, http.MethodGet, console+"/dl/gpuworks-agent.run.sha256", "", nil); code != 200 && code != 404 {
		t.Errorf("GET console offline package checksum = %d (%s)", code, body)
	}
	if code, _ := status(t, http.MethodGet, gateway+"/dl/gpuworks-agent.run", "", nil); code != 404 {
		t.Errorf("GET gateway offline package = %d, want 404", code)
	}
}

func TestSavingListenAddressesWritesTheFileAndGuardsTheConsole(t *testing.T) {
	port := freePortFor(t, "127.0.0.2")
	path := filepath.Join(t.TempDir(), serverconfig.FileName)
	if err := os.WriteFile(path, []byte("# ops note\nlisten:\n  console: [127.0.0.2:"+port+"]\ndata_dir: /keep\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	listenApp(t, serverconfig.Listen{Console: []string{"127.0.0.2:" + port}}, path, false)
	base := "http://127.0.0.2:" + port
	token := login(t, base)

	code, body := status(t, http.MethodGet, base+"/api/v1/server/listen", token, nil)
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if code != 200 || got["editable"] != true || got["restart_pending"] != false || got["restart_mode"] != "command" {
		t.Fatalf("GET listen = %d %s", code, body)
	}

	// Moving the console away from the address this request uses would lock
	// the operator out after the restart.
	code, body = status(t, http.MethodPut, base+"/api/v1/server/listen", token, []byte(`{"console":["127.0.0.3:`+port+`"]}`))
	if code != 400 || !strings.Contains(string(body), "SERVER_LISTEN_LOCKOUT") {
		t.Fatalf("lockout = %d %s", code, body)
	}
	code, body = status(t, http.MethodPut, base+"/api/v1/server/listen", token, []byte(`{"console":["127.0.0.2:`+port+`"],"gateway":["192.0.2.1:443"]}`))
	if code != 400 || !strings.Contains(string(body), "SERVER_LISTEN_NOT_LOCAL") {
		t.Fatalf("foreign address = %d %s", code, body)
	}
	code, body = status(t, http.MethodPut, base+"/api/v1/server/listen", token, []byte(`{"console":["example.com:80"]}`))
	if code != 400 || !strings.Contains(string(body), "SERVER_LISTEN_INVALID") {
		t.Fatalf("hostname = %d %s", code, body)
	}

	code, body = status(t, http.MethodPut, base+"/api/v1/server/listen", token, []byte(`{"console":["127.0.0.2:`+port+`"],"gateway":["127.0.0.3:`+port+`"]}`))
	if code != 200 || !strings.Contains(string(body), `"restart_pending":true`) {
		t.Fatalf("save = %d %s", code, body)
	}
	saved, err := serverconfig.Load(path)
	if err != nil || saved.DataDir != "/keep" || len(saved.Listen.Gateway) != 1 {
		t.Fatalf("file = %+v, %v", saved, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# ops note") {
		t.Fatalf("comment lost:\n%s", raw)
	}
	// A nohup server has nobody to start it again.
	code, body = status(t, http.MethodPost, base+"/api/v1/server/restart", token, nil)
	if code != 409 || !strings.Contains(string(body), "SERVER_RESTART_MANUAL") || !strings.Contains(string(body), "gpuworks restart --dir") {
		t.Fatalf("manual restart = %d %s", code, body)
	}
}

func TestListenIsReadOnlyWithoutAConfigFile(t *testing.T) {
	port := freePortFor(t, "127.0.0.2")
	listenApp(t, serverconfig.Listen{Console: []string{"127.0.0.2:" + port}}, "", false)
	base := "http://127.0.0.2:" + port
	token := login(t, base)
	code, body := status(t, http.MethodPut, base+"/api/v1/server/listen", token, []byte(`{"console":["127.0.0.2:`+port+`"]}`))
	if code != 409 || !strings.Contains(string(body), "SERVER_LISTEN_READ_ONLY") {
		t.Fatalf("read-only = %d %s", code, body)
	}
}

func TestRestartEndsRunForTheServiceManager(t *testing.T) {
	port := freePortFor(t, "127.0.0.2")
	_, done, _ := listenApp(t, serverconfig.Listen{Console: []string{"127.0.0.2:" + port}}, "", true)
	base := "http://127.0.0.2:" + port
	token := login(t, base)
	if code, body := status(t, http.MethodGet, base+"/api/v1/server/listen", token, nil); !strings.Contains(string(body), `"restart_mode":"self"`) {
		t.Fatalf("GET listen = %d %s", code, body)
	}
	if code, body := status(t, http.MethodPost, base+"/api/v1/server/restart", token, nil); code != 202 {
		t.Fatalf("restart = %d %s", code, body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after a restart request")
	}
}

func TestListenAllOpensEveryAddressOrNone(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip(err)
	}
	defer busy.Close()
	free := freePortFor(t, "127.0.0.3")
	_, err = listenAll([]serverconfig.Binding{{Addr: "127.0.0.3:" + free, Functions: serverconfig.Functions}, {Addr: busy.Addr().String()}})
	if err == nil || !strings.Contains(err.Error(), busy.Addr().String()) {
		t.Fatalf("err = %v", err)
	}
	// The first socket was released again.
	again, err := net.Listen("tcp", "127.0.0.3:"+free)
	if err != nil {
		t.Fatalf("partial bind leaked: %v", err)
	}
	_ = again.Close()
}
