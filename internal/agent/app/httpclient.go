package app

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/httpproxy"
)

// httpClientFor builds a download client honoring the node's HTTP(S) proxy.
func httpClientFor(net proto.NetworkConfig) *http.Client {
	if net.HTTPProxy == "" && net.HTTPSProxy == "" {
		return nil // downloader uses its default client
	}
	return &http.Client{
		Timeout:   5 * time.Minute,
		Transport: routeNamingTransport{httpproxy.Transport(httpproxy.Config{HTTPProxy: net.HTTPProxy, HTTPSProxy: net.HTTPSProxy, NoProxy: net.NoProxy}, false)},
	}
}

// routeNamingTransport appends to a failed request's error the route it
// took, "direct" or the proxy's host: a TLS handshake timeout reads the same
// either way, and which one it was decides whether no_proxy is the fix.
type routeNamingTransport struct{ *http.Transport }

func (t routeNamingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.Transport.RoundTrip(req)
	if err == nil || t.Proxy == nil {
		return resp, err
	}
	// http.Client turns this one into ErrSchemeMismatch by its type.
	if _, ok := err.(tls.RecordHeaderError); ok {
		return nil, err
	}
	proxy, proxyErr := t.Proxy(req)
	if proxyErr != nil {
		return nil, err
	}
	route := "direct"
	if proxy != nil {
		route = "via proxy " + proxy.Host
	}
	return nil, routedError{err: err, route: route}
}

// routedError is a transport error with its route. url.Error asks the error
// it wraps for Timeout and Temporary directly, without unwrapping, so they
// are passed through for the retry and source-switch decisions.
type routedError struct {
	err   error
	route string
}

func (e routedError) Error() string { return fmt.Sprintf("%v (%s)", e.err, e.route) }
func (e routedError) Unwrap() error { return e.err }

func (e routedError) Timeout() bool {
	t, ok := e.err.(interface{ Timeout() bool })
	return ok && t.Timeout()
}

func (e routedError) Temporary() bool {
	t, ok := e.err.(interface{ Temporary() bool })
	return ok && t.Temporary()
}
