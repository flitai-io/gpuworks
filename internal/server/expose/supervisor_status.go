package expose

import (
	"errors"
	"fmt"
	"net"
	"time"
)

func (s *Supervisor) writeStatus() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeStatusLocked()
}

func (s *Supervisor) writeStatusLocked() {
	st := &Status{
		Revision:      s.revision,
		Provider:      s.providerID,
		State:         s.state,
		URL:           s.url,
		LocalOrigin:   s.originLocked(),
		Binary:        s.binary,
		BinaryVersion: s.version,
		Edge:          s.edge,
		Protocol:      s.protocol,
		Candidates:    append([]string(nil), s.candidates...),
		Failures:      s.failures,
		LastError:     s.lastErr,
		Message:       s.message,
	}
	if err := s.store.WriteStatus(st); err != nil {
		s.log.Warn("cannot write the exposure observation", "err", err)
	}
}

// OriginFromListenAddr turns the address the server bound into the address the
// connector dials: a wildcard bind becomes loopback, and IPv6 literals keep
// their brackets.

func OriginFromListenAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("cannot use the listen address %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	if port == "" {
		return "", fmt.Errorf("listen address %q has no port", addr)
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// nextNetwork decides whether a different edge setting could plausibly fix
// this failure, and which one. Only a connector that failed to register says
// anything about the edge path, and what it said is classified where its
// output is parsed (edgeTroubleOf). The registration call to
// api.trycloudflare.com is its own network path: no edge option changes it, so
// that error is reported for what it is rather than retried with a knob that
// cannot help.
func nextNetwork(current Network, err error) (Network, bool) {
	var start *connectorStartError
	if !errors.As(err, &start) {
		return current, false
	}
	switch start.trouble {
	case edgeTroubleQUIC:
		return Network{EdgeIPVersion: "4", Protocol: "http2"}, true
	case edgeTroubleIPv6:
		return Network{EdgeIPVersion: "4", Protocol: current.Protocol}, true
	}
	return current, false
}

// connectorStartError is a connector that exited or timed out before it
// registered, with what its last trouble line said about the edge path.
type connectorStartError struct {
	msg     string
	trouble edgeTrouble
}

func (e *connectorStartError) Error() string { return e.msg }

func newConnectorStartError(stream StreamState, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...) + troubleSuffix(stream)
	trouble := edgeTroubleNone
	if n := len(stream.Trouble); n > 0 {
		trouble = edgeTroubleOf(stream.Trouble[n-1])
	}
	return &connectorStartError{msg: msg, trouble: trouble}
}

func describeNetwork(net Network) string {
	switch {
	case net.Protocol == "http2" && net.EdgeIPVersion == "4":
		return "IPv4-only edge connections over HTTP/2"
	case net.Protocol == "http2":
		return "HTTP/2 instead of QUIC"
	case net.EdgeIPVersion == "4":
		return "IPv4-only edge connections"
	default:
		return "the default network settings"
	}
}

func troubleSuffix(stream StreamState) string {
	if len(stream.Trouble) == 0 {
		return ""
	}
	return ": " + stream.Trouble[len(stream.Trouble)-1]
}

// backoffFor is the delay before attempt number failures+1.

func backoffFor(steps []time.Duration, failures int) time.Duration {
	if failures <= 0 || len(steps) == 0 {
		return 0
	}
	if failures > len(steps) {
		return steps[len(steps)-1]
	}
	return steps[failures-1]
}
