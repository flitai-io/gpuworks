package outbound

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Proxy routes and validated DNS targets select different pools. A request
// must never reuse a connection to an address absent from its validated DNS
// result, even if that connection was permitted when originally dialed.
// In-flight requests keep their transport until they finish.
const maxTransportPools = 8

func transportPoolKey(proxy *url.URL, target pinnedTarget) string {
	var key strings.Builder
	if proxy != nil {
		key.WriteString(proxy.String())
	}
	key.WriteByte(0)
	key.WriteString(strings.ToLower(target.host))
	key.WriteByte(0)
	key.WriteString(target.port)
	addresses := make([]string, 0, len(target.addresses))
	for _, ip := range target.addresses {
		addresses = append(addresses, ip.Unmap().String())
	}
	sort.Strings(addresses)
	previous := ""
	for _, address := range addresses {
		if address != previous {
			key.WriteByte(0)
			key.WriteString(address)
		}
		previous = address
	}
	return key.String()
}

func (t *SafeTransport) requestTransport(proxy *url.URL, target pinnedTarget) *http.Transport {
	key := transportPoolKey(proxy, target)
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing := t.transports[key]; existing != nil {
		return existing
	}
	transport := t.Transport.Clone()
	transport.Proxy = nil
	transport.DialTLSContext = nil
	transport.DialTLS = nil //nolint:staticcheck // SA1019: clear inherited legacy TLS hooks so they cannot bypass address pinning.
	transport.DialContext = pinnedDial
	if proxy != nil {
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			target, err := checkedPinnedTarget(ctx, address)
			if err != nil {
				return nil, err
			}
			// net/http detaches dialing from request cancellation so it can
			// reuse a completed dial. A proxy handshake still belongs to the
			// initiating request and must not wait after it is canceled.
			ctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(target.requestContext, cancel)
			defer stop()
			defer cancel()
			var last error
			for _, ip := range target.addresses {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				destination := net.JoinHostPort(ip.Unmap().String(), target.port)
				var conn net.Conn
				if proxy.Scheme == "socks5" || proxy.Scheme == "socks5h" {
					conn, err = dialSOCKSTunnel(ctx, proxy, destination)
				} else {
					conn, err = dialProxyTunnel(ctx, proxy, destination, transport.TLSClientConfig)
				}
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, requestFailure{last}
		}
	}
	if t.transports == nil {
		t.transports = make(map[string]*http.Transport)
	}
	if len(t.transportOrder) == maxTransportPools {
		oldest := t.transportOrder[0]
		t.transports[oldest].CloseIdleConnections()
		delete(t.transports, oldest)
		t.transportOrder = t.transportOrder[1:]
	}
	t.transports[key] = transport
	t.transportOrder = append(t.transportOrder, key)
	return transport
}

func (t *SafeTransport) closeRetiredIdle(transport *http.Transport) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, active := range t.transports {
		if active == transport {
			return
		}
	}
	transport.CloseIdleConnections()
}

// CloseIdleConnections also covers cached transports and is safe while
// requests are in flight, matching the standard http.Transport contract.
func (t *SafeTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Transport.CloseIdleConnections()
	for _, transport := range t.transports {
		transport.CloseIdleConnections()
	}
}
