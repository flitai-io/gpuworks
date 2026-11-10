package provision

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"gpuworks.ai/gpuworks/internal/agentconf"
)

// SSHProbeRequest contains both the write-only SSH authentication material and
// the persisted agent launch settings needed by a rootless restart fallback.
type SSHProbeRequest struct {
	Target string
	Host   string
	Port   int
	User   string
	SSHAuth
	InstallDir       string
	ServerURL        string
	Token            string
	ConnectMode      string
	RuntimeDriver    string
	ServerDialListen string
	UdockerBundle    string
}

type ProbeResult struct {
	Reachable      bool `json:"reachable"`
	AgentRunning   bool `json:"agent_running"`
	AgentInstalled bool `json:"agent_installed"`
	// ForeignServerURL reports a server mismatch in this node's active unit.
	// Units belonging to other nodes do not describe this node's state.
	ForeignServerURL string `json:"foreign_server_url,omitempty"`
	Detail           string `json:"detail,omitempty"`
	Err              error  `json:"-"`
}

var defaultRecoverySSH = NewSSHInstaller(nil)

// Probe is a convenience entry point for callers that do not need to share a
// persistent TOFU host-key store. The server app uses SSHInstaller.Probe so its
// install, probe, restart, and reinstall paths all share one store.
func Probe(ctx context.Context, req SSHProbeRequest) ProbeResult {
	return defaultRecoverySSH.Probe(ctx, req)
}

func RestartAgent(ctx context.Context, req SSHProbeRequest) error {
	return defaultRecoverySSH.RestartAgent(ctx, req)
}

func ValidateSSHProbeRequest(req SSHProbeRequest) error {
	_, err := NormalizeSSHProbeRequest(req)
	return err
}

// NormalizeSSHProbeRequest validates the endpoint and authentication material,
// then returns a canonical host/user/port tuple suitable for persistence.
func NormalizeSSHProbeRequest(req SSHProbeRequest) (SSHProbeRequest, error) {
	user, host, port, err := probeEndpoint(req)
	if err != nil {
		return SSHProbeRequest{}, err
	}
	if err := req.SSHAuth.Validate(); err != nil {
		return SSHProbeRequest{}, err
	}
	req.Target = ""
	req.Host, req.Port, req.User = host, port, user
	return req, nil
}

func (i *SSHInstaller) Probe(ctx context.Context, req SSHProbeRequest) ProbeResult {
	user, host, port, err := probeEndpoint(req)
	if err != nil {
		return ProbeResult{Detail: err.Error(), Err: err}
	}
	if err := req.SSHAuth.Validate(); err != nil {
		return ProbeResult{Detail: err.Error(), Err: err}
	}
	client, _, err := DialSSH(ctx, user, host, port, req.SSHAuth, i.timeout(), i.HostKeys)
	if err != nil {
		err = &SSHConnectionError{Err: err}
		return ProbeResult{Detail: err.Error(), Err: err}
	}
	defer client.Close()

	dir := installDir(req.InstallDir)
	installedOut, installedErr := runContext(ctx, client, "test -x "+shellQuote(dir+"/gpuworks-agent")+" && echo installed || echo missing")
	if installedErr != nil {
		err := fmt.Errorf("probe agent installation: %w", installedErr)
		return ProbeResult{Reachable: true, Detail: err.Error(), Err: err}
	}
	runningOut, runningErr := runContext(ctx, client, agentRecoveryProbeCommand(dir, req.ServerURL, req.Token))
	if runningErr != nil {
		err := fmt.Errorf("probe agent process: %w", runningErr)
		return ProbeResult{Reachable: true, AgentInstalled: strings.Contains(installedOut, "installed"), Detail: err.Error(), Err: err}
	}
	detail := strings.TrimSpace(runningOut)
	result := ProbeResult{Reachable: true, AgentInstalled: strings.Contains(installedOut, "installed"), Detail: detail}
	switch state, rest, _ := strings.Cut(detail, " "); state {
	case "systemd", "process":
		result.AgentRunning = true
	case "foreign-systemd":
		result.ForeignServerURL = strings.TrimSpace(rest)
	}
	if result.Detail == "" {
		result.Detail = "agent process state unavailable"
	}
	return result
}

// AuthorizePlatformKey logs in with req's credentials and adds the platform
// key to the target's authorized_keys, so the credentials need not be kept.
func (i *SSHInstaller) AuthorizePlatformKey(ctx context.Context, req SSHProbeRequest) (CheckResult, bool) {
	user, host, port, err := probeEndpoint(req)
	if err != nil {
		return CheckResult{Name: "Platform key", Status: "warn", Detail: err.Error()}, false
	}
	client, outcome, err := DialSSH(ctx, user, host, port, req.SSHAuth, i.timeout(), i.HostKeys)
	if err != nil {
		return CheckResult{Name: "Platform key", Status: "warn", Detail: "could not log in to add the platform key: " + err.Error()}, false
	}
	defer client.Close()
	return AuthorizePlatformKey(ctx, client, req.SSHAuth, outcome)
}

func (i *SSHInstaller) RestartAgent(ctx context.Context, req SSHProbeRequest) error {
	user, host, port, err := probeEndpoint(req)
	if err != nil {
		return err
	}
	if err := req.SSHAuth.Validate(); err != nil {
		return err
	}
	client, _, err := DialSSH(ctx, user, host, port, req.SSHAuth, i.timeout(), i.HostKeys)
	if err != nil {
		return &SSHConnectionError{Err: err}
	}
	defer client.Close()

	if remoteOS(ctx, client) == "darwin" {
		return i.restartMacAgent(ctx, client, req.ServerURL)
	}
	dir := installDir(req.InstallDir)
	// Prefer the existing unit: it preserves the exact arguments and environment
	// used by the original install. Rootless/no-systemd nodes fall back to the
	// same start command used by InstallSSH, kept on nohup.
	unit := `command -v systemctl >/dev/null 2>&1 && systemctl cat gpuworks-agent >/dev/null 2>&1`
	if _, unitErr := runContext(ctx, client, unit); unitErr == nil && i.unitIsThisNodes(ctx, client, req.Token) {
		// The unit name is shared by every install; one written for another
		// server would only bring that server's agent back. Fail so the
		// caller reinstalls the unit for this node instead.
		if foreign, err := foreignUnitServer(ctx, client, req.ServerURL); err != nil {
			return err
		} else if foreign != "" {
			return fmt.Errorf("gpuworks-agent unit is installed for another server (%s); not restarting it", foreign)
		}
		command := "systemctl restart gpuworks-agent"
		if needsSudo(ctx, client) {
			command = "sudo -n " + command
		}
		// Point of no return: the restart command below is the mutating
		// action. Do not let cancellation (server shutdown, caller
		// disconnect) tear down the SSH session mid-restart - that can leave
		// the unit stopped rather than restarted.
		restartCtx, cancelRestart := boundedDetached(ctx, i.timeout())
		defer cancelRestart()
		if out, err := runContext(restartCtx, client, command); err != nil {
			return fmt.Errorf("restart gpuworks-agent service: %s: %w", strings.TrimSpace(out), err)
		}
		return nil
	}
	conf := agentconf.Config{Server: req.ServerURL, Token: req.Token, ConnectMode: req.ConnectMode, RuntimeDriver: req.RuntimeDriver, UdockerBundle: req.UdockerBundle}
	if conf.ServerDials() {
		conf.Listen = serverDialListen(req.ServerDialListen)
	}
	if err := conf.Validate(); err != nil {
		return fmt.Errorf("rootless agent restart needs the node's persisted configuration: %w", err)
	}
	if out, err := runContext(ctx, client, "test -x "+shellQuote(dir+"/gpuworks-agent")); err != nil {
		return fmt.Errorf("agent binary is not installed: %s: %w", strings.TrimSpace(out), err)
	}
	// Point of no return: same reasoning as the systemd branch above - the
	// install script is the mutating action and must run to completion once
	// issued. It rewrites the configuration from what the server persisted
	// and starts the agent the way the host already ran it.
	startCtx, cancelStart := boundedDetached(ctx, i.timeout()+i.uploadTimeout())
	defer cancelStart()
	if out, err := i.runInstallScript(startCtx, client, scriptInstall{installDir: dir, conf: conf, supervisor: supervisorKeep, noSudo: !needsSudo(ctx, client)}); err != nil {
		return fmt.Errorf("restart rootless agent: %s: %w", strings.TrimSpace(out), err)
	}
	return nil
}

// unitIsThisNodes reports whether the gpuworks-agent unit runs this node's
// agent. A host may run several agents (one unit, the others under nohup from
// their own directories), and restarting one node must not restart another;
// a unit for another node falls through to this node's nohup restart. Without
// a token to compare the unit is taken as this node's.
func (i *SSHInstaller) unitIsThisNodes(ctx context.Context, client *ssh.Client, token string) bool {
	if strings.TrimSpace(token) == "" {
		return true
	}
	_, err := runContext(ctx, client, agentconf.ShellFuncs+`[ "$(gw_unit_value TOKEN)" = `+shellQuote(strings.TrimSpace(token))+` ]`)
	return err == nil
}

func (i *SSHInstaller) timeout() time.Duration {
	if i != nil && i.Timeout > 0 {
		return i.Timeout
	}
	return 30 * time.Second
}

func probeEndpoint(req SSHProbeRequest) (string, string, int, error) {
	if strings.TrimSpace(req.Target) != "" {
		return ParseTarget(req.Target, req.User, req.Port)
	}
	host := strings.TrimSpace(req.Host)
	user := strings.TrimSpace(req.User)
	port := req.Port
	if port == 0 {
		port = 22
	}
	if host == "" || user == "" || port < 1 || port > 65535 {
		return "", "", 0, fmt.Errorf("ssh host, user, and a valid port are required")
	}
	// Host and port are separate API fields. Reject an embedded port so the
	// persisted endpoint cannot later become a double-joined host:port. Bare or
	// bracketed IPv6 literals remain valid and are normalized without brackets.
	if parsedHost, _, splitErr := net.SplitHostPort(host); splitErr == nil && parsedHost != "" {
		return "", "", 0, fmt.Errorf("ssh host must not include a port")
	}
	if strings.Contains(host, ":") {
		host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
		if net.ParseIP(host) == nil {
			return "", "", 0, fmt.Errorf("ssh host is invalid")
		}
	}
	return user, host, port, nil
}

// unitServerCommand prints the server of the gpuworks-agent unit's agent
// (its configuration file's, or an earlier install's --server argument),
// never anything else of its command line (an earlier install's carries the
// node token).
var unitServerCommand = agentconf.ShellFuncs + `gw_unit_value SERVER`

// agentRecoveryProbeCommand identifies the node by token before reporting
// its service or process state. A shared unit for another node is ignored.
func agentRecoveryProbeCommand(installDir, serverURL, token string) string {
	return "token=" + shellQuote(strings.TrimSpace(token)) + "\n" + agentProcessProbeCommand(installDir, serverURL)
}

// agentProcessProbeCommand reads the node token from its caller's shell scope.
// Diagnostics supply it over stdin instead of embedding it in the SSH command.
func agentProcessProbeCommand(installDir, serverURL string) string {
	needle := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	dir := strings.TrimRight(installDir, "/")
	return fmt.Sprintf(`set +e
want=%s
dir=%s
bin="$dir/gpuworks-agent"
%s
[ -n "$token" ] || { echo 'node token is required to identify the agent' >&2; exit 1; }
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet gpuworks-agent 2>/dev/null; then
  unit=$(gw_unit_start) || { echo 'cannot read the active agent unit' >&2; exit 1; }
  unit_token=$(gw_line_value "$unit" TOKEN)
  if [ "$unit_token" = "$token" ]; then
    srv=$(gw_line_value "$unit" SERVER)
    if [ -n "$want" ] && [ -z "$srv" ]; then echo 'cannot read the target unit server' >&2; exit 1; fi
    if [ -z "$want" ] || [ "${srv%%/}" = "$want" ]; then echo systemd; else echo "foreign-systemd $srv"; fi
    exit 0
  fi
  unit_state=$(printf '%%s\n' "$unit" | gw_arg state)
  if [ "$unit_state" = "$dir" ] || { [ -z "$unit_token" ] && [ -z "$unit_state" ]; }; then
    echo 'cannot verify agent unit ownership for the target directory' >&2; exit 1
  fi
fi
processes=$(ps axww -o args= 2>/dev/null || ps axww 2>/dev/null) || { echo 'cannot list agent processes' >&2; exit 1; }
processes=$(printf '%%s\n' "$processes" | awk -v bin="$bin" '
  { pos=index($0, bin " run "); prefix=substr($0, 1, pos-1) }
  pos && (prefix ~ /^[[:space:]]*$/ || prefix ~ /^[[:space:]]*[0-9]+[[:space:]][^\/]*[[:space:]]$/) { print substr($0, pos) }
') || { echo 'cannot filter agent processes' >&2; exit 1; }
result=$(printf '%%s\n' "$processes" | while IFS= read -r line; do
  state=$(printf '%%s\n' "$line" | gw_arg state)
  [ "$state" = "$dir" ] || continue
  if [ "$(gw_line_value "$line" TOKEN)" != "$token" ]; then
    echo 'cannot verify agent process ownership for the target directory' >&2; exit 1
  fi
  srv=$(gw_line_value "$line" SERVER)
  if [ -n "$want" ] && [ "${srv%%/}" != "$want" ]; then
    echo 'target agent process has an unexpected server' >&2; exit 1
  fi
  echo process; break
done) || exit 1
if [ "$result" = process ]; then echo process; else echo not-running; fi`, shellQuote(needle), shellQuote(dir), agentconf.ShellFuncs)
}

// foreignUnitServer returns the server the gpuworks-agent unit was installed
// for when it differs from serverURL, or "" when it matches or is unknown.
func foreignUnitServer(ctx context.Context, client *ssh.Client, serverURL string) (string, error) {
	needle := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	if needle == "" {
		return "", nil
	}
	out, err := runContext(ctx, client, unitServerCommand)
	if err != nil {
		return "", fmt.Errorf("read gpuworks-agent unit: %s: %w", strings.TrimSpace(out), err)
	}
	got := strings.TrimRight(strings.TrimSpace(out), "/")
	if got == "" || got == needle {
		return "", nil
	}
	return got, nil
}
