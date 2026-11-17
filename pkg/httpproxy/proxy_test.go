package httpproxy_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/pkg/httpproxy"
)

func TestConfiguredProxyCarriesRequests(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.String() != "http://registry.invalid/tags" {
			t.Errorf("target = %s", r.URL)
		}
		_, _ = io.WriteString(w, "registry-tags")
	}))
	defer proxy.Close()
	tr := httpproxy.Transport(httpproxy.Config{HTTPProxy: proxy.URL}, false)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	response, err := client.Get("http://registry.invalid/tags")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	b, _ := io.ReadAll(response.Body)
	if string(b) != "registry-tags" || hits.Load() != 1 {
		t.Fatalf("body=%q hits=%d", b, hits.Load())
	}
}

func TestDirectBypassesConfiguredAndEnvironmentProxies(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "direct") }))
	defer origin.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	tr := httpproxy.Transport(httpproxy.Config{HTTPProxy: "http://127.0.0.1:1"}, true)
	defer tr.CloseIdleConnections()
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "origin.invalid:80" {
			address = origin.Listener.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	response, err := (&http.Client{Transport: tr}).Get("http://origin.invalid")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	b, _ := io.ReadAll(response.Body)
	if string(b) != "direct" {
		t.Fatalf("body=%q", b)
	}
}

func TestInvalidProxyFailsWithoutLeakingCredentials(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://registry.invalid", nil)
	for _, raw := range []string{"http://alice:secret@", "ftp://alice:secret@proxy.invalid", "http://proxy.invalid:bad", "http://proxy.invalid/path"} {
		_, err := httpproxy.Proxy(req, httpproxy.Config{HTTPProxy: raw}, false)
		if err == nil {
			t.Errorf("invalid proxy accepted")
		} else if strings.Contains(err.Error(), "secret") {
			t.Error("credential in error")
		}
	}
}

func TestProxySelectionContract(t *testing.T) {
	t.Setenv("NO_PROXY", "*")
	for _, tt := range []struct{ name, scheme, hp, sp, want string }{
		{"http uses HTTP proxy", "http", "http://one.invalid:80", "http://two.invalid:81", "http://one.invalid:80"},
		{"https uses HTTPS proxy", "https", "http://one.invalid:80", "http://two.invalid:81", "http://two.invalid:81"},
		{"HTTPS falls back to sole HTTP proxy", "https", "proxy.invalid:82", "", "http://proxy.invalid:82"},
		{"HTTP falls back to sole HTTPS proxy", "http", "", "http://proxy.invalid:83", "http://proxy.invalid:83"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, tt.scheme+"://registry.invalid/tags", nil)
			got, err := httpproxy.Proxy(req, httpproxy.Config{HTTPProxy: tt.hp, HTTPSProxy: tt.sp}, false)
			if err != nil || got == nil || got.String() != tt.want {
				t.Fatalf("proxy=%v err=%v want=%s", got, err, tt.want)
			}
		})
	}
}

func TestInheritedEnvironment(t *testing.T) {
	if os.Getenv("GPUWORKS_PROXY_TEST_CHILD") == "1" {
		tr := httpproxy.Transport(httpproxy.Config{}, false)
		defer tr.CloseIdleConnections()
		response, err := (&http.Client{Transport: tr}).Get("http://registry.invalid/tags")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		b, _ := io.ReadAll(response.Body)
		if string(b) != "inherited" {
			t.Fatalf("body=%q", b)
		}
		return
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "inherited") }))
	defer proxy.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInheritedEnvironment$")
	for _, entry := range os.Environ() {
		k, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "REQUEST_METHOD", "GPUWORKS_PROXY_TEST_CHILD":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "HTTP_PROXY="+proxy.URL, "GPUWORKS_PROXY_TEST_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("inherited proxy: %v\n%s", err, out)
	}
}

// NO_PROXY is matched the way Go matches the environment variable
// (golang.org/x/net/http/httpproxy), so the agent's own requests, the Docker
// daemon and the CLI runtimes agree on what bypasses the proxy.
func TestBypassesFollowsGoNoProxySemantics(t *testing.T) {
	for _, tc := range []struct {
		noProxy, hostport string
		want              bool
	}{
		{"", "registry.example:443", false},
		{"*", "registry.example:443", true},
		{"example.com", "example.com:443", true},
		{"example.com", "sub.example.com:443", true},
		{"example.com", "notexample.com:443", false},
		{".example.com", "example.com:443", false},
		{".example.com", "sub.example.com:443", true},
		{"*.example.com", "example.com:443", false},
		{"*.example.com", "a.b.example.com:443", true},
		{"example.com:8443", "example.com:443", false},
		{"example.com:8443", "example.com:8443", true},
		{"10.0.0.0/8", "10.1.2.3:80", true},
		{"10.0.0.0/8", "11.1.2.3:80", false},
		{"192.168.1.5", "192.168.1.5:80", true},
		{"192.168.1.5:80", "192.168.1.5:8080", false},
		{"[fd00::1]:8090", "[fd00::1]:8090", true},
		{"fd00::/8", "[fd12::5]:443", true},
		{" Files.Corp , other.example ", "files.corp:80", true},
		{"other.example", "localhost:9000", true},
		{"other.example", "127.0.0.1:9000", true},
		{"other.example", "[::1]:9000", true},
	} {
		if got := httpproxy.Bypasses(tc.noProxy, tc.hostport); got != tc.want {
			t.Errorf("Bypasses(%q, %q) = %v, want %v", tc.noProxy, tc.hostport, got, tc.want)
		}
	}
}

// An explicit proxy skips the hosts its own NoProxy lists - and only those:
// the environment's NO_PROXY does not apply to an explicit choice.
func TestExplicitProxyHonorsItsNoProxy(t *testing.T) {
	t.Setenv("NO_PROXY", "proxied.example")
	cfg := httpproxy.Config{HTTPProxy: "http://proxy.invalid:3128", HTTPSProxy: "http://proxy.invalid:3128", NoProxy: "files.corp,registry.corp:443"}
	for target, direct := range map[string]bool{
		"http://files.corp/models/":    true,
		"https://registry.corp/v2/":    true,
		"http://registry.corp/v2/":     false,
		"https://proxied.example/v2/":  false,
		"https://huggingface.co/api/x": false,
	} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		got, err := httpproxy.Proxy(req, cfg, false)
		if err != nil || (got == nil) != direct {
			t.Errorf("%s: proxy = %v, err = %v; want direct=%v", target, got, err, direct)
		}
	}
}

func TestValidateAcceptsWhatProxyUsesAndNeverRepeatsTheURL(t *testing.T) {
	for _, ok := range []string{"http://proxy:3128", "https://u:p@proxy:443", "socks5://proxy:1080", "socks5h://u:p@proxy:1080/", "proxy.example:3128"} {
		if err := httpproxy.Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", "ftp://u:s3cret@proxy:21", "http://u:s3cret@proxy:3128/path", "http://u:s3cret@proxy:3128?x=1", "http://u:s3cret@"} {
		err := httpproxy.Validate(bad)
		if err == nil {
			t.Errorf("Validate(%q) accepted", bad)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("Validate(%q) error leaks the password: %v", bad, err)
		}
	}
}
