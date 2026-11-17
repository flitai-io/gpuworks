package expose

import (
	"io"
	"regexp"
	"strings"
	"sync"
)

// Registration is what a connector reports when it has attached: the address
// the operator will use, and where it attached.
type Registration struct {
	URL      string
	Host     string
	Edge     string
	Protocol string
	EdgeIP   string
}

// quickURLRe matches a quick-tunnel hostname. The subdomain is captured so the
// API host can be rejected: cloudflared's own error output mentions
// "https://api.trycloudflare.com/tunnel", and treating that as the tunnel URL
// is the bug Unsloth fixed in PR #6264 — the first measurement run for this
// feature hit it too.
var quickURLRe = regexp.MustCompile(`https://([a-z0-9][a-z0-9-]*)\.trycloudflare\.com`)

// controlPlaneHosts are hostnames that appear in cloudflared output but never
// name an entry point.
var controlPlaneHosts = map[string]bool{
	"api": true, "update": true, "www": true, "developers": true, "blog": true,
}

var (
	registeredLineRe = regexp.MustCompile(`Registered tunnel connection\b`)
	edgeFieldRe      = regexp.MustCompile(`\blocation=(\S+)`)
	protocolFieldRe  = regexp.MustCompile(`\bprotocol=(\S+)`)
	edgeIPFieldRe    = regexp.MustCompile(`\bip=(\S+)`)
	troubleLineRe    = regexp.MustCompile(`\b(ERR|FTL|WRN)\b`)
)

const maxTroubleLines = 8

// maxPartialLine bounds what is held back while waiting for a newline. Every
// fact parsed from this stream lives on a short log line; a connector that
// emits megabytes without one (a binary dumped into the log, a truncated
// write) must not make the buffer grow without bound — appending each chunk to
// an ever-growing string is quadratic, which is how a log line turns into a
// memory problem.
const maxPartialLine = 64 << 10

// configBodyRe matches the remote configuration cloudflared prints when it
// applies one. That body carries the ingress rules — the hostnames this server
// is published under and the service they point at — and the plan requires it
// never to reach the log. It matches the quoted form (which escapes its inner
// quotes) and a bare object.
var configBodyRe = regexp.MustCompile(`config=(?:"(?:[^"\\]|\\.)*"|\{[^\n]*)`)

// redactConfigBody removes the configuration body from a line, leaving the fact
// that a configuration was applied. Both the log and the parser see only the
// redacted line, so no path can carry it out of the process.
func redactConfigBody(line string) string {
	if !strings.Contains(line, "config=") {
		return line
	}
	return configBodyRe.ReplaceAllString(line, "config=[redacted]")
}

// ConnectorOutput is the server's view of a connector's output: it rotates the
// bytes and reads the facts the supervisor needs out of the same stream. Both
// jobs belong to the server because the server owns the pipe — an external log
// file could not tell a rotation from a truncation.
type ConnectorOutput struct {
	logger io.Writer

	mu    sync.Mutex
	part  string // an incomplete trailing line, for the parser
	held  string // an incomplete trailing line, for the log
	state StreamState
}

// StreamState is what the current attempt's output has said so far.
type StreamState struct {
	URL          string
	Registration *Registration
	Trouble      []string
}

// NewConnectorOutput tees the connector's output into logger and into the
// parser. logger may be nil when only the parse matters.
func NewConnectorOutput(logger io.Writer) *ConnectorOutput {
	return &ConnectorOutput{logger: logger}
}

// Write implements io.Writer for the connector's stdout and stderr.
func (c *ConnectorOutput) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeLog(p)
	c.part += string(p)
	if len(c.part) > maxPartialLine {
		// Keep the tail: a line that long carries no fact worth parsing.
		c.part = c.part[len(c.part)-maxPartialLine:]
	}
	for {
		line, rest, found := strings.Cut(c.part, "\n")
		if !found {
			break
		}
		// Redacted here as well as on the way to the log: a trouble line is
		// kept in the observation and can end up in a status message, which the
		// operator reads and a bug report may quote.
		c.consume(redactConfigBody(strings.TrimRight(line, "\r")))
		c.part = rest
	}
	return len(p), nil
}

// maxLogHoldback bounds how much of an unfinished line the log holds back. A
// line longer than this cannot be one of cloudflared's configuration lines
// (those are orders of magnitude shorter), and holding it back to redact it
// would cost the log the property it is there for: rotating everything the
// connector said.
const maxLogHoldback = 256 << 10

// writeLog mirrors the connector's output into the log one complete line at a
// time, so a configuration body can be removed before any of it is written.
// The line framing is the log's own; the parser keeps its own copy with its own
// cap, because the two want different things from the same bytes.
func (c *ConnectorOutput) writeLog(p []byte) {
	if c.logger == nil {
		return
	}
	c.held += string(p)
	for {
		line, rest, found := strings.Cut(c.held, "\n")
		if !found {
			break
		}
		c.logLine(redactConfigBody(strings.TrimRight(line, "\r")) + "\n")
		c.held = rest
	}
	if len(c.held) > maxLogHoldback {
		c.logLine(c.held)
		c.held = ""
	}
}

// logLine writes one line to the log. A log that cannot be written must not
// stop the connector: the exposure stays observable through the public probe
// even when the log is gone.
func (c *ConnectorOutput) logLine(line string) {
	if _, err := c.logger.Write([]byte(line)); err != nil {
		_ = err
	}
}

// consume folds one complete line into the state.
func (c *ConnectorOutput) consume(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	if m := quickURLRe.FindStringSubmatch(line); m != nil && !controlPlaneHosts[m[1]] {
		c.state.URL = m[0]
	}
	if registeredLineRe.MatchString(line) {
		reg := &Registration{URL: c.state.URL, Host: hostOf(c.state.URL)}
		if m := edgeFieldRe.FindStringSubmatch(line); m != nil {
			reg.Edge = m[1]
		}
		if m := protocolFieldRe.FindStringSubmatch(line); m != nil {
			reg.Protocol = m[1]
		}
		if m := edgeIPFieldRe.FindStringSubmatch(line); m != nil {
			reg.EdgeIP = m[1]
		}
		c.state.Registration = reg
	}
	if troubleLineRe.MatchString(line) {
		c.state.Trouble = append(c.state.Trouble, line)
		if len(c.state.Trouble) > maxTroubleLines {
			c.state.Trouble = c.state.Trouble[len(c.state.Trouble)-maxTroubleLines:]
		}
	}
}

// Snapshot returns what the stream has said so far.
func (c *ConnectorOutput) Snapshot() StreamState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.state
	out.Trouble = append([]string(nil), c.state.Trouble...)
	return out
}

// Reset forgets this attempt's observations. Every fallback starts a new
// connector, and the previous attempt's URL must never be mistaken for the new
// one's — that is the difference between a working link and a 530.
func (c *ConnectorOutput) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.part = ""
	c.held = ""
	c.state = StreamState{}
}

// hintFor turns the trouble lines of a failed attempt into the one sentence an
// operator can act on.
func hintFor(trouble []string) string {
	joined := strings.Join(trouble, "\n")
	switch {
	case strings.Contains(joined, "network is unreachable") && strings.Contains(joined, "api.trycloudflare.com"):
		// The registration call is a different network path from the edge
		// connection, and no edge option changes it: say so instead of
		// pretending a retry with different edge parameters would help.
		return "the registration call to api.trycloudflare.com could not reach the network (an IPv6 address on a host without an IPv6 route does this); check this host's connectivity and DNS"
	case strings.Contains(joined, "Failed to dial to edge with quic") || strings.Contains(joined, "no recent network activity"):
		return "the QUIC path to the Cloudflare edge did not connect; retrying with http2"
	case strings.Contains(strings.ToLower(joined), "proxy"):
		return "a proxy setting is in the way of the connector's control connection"
	}
	return ""
}

// edgeTrouble is what a cloudflared trouble line says about the path to the
// Cloudflare edge, as far as choosing another edge setting goes.
type edgeTrouble int

const (
	edgeTroubleNone edgeTrouble = iota
	// edgeTroubleRegistration: the quick-tunnel registration call to
	// api.trycloudflare.com failed; no edge setting changes that path.
	edgeTroubleRegistration
	// edgeTroubleQUIC: the QUIC (UDP) edge connection did not come up.
	edgeTroubleQUIC
	// edgeTroubleIPv6: the edge address was unreachable, typically an IPv6
	// edge on a host without an IPv6 route.
	edgeTroubleIPv6
)

// edgeTroubleOf classifies one line of cloudflared's own output (external
// text: cloudflared logs, e.g. `ERR Failed to dial to edge with quic: timeout:
// no recent network activity`, `ERR ... dial udp [2606:4700::]:7844: connect:
// network is unreachable`). This parser is the adapter that reads the
// connector's output, so the text is classified here and nowhere else.
func edgeTroubleOf(line string) edgeTrouble {
	text := strings.ToLower(line)
	switch {
	case strings.Contains(text, "api.trycloudflare.com"):
		return edgeTroubleRegistration
	case strings.Contains(text, "quic"), strings.Contains(text, "no recent network activity"):
		return edgeTroubleQUIC
	case strings.Contains(text, "network is unreachable"), strings.Contains(text, "ipv6"):
		return edgeTroubleIPv6
	}
	return edgeTroubleNone
}

func hostOf(rawURL string) string {
	return strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
}
