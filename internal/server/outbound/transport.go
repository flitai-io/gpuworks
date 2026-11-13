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
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		cfg, err := readProxy()
		if err != nil {
			return nil, fmt.Errorf("cannot read the management node's outbound proxy")
		}
		return httpproxy.Proxy(req, httpproxy.Config{HTTPProxy: cfg.HTTPProxy, HTTPSProxy: cfg.HTTPSProxy, NoProxy: cfg.NoProxy}, false)
	}
	tr.DialContext = pinnedDial
	tr.DialTLSContext = nil
	tr.DialTLS = nil //nolint:staticcheck // SA1019: clear inherited legacy TLS hooks so they cannot bypass address pinning.
	return &SafeTransport{Transport: tr, policy: policy{allowed: append([]string(nil), allowPrivateHosts...)}}
}

// TransportWithPrivateHosts reads administrator permissions for every request,
// including redirects and requests reusing an idle connection. A nil or empty
// answer grants no private exceptions. The reader must support concurrent calls.
func TransportWithPrivateHosts(readProxy func() (proto.NetworkConfig, error), readHosts func() []string) *SafeTransport {
	tr := Transport(readProxy)
	tr.policy.readAllowed = readHosts
	return tr
}
