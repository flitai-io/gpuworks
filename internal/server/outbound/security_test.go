package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestUntrustedTargetsDoNotReachNetwork(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer server.Close()
	tr := Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil })
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	for _, target := range []string{server.URL, "http://localhost:9/", "http://169.254.169.254/latest/meta-data/", "http://[::1]:9/", "http://10.1.2.3:9/"} {
		_, err := client.Get(target)
		if err == nil || strings.Contains(err.Error(), "connection refused") {
			t.Errorf("target %s: unsafe error %v", target, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("untrusted origin received %d requests", hits.Load())
	}
}

func TestPrivateMissCanBeAllowedAndReservedAddressCannot(t *testing.T) {
	p := policy{}
	if _, err := p.addresses(context.Background(), "10.1.2.3"); !errors.Is(err, ErrTargetNeedsAllow) {
		t.Fatalf("private miss: %v", err)
	}
	_, err := p.addresses(context.Background(), "169.254.169.254")
	if !errors.Is(err, ErrTargetDenied) || errors.Is(err, ErrTargetNeedsAllow) {
		t.Fatalf("reserved: %v", err)
	}
}

func TestAllowedPrivateHostStillCannotRedirectToMetadata(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer server.Close()
	tr := Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, "127.0.0.1")
	defer tr.CloseIdleConnections()
	_, err := (&http.Client{Transport: tr}).Get(server.URL)
	if err == nil || hits.Load() != 1 {
		t.Fatalf("hits=%d error=%v", hits.Load(), err)
	}
}

func TestPolicyRejectsEveryNonPublicDNSAnswerBeforeDial(t *testing.T) {
	for _, ips := range [][]netip.Addr{
		{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")},
		{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")},
	} {
		p := policy{lookup: func(context.Context, string, string) ([]netip.Addr, error) { return ips, nil }}
		if _, err := p.addresses(context.Background(), "models.example"); !errors.Is(err, ErrTargetDenied) {
			t.Fatalf("addresses=%v error=%v", ips, err)
		}
	}
}

func TestPolicyAllowsScopedPrivateSourcesOnly(t *testing.T) {
	p := policy{allowed: []string{"mirror.corp", "10.20.0.0/16", "127.0.0.1", "0.0.0.0/0"}}
	for _, tc := range []struct {
		host, ip string
		allow    bool
	}{
		{"mirror.corp", "192.168.1.10", true}, {"other.corp", "192.168.1.10", true},
		{"other.corp", "10.20.2.3", true}, {"127.0.0.1", "127.0.0.1", true},
		{"mirror.corp", "169.254.169.254", false}, {"mirror.corp", "100.100.100.200", false},
		{"mirror.corp", "fd00:ec2::254", false}, {"mirror.corp", "168.63.129.16", false},
		{"mirror.corp", "0.0.0.0", false}, {"mirror.corp", "224.0.0.1", false},
		{"mirror.corp", "::ffff:169.254.169.254", false}, {"mirror.corp", "fe80::1", false},
	} {
		if got := p.permits(tc.host, netip.MustParseAddr(tc.ip)); got != tc.allow {
			t.Errorf("%s (%s) allowed=%v", tc.host, tc.ip, got)
		}
	}
	// Exact host exceptions never match a suffix supplied by a request.
	exact := policy{allowed: []string{"mirror.corp"}}
	if exact.permits("mirror.corp.attacker.example", netip.MustParseAddr("10.0.0.1")) {
		t.Fatal("hostname exception matched suffix")
	}
}

func TestExplicitIPv6LoopbackExceptionsDoNotPermitAddressWrappers(t *testing.T) {
	for _, tc := range []struct {
		host, address string
		allowed       []string
		want          bool
	}{
		{"::1", "::1", nil, false},
		{"::1", "::1", []string{"::1"}, true},
		{"localhost", "::1", []string{"localhost"}, true},
		{"localhost", "::7f00:1", []string{"localhost"}, false},
		{"localhost", "::a9fe:a9fe", []string{"localhost"}, false},
		{"localhost", "64:ff9b::7f00:1", []string{"localhost"}, false},
		{"localhost", "64:ff9b::a9fe:a9fe", []string{"localhost"}, false},
		{"localhost", "fd00:ec2::254", []string{"localhost"}, false},
	} {
		got := (policy{allowed: tc.allowed}).permits(tc.host, netip.MustParseAddr(tc.address))
		if got != tc.want {
			t.Errorf("host=%s address=%s exceptions=%v: allowed=%v want=%v", tc.host, tc.address, tc.allowed, got, tc.want)
		}
	}
}

func TestDeniedTargetNeverContactsConfiguredProxy(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer proxy.Close()
	tr := Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{HTTPProxy: proxy.URL}, nil })
	defer tr.CloseIdleConnections()
	_, err := (&http.Client{Transport: tr}).Get("http://169.254.169.254/latest/meta-data/")
	if err == nil || hits.Load() != 0 {
		t.Fatalf("proxy hits=%d error=%v", hits.Load(), err)
	}
}

func TestTransportPinsValidatedDNSAddress(t *testing.T) {
	var lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "mirror.corp:") {
			t.Errorf("host header=%q", r.Host)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	tr := Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, "mirror.corp")
	tr.Proxy = nil
	tr.policy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		if lookups.Add(1) > 1 {
			return nil, errors.New("DNS changed after validation")
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	defer tr.CloseIdleConnections()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	response, err := (&http.Client{Transport: tr}).Get("http://mirror.corp:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if lookups.Load() != 1 || response.StatusCode != http.StatusNoContent {
		t.Fatalf("lookups=%d status=%d", lookups.Load(), response.StatusCode)
	}
}

func TestProxyTunnelsPinAuthorityAndRetainHostAndSNI(t *testing.T) {
	for _, secureProxy := range []bool{false, true} {
		for _, secureOrigin := range []bool{false, true} {
			t.Run(fmt.Sprintf("proxyTLS=%v/originTLS=%v", secureProxy, secureOrigin), func(t *testing.T) {
				var requests, tunnels, lookups atomic.Int32
				originHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if !strings.HasPrefix(r.Host, "example.com:") {
						t.Errorf("origin Host=%q", r.Host)
					}
					if secureOrigin && r.TLS.ServerName != "example.com" {
						t.Errorf("SNI=%q", r.TLS.ServerName)
					}
					w.WriteHeader(http.StatusNoContent)
				})
				var origin *httptest.Server
				if secureOrigin {
					origin = httptest.NewTLSServer(originHandler)
				} else {
					origin = httptest.NewServer(originHandler)
				}
				defer origin.Close()
				originURL, _ := url.Parse(origin.URL)
				proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					tunnels.Add(1)
					if r.Method != http.MethodConnect || r.Host != originURL.Host {
						t.Errorf("proxy request=%s %s", r.Method, r.Host)
						w.WriteHeader(400)
						return
					}
					target, err := net.Dial("tcp", originURL.Host)
					if err != nil {
						t.Error(err)
						w.WriteHeader(502)
						return
					}
					defer target.Close()
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
					done := make(chan struct{})
					go func() { _, _ = io.Copy(target, conn); close(done); target.Close() }()
					_, _ = io.Copy(conn, target)
					conn.Close()
					<-done
				})
				var proxy *httptest.Server
				if secureProxy {
					proxy = httptest.NewUnstartedServer(proxyHandler)
					proxy.EnableHTTP2 = true
					proxy.StartTLS()
				} else {
					proxy = httptest.NewServer(proxyHandler)
				}
				defer proxy.Close()
				tr := Transport(func() (proto.NetworkConfig, error) {
					return proto.NetworkConfig{HTTPProxy: proxy.URL, HTTPSProxy: proxy.URL}, nil
				}, "example.com")
				// httptest's TLS certificates are used only for these local endpoints.
				if secureProxy {
					tr.TLSClientConfig = proxy.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				} else if secureOrigin {
					tr.TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				} else {
					tr.TLSClientConfig = &tls.Config{}
				}
				tr.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
				tr.policy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
					lookups.Add(1)
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				}
				defer tr.CloseIdleConnections()
				client := &http.Client{Transport: tr}
				for range 2 {
					response, err := client.Get(originURL.Scheme + "://example.com:" + originURL.Port() + "/config.json")
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if response.StatusCode != http.StatusNoContent {
						t.Fatalf("status=%d", response.StatusCode)
					}
				}
				if requests.Load() != 2 || tunnels.Load() != 1 || lookups.Load() != 2 {
					t.Fatalf("requests=%d tunnels=%d DNS checks=%d", requests.Load(), tunnels.Load(), lookups.Load())
				}
			})
		}
	}
}

func TestProxyTunnelCancellationClosesPendingHandshake(t *testing.T) {
	connected, closed := make(chan struct{}), make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		close(connected)
		_, _ = io.Copy(io.Discard, conn)
		close(closed)
	}))
	defer proxy.Close()
	tr := Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{HTTPProxy: proxy.URL}, nil })
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://8.8.8.8/model", nil)
	result := make(chan error, 1)
	go func() { _, err := (&http.Client{Transport: tr}).Do(request); result <- err }()
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not receive CONNECT")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not stop request")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation leaked proxy connection")
	}
}
