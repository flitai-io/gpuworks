// Package outbound connects server HTTP requests to the management node's
// current proxy without depending on storage or changing process-wide HTTP
// defaults.
package outbound

import (
	"fmt"
	"net/http"
	"net/url"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/httpproxy"
)

// Transport resolves the proxy for every outbound request, so a change takes
// effect without restarting the server; requests in flight keep their route.
// readProxy is the management node's proxy (planning.ControlPlaneNetwork):
// none means the server process's own environment. A proxy that cannot be
// read fails the request rather than sending it direct. The caller owns the
// transport and its idle connections.
func Transport(readProxy func() (proto.NetworkConfig, error), allowPrivateHosts ...string) *SafeTransport {
	tr := routedTransport(proxyRoute(readProxy))
	tr.policy = policy{allowed: append([]string(nil), allowPrivateHosts...)}
	return tr
}

// Route is where one outbound request goes: through Proxy, or straight to the
// target when Direct is set. A zero Route inherits the server process's proxy
// environment, as an empty proxy does in Transport.
type Route struct {
	Proxy  proto.NetworkConfig
	Direct bool
}

// TransportWithRoute is TransportWithPrivateHosts for a caller that picks its
// own route per request rather than always following the management node's
// proxy - an AI service configured to go direct or through a proxy of its own.
// A route that cannot be read fails the request rather than sending it direct.
func TransportWithRoute(readRoute func() (Route, error), readHosts func() []string) *SafeTransport {
	tr := routedTransport(readRoute)
	tr.policy.readAllowed = readHosts
	return tr
}

func proxyRoute(readProxy func() (proto.NetworkConfig, error)) func() (Route, error) {
	return func() (Route, error) {
		cfg, err := readProxy()
		return Route{Proxy: cfg}, err
	}
}

func routedTransport(readRoute func() (Route, error)) *SafeTransport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		route, err := readRoute()
		if err != nil {
			return nil, fmt.Errorf("cannot read the outbound proxy")
		}
		cfg := route.Proxy
		return httpproxy.Proxy(req, httpproxy.Config{HTTPProxy: cfg.HTTPProxy, HTTPSProxy: cfg.HTTPSProxy, NoProxy: cfg.NoProxy}, route.Direct)
	}
	tr.DialContext = pinnedDial
	tr.DialTLSContext = nil
	tr.DialTLS = nil //nolint:staticcheck // SA1019: clear inherited legacy TLS hooks so they cannot bypass address pinning.
	return &SafeTransport{Transport: tr}
}

// TransportWithPrivateHosts reads administrator permissions for every request,
// including redirects and requests reusing an idle connection. A nil or empty
// answer grants no private exceptions. The reader must support concurrent calls.
func TransportWithPrivateHosts(readProxy func() (proto.NetworkConfig, error), readHosts func() []string) *SafeTransport {
	tr := Transport(readProxy)
	tr.policy.readAllowed = readHosts
	return tr
}
