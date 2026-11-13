package outbound

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRevokedPrivateConnectionCannotSurviveDNSBecomingPublic(t *testing.T) {
	var hits, connects atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, "private-origin") }))
	defer origin.Close()
	parsed, _ := url.Parse(origin.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connects.Add(1)
		if r.Method != "CONNECT" || r.Host != parsed.Host {
			w.WriteHeader(502)
			return
		}
		up, err := net.Dial("tcp", parsed.Host)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer up.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { io.Copy(up, conn); up.Close(); close(done) }()
		io.Copy(conn, up)
		conn.Close()
		<-done
	}))
	defer proxy.Close()
	var allowed atomic.Bool
	allowed.Store(true)
	tr := TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{HTTPProxy: proxy.URL}, nil }, func() []string {
		if allowed.Load() {
			return []string{"mirror.example"}
		}
		return nil
	})
	defer tr.CloseIdleConnections()
	tr.policy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		if allowed.Load() {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	client := &http.Client{Transport: tr}
	target := "http://mirror.example:" + parsed.Port() + "/image.tar"
	resp, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	allowed.Store(false)
	resp, err = client.Get(target)
	if resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	t.Logf("private_origin_requests=%d CONNECT_attempts=%d second_error=%v", hits.Load(), connects.Load(), err)
	if hits.Load() != 1 || connects.Load() != 2 || err == nil {
		t.Fatalf("revoked grant still reached private connection after DNS became public: hits=%d", hits.Load())
	}
}
