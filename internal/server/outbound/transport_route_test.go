package outbound_test

import (
	"errors"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
)

// A route is read for every request: Direct goes straight to the target even
// with a proxy configured, a proxy is used as written, and a route that cannot
// be read fails the request instead of sending it direct.
func TestTransportWithRouteFollowsTheRouteOfEachRequest(t *testing.T) {
	var route outbound.Route
	var readErr error
	tr := outbound.TransportWithRoute(func() (outbound.Route, error) { return route, readErr }, nil)
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, "https://api.example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}

	route = outbound.Route{Proxy: proto.NetworkConfig{HTTPSProxy: "http://alice:secret@proxy.example:3128"}}
	got, err := tr.Proxy(req)
	if err != nil || got == nil || got.Host != "proxy.example:3128" || got.User.Username() != "alice" {
		t.Fatalf("custom proxy = %v err=%v", got, err)
	}

	route.Direct = true
	if got, err := tr.Proxy(req); err != nil || got != nil {
		t.Fatalf("direct = %v err=%v, want no proxy", got, err)
	}

	route = outbound.Route{Proxy: proto.NetworkConfig{HTTPSProxy: "socks5://proxy.example:1080", NoProxy: "api.example.com"}}
	if got, err := tr.Proxy(req); err != nil || got != nil {
		t.Fatalf("no_proxy match = %v err=%v, want direct", got, err)
	}

	readErr = errors.New("decrypt: bad key")
	got, err = tr.Proxy(req)
	if err == nil || got != nil {
		t.Fatalf("unreadable route = %v err=%v, want a failed request", got, err)
	}
	if err.Error() != "cannot read the outbound proxy" {
		t.Fatalf("error = %q leaks the cause", err)
	}
}
