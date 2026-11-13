package provision

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/launchd"
)

// UninstallAgent uses SSH so stopping the agent does not interrupt its reply.
func (i *SSHInstaller) UninstallAgent(ctx context.Context, req SSHProbeRequest) error {
	ctx, cancelInspect := context.WithTimeout(ctx, i.timeout())
	defer cancelInspect()
	req, err := NormalizeSSHProbeRequest(req)
	if err != nil {
		return err
	}
	if trimServer(req.ServerURL) == "" && strings.TrimSpace(req.Token) == "" {
		return fmt.Errorf("the node's server URL or token is required to verify agent ownership")
	}
	client, _, err := DialSSH(ctx, req.User, req.Host, req.Port, req.SSHAuth, i.timeout(), i.HostKeys)
	if err != nil {
		return &SSHConnectionError{Err: err}
	}
	defer client.Close()
	sudo := ""
	if needsSudo(ctx, client) {
		sudo = "sudo -n "
	}
	inspect := agentUninstallInspectCommand(req.ServerURL, req.Token)
	// Expand HOME before sudo changes it, so a user's LaunchAgent is visible.
	inspectCommand := sudo + "sh -s -- \"$HOME\""
	out, err := runWithInput(ctx, client, inspectCommand, inspect)
	if err != nil {
		return fmt.Errorf("locate the node's agent installation: %s: %w", strings.TrimSpace(out), err)
	}
	installation, err := parseAgentUninstallInstallation(out, req.ServerURL)
	if err != nil {
		return err
	}
	command := sudo + shellQuote(installation.binary) + " uninstall --state " + shellQuote(installation.state)
	// New agents require explicit unattended confirmation. Probe help without
	// changing the host, while keeping older agents' command syntax intact.
	helpCtx, cancelHelp := context.WithTimeout(ctx, 5*time.Second)
	help, helpErr := runContext(helpCtx, client, sudo+shellQuote(installation.binary)+" uninstall --help")
	cancelHelp()
	if helpErr == nil && strings.Contains(help, "-keep-resources") && strings.Contains(help, "-yes") {
		command += " --yes"
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	uninstallCtx, cancel := boundedDetached(ctx, i.timeout())
	defer cancel()
	if out, err := runContext(uninstallCtx, client, command); err != nil {
		return fmt.Errorf("uninstall agent: %s: %w", strings.TrimSpace(out), err)
	}
	// Older agents could report exit 0 even when stopping failed. Verify both
	// the service definition and running processes through the same SSH session.
	out, err = runWithInput(uninstallCtx, client, inspectCommand, inspect)
	if err != nil {
		return fmt.Errorf("verify agent uninstall: %s: %w", strings.TrimSpace(out), err)
	}
	if strings.TrimSpace(out) != "absent" {
		return fmt.Errorf("agent service or process remains after uninstall")
	}
	return nil
}

type agentUninstallInstallation struct {
	binary string
	state  string
}

// parseAgentUninstallInstallation reads the inspection's three lines: binary,
// state directory, and the server the agent dials, "-" for an agent the
// server dials, which has none (an empty last line would not survive the
// shell's command substitution).
func parseAgentUninstallInstallation(out, server string) (agentUninstallInstallation, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		return agentUninstallInstallation{}, fmt.Errorf("cannot identify a unique agent installation; configure SSH access to the correct node or use explicit force removal")
	}
	if strings.TrimSpace(lines[2]) == "-" {
		lines[2] = ""
	}
	if got := trimServer(lines[2]); got != trimServer(server) && (got != "" || trimServer(server) != "") {
		return agentUninstallInstallation{}, fmt.Errorf("agent service belongs to another server; refusing to uninstall it")
	}
	if !path.IsAbs(lines[0]) || !path.IsAbs(lines[1]) || path.Base(lines[0]) != "gpuworks-agent" {
		return agentUninstallInstallation{}, fmt.Errorf("agent binary and state directory could not be determined from its installation")
	}
	return agentUninstallInstallation{binary: lines[0], state: lines[1]}, nil
}

// Only installation locations and the server URL leave the host. Never print
// a unit or process command line, which can contain the node's token.
//
// With the node's token the installation is the one running with it: a host
// may run several agents (the unit for one node, nohup agents from other
// directories for others), and deleting one node must leave the others
// alone. The script arrives on stdin, so the token is not on a command line.
// Without a token (older records) it is the unit, else this server's agent.
// An agent's server and token are its configuration file's (agentconf), or
// for one installed before the file, its arguments'. A node the server dials
// has no server, and is found by its token.
func agentUninstallInspectCommand(server, token string) string {
	return `set -eu
expected=` + shellQuote(trimServer(server)) + `
token=` + shellQuote(strings.TrimSpace(token)) + `
` + agentconf.ShellFuncs + `
unit_start() {
  systemctl cat gpuworks-agent.service 2>/dev/null | grep '^ExecStart=' | tail -n 1
}
own_unit() {
  [ -z "$token" ] && return 0
  [ "$(gw_line_value "$(unit_start)" TOKEN)" = "$token" ]
}
if command -v systemctl >/dev/null 2>&1 && systemctl cat gpuworks-agent.service >/dev/null 2>&1 && own_unit; then
  start=$(unit_start)
  binary=$(printf '%s\n' "$start" | sed -n 's/^ExecStart=\(.*\) run .*/\1/p')
  state=$(printf '%s\n' "$start" | gw_arg state)
  server=$(gw_line_value "$start" SERVER)
  [ -n "$binary" ] && [ -n "$state" ] && { [ -n "$server" ] || [ -n "$token" ]; } || { echo 'cannot identify the systemd agent installation' >&2; exit 1; }
  printf '%s\n%s\n%s\n' "$binary" "$state" "${server:--}"
  exit 0
fi
found=''
for plist in ` + shellQuote(launchd.DaemonPlist()) + ` "${1:-$HOME}/` + launchd.AgentPlist("") + `"; do
  [ -e "$plist" ] || continue
  [ -z "$found" ] || { echo 'multiple launchd agent installations; refusing to remove both' >&2; exit 1; }
  binary=$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$plist")
  state=$(/usr/libexec/PlistBuddy -c 'Print :WorkingDirectory' "$plist")
  server=''
  index=1
  while argument=$(/usr/libexec/PlistBuddy -c "Print :ProgramArguments:$index" "$plist" 2>/dev/null); do
    index=$((index + 1))
    if [ "$argument" = '--server' ]; then
      server=$(/usr/libexec/PlistBuddy -c "Print :ProgramArguments:$index" "$plist")
      break
    fi
  done
  [ -n "$server" ] || server=$(gw_conf "$state" SERVER)
  found=$(printf '%s\n%s\n%s' "$binary" "$state" "${server:--}")
done
if [ -n "$found" ]; then printf '%s\n' "$found"; exit 0; fi
if [ "$(uname -s)" = Darwin ]; then
  if pgrep -x gpuworks-agent >/dev/null 2>&1; then
    echo 'agent process remains without an identifiable launchd service' >&2
    exit 1
  else
    status=$?
    [ "$status" = 1 ] || exit "$status"
  fi
  echo absent
  exit 0
fi
# Shortlist with one binary-safe scan. A match only selects candidates; argv
# below still verifies the binary, action, server, and installation identity.
# Processes can disappear during the scan (grep status 2). Tool launch failures
# must still fail verification rather than claim that the agent is absent.
if candidates=$(grep -l '/gpuworks-agent' /proc/[0-9]*/cmdline 2>/dev/null); then
  :
else
  status=$?
  [ "$status" -le 2 ] || { echo 'cannot inspect running agent processes' >&2; exit "$status"; }
fi
printf '%s\n' "$candidates" | {
found=''
while IFS= read -r cmdline; do
  [ -r "$cmdline" ] || continue
  binary=$(tr '\000' '\n' 2>/dev/null < "$cmdline" | sed -n '1p') || continue
  case "$binary" in */gpuworks-agent) ;; *) continue ;; esac
  action=$(tr '\000' '\n' 2>/dev/null < "$cmdline" | sed -n '2p') || continue
  [ "$action" = 'run' ] || continue
  state=$(tr '\000' '\n' 2>/dev/null < "$cmdline" | awk 'previous == "--state" {print; exit} /^--state=/ {sub(/^--state=/, ""); print; exit} {previous=$0}') || continue
  server=$(tr '\000' '\n' 2>/dev/null < "$cmdline" | awk 'previous == "--server" {print; exit} /^--server=/ {sub(/^--server=/, ""); print; exit} {previous=$0}') || continue
  running=$(tr '\000' '\n' 2>/dev/null < "$cmdline" | awk 'previous == "--token" {print; exit} /^--token=/ {sub(/^--token=/, ""); print; exit} {previous=$0}') || continue
  if [ -n "$state" ] && [ -e "$state/` + agentconf.FileName + `" ]; then
    server=$(gw_conf "$state" SERVER)
    running=$(gw_conf "$state" TOKEN)
  fi
  if [ -n "$token" ]; then
    [ "$running" = "$token" ] || continue
  else
    [ -n "$expected" ] && [ "${server%/}" = "$expected" ] || continue
  fi
  [ -n "$state" ] || { echo 'cannot determine the running agent state directory' >&2; exit 1; }
  installation=$(printf '%s\n%s\n%s' "$binary" "$state" "${server:--}")
  if [ -n "$found" ] && [ "$found" != "$installation" ]; then echo 'multiple agent installations match this server' >&2; exit 1; fi
  found=$installation
done
if [ -n "$found" ]; then printf '%s\n' "$found"; else echo absent; fi
}`
}
