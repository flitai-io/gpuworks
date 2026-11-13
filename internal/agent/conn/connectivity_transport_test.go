package conn

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

func connectivityTransport(t *testing.T, edit func(*http.Transport)) {
	t.Helper()
	previous := http.DefaultTransport
	clone := previous.(*http.Transport).Clone()
	edit(clone)
	http.DefaultTransport = clone
	t.Cleanup(func() { http.DefaultTransport = previous; clone.CloseIdleConnections() })
}
func TestConnectivityActualProxySelectionHidesCredentials(t *testing.T) {
	peer := &laneTestServer{}
	var authenticated atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated.Store(r.Header.Get("Proxy-Authorization") == "Basic "+base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-password")))
		peer.handle(w, r)
	}))
	defer proxy.Close()
	address, _ := url.Parse(proxy.URL)
	address.User = url.UserPassword("proxy-user", "proxy-password")
	var selections atomic.Int32
	connectivityTransport(t, func(transport *http.Transport) {
		transport.Proxy = func(*http.Request) (*url.URL, error) { selections.Add(1); return address, nil }
	})
	var log bytes.Buffer
	report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: "http://engine.invalid:8800/prefix", NodeToken: "private-node"}, slog.New(slog.NewTextHandler(&log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if !authenticated.Load() || selections.Load() != 1 || proto.ConnectionValue(report, "connection.proxy") != "environment_proxy" || proto.ConnectionValue(report, "connection.proxy_url") != proxy.URL {
		t.Fatalf("actual proxy lost: %+v auth=%v selections=%d", report.Facts, authenticated.Load(), selections.Load())
	}
	if proto.ConnectionValue(report, "connection.phase.dns") == "ok" {
		t.Fatal("claimed proxy-side engine DNS as a local probe")
	}
	data, _ := json.Marshal(report)
	if strings.Contains(string(data)+log.String(), "proxy-password") || strings.Contains(string(data)+log.String(), "private-node") {
		t.Fatal("proxy/token leaked")
	}
}
func TestConnectivityTLSAndProxyConnectUseActualTransport(t *testing.T) {
	peer := &laneTestServer{}
	engine := httptest.NewTLSServer(http.HandlerFunc(peer.handle))
	defer engine.Close()
	target, _ := url.Parse(engine.URL)
	var tunnels, previousHooks atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != target.Host {
			w.WriteHeader(403)
			return
		}
		upstream, err := net.Dial("tcp", target.Host)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		downstream, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		tunnels.Add(1)
		defer downstream.Close()
		defer upstream.Close()
		_, _ = io.WriteString(downstream, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, downstream); upstream.Close(); close(done) }()
		_, _ = io.Copy(downstream, upstream)
		downstream.Close()
		<-done
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	connectivityTransport(t, func(transport *http.Transport) {
		transport.Proxy = http.ProxyURL(proxyURL)
		roots := x509.NewCertPool()
		roots.AddCert(engine.Certificate())
		transport.TLSClientConfig = engine.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		transport.TLSClientConfig.InsecureSkipVerify = false
		transport.TLSClientConfig.RootCAs = roots
		transport.OnProxyConnectResponse = func(context.Context, *url.URL, *http.Request, *http.Response) error { previousHooks.Add(1); return nil }
	})
	report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: engine.URL, NodeToken: "private-node"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if tunnels.Load() != 1 || previousHooks.Load() != 1 || proto.ConnectionValue(report, "connection.phase.proxy_connect") != "ok" || proto.ConnectionValue(report, "connection.phase.tls") != "ok" {
		t.Fatalf("actual CONNECT/TLS evidence missing: %+v", report.Facts)
	}
	if proto.ConnectionValue(report, "connection.elapsed_ms.proxy_connect") != "" {
		t.Fatal("invented CONNECT start time")
	}
}
func TestConnectivityTLSFailureAndCrossOriginRedirect(t *testing.T) {
	t.Run("certificate", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc((&laneTestServer{}).handle))
		defer server.Close()
		report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: server.URL, NodeToken: "private-node"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil || proto.ConnectionValue(report, "connection.reason") != "tls" || proto.ConnectionValue(report, "connection.phase.tls") != "failed" {
			t.Fatalf("TLS failure lost: %+v %v", report.Facts, err)
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var reached atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
		defer other.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL+"/secret", 302) }))
		defer server.Close()
		_, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: server.URL, NodeToken: "private-node"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil || reached.Load() != 0 {
			t.Fatalf("redirect escaped origin: %v %d", err, reached.Load())
		}
	})
}

func TestConnectivityDNSFailureIsNodeResolverEvidence(t *testing.T) {
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 2048)
		for {
			n, peer, err := socket.ReadFrom(buffer)
			if err != nil {
				return
			}
			if n < 12 {
				continue
			}
			end := 12
			for end < n && buffer[end] != 0 {
				end += int(buffer[end]) + 1
			}
			end += 5
			if end > n {
				continue
			}
			response := append([]byte(nil), buffer[:end]...)
			binary.BigEndian.PutUint16(response[2:4], 0x8183)
			binary.BigEndian.PutUint16(response[6:8], 0)
			binary.BigEndian.PutUint16(response[8:10], 0)
			binary.BigEndian.PutUint16(response[10:12], 0)
			_, _ = socket.WriteTo(response, peer)
		}
	}()
	original := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", socket.LocalAddr().String())
	}}
	defer func() { net.DefaultResolver = original; socket.Close(); <-done }()
	connectivityTransport(t, func(transport *http.Transport) { transport.Proxy = nil })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	report, err := DiagnoseConnectivity(ctx, Credentials{ServerURL: "http://missing.fixture.invalid:8800", NodeToken: "private-node"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || proto.ConnectionValue(report, "connection.reason") != "dns" || proto.ConnectionValue(report, "connection.phase.dns") != "failed" {
		t.Fatalf("DNS evidence missing: %+v %v", report.Facts, err)
	}
}

func TestConnectivityHostileRejectIsSafeAcrossSinks(t *testing.T) {
	const secrets = "private-token proxy-password query-secret auth-session-private"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		raw := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
		ys, err := yamux.Server(raw, laneYamuxConfig(proto.LaneData, io.Discard), nil)
		if err != nil {
			return
		}
		defer ys.Close()
		stream, err := ys.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close()
		_, _ = readLine(stream)
		var hello proto.Envelope
		_ = json.NewDecoder(stream).Decode(&hello)
		body, _ := json.Marshal(proto.Reject{Reason: "bad_token", Detail: secrets})
		_ = json.NewEncoder(stream).Encode(proto.Envelope{T: proto.TReject, D: body})
		_, _ = io.Copy(io.Discard, stream)
	}))
	defer server.Close()
	var log bytes.Buffer
	report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: server.URL, NodeToken: "private-token"}, slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	var rejected *RejectError
	if !errors.As(err, &rejected) || proto.ConnectionValue(report, "connection.reason") != "bad_token" {
		t.Fatalf("typed reject lost: %+v %v", report.Facts, err)
	}
	data, _ := json.Marshal(report)
	for _, secret := range strings.Fields(secrets) {
		if strings.Contains(string(data)+report.Report+log.String()+err.Error(), secret) {
			t.Fatalf("sink leaked %q", secret)
		}
	}
}

func TestConnectivityLegacySmokePreservesRedirect(t *testing.T) {
	peer := &laneTestServer{}
	other := httptest.NewServer(http.HandlerFunc(peer.handle))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/v1/agent/connect", http.StatusFound)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	legacy := New(Credentials{ServerURL: server.URL, NodeToken: "private-node", Smoke: true}, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := legacy.Run(ctx); err != nil {
		t.Fatalf("legacy redirect changed: %v", err)
	}
	lanes, _ := peer.snapshot()
	if len(lanes) != 1 {
		t.Fatalf("legacy did not reach redirected peer: %v", lanes)
	}
	if _, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: server.URL, NodeToken: "private-node"}, nil); err == nil {
		t.Fatal("new diagnostic followed a different origin")
	}
	lanes, _ = peer.snapshot()
	if len(lanes) != 1 {
		t.Fatal("new diagnostic sent credentials to redirect target")
	}
}

func TestConnectivityDiagnosticCapsActualTransportDeadline(t *testing.T) {
	observed := make(chan time.Time, 1)
	connectivityTransport(t, func(transport *http.Transport) {
		transport.Proxy = func(r *http.Request) (*url.URL, error) {
			at, ok := r.Context().Deadline()
			if ok {
				observed <- at
			}
			return nil, nil
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := DiagnoseConnectivity(ctx, Credentials{ServerURL: server.URL, NodeToken: "private-token"}, nil); err == nil {
		t.Fatal("expected HTTP refusal")
	}
	select {
	case at := <-observed:
		if remaining := time.Until(at); remaining <= 0 || remaining > 10*time.Second {
			t.Fatalf("not the owned10s transport deadline: %s", remaining)
		}
	default:
		t.Fatal("actual transport had no deadline")
	}
}

func TestConnectivityHTTPSProxyTLSIsNotTargetTLS(t *testing.T) {
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	connectivityTransport(t, func(transport *http.Transport) {
		transport.Proxy = http.ProxyURL(proxyURL)
		transport.TLSClientConfig = proxy.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		transport.TLSClientConfig.InsecureSkipVerify = false
		roots := x509.NewCertPool()
		roots.AddCert(proxy.Certificate())
		transport.TLSClientConfig.RootCAs = roots
	})
	report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: "https://target.fixture.invalid/prefix", NodeToken: "private-node"}, nil)
	if err == nil || proto.ConnectionValue(report, "connection.phase.proxy_connect") != "failed" || proto.ConnectionValue(report, "connection.phase.tls") != "not_observed" {
		t.Fatalf("proxy TLS was presented as target TLS: %+v %v", report.Facts, err)
	}
}

func TestConnectivityNewProbeRequiresRegisteredWithoutBreakingLegacy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		raw := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
		session, err := yamux.Server(raw, laneYamuxConfig(proto.LaneData, io.Discard), nil)
		if err != nil {
			return
		}
		defer session.Close()
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close()
		_, _ = readLine(stream)
		var hello proto.Envelope
		_ = json.NewDecoder(stream).Decode(&hello)
		body, _ := json.Marshal(proto.Hello{ProtoMin: 1, ProtoMax: 1})
		_ = json.NewEncoder(stream).Encode(proto.Envelope{T: proto.THello, D: body})
		_, _ = io.Copy(io.Discard, stream)
	}))
	defer server.Close()
	legacy := New(Credentials{ServerURL: server.URL, NodeToken: "private-token", Smoke: true}, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := legacy.Run(ctx); err != nil {
		t.Fatalf("legacy Hello-only handshake changed: %v", err)
	}
	report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: server.URL, NodeToken: "private-token"}, nil)
	if err == nil || proto.ConnectionValue(report, "connection.control") == "smoke_verified" || proto.ConnectionValue(report, "connection.phase.wait_registered") != "failed" {
		t.Fatalf("new probe claimed token registration without Registered: %+v %v", report.Facts, err)
	}
}
