package app

import (
	"context"
	"errors"
	"io"
	stdnet "net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/modelfetch"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A model download goes through the node's proxy except to the hosts the
// node's no_proxy lists (plus loopback and the server, which the agent adds),
// matched as Go matches NO_PROXY. A directory source is no exception.
func TestModelSourceClientSkipsTheProxyForNoProxyHosts(t *testing.T) {
	net := proto.NetworkConfig{HTTPProxy: "http://proxy.corp:3128", HTTPSProxy: "http://proxy.corp:3128", NoProxy: "files.corp,.mirror.corp"}
	for _, tc := range []struct {
		source, endpoint, target string
		direct                   bool
	}{
		{"url", "http://files.corp/models/qwen", "http://files.corp/models/qwen/config.json", true},
		{"huggingface", "https://hf.mirror.corp", "https://hf.mirror.corp/api/models/x", true},
		{"huggingface", "https://huggingface.co", "https://huggingface.co/api/models/x", false},
		{"url", "http://other.example/m", "http://other.example/m/config.json", false},
	} {
		client, err := modelSourceClient(tc.source, tc.endpoint, net)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, tc.target, nil)
		got, err := client.Transport.(routeNamingTransport).Proxy(req)
		if err != nil || (got == nil) != tc.direct {
			t.Errorf("%s: proxy = %v, err = %v; want direct=%v", tc.target, got, err, tc.direct)
		}
	}
}

// A directory source is downloaded through the node's proxy - the HTTP
// proxy for an http:// directory - even when the agent's own environment says
// NO_PROXY=*: an explicit proxy answers only to its own no_proxy.
func TestStateSyncModelDownloadSelectsHTTPProxyForHTTPSource(t *testing.T) {
	files := urlModelFiles()
	_, fixture, _ := newAutoindexFixture(t, "/models/proxy", files)
	fixtureURL, err := url.Parse(fixture)
	if err != nil {
		t.Fatal(err)
	}
	// Loopback is never proxied, so the task names a host only the proxy
	// knows how to reach.
	endpoint := "http://files.example" + fixtureURL.Path
	var proxyRequests atomic.Int64
	forward := http.DefaultTransport.(*http.Transport).Clone()
	forward.Proxy = nil
	defer forward.CloseIdleConnections()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		req := r.Clone(r.Context())
		req.RequestURI = ""
		if req.URL.Host == "files.example" {
			req.URL.Host, req.Host = fixtureURL.Host, fixtureURL.Host
		}
		response, err := forward.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			w.Header()[name] = values
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer proxy.Close()
	wrongProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "HTTPS-only proxy must not receive HTTP model requests", http.StatusBadGateway)
	}))
	defer wrongProxy.Close()
	t.Setenv("NO_PROXY", "*")
	root := t.TempDir()
	node, _ := urlModelRuntime(t, root)
	final := make(chan proto.ModelFileStatus, 1)
	node.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			status := value.(proto.ModelFileStatus)
			if status.State == proto.MFPresent || status.State == proto.MFFailed || status.State == proto.MFInsufficientDisk {
				final <- status
			}
		}
		return nil
	})
	task := urlTask(endpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	node.OnStateSync(ctx, proto.DesiredState{Network: proto.NetworkConfig{HTTPProxy: proxy.URL, HTTPSProxy: wrongProxy.URL}, ModelFiles: []proto.ModelFileTask{task}})
	select {
	case status := <-final:
		if status.State != proto.MFPresent {
			t.Fatalf("HTTP proxy model download failed: %+v", status)
		}
	case <-ctx.Done():
		t.Fatal("model download did not finish")
	}
	if proxyRequests.Load() == 0 {
		t.Fatal("explicit HTTP proxy was bypassed by ambient NO_PROXY")
	}
	for name, file := range files {
		actual, err := os.ReadFile(filepath.Join(root, "model", name))
		if err != nil || string(actual) != file.body {
			t.Fatalf("downloaded %s = %q, err=%v", name, actual, err)
		}
	}
}

// A mirror that answers with a 302 to a host the node's no_proxy lists is
// followed there directly, the way hf-mirror.com sends a Xet-stored file to
// cas-bridge.xethub.hf.co (GWKS-508); a redirect to an unlisted host goes
// through the proxy. A request that fails says which of the two it took.
func TestModelSourceClientRoutesEachRedirectHopByNoProxy(t *testing.T) {
	var proxied atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		// A proxy that cannot reach the host: the connection just ends.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer proxy.Close()
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "weights")
	}))
	defer storage.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.URL.Query().Get("to")+"/model.safetensors", http.StatusFound)
	}))
	defer mirror.Close()
	// Loopback is never proxied, so the hosts get names and the dialer
	// takes them to the test servers.
	addrs := map[string]string{
		"mirror.example:80":    mirror.Listener.Addr().String(),
		"storage.example:80":   storage.Listener.Addr().String(),
		"elsewhere.example:80": storage.Listener.Addr().String(),
		"proxy.example:3128":   proxy.Listener.Addr().String(),
	}
	net := proto.NetworkConfig{HTTPProxy: "http://proxy.example:3128", HTTPSProxy: "http://proxy.example:3128", NoProxy: "mirror.example,storage.example"}
	client, err := modelSourceClient("huggingface", "http://mirror.example", net)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(routeNamingTransport)
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, addr string) (stdnet.Conn, error) {
		target, ok := addrs[addr]
		if !ok {
			t.Errorf("dialed unexpected address %s", addr)
			target = addr
		}
		return dial(ctx, network, target)
	}
	defer transport.CloseIdleConnections()

	resp, err := client.Get("http://mirror.example/resolve?to=storage.example")
	if err != nil {
		t.Fatalf("redirect to a no_proxy host: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "weights" || proxied.Load() != 0 {
		t.Fatalf("body %q, proxy requests %d; want the weights fetched directly", body, proxied.Load())
	}

	_, err = client.Get("http://mirror.example/resolve?to=elsewhere.example")
	if err == nil || proxied.Load() != 1 {
		t.Fatalf("redirect to an unlisted host: err %v, proxy requests %d; want one failed proxied request", err, proxied.Load())
	}
	if want := "(via proxy proxy.example:3128)"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name the route %s", err, want)
	}

	storage.Close()
	_, err = client.Get("http://mirror.example/resolve?to=storage.example")
	if err == nil || !strings.Contains(err.Error(), "(direct)") || proxied.Load() != 1 {
		t.Fatalf("direct failure: err %v, proxy requests %d; want an error naming the direct route", err, proxied.Load())
	}
}

// Naming the route keeps a timeout a timeout: url.Error asks the error it
// wraps directly, and the retry and source-switch decisions read it.
func TestRoutedTimeoutStillReadsAsATimeout(t *testing.T) {
	listener, err := stdnet.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		var held []stdnet.Conn // accepted, never answering the handshake
		for {
			conn, err := listener.Accept()
			if err != nil {
				for _, c := range held {
					_ = c.Close()
				}
				return
			}
			held = append(held, conn)
		}
	}()
	client := httpClientFor(proto.NetworkConfig{HTTPSProxy: "http://proxy.example:3128"})
	client.Transport.(routeNamingTransport).TLSHandshakeTimeout = 100 * time.Millisecond
	_, err = client.Get("https://" + listener.Addr().String() + "/model.safetensors")
	if err == nil {
		t.Fatal("handshake with a silent server succeeded")
	}
	var netErr stdnet.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("error %q is not a timeout", err)
	}
	if !modelfetch.IsTransient(err) || !strings.Contains(err.Error(), "(direct)") {
		t.Fatalf("error %q: transient %v; want a transient error naming the direct route", err, modelfetch.IsTransient(err))
	}
}

// An https address answered in plain HTTP keeps net/http's own explanation,
// which it derives from the TLS error's type.
func TestRoutedClientKeepsTheSchemeMismatchError(t *testing.T) {
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	client := httpClientFor(proto.NetworkConfig{HTTPSProxy: "http://proxy.example:3128"})
	_, err := client.Get("https://" + plain.Listener.Addr().String() + "/model.safetensors")
	if !errors.Is(err, http.ErrSchemeMismatch) {
		t.Fatalf("error %v; want http.ErrSchemeMismatch", err)
	}
}
