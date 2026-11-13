package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrTargetDenied deliberately reveals neither DNS answers nor network errors.
// Reserved, link-local and non-global addresses use this error: an allow-list
// entry does not make them reachable.
var ErrTargetDenied = errors.New("outbound URL is not permitted")

// ErrTargetNeedsAllow is a private or loopback target the allow list can
// permit. Callers can offer that approval without treating it as a reserved
// address.
var ErrTargetNeedsAllow = fmt.Errorf("%w: private host is not allowed", ErrTargetDenied)

// policy applies only to user-configurable HTTP sources, never node RPC/SSH.
// Exceptions are administrator configuration, not fields accepted by the API.
type policy struct {
	allowed     []string
	readAllowed func() []string
	lookup      func(context.Context, string, string) ([]netip.Addr, error)
}

func (p policy) addresses(ctx context.Context, host string) ([]netip.Addr, error) {
	// Use the same ASCII authority for policy, DNS and the transport's dial.
	// Unicode domain names must be supplied in their Punycode form; paths may
	// still contain Unicode. Do not resolve an ambiguous authority first.
	for _, r := range host {
		if r > 127 {
			return nil, fmt.Errorf("%w: hostname must use ASCII/Punycode", ErrTargetDenied)
		}
	}
	if strings.Contains(host, "%") || host == "" {
		return nil, ErrTargetDenied
	}
	var addresses []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{ip.Unmap()}
	} else {
		lookup := p.lookup
		if lookup == nil {
			lookup = net.DefaultResolver.LookupNetIP
		}
		var err error
		addresses, err = lookup(ctx, "ip", host)
		if err != nil {
			return nil, requestFailure{err}
		}
	}
	if len(addresses) == 0 {
		return nil, ErrTargetDenied
	}
	if p.readAllowed != nil {
		p.allowed = p.readAllowed()
	}
	// Validate the entire DNS answer before any dial. Mixed public/private
	// answers must not allow a public first address to hide a private fallback.
	for _, address := range addresses {
		address = address.Unmap()
		if addressUnallowable(address) {
			return nil, ErrTargetDenied
		}
	}
	// An allow-list miss must not hide a permanently forbidden later answer.
	for _, address := range addresses {
		address = address.Unmap()
		if !p.permits(host, address) {
			return nil, ErrTargetNeedsAllow
		}
	}
	return addresses, nil
}

// addressUnallowable reports addresses the allow list is not allowed to permit.
func addressUnallowable(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || (!address.IsGlobalUnicast() && !address.IsLoopback()) || address.IsLinkLocalUnicast() {
		return true
	}
	for _, prefix := range forbiddenRanges {
		// ::/96 covers IPv4-compatible wrappers and genuine ::1. Only the
		// latter follows the explicit loopback exception policy below.
		if prefix.Contains(address) && address != netip.IPv6Loopback() {
			return true
		}
	}
	return false
}

var forbiddenRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("168.63.129.16/32"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

func (p policy) permits(host string, address netip.Addr) bool {
	address = address.Unmap()
	if addressUnallowable(address) {
		return false
	}
	if !address.IsPrivate() && !address.IsLoopback() {
		return true
	}
	for _, allowed := range p.allowed {
		allowed = strings.TrimSpace(allowed)
		if prefix, err := netip.ParsePrefix(allowed); err == nil {
			if prefix.Contains(address) {
				return true
			}
		} else if strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(allowed, ".")) {
			return true
		} else if ip, err := netip.ParseAddr(allowed); err == nil && ip.Unmap() == address {
			return true
		}
	}
	return false
}

type pinnedTarget struct {
	host, port     string
	addresses      []netip.Addr
	requestContext context.Context
}
type targetKey struct{}

// SafeTransport checks each request (including redirects) and pins its DNS
// result to the socket. The embedded transport retains the usual Proxy hook.
type SafeTransport struct {
	*http.Transport
	policy         policy
	mu             sync.Mutex
	transports     map[string]*http.Transport
	transportOrder []string
}

// requestFailure preserves retry/cancellation classification without exposing
// the resolver, socket address, proxy credentials, or remote response details.
type requestFailure struct{ cause error }

func (e requestFailure) Error() string { return "outbound request failed" }
func (e requestFailure) Unwrap() error { return e.cause }

func (t *SafeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.User != nil {
		return nil, ErrTargetDenied
	}
	host := req.URL.Hostname()
	addresses, err := t.policy.addresses(req.Context(), host)
	if err != nil {
		return nil, err
	}
	port := req.URL.Port()
	if port == "" {
		port = "80"
		if req.URL.Scheme == "https" {
			port = "443"
		}
	}
	pinned := pinnedTarget{host: host, port: port, addresses: addresses, requestContext: req.Context()}
	clone := req.Clone(context.WithValue(req.Context(), targetKey{}, pinned))
	var proxy *url.URL
	if t.Transport.Proxy != nil {
		proxy, err = t.Transport.Proxy(req)
		if err != nil {
			return nil, requestFailure{err}
		}
	}
	if proxy != nil && proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h" {
		return nil, ErrTargetDenied
	}
	transport := t.requestTransport(proxy, pinned)
	resp, err := transport.RoundTrip(clone)
	if resp != nil && resp.Body != nil {
		resp.Body = &closeTransportBody{ReadCloser: resp.Body, transport: transport, owner: t}
	}
	t.closeRetiredIdle(transport)
	if err != nil {
		return nil, requestFailure{err}
	}
	// Redirect decisions belong to the original URL, not the pinned proxy IP.
	resp.Request = req
	return resp, nil
}

type closeTransportBody struct {
	io.ReadCloser
	transport *http.Transport
	owner     *SafeTransport
}

func (b *closeTransportBody) Close() error {
	defer b.owner.closeRetiredIdle(b.transport)
	return b.ReadCloser.Close()
}

func pinnedDial(ctx context.Context, network, address string) (net.Conn, error) {
	target, err := checkedPinnedTarget(ctx, address)
	if err != nil {
		return nil, err
	}
	var last error
	for _, ip := range target.addresses {
		conn, err := (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), target.port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, requestFailure{last}
}

func checkedPinnedTarget(ctx context.Context, address string) (pinnedTarget, error) {
	target, ok := ctx.Value(targetKey{}).(pinnedTarget)
	host, port, err := net.SplitHostPort(address)
	if err != nil || !ok || !strings.EqualFold(host, target.host) || port != target.port {
		return pinnedTarget{}, ErrTargetDenied
	}
	return target, nil
}
