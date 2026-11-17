package aihelper

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/pkg/httpproxy"
)

// ValidateProxyURL reports whether raw is a custom proxy the outbound
// transport can use: http, https, socks5 or socks5h with a host and no path.
// The error never repeats raw, which may carry a password.
func ValidateProxyURL(raw string) error { return httpproxy.Validate(raw) }

// ProxyMode is how calls to an AI service leave the server.
type ProxyMode string

const (
	// ProxyManagement follows the management node's outbound proxy, the one
	// image catalog and metadata requests use; with none chosen, the server
	// process's proxy environment. It is the default, and what a service
	// stored before the choice existed reads as.
	ProxyManagement ProxyMode = "management"
	// ProxyDirect calls the service without any proxy, the environment's
	// included.
	ProxyDirect ProxyMode = "direct"
	// ProxyCustom calls the service through a proxy URL of its own.
	ProxyCustom ProxyMode = "custom"
)

// ProxyModes is every mode, in the order the console offers them.
var ProxyModes = []ProxyMode{ProxyManagement, ProxyDirect, ProxyCustom}

// KnownProxyMode reports whether mode is one of ProxyModes.
func KnownProxyMode(mode string) bool {
	for _, m := range ProxyModes {
		if string(m) == mode {
			return true
		}
	}
	return false
}

// ProxyRoute answers the route of each call to a service: management is read
// per call, so a change to the management node's proxy reaches a running
// assistant without a restart. An unknown or empty mode is ProxyManagement.
// customURL is the plaintext proxy URL, used only for ProxyCustom.
func ProxyRoute(mode ProxyMode, customURL string, management func() (proto.NetworkConfig, error)) func() (outbound.Route, error) {
	switch mode {
	case ProxyDirect:
		return func() (outbound.Route, error) { return outbound.Route{Direct: true}, nil }
	case ProxyCustom:
		route := outbound.Route{Proxy: proto.NetworkConfig{HTTPProxy: customURL, HTTPSProxy: customURL}}
		return func() (outbound.Route, error) { return route, nil }
	}
	return func() (outbound.Route, error) {
		if management == nil {
			return outbound.Route{}, nil
		}
		cfg, err := management()
		return outbound.Route{Proxy: cfg}, err
	}
}
