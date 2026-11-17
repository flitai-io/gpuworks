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
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
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

func accessCheck(stage, vantage, code string) DoctorCheck {
	return DoctorCheck{Stage: stage, Vantage: vantage, Status: proto.CheckSkip, Code: code}
}

func (d ConnectionDiagnoser) Diagnose(ctx context.Context, node ConnectionNode, permitted bool) DoctorReport {
	out := DoctorReport{CheckedAt: time.Now().UTC()}
	ssh := accessCheck("ssh", "control_plane", messages.DoctorCredentialsUnavailable)
	installed := accessCheck("installation", "node", messages.DoctorInspectionUnavailable)
	process := accessCheck("process", "node", messages.DoctorInspectionUnavailable)
	connection := accessCheck("connection", "node", messages.DoctorCredentialsUnavailable)
	authentication := accessCheck("authentication", "node", messages.DoctorAuthenticationNotChecked)
	if node.Mode == agentconf.ServerDial {
		connection.Vantage, authentication.Vantage = "control_plane", "control_plane"
		connection.Target = safeDialAddress(node.Address, node.Token)
	} else {
		connection.Target = nodeHTTPProbeTarget(node.ServerURL, node.Token)
	}
	if !permitted {
		for _, c := range []*DoctorCheck{&ssh, &installed, &process, &connection, &authentication} {
			c.Code = messages.DoctorAdministratorRequired
		}
	} else {
		inspection := provision.NewAccessInspection(messages.DoctorCredentialsUnavailable)
		if d.Inspect != nil && ctx.Err() == nil {
			inspection = d.Inspect(ctx, node.UUID)
		}
		ssh.Status, ssh.Code = checkStatus(inspection.SSH), inspection.Code
		installed.Status, process.Status = checkStatus(inspection.Installation), checkStatus(inspection.Process)
		installed.Code, process.Code = inspection.Code, inspection.Code
		for _, c := range []*DoctorCheck{&ssh, &installed, &process} {
			c.Source = "ssh"
			setObserved(c, inspection.ObservedAt)
		}
		if ssh.Status == proto.CheckPass {
			ssh.Code = messages.DoctorSSHConnected
		}
		if installed.Status == proto.CheckPass {
			installed.Code = messages.DoctorInstalled
		} else if installed.Status == proto.CheckFail {
			installed.Code = messages.DoctorNotInstalled
		}
		if process.Status == proto.CheckPass {
			process.Code = messages.DoctorRunning
		} else if process.Status == proto.CheckFail {
			process.Code = messages.DoctorNotRunning
		}
		if node.Mode == agentconf.ServerDial {
			connection = d.probe(ctx, connection)
		} else {
			connection.Code = inspection.Code
			if connection.Target == "" {
				connection.Code = messages.DoctorInvalidAddress
			} else if ssh.Status == proto.CheckPass && inspection.Connection.Target == connection.Target {
				connection.Source = "http_probe"
				setObserved(&connection, inspection.Connection.ObservedAt)
				if inspection.ConnectionStatus != "" {
					connection.Status, connection.Code = inspection.ConnectionStatus, inspection.ConnectionCode
				} else {
					connection.Code = messages.DoctorInspectionUnavailable
				}
			}
		}
		if ctx.Err() != nil {
			code := messages.DoctorCancelled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				code = messages.DoctorTimeout
			}
			for _, c := range []*DoctorCheck{&ssh, &installed, &process, &connection} {
				if c.Status == proto.CheckSkip {
					c.Code = code
				}
			}
		}
	}
	out.Checks = []DoctorCheck{ssh, installed, process, connection, authentication}
	out.Summary = doctorSummary(out.Checks)
	return out
}

// checkStatus is an inspection stage's status, or skip for one the inspection
// did not state.
func checkStatus(s string) string {
	if proto.ValidCheckStatus(s) {
		return s
	}
	return proto.CheckSkip
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
		check.Code = messages.DoctorInvalidAddress
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
		check.Status, check.Code = proto.CheckPass, messages.DoctorTCPConnected
		return check
	}
	check.Status, check.Code = proto.CheckFail, messages.DoctorConnect
	var netErr net.Error
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		check.Status, check.Code = proto.CheckSkip, messages.DoctorCancelled
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		check.Code = messages.DoctorTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		check.Code = messages.DoctorRefused
	}
	return check
}
