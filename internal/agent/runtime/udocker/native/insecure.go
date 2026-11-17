package native

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
)

// InsecureRegistriesEnv lists registries the agent may reach over plain HTTP
// or HTTPS without certificate verification, the udocker-driver counterpart
// of dockerd's insecure-registries: comma-separated hosts, host:port pairs or
// CIDRs. Loopback registries are always insecure, as in docker.
const InsecureRegistriesEnv = "GPUWORKS_INSECURE_REGISTRIES"

// isInsecure reports whether host (a registry host, possibly with a port) may
// be used without verified TLS.
func (c *Client) isInsecure(host string) bool {
	if c.Insecure != nil {
		return c.Insecure(host)
	}
	return insecureRegistry(host, os.Getenv(InsecureRegistriesEnv))
}

func insecureRegistry(host, configured string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	ip := net.ParseIP(name)
	if name == "localhost" || (ip != nil && ip.IsLoopback()) {
		return true
	}
	for _, entry := range strings.Split(configured, ",") {
		entry = strings.TrimSpace(entry)
		switch {
		case entry == "":
		case entry == host || entry == name:
			return true
		case strings.Contains(entry, "/"):
			if _, cidr, err := net.ParseCIDR(entry); err == nil && ip != nil && cidr.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// baseURL is scheme://host for a registry. A verified registry is always
// HTTPS. An insecure one is probed once, the way docker does: HTTPS without
// verification first, plain HTTP when the server answers HTTPS with HTTP.
func (c *Client) baseURL(ctx context.Context, host string) string {
	if !c.isInsecure(host) {
		return "https://" + host
	}
	c.mu.Lock()
	scheme, known := c.schemes[host]
	c.mu.Unlock()
	if known {
		return scheme + "://" + host
	}
	scheme = "https"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/v2/", nil)
	if err != nil {
		return scheme + "://" + host
	}
	resp, err := c.clientFor(host).Do(req)
	switch {
	case err == nil:
		resp.Body.Close()
	case errors.Is(err, http.ErrSchemeMismatch):
		scheme = "http"
	default:
		// Unreachable for now: do not remember a guess.
		return scheme + "://" + host
	}
	c.mu.Lock()
	if c.schemes == nil {
		c.schemes = map[string]string{}
	}
	c.schemes[host] = scheme
	c.mu.Unlock()
	return scheme + "://" + host
}

// knownBaseURL is baseURL without probing, for callers that run after the
// manifest resolution has already probed the registry.
func (c *Client) knownBaseURL(host string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if scheme, ok := c.schemes[host]; ok {
		return scheme + "://" + host
	}
	return "https://" + host
}

// clientFor returns the HTTP client for a request to host: the regular one,
// or for an insecure registry a copy that skips certificate verification.
func (c *Client) clientFor(host string) *http.Client {
	if !c.isInsecure(host) {
		return c.HTTP
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.insecureHTTP != nil && c.insecureBase == c.HTTP {
		return c.insecureHTTP
	}
	transport, ok := c.HTTP.Transport.(*http.Transport)
	if !ok || transport == nil {
		transport = http.DefaultTransport.(*http.Transport)
	}
	clone := transport.Clone()
	if clone.TLSClientConfig == nil {
		clone.TLSClientConfig = &tls.Config{}
	}
	clone.TLSClientConfig.InsecureSkipVerify = true // #nosec G402 -- explicitly insecure registry
	client := *c.HTTP
	client.Transport = clone
	c.insecureHTTP, c.insecureBase = &client, c.HTTP
	return c.insecureHTTP
}
