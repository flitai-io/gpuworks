package provision

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
)

type AccessInspection struct {
	SSH          string
	Installation string
	Process      string
	Reason       string
	ObservedAt   time.Time
	Connection   ConnectionCheck
}

// InspectAccess performs read-only checks for this request. It neither reads
// historical connection records nor changes the existing DiagnoseConnection API.
func (i *SSHInstaller) InspectAccess(ctx context.Context, req SSHProbeRequest) AccessInspection {
	out := AccessInspection{SSH: "unavailable", Installation: "unavailable", Process: "unavailable", Reason: "credentials_unavailable"}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	normalized, err := NormalizeSSHProbeRequest(req)
	if err != nil {
		return out
	}
	client, _, err := DialSSH(ctx, normalized.User, normalized.Host, normalized.Port, normalized.SSHAuth, i.timeout(), i.HostKeys)
	out.ObservedAt = time.Now().UTC()
	if err != nil {
		out.SSH, out.Reason = "failed", diagnosticContextOutcome(ctx, "ssh_unavailable")
		if ctx.Err() != nil {
			out.SSH = "unavailable"
		}
		return out
	}
	defer client.Close()
	// This inspection owns the connection. Closing the transport on timeout
	// also interrupts NewSession, before the command's cancellable wait starts.
	stopClose := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClose()
	out.SSH, out.Reason = "passed", "inspection_unavailable"
	if strings.TrimSpace(req.Token) == "" || strings.ContainsAny(req.Token, "\r\n") {
		out.Reason = "identity_unverified"
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
		out.Reason = diagnosticContextOutcome(ctx, "inspection_unavailable")
	}
	out = parseAccessInspection(raw, out)
	if req.ConnectMode != agentconf.ServerDial {
		if invalidAddress {
			out.Connection = ConnectionCheck{Outcome: "invalid_address"}
		} else if out.Connection.Outcome != "" {
			out.Connection.Target = strings.TrimRight(req.ServerURL, "/") + "/healthz"
		}
	}
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
	status := func(value string) string {
		if value == "passed" || value == "failed" {
			return value
		}
		return "unavailable"
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
	if health.Outcome == "not_checked" && configuration.Outcome == "unverified_identity" {
		parsed.Connection.Outcome = "identity_unverified"
	}
	if health.Outcome != "not_checked" && health.Outcome != "not_applicable" {
		parsed.Connection.ObservedAt = out.ObservedAt
	}
	return parsed
}
