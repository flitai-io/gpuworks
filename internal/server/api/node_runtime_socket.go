package api

import (
	"fmt"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// agentStopWait is how long the nohup restart command waits for the old
// agent to exit before it gives up rather than start a second one; a
// variable so a test need not wait that long.
var agentStopWait = 30

// runtimeSocketDenialDTO is the node's socket facts, what they mean, and the
// command that lets its agent in; nil when there are none.
func runtimeSocketDenialDTO(d *proto.RuntimeSocketDenial) *dto.RuntimeSocketDenial {
	if d == nil {
		return nil
	}
	out := &dto.RuntimeSocketDenial{
		Cause:       runtimeSocketCause(*d),
		Socket:      d.Socket,
		Group:       d.Group,
		Account:     d.Account,
		SameNameGID: d.SameNameGID,
		Supervisor:  d.Supervisor,
		Command:     runtimeSocketCommand(*d),
	}
	if d.GID != proto.GIDUnknown {
		gid := d.GID
		out.GID = &gid
	}
	return out
}

// runtimeSocketCause reads the facts: group membership is the cause only
// when the socket's group is known, is not root's, and the agent does not
// hold it already.
func runtimeSocketCause(d proto.RuntimeSocketDenial) string {
	switch {
	case d.GID == proto.GIDUnknown:
		return dto.RuntimeSocketCauseUnknown
	case d.GroupHeld:
		return dto.RuntimeSocketCauseGroupHeld
	case d.GID == 0:
		return dto.RuntimeSocketCauseRootGroup
	case d.RestartPending:
		return dto.RuntimeSocketCauseRestartPending
	}
	return dto.RuntimeSocketCauseNotInGroup
}

// runtimeSocketCommand adds the agent's account to the socket's group and
// restarts the agent, the only way the running process gets the group: its
// supplementary groups are fixed when it starts. gpasswd edits the host's
// own group, which is what the socket names even when a directory service
// has a group of the same name. A systemd agent restarts through its unit;
// a nohup one by runtimeSocketNohupRestart. Without a known supervisor only
// the first line is given, and the notice says how to restart. "" when
// membership is not the cause, or the account or the group's name is
// unknown.
func runtimeSocketCommand(d proto.RuntimeSocketDenial) string {
	cause := runtimeSocketCause(d)
	if d.Account == "" || (cause != dto.RuntimeSocketCauseNotInGroup && cause != dto.RuntimeSocketCauseRestartPending) {
		return ""
	}
	if cause == dto.RuntimeSocketCauseNotInGroup && d.Group == "" {
		return ""
	}
	var lines []string
	if cause == dto.RuntimeSocketCauseNotInGroup {
		lines = append(lines, fmt.Sprintf("sudo gpasswd -a %s %s", shellWord(d.Account), shellWord(d.Group)))
	}
	switch {
	case d.Supervisor == proto.AgentSupervisorSystemd:
		lines = append(lines, "sudo systemctl restart gpuworks-agent")
	case d.Supervisor == proto.AgentSupervisorNohup && filepath.IsAbs(d.StateDir):
		lines = append(lines, fmt.Sprintf("sudo -u %s sh -c %s", shellWord(d.Account), shellSingleQuote(runtimeSocketNohupRestart(filepath.Clean(d.StateDir)))))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// runtimeSocketNohupRestart stops the nohup agent of stateDir and starts it
// again with its agent-restart.sh, which does not stop the old one itself.
// Like the install script's stop_nohup_agent it signals only a process whose
// command line is this agent (a gpuworks-agent binary running `run` with
// --state stateDir), never whatever now holds a pid from gpuworks-agent.pid; it
// looks at every process rather than the pid file, so a stale or missing
// pid file cannot leave a running agent behind. The new agent starts only
// once no such process is left; if one outlives agentStopWait seconds the
// script says so and fails instead of starting a second agent on the same
// state directory.
func runtimeSocketNohupRestart(stateDir string) string {
	match := `NR == 1 { a = ($0 ~ /(^|\/)gpuworks-agent$/) } $0 == "run" { r = 1 } p == "--state" && $0 == s { f = 1 } { p = $0 } END { exit !(a && r && f) }`
	return `s=` + shellWord(stateDir) + `; agents=; ` +
		`for c in /proc/[0-9]*/cmdline; do p=${c#/proc/}; p=${p%/cmdline}; ` +
		`tr "\000" "\n" < "$c" 2>/dev/null | awk -v s="$s" '` + match + `' && agents="$agents $p"; done; ` +
		`[ -z "$agents" ] || kill $agents 2>/dev/null; n=0; ` +
		`for p in $agents; do while kill -0 "$p" 2>/dev/null; do ` +
		`n=$((n+1)); if [ $n -gt ` + fmt.Sprint(agentStopWait) + ` ]; then echo "gpuworks-agent $p did not stop; not starting another" >&2; exit 1; fi; sleep 1; ` +
		`done; done; exec "$s/agent-restart.sh"`
}

// shellWord leaves a plain word as it is and single-quotes anything a shell
// would split or expand.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:@%") == "" {
		return s
	}
	return shellSingleQuote(s)
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
