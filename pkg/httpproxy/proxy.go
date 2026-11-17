// Package httpproxy resolves outbound HTTP routing consistently for server and
// agent requests. It does not change process-wide clients or environment.
package httpproxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Config is an explicit proxy choice: the proxy for http:// and for https://
// requests, and the hosts reached without either (NO_PROXY syntax).
type Config struct {
	HTTPProxy  string
	HTTPSProxy string
	NoProxy    string
}

// Proxy resolves one request. Direct bypasses every proxy. Explicit HTTP(S)
// configuration overrides the environment, including NO_PROXY: only
// cfg.NoProxy decides what bypasses it, with the same matching Go applies to
// the environment variable (Bypasses). An omitted scheme uses the other
// configured proxy for compatibility. Only a completely unspecified
// configuration inherits the standard proxy environment. Errors never include
// proxy credentials or the configured URL.
func Proxy(req *http.Request, cfg Config, direct bool) (*url.URL, error) {
	if direct {
		return nil, nil
	}
	httpProxy, httpsProxy := strings.TrimSpace(cfg.HTTPProxy), strings.TrimSpace(cfg.HTTPSProxy)
	if httpProxy == "" && httpsProxy == "" {
		return http.ProxyFromEnvironment(req)
	}
	hp, err := parse(httpProxy)
	if err != nil {
		return nil, err
	}
	sp, err := parse(httpsProxy)
	if err != nil {
		return nil, err
	}
	if hp == nil {
		hp = sp
	}
	if sp == nil {
		sp = hp
	}
	if Bypasses(cfg.NoProxy, canonicalAddr(req.URL)) {
		return nil, nil
	}
	if req.URL.Scheme == "https" {
		return sp, nil
	}
	return hp, nil
}

// Bypasses reports whether a request to hostport goes without the proxy, as
// Go's NO_PROXY handling decides it (golang.org/x/net/http/httpproxy): "*"
// bypasses everything; an IP address or CIDR block matches addresses; a
// domain name matches itself and its subdomains, and with a leading "." (or
// "*.") its subdomains only; any entry may name a port. Loopback addresses
// and localhost are never proxied.
func Bypasses(noProxy, hostport string) bool {
	host, port := hostport, ""
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, port = h, p
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	for _, entry := range strings.Split(noProxy, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		if _, block, err := net.ParseCIDR(entry); err == nil {
			if ip != nil && block.Contains(ip) {
				return true
			}
			continue
		}
		entryHost, entryPort := entry, ""
		if h, p, err := net.SplitHostPort(entry); err == nil {
			entryHost, entryPort = h, p
		}
		if entryPort != "" && entryPort != port {
			continue
		}
		if entryIP := net.ParseIP(entryHost); entryIP != nil {
			if ip != nil && entryIP.Equal(ip) {
				return true
			}
			continue
		}
		entryHost = strings.TrimSuffix(strings.TrimPrefix(entryHost, "*"), ".")
		if strings.HasPrefix(entryHost, ".") {
			if strings.HasSuffix(host, entryHost) {
				return true
			}
			continue
		}
		if entryHost != "" && (host == entryHost || strings.HasSuffix(host, "."+entryHost)) {
			return true
		}
	}
	return false
}

// canonicalAddr is the request's host and port, the scheme's default port
// when none is written, as NO_PROXY entries with a port are compared with it.
func canonicalAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// Validate reports whether raw is a proxy URL Proxy accepts: http, https,
// socks5 or socks5h with a host and no path. A bare host:port is read as
// http. The error never contains raw, which may carry a login.
func Validate(raw string) error {
	u, err := parse(strings.TrimSpace(raw))
	if err == nil && u == nil {
		return fmt.Errorf("invalid outbound proxy URL")
	}
	return err
}

func parse(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid outbound proxy URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported outbound proxy scheme")
	}
	if u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid outbound proxy URL")
	}
	return u, nil
}

// Transport clones the standard transport to retain connection pooling and
// timeout defaults. Callers own its lifetime and retain their own HTTP client
// timeout and redirect policy. Invalid configuration fails when a request runs.
func Transport(cfg Config, direct bool) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = func(req *http.Request) (*url.URL, error) { return Proxy(req, cfg, direct) }
	return tr
}
