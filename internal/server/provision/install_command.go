package provision

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/server/nodeaddr"
	"gpuworks.ai/gpuworks/internal/server/offlinepkg"
)

func goarchFor(uname string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(uname)) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported arch: %s", uname)
	}
}

func detectGOARCH(runCommand func(string) (string, error)) (string, string, error) {
	return detectGOARCHWithSleep(runCommand, time.Sleep)
}

func detectGOARCHWithSleep(runCommand func(string) (string, error), sleep func(time.Duration)) (string, string, error) {
	const maxAttempts = 8
	var lastErr error
	probes := []string{"uname -m 2>/dev/null", "/bin/uname -m 2>/dev/null", "arch 2>/dev/null", "dpkg --print-architecture 2>/dev/null", "apk --print-arch 2>/dev/null"}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		for _, probe := range probes {
			out, err := runCommand(probe)
			if err != nil {
				lastErr = err
				continue
			}
			uname, goarch, archErr := supportedArchFromProbe(out)
			if archErr == nil {
				return uname, goarch, nil
			}
			lastErr = fmt.Errorf("remote architecture probe %q: %w", probe, archErr)
		}
		if attempt < maxAttempts {
			delay := time.Duration(1<<(attempt-1)) * 500 * time.Millisecond
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
			sleep(delay)
		}
	}
	return "", "", fmt.Errorf("detect arch after %d attempts: %w", maxAttempts, lastErr)
}

func supportedArchFromProbe(out string) (string, string, error) {
	for _, field := range strings.FieldsFunc(out, func(r rune) bool {
		return r == 0 || r <= ' ' || r == ':' || r == '=' || r == ',' || r == ';'
	}) {
		if goarch, err := goarchFor(field); err == nil {
			return field, goarch, nil
		}
	}
	if strings.Trim(out, "\x00\t\r\n ") == "" {
		return "", "", fmt.Errorf("returned empty output")
	}
	return "", "", fmt.Errorf("unsupported arch output: %q", sanitizeProbeOutput(out))
}

func sanitizeProbeOutput(out string) string {
	const maxLen = 160
	clean := strings.Map(func(r rune) rune {
		if r == 0 || (r < ' ' && r != '\t' && r != '\n' && r != '\r') {
			return -1
		}
		return r
	}, out)
	clean = strings.TrimSpace(clean)
	if len(clean) > maxLen {
		clean = clean[:maxLen] + "..."
	}
	return clean
}

func atomicReplaceCommand(stagedPath, finalPath string) string {
	return "mv -f " + shellQuote(stagedPath) + " " + shellQuote(finalPath)
}

// ResolvedInstallDir is dir as the installer uses it: trimmed, and the
// default install directory when empty.
func ResolvedInstallDir(dir string) string { return installDir(dir) }

func installDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return "/opt/gpuworks"
	}
	return strings.TrimRight(strings.TrimSpace(dir), "/")
}

func dirName(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "."
}

// serverDialListen is a server_dial listen address with the default applied.
func serverDialListen(listen string) string {
	if listen = agentconf.NormalizeListen(listen); listen != "" {
		return listen
	}
	return agentconf.DefaultListen
}

// ValidServerDialListen reports whether listen is an address a server_dial
// agent can listen on (empty means the default).
func ValidServerDialListen(listen string) bool {
	_, err := agentconf.ListenPort(serverDialListen(listen))
	return err == nil
}

// ServerDialAddress maps the advertised agent endpoint to the control-plane
// dial address. An explicit port in hostAddress wins for TCP tunnels.
func ServerDialAddress(hostAddress, listen string) string {
	info, err := nodeaddr.ParseEndpoint(hostAddress)
	if err != nil {
		return ""
	}
	if info.HasPort {
		return net.JoinHostPort(info.Host, strconv.Itoa(info.Port))
	}
	port, err := agentconf.ListenPort(serverDialListen(listen))
	if err != nil {
		return ""
	}
	return net.JoinHostPort(info.Host, strconv.Itoa(port))
}

// ScriptInstallCommand is the one-line install a node runs itself: fetch
// install.sh from the server and run it with the node's token. It is what the
// add-node page shows for an operator to paste.
func ScriptInstallCommand(serverURL, token, connectMode, listen string) string {
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	return "curl -fsSL " + shellQuote(serverURL+"/install.sh") + " | sh -s -- " + scriptInstallArgs(serverURL, token, connectMode, listen)
}

// OfflineInstallCommand runs the offline package (offlinepkg) the operator
// copied to a node that cannot download from the server: a node the server
// dials, without SSH to upload through. It names no server.
func OfflineInstallCommand(token, listen string) string {
	return "sh " + offlinepkg.FileName + " --token " + shellQuote(token) + " --connect-mode server_dial --listen " + shellQuote(serverDialListen(listen))
}

// InstallCommand is the command a node runs itself to install its agent:
// the offline package when the server dials it, else the script from the
// server it dials.
func InstallCommand(serverURL, token, connectMode, listen string) string {
	if strings.TrimSpace(connectMode) == agentconf.ServerDial {
		return OfflineInstallCommand(token, listen)
	}
	return ScriptInstallCommand(serverURL, token, connectMode, listen)
}

// ScriptInstallSteps is the same install for a provider command channel
// (Cloud Assistant, or a provider's SSH proxy), where nobody watches the
// output: the script is downloaded first, so a server the node cannot reach
// fails the run (in "curl | sh" the pipeline's status is sh's, which reads an
// empty script and succeeds); each connection attempt gives up after ten
// seconds and a dropped one is retried twice. The node downloads from the
// server, so this is for agents that dial the server.
func ScriptInstallSteps(serverURL, token, connectMode, listen string, opts ScriptOptions) string {
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	return strings.Join([]string{
		"set -e",
		`script=$(mktemp)`,
		`trap 'rm -f "$script"' EXIT`,
		`n=0`,
		"until curl -fsSL --connect-timeout 10 -o \"$script\" " + shellQuote(serverURL+"/install.sh") + `; do n=$((n+1)); [ "$n" -ge 3 ] && exit 1; sleep 1; done`,
		`sh "$script" ` + scriptInstallArgs(serverURL, token, connectMode, listen) + opts.args(),
	}, "\n")
}

// Markers AgentRunningCheckCommand prints.
const (
	agentRunningOK   = "__GPUWORKS_AGENT_PROCESS_OK__"
	agentRunningWarn = "__GPUWORKS_AGENT_PROCESS_WARN__"
)

// ScriptOptions are the operator's choices about where and how install.sh
// puts the agent on a host a provider command channel installs.
type ScriptOptions struct {
	// InstallDir is the install directory, "" for install.sh's default.
	InstallDir string
	// NoSystemd runs the agent under nohup instead of the gpuworks-agent
	// unit (install.sh --no-systemd).
	NoSystemd bool
}

func (o ScriptOptions) args() string {
	var args string
	if dir := strings.TrimRight(strings.TrimSpace(o.InstallDir), "/"); dir != "" {
		args += " --install-dir " + shellQuote(dir)
	}
	if o.NoSystemd {
		args += " --no-systemd"
	}
	return args
}

// AgentRunningCheckCommand prints a marker AgentRunningCheck reads: whether
// an agent runs from dir ("" for the default install directory), as
// install.sh started it. Providers append it to ScriptInstallSteps on the
// same channel.
func AgentRunningCheckCommand(dir string) string {
	return "if ( " + agentStateProbeCommand(installDir(dir)) + " ) >/dev/null 2>&1; then echo " + agentRunningOK + "; else echo " + agentRunningWarn + "; fi"
}

// AgentRunningCheck is the install report check for AgentRunningCheckCommand's
// output for the same dir.
func AgentRunningCheck(output, dir string) CheckResult {
	if strings.Contains(output, agentRunningOK) {
		return agentRunningCheck(installDir(dir), "")
	}
	return agentNotObservedCheck(installDir(dir), "")
}

func scriptInstallArgs(serverURL, token, connectMode, listen string) string {
	args := "--server " + shellQuote(serverURL) + " --token " + shellQuote(token)
	if strings.TrimSpace(connectMode) == "server_dial" {
		args += " --connect-mode server_dial --listen " + shellQuote(listen)
	}
	return args
}
