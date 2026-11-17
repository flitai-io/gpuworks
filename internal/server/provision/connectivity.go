package provision

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// DefaultServerDialListen is where a server_dial agent listens unless told
// otherwise (agentconf, the one definition the agent shares).
const DefaultServerDialListen = agentconf.DefaultListen

// connectivityDocs is where an operator reads how to connect a node the server
// and the node cannot reach in either direction. The check only points there:
// NAT gateways, VPC peering and in-VPC proxies do not fit in a check row.
const connectivityDocs = `docs: "Connect nodes" → "Node and server cannot reach each other" (docs/how-to/connect-nodes.md)`

// dialServerToAgent is the probe's dialer (tests replace it: a network
// with a transparent proxy accepts every connection).
var dialServerToAgent = (&net.Dialer{}).DialContext

// ServerToAgentProbe dials, from the server, the address it would dial the
// agent at: host's own port when it names one (a platform's port mapping),
// else the agent's listen port. Nothing listens there before the install, so
// a refused connection is the good answer: the packets reached the host and
// came back, and only the agent is missing. A timeout means a firewall, a
// security group or the absence of a route. A transparent proxy on the
// server's network accepts every connection, so "reached" is the best this
// direction can prove.
func ServerToAgentProbe(ctx context.Context, host, listen string, timeout time.Duration) CheckResult {
	address, err := serverDialProbeAddress(host, listen)
	if err != nil {
		return SkipCheck(CheckIDServerToAgent, messages.CheckNodeAddressInvalid, "there is no valid address to reach the node at", map[string]any{"host": host, "listen": listen}).WithEvidence(err.Error())
	}
	params := map[string]any{"address": address}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dialServerToAgent(dialCtx, "tcp", address)
	switch {
	case err == nil:
		_ = conn.Close()
		return PassCheck(CheckIDServerToAgent, messages.CheckServerReachedNode, fmt.Sprintf("the server reached %s", address), params)
	case errors.Is(err, syscall.ECONNREFUSED):
		return PassCheck(CheckIDServerToAgent, messages.CheckServerReachedNodeNoListener, fmt.Sprintf("the server reached %s (nothing listens there yet)", address), params)
	case isTimeout(err):
		_, port, _ := net.SplitHostPort(address)
		params["timeout"], params["port"] = timeout.String(), port
		return WarnCheck(CheckIDServerToAgent, messages.CheckServerNodeTimeout, fmt.Sprintf("the server cannot reach %s: no answer within %s; check routing, security groups and firewalls, including inbound TCP %s on the node for the server", address, timeout, port), params)
	default:
		return WarnCheck(CheckIDServerToAgent, messages.CheckServerNodeUnreachable, fmt.Sprintf("the server cannot reach %s", address), params).WithEvidence(err.Error())
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout()
}

func serverDialProbeAddress(host, listen string) (string, error) {
	if strings.TrimSpace(host) == "" {
		return "", errors.New("no address to reach the node at")
	}
	if !ValidServerDialListen(listen) {
		return "", fmt.Errorf("invalid listen address %q", listen)
	}
	address := ServerDialAddress(host, listen)
	if address == "" {
		return "", fmt.Errorf("invalid address %q to reach the node at", host)
	}
	return address, nil
}

// ConnectivityVerdict reads the two directions and says what the install can
// rely on: nothing when the chosen mode works, a hint when only the other
// mode would, and a warning naming the docs when neither direction works.
// agentToServer is the "Server reachability" row, nil when it was not run;
// serverToAgent is nil when there was no address to probe.
func ConnectivityVerdict(connectMode string, agentToServer, serverToAgent *CheckResult) *CheckResult {
	a2s := agentToServer != nil && agentToServer.Passed()
	s2a := serverToAgent != nil && serverToAgent.Passed()
	serverDial := strings.TrimSpace(connectMode) == "server_dial"
	switch {
	case serverDial && s2a, !serverDial && a2s:
		return nil
	case serverDial && agentToServer != nil && a2s:
		check := WarnCheck(CheckIDConnectivity, messages.CheckConnectivityUseAgentDial, "the server cannot reach the agent's listen port, but the node reaches the server: choose agent-initiated connection (agent_dial)", nil)
		return &check
	case !serverDial && serverToAgent != nil && s2a:
		check := WarnCheck(CheckIDConnectivity, messages.CheckConnectivityUseServerDial, "the node cannot reach the server, but the server reaches the node: choose server-initiated connection (server_dial), and allow its listen port in the node's firewall", nil)
		return &check
	case agentToServer == nil && serverToAgent == nil:
		return nil
	case serverDial && agentToServer == nil:
		// Only the direction a server-dialed agent needs was checked, and its
		// row already says what drops it (a security group, a firewall):
		// "neither direction works" would claim the other one failed too.
		return nil
	default:
		check := WarnCheck(CheckIDConnectivity, messages.CheckConnectivityNone, "the node and the server cannot reach each other in either direction checked, so the agent will not connect; see "+connectivityDocs, nil)
		return &check
	}
}

// findCheck returns the row with id, or nil.
func findCheck(checks []CheckResult, id string) *CheckResult {
	for i := range checks {
		if checks[i].ID == id {
			return &checks[i]
		}
	}
	return nil
}

// AppendConnectivity adds the server → agent probe and the verdict to a
// precheck report that already holds the node's own checks. host is the
// address the server would dial; empty skips that direction (an SSH proxy
// host, say, is not the node). With agent_dial the probe row is shown only
// when the node cannot reach the server: a node behind NAT that the server
// cannot dial is the normal case for agent_dial, not a warning.
func AppendConnectivity(ctx context.Context, checks []CheckResult, connectMode, host, listen string) []CheckResult {
	agentToServer := findCheck(checks, CheckIDServerReachability)
	var a2s *CheckResult
	if agentToServer != nil {
		copied := *agentToServer
		a2s = &copied
	}
	var s2a *CheckResult
	if strings.TrimSpace(host) != "" {
		probe := ServerToAgentProbe(ctx, host, listen, 3*time.Second)
		s2a = &probe
		if strings.TrimSpace(connectMode) == "server_dial" {
			// The one thing a server-dialed agent needs: an install that
			// cannot be reached would never connect.
			if !probe.Passed() {
				probe.Status = proto.CheckFail
			}
			checks = append(checks, probe)
		} else if a2s == nil || !a2s.Passed() {
			checks = append(checks, probe)
		}
	}
	if verdict := ConnectivityVerdict(connectMode, a2s, s2a); verdict != nil {
		checks = append(checks, *verdict)
	}
	return checks
}
