package outbound

import (
	"context"
	"errors"
	"net/netip"
	"strings"
)

var ErrInvalidHost = errors.New("source must be an exact ASCII hostname or IP without a URL, port, CIDR or wildcard")

// NormalizeHost accepts only a source host, without URL paths or credentials.
// Permission and DNS checks remain in the transport's policy.
func NormalizeHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if host == "" || len(host) > 253 {
		return "", ErrInvalidHost
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Zone() != "" {
			return "", ErrInvalidHost
		}
		return address.Unmap().String(), nil
	}
	hasName := false
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalidHost
		}
		for _, ch := range label {
			switch {
			case ch >= 'a' && ch <= 'z' || ch == '-':
				hasName = true
			case ch >= '0' && ch <= '9':
			default:
				return "", ErrInvalidHost
			}
		}
	}
	if !hasName {
		return "", ErrInvalidHost
	}
	return host, nil
}

// CheckHost checks current DNS answers against the transport's live policy.
// It does not connect to the host or send an HTTP request.
func (t *SafeTransport) CheckHost(ctx context.Context, host string) error {
	host, err := NormalizeHost(host)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = t.policy.addresses(ctx, host)
	return err
}
