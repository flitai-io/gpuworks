package conn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestReplacementExecArgsPreservesManualRunOptions(t *testing.T) {
	creds := Credentials{
		ServerURL:  "https://server.example",
		NodeToken:  "node-token",
		ResumeArgs: []string{"--server", "https://server.example", "--token", "node-token", "--connect-mode", "server_dial", "--listen", ":19091", "--state", "/var/lib/gpuworks", "--driver", "udocker"},
	}
	got := replacementExecArgs("/opt/gpuworks/gpuworks-agent", creds, []string{"/opt/gpuworks/gpuworks-agent", "upgrade"})
	want := []string{"/opt/gpuworks/gpuworks-agent", "run", "--server", "https://server.example", "--token", "node-token", "--connect-mode", "server_dial", "--listen", ":19091", "--state", "/var/lib/gpuworks", "--driver", "udocker"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("replacement args = %#v, want %#v", got, want)
	}
	// An agent configured by its state directory's file restarts from it.
	creds.ResumeArgs = []string{"--state", "/var/lib/gpuworks"}
	got = replacementExecArgs("/opt/gpuworks/gpuworks-agent", creds, []string{"/opt/gpuworks/gpuworks-agent", "upgrade"})
	if want := "/opt/gpuworks/gpuworks-agent run --state /var/lib/gpuworks"; strings.Join(got, " ") != want {
		t.Fatalf("replacement args = %q, want %q", strings.Join(got, " "), want)
	}
	// Under run (the RPC upgrade) the running arguments stay as they are.
	running := []string{"/opt/gpuworks/gpuworks-agent", "run", "--state", "/s"}
	if got := replacementExecArgs("/opt/gpuworks/gpuworks-agent", creds, running); strings.Join(got, " ") != strings.Join(running, " ") {
		t.Fatalf("replacement args under run = %q", got)
	}
}

func TestUpgradeHTTPClientDoesNotCapSlowResponseBody(t *testing.T) {
	client := upgradeHTTPClient()
	if client.Timeout != 0 {
		t.Fatalf("upgrade HTTP client timeout = %s, want context-only body timeout", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("upgrade HTTP transport = %T, want *http.Transport", client.Transport)
	}
	if transport.TLSHandshakeTimeout != upgradeConnectTimeout {
		t.Fatalf("TLS handshake timeout = %s, want %s", transport.TLSHandshakeTimeout, upgradeConnectTimeout)
	}
	if transport.DialContext == nil {
		t.Fatal("upgrade HTTP transport has no dial timeout")
	}
	if transport.ResponseHeaderTimeout != upgradeResponseHeaderTimeout {
		t.Fatalf("response header timeout = %s, want %s", transport.ResponseHeaderTimeout, upgradeResponseHeaderTimeout)
	}
}

func TestDownloadAndSmokeValidatesArtifactBeforeInstall(t *testing.T) {
	const script = "#!/bin/sh\n[ \"$1\" = smoke ] && [ \"$5\" = node-token ]\n"
	var requested string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(script))
	}))
	defer server.Close()
	tmp := filepath.Join(t.TempDir(), "gpuworks-agent.new")
	if err := downloadAndSmoke(context.Background(), tmp, server.URL, "node-token"); err != nil {
		t.Fatalf("downloadAndSmoke: %v", err)
	}
	if requested == "" || !strings.HasPrefix(requested, "/dl/gpuworks-agent-linux-") {
		t.Fatalf("artifact request path = %q", requested)
	}
	info, err := os.Stat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("downloaded artifact is not executable: mode=%v", info.Mode())
	}
}

func TestDownloadAndSmokeRejectsBadArtifact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\nexit 7\n"))
	}))
	defer server.Close()
	tmp := filepath.Join(t.TempDir(), "gpuworks-agent.new")
	if err := downloadAndSmoke(context.Background(), tmp, server.URL, "node-token"); err == nil || !strings.Contains(err.Error(), "smoke test failed") {
		t.Fatalf("downloadAndSmoke error = %v, want smoke failure", err)
	}
}

func TestHandshakeReturnsErrorWhenServerRejectsAuthentication(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := New(Credentials{NodeToken: "node-token"}, nil, nil, nil, slog.Default())
	c.ctrl = client
	c.dec = json.NewDecoder(bufio.NewReader(client))
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		var env proto.Envelope
		if err := json.NewDecoder(server).Decode(&env); err != nil {
			return
		}
		body, _ := json.Marshal(proto.Envelope{T: "auth.failed", D: json.RawMessage(`{"error":"invalid node token"}`)})
		_, _ = server.Write(append(body, '\n'))
	}()
	err := c.handshake()
	if err == nil || !strings.Contains(err.Error(), "expected hello") {
		t.Fatalf("handshake error = %v, want rejected server hello", err)
	}
	<-serverDone
}

func TestHandshakeAcceptsRegistrationAndInvokesCallback(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	var registered string
	c := New(Credentials{NodeToken: "old-token"}, func(uuid, token string) error {
		registered = uuid + ":" + token
		return nil
	}, nil, nil, slog.Default())
	c.ctrl = client
	c.dec = json.NewDecoder(bufio.NewReader(client))
	done := make(chan struct{})
	go func() {
		defer close(done)
		var hello proto.Envelope
		if err := json.NewDecoder(server).Decode(&hello); err != nil {
			return
		}
		reg, _ := json.Marshal(proto.Envelope{T: proto.TRegistered, D: json.RawMessage(`{"node_uuid":"node-new","node_token":"new-token"}`)})
		welcome, _ := json.Marshal(proto.Envelope{T: proto.THello, D: json.RawMessage(`{"proto_min":1,"proto_max":1}`)})
		_, _ = server.Write(append(reg, '\n'))
		_, _ = server.Write(append(welcome, '\n'))
	}()
	if err := c.handshake(); err != nil {
		t.Fatal(err)
	}
	if registered != "node-new:new-token" || c.creds.NodeUUID != "node-new" || c.creds.NodeToken != "new-token" {
		t.Fatalf("registration=%q creds=%+v", registered, c.creds)
	}
	<-done
}

func TestToWSURLNormalizesHTTPAndTrailingSlashes(t *testing.T) {
	for input, want := range map[string]string{
		"https://example.test/":  "wss://example.test",
		"http://example.test///": "ws://example.test",
		"ws://example.test":      "ws://example.test",
		"example.test":           "example.test",
	} {
		if got := toWSURL(input); got != want {
			t.Errorf("toWSURL(%q)=%q want %q", input, got, want)
		}
	}
}

func TestConnectionErrorsAndUpgradeValidation(t *testing.T) {
	if got := (&RejectError{Reason: "auth"}).Error(); got != "server rejected connection: auth" {
		t.Fatalf("reject error without detail = %q", got)
	}
	if got := (&RejectError{Reason: "auth", Detail: "expired"}).Error(); got != "server rejected connection: auth: expired" {
		t.Fatalf("reject error with detail = %q", got)
	}
	c := New(Credentials{}, nil, nil, nil, slog.Default())
	if err := c.selfUpgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "server URL and node token") {
		t.Fatalf("selfUpgrade validation = %v", err)
	}
	if err := Upgrade(context.Background(), "", "", slog.Default()); err == nil || !strings.Contains(err.Error(), "server URL and node token") {
		t.Fatalf("Upgrade validation = %v", err)
	}
	if err := UpgradeWithResumeArgs(context.Background(), "", "", []string{"--driver", "none"}, slog.Default()); err == nil || !strings.Contains(err.Error(), "server URL and node token") {
		t.Fatalf("UpgradeWithResumeArgs validation = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(Credentials{ServerURL: "http://127.0.0.1:1"}, nil, nil, nil, slog.Default()).Run(ctx); err == nil {
		t.Fatal("Run with canceled context unexpectedly succeeded")
	}
}

func TestRunAcceptedPerformsServerDialAuthAndHandshake(t *testing.T) {
	clientRaw, serverRaw := net.Pipe()
	defer clientRaw.Close()
	defer serverRaw.Close()
	// status.node is the protocol-level acknowledgement that the client consumed
	// the welcome and entered serve. Waiting for it avoids relying on yamux's
	// asynchronous Stream.Write completion before closing either session.
	c := New(Credentials{NodeUUID: "node-1", NodeToken: "secret"}, nil, nil, capabilityProber{}, slog.Default())
	serverReady := make(chan error, 1)
	serverDone := make(chan struct{})
	releaseServer := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseServer) }) }
	defer release()
	go func() {
		defer close(serverDone)
		var challenge proto.ServerDialChallenge
		line, err := readLine(serverRaw)
		if err != nil {
			serverReady <- err
			return
		}
		if err := json.Unmarshal(line, &challenge); err != nil {
			serverReady <- err
			return
		}
		proof, _ := json.Marshal(proto.ServerDialProof{HMAC: proto.ServerDialHMAC(proto.ServerDialNodeSecret("secret"), challenge.Nonce)})
		if _, err := serverRaw.Write(append(proof, '\n')); err != nil {
			serverReady <- err
			return
		}
		ys, err := yamux.Server(serverRaw, yamux.DefaultConfig(), nil)
		if err != nil {
			serverReady <- err
			return
		}
		defer ys.Close()
		stream, err := ys.AcceptStream()
		if err != nil {
			serverReady <- err
			return
		}
		defer stream.Close()
		reader := bufio.NewReader(stream)
		decoder := json.NewDecoder(reader)
		var header proto.StreamHeader
		if err := decoder.Decode(&header); err != nil {
			serverReady <- err
			return
		}
		if header.Kind != proto.StreamCtrl {
			serverReady <- errors.New("unexpected control stream header")
			return
		}
		var hello proto.Envelope
		if err := decoder.Decode(&hello); err != nil {
			serverReady <- err
			return
		}
		if hello.T != proto.THello {
			serverReady <- errors.New("unexpected agent hello")
			return
		}
		welcome, _ := json.Marshal(proto.Envelope{T: proto.THello, D: json.RawMessage(`{}`)})
		if _, err := stream.Write(append(welcome, '\n')); err != nil {
			serverReady <- err
			return
		}
		var status proto.Envelope
		if err := decoder.Decode(&status); err != nil {
			serverReady <- err
			return
		}
		if status.T != proto.TStatusNode {
			serverReady <- errors.New("expected initial status.node")
			return
		}
		serverReady <- nil
		<-releaseServer
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- c.RunAccepted(ctx, clientRaw) }()
	if err := <-serverReady; err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("RunAccepted=%v", err)
		}
	case <-time.After(5 * time.Second):
		_ = clientRaw.Close()
		t.Fatal("RunAccepted did not stop after context cancellation")
	}
	release()
	<-serverDone
}

type inboundHandlers struct {
	mu         sync.Mutex
	states     int
	updates    int
	rpcDone    chan struct{}
	smokeErr   error
	smokeCalls int
}

// CUDARuntimeSmoke is a canary: conn no longer asks a handler to run a
// node-level runtime check, and a report that starts calling one again is
// caught by the count below rather than by a node in the field.
func (h *inboundHandlers) CUDARuntimeSmoke(context.Context) error {
	h.mu.Lock()
	h.smokeCalls++
	err := h.smokeErr
	h.mu.Unlock()
	return err
}

func (h *inboundHandlers) Bind(SendFunc) {}
func (h *inboundHandlers) Unbind()       {}
func (h *inboundHandlers) OnStateSync(context.Context, proto.DesiredState) {
	h.mu.Lock()
	h.states++
	h.mu.Unlock()
}
func (h *inboundHandlers) OnStateUpdate(_ context.Context, update proto.StateUpdate) bool {
	h.mu.Lock()
	h.updates++
	changed := update.ImageRefs != nil
	h.mu.Unlock()
	return changed
}
func (h *inboundHandlers) OnRPC(context.Context, string, json.RawMessage) (any, error) {
	h.mu.Lock()
	if h.rpcDone != nil {
		close(h.rpcDone)
		h.rpcDone = nil
	}
	h.mu.Unlock()
	return map[string]string{"ok": "yes"}, nil
}
func (h *inboundHandlers) RuntimeDriverName() string                                    { return "test" }
func (h *inboundHandlers) StreamLogs(context.Context, string, bool, io.Writer) error    { return nil }
func (h *inboundHandlers) StreamModelArchive(context.Context, string, io.Writer) error  { return nil }
func (h *inboundHandlers) ReceiveModelArchive(context.Context, string, io.Reader) error { return nil }
func (h *inboundHandlers) StreamImage(context.Context, string, string, io.ReadWriter, bool) error {
	return nil
}
func (h *inboundHandlers) StreamLargeFile(context.Context, string, io.Writer) error { return nil }
func (h *inboundHandlers) ReceiveStorageUpload(context.Context, proto.StreamHeader, io.ReadWriter) error {
	return nil
}
func (h *inboundHandlers) SendStorageDownload(context.Context, proto.StreamHeader, io.Writer) error {
	return nil
}

type nopRWC struct {
	mu sync.Mutex
	bytes.Buffer
}

func (n *nopRWC) Close() error { return nil }
func (n *nopRWC) Write(p []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Buffer.Write(p)
}
func (n *nopRWC) BytesCopy() []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]byte(nil), n.Buffer.Bytes()...)
}

func TestHandleInboundDispatchesStateAndRPCResponses(t *testing.T) {
	done := make(chan struct{})
	h := &inboundHandlers{rpcDone: done}
	ctrl := &nopRWC{}
	c := &Conn{handlers: h, ctrl: ctrl, log: slog.Default()}
	c.handleInbound(proto.Envelope{T: proto.TStateSync, D: json.RawMessage(`{"version":1}`)})
	c.handleInbound(proto.Envelope{T: proto.TStateUpdate, D: json.RawMessage(`{"version":1}`)})
	c.handleInbound(proto.Envelope{T: proto.TRPCReq, ID: 7, D: json.RawMessage(`{"method":"test","params":{"x":1}}`)})
	select {
	case <-time.After(time.Second):
		t.Fatal("RPC handler did not run")
	case <-done:
	}
	h.mu.Lock()
	states, updates := h.states, h.updates
	h.mu.Unlock()
	if states != 1 || updates != 1 {
		t.Fatalf("dispatch state=%d update=%d", states, updates)
	}
	deadline := time.Now().Add(time.Second)
	for !bytes.Contains(ctrl.BytesCopy(), []byte(`"t":"rpc.resp"`)) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !bytes.Contains(ctrl.BytesCopy(), []byte(`"t":"rpc.resp"`)) {
		t.Fatalf("rpc response=%q", ctrl.BytesCopy())
	}
}

type failingRPCHandlers struct{ inboundHandlers }

func (h *failingRPCHandlers) OnRPC(context.Context, string, json.RawMessage) (any, error) {
	return nil, errors.New("injected RPC failure")
}

func TestHandleRPCErrorsAndUpgradeContention(t *testing.T) {
	ctrl := &nopRWC{}
	c := &Conn{handlers: &failingRPCHandlers{}, ctrl: ctrl, log: slog.Default()}
	c.handleRPC(context.Background(), proto.Envelope{T: proto.TRPCReq, ID: 1, D: json.RawMessage(`{`)})
	c.handleRPC(context.Background(), proto.Envelope{T: proto.TRPCReq, ID: 2, D: json.RawMessage(`{"method":"fixture"}`)})
	upgradeMu.Lock()
	c.handleRPC(context.Background(), proto.Envelope{T: proto.TRPCReq, ID: 3, D: json.RawMessage(`{"method":"agent.upgrade"}`)})
	upgradeMu.Unlock()
	got := string(ctrl.BytesCopy())
	for _, want := range []string{"bad request", "injected RPC failure", `{"error":"agent upgrade already in progress","code":"AGENT_UPGRADE_IN_PROGRESS"}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("RPC responses %q do not contain %q", got, want)
		}
	}
	if strings.Contains(got, `"error":"injected RPC failure","code"`) {
		t.Fatalf("an uncoded failure carries a code: %q", got)
	}
}

type unknownRPCHandlers struct{ inboundHandlers }

func (h *unknownRPCHandlers) OnRPC(_ context.Context, method string, _ json.RawMessage) (any, error) {
	return nil, fmt.Errorf("dispatch: %w", proto.UnknownRPCMethod(method))
}

// The refusal keeps the sentence older servers read and adds the code newer
// ones read, even when the handler wrapped it.
func TestHandleRPCSendsTheCodeOfAWrappedRPCError(t *testing.T) {
	ctrl := &nopRWC{}
	c := &Conn{handlers: &unknownRPCHandlers{}, ctrl: ctrl, log: slog.Default()}
	c.handleRPC(context.Background(), proto.Envelope{T: proto.TRPCReq, ID: 7, D: json.RawMessage(`{"method":"fs.stat"}`)})
	want := `"d":{"error":"dispatch: unknown rpc method: fs.stat","code":"RPC_UNKNOWN_METHOD"}`
	if got := string(ctrl.BytesCopy()); !strings.Contains(got, want) {
		t.Fatalf("rpc.resp = %q, want %q", got, want)
	}
}

func TestServeReturnsOnContextCancelAndReportStatusSendsNodeSnapshot(t *testing.T) {
	ctrl := &nopRWC{}
	p := &gpu.FixedProber{VendorName: "nvidia", GPUs: []proto.GPUInfo{{Index: 0, Vendor: "nvidia", Name: "A100", Healthy: true}}}
	h := &inboundHandlers{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, h, p, slog.Default())
	c.ctrl = ctrl
	c.dec = json.NewDecoder(strings.NewReader(""))
	c.reportStatus(context.Background())
	if !bytes.Contains(ctrl.BytesCopy(), []byte(`"t":"status.node"`)) {
		t.Fatalf("status snapshot=%q", ctrl.BytesCopy())
	}
	// A status report runs no runtime check of its own. Whether a card can
	// be used is asked of each card on its own schedule (gpu/cardsmoke)
	// and folded into that card's health, which says which card rather
	// than failing the whole report.
	h.mu.Lock()
	if h.smokeCalls != 0 {
		t.Fatalf("cuda smoke calls=%d, want the report to run none", h.smokeCalls)
	}
	h.mu.Unlock()
	c.reportStatus(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.serve(ctx); err != nil {
		t.Fatalf("serve canceled=%v", err)
	}
}

type blockingStatusProber struct {
	*gpu.FixedProber
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (p *blockingStatusProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		close(p.started)
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return p.FixedProber.Probe(ctx)
}

func (p *blockingStatusProber) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestRequestStatusIsSingleFlightWithOneTrailingReport(t *testing.T) {
	prober := &blockingStatusProber{
		FixedProber: &gpu.FixedProber{VendorName: "test"},
		started:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	c := New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, prober, slog.Default())
	c.ctrl = &nopRWC{}
	c.requestStatus(context.Background())
	select {
	case <-prober.started:
	case <-time.After(time.Second):
		t.Fatal("first status report did not start")
	}
	c.requestStatus(context.Background())
	c.requestStatus(context.Background())
	if got := prober.callCount(); got != 1 {
		t.Fatalf("parallel status probes = %d, want 1", got)
	}
	close(prober.release)
	deadline := time.Now().Add(time.Second)
	for prober.callCount() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := prober.callCount(); got != 2 {
		t.Fatalf("status probes after trailing refresh = %d, want 2", got)
	}
}

// A runtime check never fails a whole status report. It used to: a failed
// node-level smoke set gpu_probe_ok to false, and the server then refused
// the entire card list — one card's runtime trouble hid every card of the
// node. The per-card check reports that card's health instead.
func TestAStatusReportIsNotFailedByARuntimeCheck(t *testing.T) {
	ctrl := &nopRWC{}
	p := &gpu.FixedProber{VendorName: "nvidia", GPUs: []proto.GPUInfo{{Index: 0, Vendor: "nvidia", Name: "A100", Healthy: true}}}
	h := &inboundHandlers{smokeErr: errors.New("cuInit=999")}
	c := New(Credentials{NodeUUID: "node-1"}, nil, h, p, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	got := string(ctrl.BytesCopy())
	if !strings.Contains(got, `"gpu_probe_ok":true`) {
		t.Fatalf("status = %s, want the probe reported as succeeded", got)
	}
	if strings.Contains(got, "cuInit=999") {
		t.Fatalf("a runtime check leaked into the probe error: %s", got)
	}
}

func TestServerDialAgentAuthRejectsWrongHMAC(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := New(Credentials{NodeToken: "right-secret", NodeUUID: "node-1"}, nil, nil, nil, slog.Default())
	go func() {
		var challenge proto.ServerDialChallenge
		if err := json.NewDecoder(server).Decode(&challenge); err != nil {
			return
		}
		proof, _ := json.Marshal(proto.ServerDialProof{HMAC: proto.ServerDialHMAC("wrong-secret", challenge.Nonce)})
		_, _ = server.Write(append(proof, '\n'))
	}()
	if err := c.serverDialAgentAuth(client); err == nil || !strings.Contains(err.Error(), "invalid server_dial proof") {
		t.Fatalf("wrong HMAC error = %v", err)
	}
}

func TestProtocolReadersAndNilHandlerEdges(t *testing.T) {
	if err := (&Conn{}).serverDialAgentAuth(&nopNetConn{}); err == nil || !strings.Contains(err.Error(), "requires node token") {
		t.Fatalf("missing server_dial token error = %v", err)
	}
	if line, err := readLine(strings.NewReader("partial")); err != nil || string(line) != "partial" {
		t.Fatalf("partial line = %q err=%v", line, err)
	}
	if _, err := readLine(strings.NewReader(strings.Repeat("x", 64*1024+1))); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("oversized line error = %v", err)
	}
	c := &Conn{log: slog.Default()}
	c.handleInbound(proto.Envelope{T: proto.TStateSync, D: json.RawMessage(`{`)}) // nil handlers are ignored
	c.handlers = &inboundHandlers{}
	c.handleInbound(proto.Envelope{T: proto.TStateSync, D: json.RawMessage(`{`)})
	c.handleInbound(proto.Envelope{T: proto.TStateUpdate, D: json.RawMessage(`{`)})
	c.handleInbound(proto.Envelope{T: "unknown"})
}

// nopNetConn is only reached by validations that fail before I/O.
type nopNetConn struct{ nopRWC }

func (*nopNetConn) LocalAddr() net.Addr              { return nil }
func (*nopNetConn) RemoteAddr() net.Addr             { return nil }
func (*nopNetConn) SetDeadline(time.Time) error      { return nil }
func (*nopNetConn) SetReadDeadline(time.Time) error  { return nil }
func (*nopNetConn) SetWriteDeadline(time.Time) error { return nil }

type streamTestHandlers struct {
	mu             sync.Mutex
	logs           []string
	modelReads     []string
	modelWrites    []string
	imageReads     []string
	imageWrites    []string
	largeFileReads []string

	storageUploads   []string
	storageDownloads []string
}

func (h *streamTestHandlers) Bind(SendFunc)                                         {}
func (h *streamTestHandlers) Unbind()                                               {}
func (h *streamTestHandlers) OnStateSync(context.Context, proto.DesiredState)       {}
func (h *streamTestHandlers) OnStateUpdate(context.Context, proto.StateUpdate) bool { return false }
func (h *streamTestHandlers) OnRPC(context.Context, string, json.RawMessage) (any, error) {
	return nil, nil
}
func (h *streamTestHandlers) RuntimeDriverName() string { return "test" }
func (h *streamTestHandlers) StreamLogs(_ context.Context, ref string, _ bool, w io.Writer) error {
	h.mu.Lock()
	h.logs = append(h.logs, ref)
	h.mu.Unlock()
	_, err := io.WriteString(w, "log:"+ref)
	return err
}
func (h *streamTestHandlers) StreamModelArchive(_ context.Context, path string, w io.Writer) error {
	h.mu.Lock()
	h.modelReads = append(h.modelReads, path)
	h.mu.Unlock()
	_, err := io.WriteString(w, "model:"+path)
	return err
}
func (h *streamTestHandlers) ReceiveModelArchive(_ context.Context, path string, r io.Reader) error {
	b, err := io.ReadAll(r)
	h.mu.Lock()
	h.modelWrites = append(h.modelWrites, path+":"+string(b))
	h.mu.Unlock()
	return err
}
func (h *streamTestHandlers) StreamLargeFile(_ context.Context, path string, w io.Writer) error {
	h.mu.Lock()
	h.largeFileReads = append(h.largeFileReads, path)
	h.mu.Unlock()
	_, err := io.WriteString(w, "file:"+path)
	return err
}
func (h *streamTestHandlers) ReceiveStorageUpload(_ context.Context, hdr proto.StreamHeader, rw io.ReadWriter) error {
	b, err := io.ReadAll(rw)
	h.mu.Lock()
	h.storageUploads = append(h.storageUploads, hdr.Rel+":"+string(b))
	h.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = io.WriteString(rw, "uploaded:"+hdr.Rel)
	return err
}
func (h *streamTestHandlers) SendStorageDownload(_ context.Context, hdr proto.StreamHeader, w io.Writer) error {
	h.mu.Lock()
	h.storageDownloads = append(h.storageDownloads, hdr.Rel)
	h.mu.Unlock()
	_, err := io.WriteString(w, "download:"+hdr.Rel)
	return err
}
func (h *streamTestHandlers) StreamImage(_ context.Context, imageRef, transferID string, rw io.ReadWriter, load bool) error {
	if load {
		b, err := io.ReadAll(rw)
		h.mu.Lock()
		h.imageWrites = append(h.imageWrites, imageRef+":"+transferID+":"+string(b))
		h.mu.Unlock()
		return err
	}
	h.mu.Lock()
	h.imageReads = append(h.imageReads, imageRef+":"+transferID)
	h.mu.Unlock()
	_, err := io.WriteString(rw, "image:"+imageRef+":"+transferID)
	return err
}

func TestStreamHeadersDispatchTunnelLogtailModelAndImageStreams(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	client, err := yamux.Client(clientConn, yamux.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := yamux.Server(serverConn, yamux.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer server.Close()
	handlers := &streamTestHandlers{}
	c := &Conn{handlers: handlers, log: slog.Default()}

	open := func(t *testing.T, hdr proto.StreamHeader, payload string, wantResponse string) {
		t.Helper()
		stream, err := client.OpenStream(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			accepted, acceptErr := server.AcceptStream()
			if acceptErr == nil {
				c.handleStream(context.Background(), accepted)
			}
			close(done)
		}()
		header, _ := json.Marshal(hdr)
		if _, err := stream.Write(append(header, '\n')); err != nil {
			t.Fatal(err)
		}
		if payload != "" {
			if _, err := io.WriteString(stream, payload); err != nil {
				t.Fatal(err)
			}
		}
		if hdr.Kind == proto.StreamModelWrite || hdr.Kind == proto.StreamImageWrite {
			_ = stream.Close()
		}
		if hdr.Kind == proto.StreamStorageUpload {
			_ = stream.CloseWrite() // the server half-closes and then reads the answer
		}
		if wantResponse != "" {
			_ = stream.SetReadDeadline(time.Now().Add(2 * time.Second))
			got, err := io.ReadAll(stream)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != wantResponse {
				t.Fatalf("stream %s response = %q, want %q", hdr.Kind, got, wantResponse)
			}
		}
		_ = stream.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("stream %s handler did not finish", hdr.Kind)
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_, _ = io.Copy(conn, strings.NewReader("pong"))
			_ = conn.Close()
		}
	}()
	open(t, proto.StreamHeader{Kind: proto.StreamTunnel, Dst: listener.Addr().String()}, "ping", "pong")
	open(t, proto.StreamHeader{Kind: proto.StreamLogtail, Ref: "inst-1"}, "", "log:inst-1")
	open(t, proto.StreamHeader{Kind: proto.StreamModelRead, Dst: "/models/a"}, "", "model:/models/a")
	open(t, proto.StreamHeader{Kind: proto.StreamModelWrite, Dst: "/models/b"}, "archive", "")
	open(t, proto.StreamHeader{Kind: proto.StreamImageRead, Dst: "image:1", Ref: "transfer-a"}, "", "image:image:1:transfer-a")
	open(t, proto.StreamHeader{Kind: proto.StreamImageWrite, Dst: "image:2", Ref: "transfer-b"}, "load", "")
	open(t, proto.StreamHeader{Kind: proto.StreamLargeFileRead, Dst: "/models/a/tokenizer.json"}, "", "file:/models/a/tokenizer.json")
	open(t, proto.StreamHeader{Kind: proto.StreamStorageUpload, StorageID: 3, Rel: "files/a.txt"}, "body", "uploaded:files/a.txt")
	open(t, proto.StreamHeader{Kind: proto.StreamStorageDownload, StorageID: 3, Rel: "files/b.txt"}, "", "download:files/b.txt")
	terminalFailure, err := proto.EncodeTermFrame(proto.TermExit, proto.EncodeExit(-1, "this agent does not support container terminals"))
	if err != nil {
		t.Fatal(err)
	}
	open(t, proto.StreamHeader{Kind: proto.StreamTerminal, Target: proto.TermTargetContainer}, "", string(terminalFailure))

	handlers.mu.Lock()
	defer handlers.mu.Unlock()
	if len(handlers.storageUploads) != 1 || handlers.storageUploads[0] != "files/a.txt:body" || len(handlers.storageDownloads) != 1 || handlers.storageDownloads[0] != "files/b.txt" {
		t.Fatalf("storage streams uploads=%v downloads=%v", handlers.storageUploads, handlers.storageDownloads)
	}
	if len(handlers.logs) != 1 || len(handlers.modelReads) != 1 || len(handlers.modelWrites) != 1 || len(handlers.imageReads) != 1 || len(handlers.imageWrites) != 1 || len(handlers.largeFileReads) != 1 {
		t.Fatalf("dispatch records logs=%v modelRead=%v modelWrite=%v imageRead=%v imageWrite=%v largeFileRead=%v", handlers.logs, handlers.modelReads, handlers.modelWrites, handlers.imageReads, handlers.imageWrites, handlers.largeFileReads)
	}
}

type inventoryHandlers struct{ inboundHandlers }

func (h *inventoryHandlers) NetworkInventory() proto.NetInventory {
	return proto.NetInventory{
		Interfaces: []proto.NetInterface{{Name: "sim0", Addrs: []string{"10.77.0.2/24"}}},
		RDMAPorts:  []proto.RDMAPort{},
	}
}

func TestReportStatusCarriesNetworkInventory(t *testing.T) {
	p := &gpu.FixedProber{VendorName: "test"}

	// Hermetic agents supply a simulated inventory.
	ctrl := &nopRWC{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, &inventoryHandlers{}, p, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	if got := string(ctrl.BytesCopy()); !strings.Contains(got, `"network":{"interfaces":[{"name":"sim0","addrs":["10.77.0.2/24"]}],"rdma_ports":[]}`) {
		t.Fatalf("status without simulated inventory: %s", got)
	}

	// Otherwise the host inventory is reported, always as a non-null object so
	// the server can tell it from an older agent.
	ctrl = &nopRWC{}
	c = New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, p, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	var env struct {
		D proto.NodeStatus `json:"d"`
	}
	line, _, _ := strings.Cut(string(ctrl.BytesCopy()), "\n")
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatal(err)
	}
	if env.D.Network == nil || env.D.Network.Interfaces == nil || env.D.Network.RDMAPorts == nil {
		t.Fatalf("host inventory missing: %+v", env.D.Network)
	}
}

// portProber is a prober whose cards own network ports.
type portProber struct {
	gpu.FixedProber
	ports    []proto.AcceleratorPort
	probeErr error
	asked    int
}

func (p *portProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	if p.probeErr != nil {
		return nil, p.probeErr
	}
	return p.FixedProber.Probe(ctx)
}

func (p *portProber) AcceleratorPorts(context.Context) []proto.AcceleratorPort {
	p.asked++
	return p.ports
}

func TestReportStatusCarriesAcceleratorPorts(t *testing.T) {
	ports := []proto.AcceleratorPort{{Name: "npu3", Index: 3, IP: "203.0.113.59", Netmask: "255.255.0.0", State: "UP", SpeedGbps: 200}}
	p := &portProber{FixedProber: gpu.FixedProber{VendorName: "ascend"}, ports: ports}
	ctrl := &nopRWC{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, p, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	if got := string(ctrl.BytesCopy()); !strings.Contains(got, `"accelerator_ports":[{"name":"npu3","index":3,"ip":"203.0.113.59","netmask":"255.255.0.0","state":"UP","speed_gbps":200}]`) {
		t.Fatalf("status without accelerator ports: %s", got)
	}

	// A prober without such ports leaves the key out, as an older agent does.
	ctrl = &nopRWC{}
	c = New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, &gpu.FixedProber{VendorName: "test"}, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	if got := string(ctrl.BytesCopy()); strings.Contains(got, "accelerator_ports") {
		t.Fatalf("status of a prober without ports names them: %s", got)
	}

	// A failed probe does not ask for ports: the card list is unknown.
	failed := &portProber{FixedProber: gpu.FixedProber{VendorName: "ascend"}, ports: ports, probeErr: errors.New("driver busy")}
	ctrl = &nopRWC{}
	c = New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, failed, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	if got := string(ctrl.BytesCopy()); failed.asked != 0 || strings.Contains(got, "accelerator_ports") {
		t.Fatalf("failed probe asked for ports %d time(s): %s", failed.asked, got)
	}

	// A simulated inventory is reported as given.
	ctrl = &nopRWC{}
	c = New(Credentials{NodeUUID: "node-1"}, nil, &inventoryHandlers{}, p, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	if got := string(ctrl.BytesCopy()); strings.Contains(got, "accelerator_ports") {
		t.Fatalf("simulated inventory gained accelerator ports: %s", got)
	}
}
