package diagnostics

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

type ConnectionNode struct {
	UUID      string
	Mode      string
	ServerURL string
	Address   string
	Token     string
}

type ConnectionDiagnoser struct {
	Inspect func(context.Context, string) provision.AccessInspection
	Dial    func(context.Context, string, string) (net.Conn, error)
}

func accessCheck(stage, vantage, reason string) DoctorCheck {
	return DoctorCheck{Stage: stage, Vantage: vantage, Status: "unavailable", Reason: reason}
}

func (d ConnectionDiagnoser) Diagnose(ctx context.Context, node ConnectionNode, permitted bool) DoctorReport {
	out := DoctorReport{CheckedAt: time.Now().UTC(), Summary: "incomplete"}
	ssh := accessCheck("ssh", "control_plane", "credentials_unavailable")
	installed := accessCheck("installation", "node", "inspection_unavailable")
	process := accessCheck("process", "node", "inspection_unavailable")
	connection := accessCheck("connection", "node", "credentials_unavailable")
	authentication := accessCheck("authentication", "node", "authentication_not_checked")
	if node.Mode == agentconf.ServerDial {
		connection.Vantage, authentication.Vantage = "control_plane", "control_plane"
		connection.Target = safeDialAddress(node.Address, node.Token)
	} else {
		connection.Target = nodeHTTPProbeTarget(node.ServerURL, node.Token)
	}
	if !permitted {
		for _, c := range []*DoctorCheck{&ssh, &installed, &process, &connection, &authentication} {
			c.Reason = "administrator_required"
		}
	} else {
		inspection := provision.AccessInspection{Reason: "credentials_unavailable"}
		if d.Inspect != nil && ctx.Err() == nil {
			inspection = d.Inspect(ctx, node.UUID)
		}
		ssh.Status, ssh.Reason = validAccessStatus(inspection.SSH), inspection.Reason
		installed.Status, process.Status = validAccessStatus(inspection.Installation), validAccessStatus(inspection.Process)
		installed.Reason, process.Reason = inspection.Reason, inspection.Reason
		for _, c := range []*DoctorCheck{&ssh, &installed, &process} {
			c.Source = "ssh"
			setObserved(c, inspection.ObservedAt)
		}
		if ssh.Status == "passed" {
			ssh.Reason = "ssh_connected"
		}
		if installed.Status == "passed" {
			installed.Reason = "installed"
		} else if installed.Status == "failed" {
			installed.Reason = "not_installed"
		}
		if process.Status == "passed" {
			process.Reason = "running"
		} else if process.Status == "failed" {
			process.Reason = "not_running"
		}
		if node.Mode == agentconf.ServerDial {
			connection = d.probe(ctx, connection)
		} else {
			connection.Reason = inspection.Reason
			if connection.Target == "" {
				connection.Reason = "invalid_address"
			} else if ssh.Status == "passed" && inspection.Connection.Target == connection.Target {
				connection.Source = "http_probe"
				setObserved(&connection, inspection.Connection.ObservedAt)
				switch outcome := inspection.Connection.Outcome; outcome {
				case "http_ok":
					connection.Status, connection.Reason = "passed", "http_connected"
				case "dns", "connect", "tls", "timeout", "http_error", "transport":
					connection.Status, connection.Reason = "failed", outcome
				case "tool_missing", "identity_unverified", "cancelled":
					connection.Reason = outcome
				default:
					connection.Reason = "inspection_unavailable"
				}
			}
		}
		if ctx.Err() != nil {
			reason := "cancelled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				reason = "timeout"
			}
			for _, c := range []*DoctorCheck{&ssh, &installed, &process, &connection} {
				if c.Status == "unavailable" {
					c.Reason = reason
				}
			}
		}
	}
	out.Checks = []DoctorCheck{ssh, installed, process, connection, authentication}
	out.Summary = doctorSummary(out.Checks)
	return out
}

func validAccessStatus(s string) string {
	if s == "passed" || s == "failed" {
		return s
	}
	return "unavailable"
}

func setObserved(check *DoctorCheck, at time.Time) {
	if !at.IsZero() {
		utc := at.UTC()
		check.ObservedAt = &utc
	}
}

func safeDialAddress(address, token string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" || strings.ContainsAny(address, "\r\n\t /?#@") || (token != "" && strings.Contains(address, token)) {
		return ""
	}
	return address
}

func nodeHTTPProbeTarget(server, token string) string {
	if agentconf.ValidateServer(server) != nil {
		return ""
	}
	u, err := url.Parse(server)
	if err != nil || (token != "" && (strings.Contains(server, token) || strings.Contains(u.Path, token))) {
		return ""
	}
	return strings.TrimRight(server, "/") + "/healthz"
}

func (d ConnectionDiagnoser) probe(ctx context.Context, check DoctorCheck) DoctorCheck {
	if check.Target == "" {
		check.Reason = "invalid_address"
		return check
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dial := d.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", check.Target)
	setObserved(&check, time.Now().UTC())
	check.Source = "tcp_probe"
	if err == nil {
		conn.Close()
		check.Status, check.Reason = "passed", "tcp_connected"
		return check
	}
	check.Status, check.Reason = "failed", "connect"
	var netErr net.Error
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		check.Status, check.Reason = "unavailable", "cancelled"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		check.Reason = "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		check.Reason = "refused"
	}
	return check
}
