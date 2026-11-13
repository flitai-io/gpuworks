// Package provision contains node onboarding helpers used by the access wizard.
package provision

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/commandlog"
)

// SSHPrecheckRequest is the API input for a read-only SSH precheck.
type SSHPrecheckRequest struct {
	Target string `json:"target"` // user@host[:port], or host[:port] with User
	User   string `json:"user,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	SSHAuth
	ServerURL        string `json:"server_url,omitempty"`
	ConnectMode      string `json:"connect_mode,omitempty"`
	ServerDialListen string `json:"server_dial_listen,omitempty"`
	InstanceKind     string `json:"instance_kind,omitempty"`
	ProviderKind     string `json:"provider_kind,omitempty"`
	// RuntimeDriver is the runtime the install will name (docker, nerdctl,
	// udocker); empty lets the agent pick, as the wizard does.
	RuntimeDriver string `json:"runtime_driver,omitempty"`
	// InstallDir is the directory the agent will be installed into; empty
	// means the install default. The precheck inspects the filesystem behind it.
	InstallDir string `json:"install_dir,omitempty"`
	// HostAddress is where the server would dial a server_dial agent, when
	// that is not the SSH host (a private IP behind a provider's SSH proxy).
	HostAddress string `json:"host_address,omitempty"`
	// SkipServerToAgent leaves out the server → node probe: the caller knows
	// the SSH host is not the node and has no other address for it.
	SkipServerToAgent bool `json:"-"`
	// ProviderChoosesPort says the install picks a port the provider maps to
	// the instance (Vast, RunPod) and checks the server reaches it there:
	// neither the listen port nor the address is known yet, so neither is
	// probed.
	ProviderChoosesPort bool `json:"-"`
}

// CheckResult is one precheck item.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | error
	Detail string `json:"detail,omitempty"`
}

// SSHPrecheckReport is the structured precheck result.
type SSHPrecheckReport struct {
	Target string        `json:"target"`
	User   string        `json:"user"`
	Host   string        `json:"host"`
	Port   int           `json:"port"`
	Checks []CheckResult `json:"checks"`
}

// Prechecker runs an onboarding precheck.
type Prechecker interface {
	PrecheckSSH(ctx context.Context, req SSHPrecheckRequest) (SSHPrecheckReport, error)
}

// SSHPrechecker runs real SSH commands on a target host.
type SSHPrechecker struct {
	Timeout  time.Duration
	HostKeys HostKeyStore
}

// NewSSHPrechecker builds a prechecker with production defaults.
func NewSSHPrechecker() *SSHPrechecker {
	return &SSHPrechecker{Timeout: 15 * time.Second, HostKeys: newMemoryHostKeyStore()}
}

// PrecheckSSH connects to the target and runs read-only environment checks.
func (p *SSHPrechecker) PrecheckSSH(ctx context.Context, req SSHPrecheckRequest) (SSHPrecheckReport, error) {
	user, host, port, err := ParseTarget(req.Target, req.User, req.Port)
	if err != nil {
		return SSHPrecheckReport{}, err
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	client, outcome, err := DialSSH(ctx, user, host, port, req.SSHAuth, timeout, p.HostKeys)
	if err != nil {
		return SSHPrecheckReport{}, err
	}
	defer client.Close()

	report := SSHPrecheckReport{
		Target: req.Target,
		User:   user,
		Host:   host,
		Port:   port,
	}
	report.Checks = append(report.Checks, outcome.Check())
	if req.PlatformKey != nil && outcome.Used != AuthPlatformKey {
		report.Checks = append(report.Checks, PlatformKeyPresence(ctx, client, req.PlatformKey.PublicKey))
	}
	mac := remoteOS(ctx, client) == "darwin"
	commands := basePrecheckCommands()
	if mac {
		commands = macPrecheckCommands()
	}
	if strings.TrimSpace(req.ConnectMode) == "server_dial" && !req.ProviderChoosesPort {
		commands = append(commands, precheckCommand{name: "Agent listen port", command: agentListenPortCheckCommand(req.ServerDialListen), required: true})
	}
	if !mac {
		commands = append(commands, precheckCommandsForKind(req.InstanceKind)...)
	}
	if strings.TrimSpace(req.ServerURL) != "" && shouldCheckServerReachability(req.ConnectMode) {
		commands = append(commands, precheckCommand{name: checkServerReachability,
			command: fmt.Sprintf(`curl -fsS --connect-timeout 3 %s/healthz`, shellQuote(strings.TrimRight(req.ServerURL, "/"))), required: true})
	}
	report.Checks = append(report.Checks, runPrecheckCommands(ctx, client, commands)...)
	requested := expandRemoteHome(ctx, client, req.InstallDir)
	if mac {
		privilege := macPrivilegeCheck(ctx, client, req.SSHAuth)
		report.Checks = append(report.Checks, privilege)
		admin := privilege.Status == "ok"
		dir := macInstallDir(ctx, client, requested)
		report.Checks = append(report.Checks, ProbeMacInstallDir(ctx, client, dir, admin).Checks...)
	} else {
		report.Checks = append(report.Checks, ProbeInstallDirForRuntime(ctx, client, installDir(requested), req.InstanceKind, req.ProviderKind, req.RuntimeDriver).Checks...)
	}
	report.Checks = append(report.Checks, existingAgentCheck(ctx, client, req.ServerURL))
	if req.ProviderChoosesPort {
		report.Checks = append(report.Checks, CheckResult{Name: checkServerToAgent, Status: "ok",
			Detail: "checked during the install: it picks a free TCP port the provider maps to the instance, then the server connects to it"})
	} else if !req.SkipServerToAgent {
		dialHost := strings.TrimSpace(req.HostAddress)
		if dialHost == "" {
			dialHost = host
		}
		report.Checks = AppendConnectivity(ctx, report.Checks, req.ConnectMode, dialHost, req.ServerDialListen)
	}
	return report, nil
}

func basePrecheckCommands() []precheckCommand {
	return []precheckCommand{
		{name: "OS/Arch", command: `printf '%s %s %s\n' "$(uname -s)" "$(uname -m)" "$(uname -r)"`, required: true},
		{name: "Sudo", command: sudoProbeCommand()},
		// Advisory only: the default runveil launcher needs no Python; only the
		// udocker CLI launcher (GPUWORKS_UDOCKER_LAUNCHER=udocker) does.
		{name: "python3", command: pythonProbeCommand()},
		{name: "GPU", command: gpuProbeCommand(), required: true},
		{name: "FUSE", command: `if test -e /dev/fuse; then echo "/dev/fuse present"; else echo "/dev/fuse not present (optional unless udocker is selected)"; fi`, required: true},
		// Advisory only: Docker opens the devices as root whatever the SSH
		// user can do; only runveil, which the agent takes when Docker
		// cannot serve the cards, opens them as the agent's own user.
		{name: checkDeviceAccess, command: deviceAccessCommand("/dev")},
	}
}

const checkDeviceAccess = "Accelerator device access"

// deviceAccessCommand checks that a non-root agent could open the accelerator
// device nodes under devRoot, as runveil workloads do. A root agent — which
// is what root and passwordless sudo install — gives its workloads the device
// groups itself (a dedicated runtime user with the owning groups attached
// per launch), so it is not asked. Otherwise each device node the user
// cannot both read and write is named with its group, and the remedy is
// adding the user to those groups, which only an administrator can do and
// which takes effect at the next login.
//
// Only the nodes the vendor's workloads are given are checked — the same
// sets the agent binds (agent/gpu, runtime/udocker): a render node on an
// NVIDIA host belongs to its display stack and is never handed to a CUDA
// workload, so it is no reason to warn.
func deviceAccessCommand(devRoot string) string {
	return fmt.Sprintf(`if [ "$(id -u)" = "0" ]; then
  echo "root: the agent gives its workloads the device groups itself"
  exit 0
fi
if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  echo "passwordless sudo: the agent runs as root and gives its workloads the device groups itself"
  exit 0
fi
D=%s
if [ -e "$D/nvidiactl" ]; then
  set -- nvidiactl nvidia-uvm nvidia-uvm-tools nvidia-modeset 'nvidia[0-9]*'
elif [ -e "$D/davinci_manager" ]; then
  set -- 'davinci[0-9]*' davinci_manager devmm_svm hisi_hdc ummu uburma
elif [ -e "$D/alixpu_ctl" ]; then
  set -- alixpu alixpu_ctl 'alixpu_ppu[0-9]*'
elif [ -e "$D/kfd" ] || [ -e "$D/mkfd" ]; then
  set -- kfd mkfd 'dri/card*' 'dri/renderD*'
else
  echo "no accelerator device nodes found"
  exit 0
fi
denied=""
groups=""
found=0
for pattern in "$@"; do
  for d in "$D"/$pattern; do
    { [ -e "$d" ] && [ ! -d "$d" ]; } || continue
    found=$((found + 1))
    if [ -r "$d" ] && [ -w "$d" ]; then
      continue
    fi
    denied="$denied ${d#"$D"/}"
    g="$(stat -c %%G "$d" 2>/dev/null || echo '?')"
    case " $groups " in *" $g "*) ;; *) groups="$groups $g" ;; esac
  done
done
user="$(id -un)"
if [ -z "$denied" ]; then
  echo "$user can read and write $found accelerator device nodes"
  exit 0
fi
groups="$(echo $groups | tr ' ' ',')"
echo "$user cannot read and write:$denied (group $groups); the runveil runtime would fail to open them. Ask an administrator to run: sudo usermod -aG $groups $user (takes effect at the next login)"
exit 1`, shellQuote(devRoot))
}

func pythonProbeCommand() string {
	return `for python_bin in "$(command -v python3 2>/dev/null || true)" /root/miniconda3/bin/python3 /opt/conda/bin/python3; do
  if [ -n "$python_bin" ] && [ -x "$python_bin" ]; then
    "$python_bin" --version
    exit 0
  fi
done
echo "python3 not found in PATH or known Conda locations (needed only for GPUWORKS_UDOCKER_LAUNCHER=udocker)"
exit 1`
}

func runPrecheckCommands(ctx context.Context, client *ssh.Client, commands []precheckCommand) []CheckResult {
	results := make([]CheckResult, len(commands))
	var wg sync.WaitGroup
	wg.Add(len(commands))
	for i, command := range commands {
		go func() {
			defer wg.Done()
			results[i] = checkCommandContext(ctx, client, command.name, command.command, command.required)
		}()
	}
	wg.Wait()
	return results
}

func agentListenPortCheckCommand(listen string) string {
	host, port, err := parseListenAddress(listen)
	if err != nil {
		return fmt.Sprintf("echo %s >&2; exit 1", shellQuote(err.Error()))
	}
	return fmt.Sprintf(`python3 - %s %d <<'PY'
import socket
import sys

host = sys.argv[1]
port = int(sys.argv[2])
family = socket.AF_INET6 if ":" in host else socket.AF_INET
s = socket.socket(family)
s.bind((host, port))
print("available", s.getsockname()[0], s.getsockname()[1])
s.close()
PY`, shellQuote(host), port)
}

func parseListenAddress(listen string) (string, int, error) {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		listen = DefaultServerDialListen
	}
	if !strings.Contains(listen, ":") {
		listen = ":" + listen
	}
	host, rawPort, err := net.SplitHostPort(listen)
	if err != nil {
		return "", 0, fmt.Errorf("invalid agent listen address %q: %w", listen, err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid agent listen port %q", rawPort)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	return host, port, nil
}

type precheckCommand struct {
	name     string
	command  string
	required bool
}

func vmPrecheckCommands() []precheckCommand {
	return []precheckCommand{
		// Docker is preferred on a VM but not required: the agent selects
		// runveil when Docker is absent or, on an NVIDIA node, cannot give
		// containers the cards. Keep both observations visible as warnings so the install
		// wizard explains the selected runtime without rejecting a usable VM.
		{name: "Docker", command: `command -v docker >/dev/null 2>&1 && docker --version || { echo "docker not found; GPUWorks will use udocker"; exit 1; }`},
		{name: "NVIDIA Container Toolkit", command: `command -v nvidia-ctk >/dev/null 2>&1 || { echo "nvidia-container-toolkit not found; required only for the Docker runtime, without it GPUWorks uses runveil (see https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html)"; exit 1; }`},
	}
}

func precheckCommandsForKind(instanceKind string) []precheckCommand {
	if strings.EqualFold(strings.TrimSpace(instanceKind), "vm") {
		return vmPrecheckCommands()
	}
	return nil
}

func shouldCheckServerReachability(connectMode string) bool {
	return strings.TrimSpace(connectMode) != "server_dial"
}

func checkCommandContext(ctx context.Context, client *ssh.Client, name, cmd string, required bool) CheckResult {
	out, err := runContext(ctx, client, cmd)
	if err != nil {
		status := "warn"
		if required {
			status = "error"
		}
		detail := strings.TrimSpace(out)
		if detail == "" {
			detail = err.Error()
		} else {
			detail += ": " + err.Error()
		}
		return CheckResult{Name: name, Status: status, Detail: detail}
	}
	return CheckResult{Name: name, Status: "ok", Detail: strings.TrimSpace(out)}
}

// run is runContext's non-batched convenience form: every caller already has
// a ctx in scope (its own method/loop ctx, or the install/recover ctx a few
// frames up), so this just forwards it rather than defaulting to
// context.Background() and silently losing cancellation - the bug this
// signature replaced (see the App.Run shutdown-tracking changes this
// accompanies).
func run(ctx context.Context, client *ssh.Client, cmd string) (string, error) {
	return runContext(ctx, client, cmd)
}

type sshSessionOutput struct {
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func captureSSHSessionOutput(sess *ssh.Session) *sshSessionOutput {
	out := &sshSessionOutput{}
	// x/crypto/ssh copies stdout and stderr concurrently. Pointing both at one
	// bytes.Buffer races and can silently lose short probe output such as
	// "installed" or "systemd", producing a false not-running result.
	sess.Stdout = &out.stdout
	sess.Stderr = &out.stderr
	return out
}

func (o *sshSessionOutput) String() string {
	return o.stdout.String() + o.stderr.String()
}

func runContext(ctx context.Context, client *ssh.Client, cmd string) (string, error) {
	execution := commandlog.Start(cmd)
	sess, err := client.NewSession()
	if err != nil {
		execution.Finish("", err)
		return "", err
	}
	defer sess.Close()
	out := captureSSHSessionOutput(sess)
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	result, err := awaitSSHCommand(ctx, sess.Close, done, out)
	execution.Finish(result, err)
	return result, err
}

type stringOutput interface {
	String() string
}

func awaitSSHCommand(ctx context.Context, closeSession func() error, done <-chan error, out stringOutput) (string, error) {
	select {
	case err := <-done:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out.String(), ctxErr
		}
		return out.String(), err
	case <-ctx.Done():
		_ = closeSession()
		// Session.Close unblocks Run, but Run may still be copying the final SSH
		// packet into out. Wait before reading the output; returning immediately
		// races that write and has produced NUL-filled diagnostics in live cloud
		// prechecks. If a broken SSH server never acknowledges the close, return
		// the context error without touching the buffer.
		select {
		case <-done:
			return out.String(), ctx.Err()
		case <-time.After(time.Second):
			return "", ctx.Err()
		}
	}
}

// boundedDetached returns a context that survives parent's cancellation but
// is still bounded by timeout. Use it for remote steps that must run to
// completion once started - a cleanup/rollback command issued after ctx is
// already done (upload canceled/timed out), or any step past an install's
// documented "point of no return" - so a genuinely wedged remote command
// still cannot block the caller (or a server shutdown waiting on it) forever.
func boundedDetached(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

func runWithInput(ctx context.Context, client *ssh.Client, cmd, input string) (string, error) {
	return runSSHInput(ctx, client, cmd, input, false)
}

// runDiagnosticWithInput keeps untrusted remote output out of command logs.
// The input may contain an ownership token and is never logged.
func runDiagnosticWithInput(ctx context.Context, client *ssh.Client, cmd, input string) (string, error) {
	return runSSHInput(ctx, client, cmd, input, true)
}

func runSSHInput(ctx context.Context, client *ssh.Client, cmd, input string, diagnostic bool) (string, error) {
	logCommand := cmd
	if diagnostic {
		logCommand = "read node connection diagnostics"
	}
	execution := commandlog.Start(logCommand)
	finish := func(output string, err error) {
		if diagnostic {
			output = ""
			if err != nil {
				err = fmt.Errorf("diagnostic SSH command did not complete")
			}
		}
		execution.Finish(output, err)
	}
	sess, err := client.NewSession()
	if err != nil {
		finish("", err)
		return "", err
	}
	defer sess.Close()
	sess.Stdin = strings.NewReader(input + "\n")
	var output stringOutput
	if diagnostic {
		out := &connectionDiagnosticOutput{}
		sess.Stdout, sess.Stderr = out, io.Discard
		output = out
	} else {
		output = captureSSHSessionOutput(sess)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	result, err := awaitSSHCommand(ctx, sess.Close, done, output)
	finish(result, err)
	return result, err
}

func gpuProbeCommand() string {
	return `if command -v nvidia-smi >/dev/null 2>&1 && out="$(nvidia-smi -L 2>&1)"; then
  printf '%s\n' "$out" | head -20
elif command -v npu-smi >/dev/null 2>&1 && out="$(npu-smi info 2>&1)"; then
  printf '%s\n' "$out" | head -40
elif command -v ppu-smi >/dev/null 2>&1 && out="$(ppu-smi -L 2>&1)"; then
  printf '%s\n' "$out" | head -20
elif command -v hy-smi >/dev/null 2>&1 && out="$(hy-smi -L 2>&1 || hy-smi 2>&1)"; then
  printf '%s\n' "$out" | head -20
elif command -v amd-smi >/dev/null 2>&1 && out="$(amd-smi list 2>&1 || amd-smi 2>&1)"; then
  printf '%s\n' "$out" | head -20
else
  echo "no supported GPU CLI found"
  exit 1
fi`
}

func sudoProbeCommand() string {
	return `if [ "$(id -u)" = "0" ]; then
  echo "root"
elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  echo "passwordless sudo available"
else
  echo "no root and no passwordless sudo"
  exit 1
fi`
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
