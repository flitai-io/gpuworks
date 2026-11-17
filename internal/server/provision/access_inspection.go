package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// AccessInspection is what one SSH inspection of a node found. SSH,
// Installation and Process are check statuses (proto.CheckPass, CheckFail,
// CheckSkip); Code says why the stages that did not pass stand as they do
// (messages.Doctor*). Connection is the node-side HTTP probe's raw result;
// ConnectionStatus and ConnectionCode are its check result, set when the probe
// ran (empty otherwise).
type AccessInspection struct {
	SSH              string
	Installation     string
	Process          string
	Code             string
	ObservedAt       time.Time
	Connection       ConnectionCheck
	ConnectionStatus string
	ConnectionCode   string
}

// NewAccessInspection is an inspection that has not run: every stage skipped
// for code.
func NewAccessInspection(code string) AccessInspection {
	return AccessInspection{SSH: proto.CheckSkip, Installation: proto.CheckSkip, Process: proto.CheckSkip, Code: code}
}

// inspectionContextCode is why an inspection the context cut short stands
// unfinished, or fallback when the context is still live.
func inspectionContextCode(ctx context.Context, fallback string) string {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return messages.DoctorCancelled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return messages.DoctorTimeout
	}
	return fallback
}

// The node-side probe's outcomes, as the remote script and
// parseConnectionDiagnostic word them (ConnectionCheck.Outcome).
const (
	probeHTTPOK             = "http_ok"
	probeInvalidAddress     = "invalid_address"
	probeIdentityUnverified = "identity_unverified"
	probeNotChecked         = "not_checked"
	probeNotApplicable      = "not_applicable"
	probeUnverifiedIdentity = "unverified_identity"
)

// probeResults are the check result of each probe outcome. An outcome not
// listed did not produce a result the platform understands: skipped.
var probeResults = map[string]struct{ status, code string }{
	probeHTTPOK:             {proto.CheckPass, messages.DoctorHTTPConnected},
	"dns":                   {proto.CheckFail, messages.DoctorDNS},
	"connect":               {proto.CheckFail, messages.DoctorConnect},
	"tls":                   {proto.CheckFail, messages.DoctorTLS},
	"timeout":               {proto.CheckFail, messages.DoctorTimeout},
	"http_error":            {proto.CheckFail, messages.DoctorHTTPError},
	"transport":             {proto.CheckFail, messages.DoctorTransport},
	"tool_missing":          {proto.CheckSkip, messages.DoctorToolMissing},
	probeIdentityUnverified: {proto.CheckSkip, messages.DoctorIdentityUnverified},
	"cancelled":             {proto.CheckSkip, messages.DoctorCancelled},
	probeInvalidAddress:     {proto.CheckSkip, messages.DoctorInvalidAddress},
}

// WithConnectionResult is the inspection with the probe's outcome turned into
// its check result (ConnectionStatus, ConnectionCode), as InspectAccess
// returns it.
func (a AccessInspection) WithConnectionResult() AccessInspection {
	a.setConnectionResult()
	return a
}

func (a *AccessInspection) setConnectionResult() {
	if a.Connection.Outcome == "" {
		a.ConnectionStatus, a.ConnectionCode = "", ""
		return
	}
	r, ok := probeResults[a.Connection.Outcome]
	if !ok {
		r.status, r.code = proto.CheckSkip, messages.DoctorInspectionUnavailable
	}
	a.ConnectionStatus, a.ConnectionCode = r.status, r.code
}

// InspectAccess performs read-only checks for this request. It neither reads
// historical connection records nor changes the existing DiagnoseConnection API.
func (i *SSHInstaller) InspectAccess(ctx context.Context, req SSHProbeRequest) AccessInspection {
	out := NewAccessInspection(messages.DoctorCredentialsUnavailable)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	normalized, err := NormalizeSSHProbeRequest(req)
	if err != nil {
		return out
	}
	client, _, err := DialSSH(ctx, normalized.User, normalized.Host, normalized.Port, normalized.SSHAuth, i.timeout(), i.HostKeys)
	out.ObservedAt = time.Now().UTC()
	if err != nil {
		// The SSH adapter names how the connection failed (SSH_AUTH_FAILED,
		// SSH_HOST_KEY_MISMATCH, ...); SSH_UNAVAILABLE is left for a failure
		// it could not name. A context that ended first is not a failure.
		code, named := FailureCode(err)
		if !named {
			code = messages.DoctorSSHUnavailable
		}
		out.SSH, out.Code = proto.CheckFail, inspectionContextCode(ctx, code)
		if ctx.Err() != nil {
			out.SSH = proto.CheckSkip
		}
		return out
	}
	defer client.Close()
	// This inspection owns the connection. Closing the transport on timeout
	// also interrupts NewSession, before the command's cancellable wait starts.
	stopClose := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClose()
	out.SSH, out.Code = proto.CheckPass, messages.DoctorInspectionUnavailable
	if strings.TrimSpace(req.Token) == "" || strings.ContainsAny(req.Token, "\r\n") {
		out.Code = messages.DoctorIdentityUnverified
		return out
	}
	// A malformed target must not prevent installation/process inspection.
	mode := req.ConnectMode
	invalidAddress := mode != agentconf.ServerDial && agentconf.ValidateServer(req.ServerURL) != nil
	if invalidAddress {
		mode = agentconf.ServerDial // The existing command skips its HTTP probe.
	}
	command := accessInspectionCommand(installDir(req.InstallDir), req.ServerURL, mode)
	raw, err := runDiagnosticWithInput(ctx, client, command, req.Token)
	out.ObservedAt = time.Now().UTC()
	if err != nil {
		out.Code = inspectionContextCode(ctx, messages.DoctorInspectionUnavailable)
	}
	out = parseAccessInspection(raw, out)
	if req.ConnectMode != agentconf.ServerDial {
		if invalidAddress {
			out.Connection = ConnectionCheck{Outcome: probeInvalidAddress}
		} else if out.Connection.Outcome != "" {
			out.Connection.Target = strings.TrimRight(req.ServerURL, "/") + "/healthz"
		}
	}
	out.setConnectionResult()
	return out
}

func accessInspectionCommand(dir, server, mode string) string {
	return "sh -c " + shellQuote(fmt.Sprintf(`set +x
exec 2>/dev/null
IFS= read -r token || exit 0
dir=%s
installed=unavailable
if [ -d "$dir" ] && [ -r "$dir" ] && [ -x "$dir" ]; then
  if [ -f "$dir/gpuworks-agent" ]; then installed=passed
  elif [ ! -e "$dir/gpuworks-agent" ]; then installed=failed; fi
elif [ ! -e "$dir" ] && [ -r "$(dirname "$dir")" ] && [ -x "$(dirname "$dir")" ]; then installed=failed
fi
printf 'GW_INSTALL %%s\n' "$installed"
process=unavailable
if state=$(%s); then
  case "$state" in
    systemd|process) process=passed ;;
    not-running) process=failed ;;
  esac
fi
printf 'GW_PROCESS %%s\n' "$process"
printf '%%s\n' "$token" | %s
`, shellQuote(dir), agentProcessProbeCommand(dir, ""), connectionDiagnosticCommand(dir, server, mode)))
}

func parseAccessInspection(raw string, out AccessInspection) AccessInspection {
	if len(raw) > connectionDiagnosticLimit || strings.Contains(raw, "\x00") {
		return out
	}
	// Only newline-terminated observations are complete. A timeout can leave
	// any prefix of the protocol, including a partially copied final line.
	lines := strings.Split(raw, "\n")
	lines = lines[:len(lines)-1]
	prefixes := []string{"GW_INSTALL ", "GW_PROCESS ", "GW_CONFIG ", "GW_ADDRESS ", "GW_HEALTH "}
	if len(lines) == 0 || len(lines) > len(prefixes) {
		return out
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, prefixes[i]) {
			return out
		}
	}
	// The remote script's words (accessInspectionCommand) for a stage.
	status := func(value string) string {
		switch value {
		case "passed":
			return proto.CheckPass
		case "failed":
			return proto.CheckFail
		}
		return proto.CheckSkip
	}
	parsed := out
	parsed.Installation = status(strings.TrimPrefix(lines[0], "GW_INSTALL "))
	if len(lines) > 1 {
		parsed.Process = status(strings.TrimPrefix(lines[1], "GW_PROCESS "))
	}
	if len(lines) < len(prefixes) {
		return parsed
	}
	configuration, health, ok := parseConnectionDiagnostic(strings.Join(lines[2:], "\n"))
	if !ok {
		return out
	}
	parsed.Connection = health
	if health.Outcome == probeNotChecked && configuration.Outcome == probeUnverifiedIdentity {
		parsed.Connection.Outcome = probeIdentityUnverified
	}
	if health.Outcome != probeNotChecked && health.Outcome != probeNotApplicable {
		parsed.Connection.ObservedAt = out.ObservedAt
	}
	return parsed
}
