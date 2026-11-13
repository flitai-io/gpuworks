package provision

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/server/installscript"
)

// agentSupervisor is what keeps a Linux agent running; install.sh's
// --supervisor.
type agentSupervisor int

const (
	// supervisorSystemd writes the gpuworks-agent unit when systemd runs and
	// the install may write it, and falls back to nohup otherwise.
	supervisorSystemd agentSupervisor = iota
	// supervisorNohup never installs or touches the unit: the agent runs
	// under nohup with agent-restart.sh as its restart path, beside whatever
	// agents the host already runs from other install directories.
	supervisorNohup
	// supervisorKeep restarts or reinstalls the way the host already runs the
	// agent: a nohup install (agent-restart.sh without a unit) stays nohup,
	// anything else is supervisorSystemd.
	supervisorKeep
)

func (s agentSupervisor) flag() string {
	switch s {
	case supervisorNohup:
		return "nohup"
	case supervisorKeep:
		return "keep"
	default:
		return "systemd"
	}
}

// installSupervisor is the supervisor an install request asks for.
func installSupervisor(noSystemd bool) agentSupervisor {
	if noSystemd {
		return supervisorNohup
	}
	return supervisorSystemd
}

// scriptInstall is one run of install.sh on a Linux host over SSH, with a
// binary already uploaded into the install directory. install.sh is the one
// place a node's configuration is recorded and its service registered (the
// Mac SSH install, the curl command and the offline package run it too).
type scriptInstall struct {
	installDir string
	conf       agentconf.Config
	supervisor agentSupervisor
	// noSudo installs with the SSH user's own rights although sudo works:
	// sudo could not claim the requested directory and a writable one is
	// used instead.
	noSudo bool
}

// args are install.sh's arguments. Flags stay unquoted (they are constants):
// the command log redacts the value after a bare --token.
func (s scriptInstall) args() string {
	conf := s.conf.Normalized()
	pairs := [][2]string{
		{"--install-dir", s.installDir},
		{"--token", conf.Token},
		{"--connect-mode", conf.ConnectMode},
		{"--supervisor", s.supervisor.flag()},
	}
	if conf.Server != "" {
		pairs = append(pairs, [2]string{"--server", conf.Server})
	}
	if conf.ServerDials() {
		pairs = append(pairs, [2]string{"--listen", conf.Listen})
	}
	if conf.RuntimeDriver != "" {
		pairs = append(pairs, [2]string{"--driver", conf.RuntimeDriver})
	}
	if conf.UdockerBundle != "" {
		pairs = append(pairs, [2]string{"--udocker-bundle", conf.UdockerBundle})
	}
	out := make([]string, 0, 2*len(pairs)+1)
	for _, p := range pairs {
		out = append(out, p[0], shellQuote(p[1]))
	}
	if s.noSudo {
		out = append(out, "--no-sudo")
	}
	return strings.Join(out, " ")
}

// runInstallScript uploads install.sh into the install directory, runs it
// with the agent binary already there, and removes it again. The output
// ends with how the agent was started ("started via systemd" or "started via
// nohup pid N").
func (i *SSHInstaller) runInstallScript(ctx context.Context, client *ssh.Client, s scriptInstall) (string, error) {
	if err := s.conf.Validate(); err != nil {
		return "", err
	}
	dir := strings.TrimRight(s.installDir, "/")
	script := fmt.Sprintf("%s/.gpuworks-install.%d.sh", dir, time.Now().UnixNano())
	if err := uploadDataContext(ctx, client, script, []byte(installscript.Script), false, i.uploadTimeout()); err != nil {
		return "", fmt.Errorf("upload install.sh: %w", err)
	}
	defer func() { _, _ = run(context.WithoutCancel(ctx), client, "rm -f "+shellQuote(script)) }()
	cmd := "env GPUWORKS_AGENT_BINARY=" + shellQuote(dir+"/gpuworks-agent") + " sh " + shellQuote(script) + " " + s.args()
	return run(ctx, client, cmd)
}

// agentStateProbeCommand succeeds when an agent runs with installDir as its
// state directory: the active gpuworks-agent unit's, or a process's
// (`<dir>/gpuworks-agent run --state <dir>`, as install.sh starts it).
func agentStateProbeCommand(installDir string) string {
	dir := strings.TrimRight(installDir, "/")
	return fmt.Sprintf(`set +e
dir=%s
`+agentconf.ShellFuncs+`
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet gpuworks-agent 2>/dev/null && [ "$(gw_unit_state)" = "$dir" ]; then echo systemd; exit 0; fi
want="$dir/gpuworks-agent run --state $dir"
# A ps without -o (busybox) prints PID and state before the command line.
if { ps axww -o args= 2>/dev/null || ps axww 2>/dev/null; } | awk -v want="$want" 'length($0) >= length(want) && substr($0, length($0) - length(want) + 1) == want { found=1 } END { exit !found }'; then echo process; exit 0; fi
echo "no agent runs with state directory $dir"; exit 1`, shellQuote(dir))
}

// verifyAgentCommand checks that the agent install.sh started is running.
func verifyAgentCommand(ctx context.Context, client *ssh.Client, installDir string) CheckResult {
	out, err := run(ctx, client, agentStateProbeCommand(installDir))
	if err != nil {
		return CheckResult{Name: "Agent process", Status: "warn", Detail: strings.TrimSpace(out) + "; unit may need daemon-reload"}
	}
	return CheckResult{Name: "Agent process", Status: "ok", Detail: "running (" + strings.TrimSpace(out) + ") with state directory " + strings.TrimRight(installDir, "/")}
}
