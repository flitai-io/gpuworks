package provision

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodeaddr"
)

// SSHInstallRequest starts the agent on a target over SSH.
type SSHInstallRequest struct {
	Target       string `json:"target"`
	Name         string `json:"name,omitempty"`
	ProviderKind string `json:"provider_kind,omitempty"`
	InstanceKind string `json:"instance_kind,omitempty"`
	Region       string `json:"region,omitempty"`
	User         string `json:"user,omitempty"`
	Port         int    `json:"port,omitempty"`
	SSHAuth
	ServerURL        string `json:"server_url,omitempty"`
	Token            string `json:"-"`
	InstallDir       string `json:"install_dir,omitempty"`
	ConnectMode      string `json:"connect_mode,omitempty"`
	RuntimeDriver    string `json:"runtime_driver,omitempty"`
	UdockerBundle    string `json:"udocker_bundle,omitempty"`
	ServerDialListen string `json:"server_dial_listen,omitempty"`
	// NoSystemd starts a Linux agent under nohup instead of the
	// gpuworks-agent systemd unit; macOS always uses launchd.
	NoSystemd bool `json:"no_systemd,omitempty"`
	// KeepSupervisor reinstalls the way the host already runs the agent
	// (out-of-band recovery), ignoring NoSystemd.
	KeepSupervisor bool `json:"-"`
	// HostAddress is where the server reaches the host for server_dial,
	// default the SSH target host.
	HostAddress       string `json:"host_address,omitempty"`
	ServerDialAddress string `json:"server_dial_address,omitempty"`
	// GuardExistingAgent stops the install with an *ExistingAgentError when a
	// gpuworks agent is already on the host, unless ReplaceExistingAgent
	// confirms replacing it. The access wizard sets it; recovery and provider
	// installs onto hosts they created leave it off.
	GuardExistingAgent   bool `json:"-"`
	ReplaceExistingAgent bool `json:"replace_existing_agent,omitempty"`
	// KeepUntraversableInstallDir reinstalls an agent that already runs from
	// the install directory even when a directory above it is closed to the
	// dedicated runtime user: out-of-band recovery must bring the agent back
	// online, where a new install would stop and say how to fix the path.
	KeepUntraversableInstallDir bool `json:"-"`
	// OnPhase is used by out-of-band recovery to persist installation progress.
	// API callers leave it nil; it is never serialized or exposed to clients.
	OnPhase func(string) error `json:"-"`
}

// SSHInstallReport is the install result returned to the access wizard.
type SSHInstallReport struct {
	Target            string `json:"target"`
	User              string `json:"user"`
	Host              string `json:"host"`
	Port              int    `json:"port"`
	Arch              string `json:"arch"`
	BinaryName        string `json:"binary_name"`
	InstallDir        string `json:"install_dir"`
	ServerURL         string `json:"server_url,omitempty"`
	ServerDialAddress string `json:"server_dial_address,omitempty"`
	// AuthUsed is the Auth* credential the target accepted.
	AuthUsed string `json:"auth_used"`
	// PlatformKeyAuthorized: the target now accepts the platform key, so the
	// caller can keep that instead of the password or key it was given.
	PlatformKeyAuthorized bool          `json:"platform_key_authorized"`
	Checks                []CheckResult `json:"checks"`
}

// Installer installs an agent over SSH.
type Installer interface {
	InstallSSH(ctx context.Context, req SSHInstallRequest) (SSHInstallReport, error)
}

// AgentBinaryReader loads an agent binary by artifact name.
type AgentBinaryReader func(name string) ([]byte, error)

// SSHInstaller uploads and starts the gpuworks agent over SSH.
type SSHInstaller struct {
	Timeout             time.Duration
	UploadTimeout       time.Duration
	ReadBin             AgentBinaryReader
	UdockerBundleSource string
	HostKeys            HostKeyStore
}

// ErrInstallDirNotWritable: the install directory the operator chose could
// not be written, and a chosen directory is never swapped for a fallback.
var ErrInstallDirNotWritable = errors.New("cannot write the chosen install directory")

// SSHConnectionError reports a failure before remote installation work starts.
type SSHConnectionError struct{ Err error }

func (e *SSHConnectionError) Error() string { return e.Err.Error() }
func (e *SSHConnectionError) Unwrap() error { return e.Err }

// NewSSHInstaller builds an installer with production defaults.
func NewSSHInstaller(readBin AgentBinaryReader) *SSHInstaller {
	return &SSHInstaller{Timeout: 30 * time.Second, ReadBin: readBin, HostKeys: newMemoryHostKeyStore()}
}

func (i *SSHInstaller) uploadTimeout() time.Duration {
	if i != nil && i.UploadTimeout > 0 {
		return i.UploadTimeout
	}
	return 10 * time.Minute
}

// InstallSSH uploads the matching agent binary and starts it.
func (i *SSHInstaller) InstallSSH(ctx context.Context, req SSHInstallRequest) (SSHInstallReport, error) {
	user, host, port, err := ParseTarget(req.Target, req.User, req.Port)
	if err != nil {
		return SSHInstallReport{}, err
	}
	hostAddress := req.HostAddress
	if hostAddress == "" {
		hostAddress = host
	}
	hostAddress, err = nodeaddr.Endpoint(hostAddress)
	if err != nil {
		return SSHInstallReport{}, fmt.Errorf("invalid host_address: %w", err)
	}
	if err := req.agentConfig().Validate(); err != nil {
		if errors.Is(err, agentconf.ErrTokenRequired) {
			return SSHInstallReport{}, fmt.Errorf("node token is not configured")
		}
		return SSHInstallReport{}, fmt.Errorf("agent configuration: %w", err)
	}
	if i.ReadBin == nil {
		return SSHInstallReport{}, fmt.Errorf("agent binary reader is not configured")
	}
	if err := req.SSHAuth.Validate(); err != nil {
		return SSHInstallReport{}, err
	}
	timeout := i.timeout()
	client, outcome, err := DialSSH(ctx, user, host, port, req.SSHAuth, timeout, i.HostKeys)
	if err != nil {
		return SSHInstallReport{}, &SSHConnectionError{Err: err}
	}
	defer client.Close()
	probeSession, err := client.NewSession()
	if err != nil {
		return SSHInstallReport{}, &SSHConnectionError{Err: fmt.Errorf("create SSH session: %w", err)}
	}
	_ = probeSession.Close()

	// Look before the first change to the host: authorizing the platform key.
	// replaced says which existing agent a confirmed install replaces.
	var replaced string
	if req.GuardExistingAgent {
		existing, err := detectExistingAgent(ctx, client)
		if err != nil {
			return SSHInstallReport{}, err
		}
		guard := &ExistingAgentError{Found: existing, ServerURL: req.ServerURL}
		replaces := existing.Replaces(req.ServerURL)
		if req.supervisor() == supervisorNohup && remoteOS(ctx, client) != "darwin" {
			// A nohup install runs beside the host's other agents: only one
			// from the same directory is replaced.
			guard.InstallDir = installDir(expandRemoteHome(ctx, client, req.InstallDir))
			replaces = existing.ReplacesNohup(guard.InstallDir)
		}
		if replaces && !req.ReplaceExistingAgent {
			return SSHInstallReport{}, guard
		}
		if replaces || (guard.InstallDir == "" && existing.Found()) {
			replaced = guard.Error()
		}
	}

	report := SSHInstallReport{
		Target:     req.Target,
		User:       user,
		Host:       host,
		Port:       port,
		ServerURL:  strings.TrimRight(strings.TrimSpace(req.ServerURL), "/"),
		InstallDir: installDir(req.InstallDir),
		AuthUsed:   outcome.Used,
	}
	// Authorize the platform key before installing: a later failure then
	// still leaves the target reachable for the retry and for recovery.
	keyCheck, authorized := AuthorizePlatformKey(ctx, client, req.SSHAuth, outcome)
	report.PlatformKeyAuthorized = authorized
	report.Checks = append(report.Checks, outcome.Check(), keyCheck)
	if replaced != "" {
		report.Checks = append(report.Checks, CheckResult{Name: checkExistingAgent, Status: "warn", Detail: "replaced on request: " + replaced})
	}
	if req.agentConfig().ServerDials() {
		serverAddr := req.ServerDialAddress
		if serverAddr == "" {
			serverAddr = hostAddress
		}
		report.ServerDialAddress = ServerDialAddress(serverAddr, req.ServerDialListen)
	}
	// A directory the operator chose is where the agent goes or the install
	// fails: a writable fallback would put the node's images and models
	// somewhere else (/tmp, emptied at boot) behind a warning.
	chosenDir := strings.TrimSpace(req.InstallDir) != ""
	req.InstallDir = expandRemoteHome(ctx, client, req.InstallDir)
	report.InstallDir = installDir(req.InstallDir)
	if remoteOS(ctx, client) == "darwin" {
		return i.installMac(ctx, client, req, report)
	}
	if strings.EqualFold(strings.TrimSpace(req.InstanceKind), "vm") {
		report.Checks = append(report.Checks,
			checkCommandContext(ctx, client, "Docker", `command -v docker >/dev/null 2>&1 && docker --version`, true),
			checkCommandContext(ctx, client, "NVIDIA Container Toolkit", `command -v nvidia-ctk >/dev/null 2>&1 || { echo "install nvidia-container-toolkit (see https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html)"; exit 1; }`, true),
		)
	}

	useSudo := needsSudo(ctx, client)
	if useSudo {
		if err := claimInstallDirWithSudo(ctx, client, report.InstallDir); err != nil {
			// A few cloud images ship a sudo wrapper that accepts `sudo -n true`
			// but rejects mkdir/chown. If the requested directory is already
			// writable by the SSH user, continue rootless instead of failing before
			// the agent can provide useful runtime diagnostics.
			if _, directErr := run(ctx, client, "mkdir -p "+shellQuote(report.InstallDir)+" && test -w "+shellQuote(report.InstallDir)); directErr == nil {
				useSudo = false
				report.Checks = append(report.Checks, CheckResult{Name: "Privilege", Status: "warn", Detail: "sudo directory claim failed; using writable install directory without sudo: " + err.Error()})
			} else if chosenDir {
				report.Checks = append(report.Checks, CheckResult{Name: "Privilege", Status: "error", Detail: fmt.Sprintf("cannot write the chosen install directory %s: %v", report.InstallDir, err)})
				return report, fmt.Errorf("%w %s: %v", ErrInstallDirNotWritable, report.InstallDir, err)
			} else if fallbackDir, fallbackErr := writableUserInstallDir(ctx, client); fallbackErr == nil {
				requestedDir := report.InstallDir
				report.InstallDir = fallbackDir
				useSudo = false
				report.Checks = append(report.Checks, CheckResult{Name: "Privilege", Status: "warn", Detail: fmt.Sprintf("sudo cannot claim %s; using writable user install directory %s: %v", requestedDir, fallbackDir, err)})
			} else {
				report.Checks = append(report.Checks, CheckResult{Name: "Privilege", Status: "error", Detail: fmt.Sprintf("%v; rootless fallback unavailable: %v", err, fallbackErr)})
				return report, fmt.Errorf("%w; rootless fallback unavailable: %v", err, fallbackErr)
			}
		} else {
			report.Checks = append(report.Checks, CheckResult{Name: "Privilege", Status: "ok", Detail: "non-root user; using passwordless sudo"})
		}
	}

	dirOutcome := ProbeInstallDirForRuntime(ctx, client, report.InstallDir, req.InstanceKind, req.ProviderKind, req.RuntimeDriver)
	if req.KeepUntraversableInstallDir {
		dirOutcome = dirOutcome.keepUntraversable()
	}
	report.Checks = mergeChecks(report.Checks, dirOutcome.Checks)
	if err := dirOutcome.blockingError(); err != nil {
		// Nothing uploaded there could run (noexec), or the runtime could not
		// reach it (a closed ancestor): stop before sending the binary.
		return report, err
	}

	archOut, goarch, err := detectGOARCH(func(cmd string) (string, error) {
		return run(ctx, client, cmd)
	})
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{Name: "Arch", Status: "error", Detail: err.Error()})
		return report, err
	}
	report.Arch = goarch
	report.BinaryName = proto.AgentBinaryName("linux", goarch)
	report.Checks = append(report.Checks, CheckResult{Name: "Arch", Status: "ok", Detail: strings.TrimSpace(archOut) + " -> " + goarch})

	if err := notifyInstallPhase(req, "artifact_check"); err != nil {
		return report, err
	}
	bin, err := i.ReadBin(report.BinaryName)
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{Name: "Agent binary", Status: "error", Detail: err.Error()})
		return report, fmt.Errorf("read agent binary %s: %w", report.BinaryName, err)
	}
	report.Checks = append(report.Checks, CheckResult{Name: "Agent binary", Status: "ok", Detail: fmt.Sprintf("%s (%d bytes)", report.BinaryName, len(bin))})

	binPath := report.InstallDir + "/gpuworks-agent"
	// Avoid reusing a stale root-owned .new file from an interrupted install.
	// The final rename is atomic and replaces the existing agent binary.
	stagedBinPath := fmt.Sprintf("%s.new.%d", binPath, time.Now().UnixNano())
	if err := notifyInstallPhase(req, "uploading"); err != nil {
		return report, err
	}
	if err := uploadDataContext(ctx, client, stagedBinPath, bin, true, i.uploadTimeout()); err != nil {
		// Some images allow sudo mkdir/chown during preflight but expose the
		// mount read-only by the time the upload starts. Retry in a writable
		// user-owned directory before returning the remote upload error.
		if fallbackDir, fallbackErr := writableUserInstallDir(ctx, client); !chosenDir && fallbackErr == nil && fallbackDir != report.InstallDir {
			report.Checks = append(report.Checks, CheckResult{Name: "Privilege", Status: "warn", Detail: fmt.Sprintf("agent upload to %s failed; retrying in writable user directory %s: %v", report.InstallDir, fallbackDir, err)})
			report.InstallDir = fallbackDir
			useSudo = false
			// The fallback directory has its own filesystem; replace the
			// verdicts about the directory that was just abandoned.
			fallbackOutcome := ProbeInstallDirForRuntime(ctx, client, fallbackDir, req.InstanceKind, req.ProviderKind, req.RuntimeDriver)
			if req.KeepUntraversableInstallDir {
				fallbackOutcome = fallbackOutcome.keepUntraversable()
			}
			report.Checks = mergeChecks(report.Checks, fallbackOutcome.Checks)
			if err := fallbackOutcome.blockingError(); err != nil {
				return report, err
			}
			binPath = report.InstallDir + "/gpuworks-agent"
			stagedBinPath = fmt.Sprintf("%s.new.%d", binPath, time.Now().UnixNano())
			err = uploadDataContext(ctx, client, stagedBinPath, bin, true, i.uploadTimeout())
		}
		if err != nil {
			report.Checks = append(report.Checks, CheckResult{Name: "Upload", Status: "error", Detail: err.Error()})
			return report, err
		}
	}
	if err := notifyInstallPhase(req, "verifying"); err != nil {
		return report, err
	}
	if err := notifyInstallPhase(req, "replacing"); err != nil {
		return report, err
	}
	// Point of no return: the atomic replace below starts mutating remote
	// state (binary, then DNS / device access / unit start / verify). From
	// here on, canceling ctx - a browser disconnect via r.Context() in
	// api/provision.go, or the server's shutdown ctx via recovery.go - must
	// not tear down the SSH session mid-flight: that can leave the node with
	// a new binary that was never started, or a half-configured install (DNS
	// set but agent not restarted, etc). Every remaining remote step
	// therefore runs on a context detached from ctx's cancellation, bounded
	// by a generous combined timeout so a genuinely wedged remote command
	// still cannot block the caller (or a server shutdown waiting on it)
	// forever; uploadStream keeps its own tighter timer on top of this.
	installCtx, cancelInstall := boundedDetached(ctx, i.timeout()+i.uploadTimeout())
	defer cancelInstall()
	ctx = installCtx
	if out, err := run(ctx, client, atomicReplaceCommand(stagedBinPath, binPath)); err != nil {
		detail := strings.TrimSpace(out)
		if detail != "" {
			detail += ": "
		}
		detail += err.Error()
		report.Checks = append(report.Checks, CheckResult{Name: "Upload", Status: "error", Detail: detail})
		return report, fmt.Errorf("replace agent binary: %w", err)
	}
	report.Checks = append(report.Checks, CheckResult{Name: "Upload", Status: "ok", Detail: binPath})

	if remoteBundle := strings.TrimSpace(req.UdockerBundle); remoteBundle != "" {
		check, err := i.installUdockerBundle(ctx, client, remoteBundle, report.InstallDir)
		report.Checks = append(report.Checks, check)
		if err != nil {
			return report, err
		}
	}
	if check, ok := configureProviderDNS(ctx, client, req.ProviderKind, req.Region, useSudo, req.Password); ok {
		report.Checks = append(report.Checks, check)
	}
	if err := notifyInstallPhase(req, "restarting"); err != nil {
		return report, err
	}
	// install.sh records the configuration, grants a non-root udocker user
	// its device groups, and starts the agent: the same script every other
	// install runs.
	out, err := i.runInstallScript(ctx, client, scriptInstall{
		installDir: report.InstallDir, conf: req.agentConfig(), supervisor: req.supervisor(), noSudo: !useSudo,
	})
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{Name: "Start agent", Status: "error", Detail: strings.TrimSpace(out) + ": " + err.Error()})
		return report, fmt.Errorf("start agent: %w: %s", err, lastLines(out, 3))
	}
	report.Checks = append(report.Checks, CheckResult{Name: "Start agent", Status: "ok", Detail: strings.TrimSpace(out)})
	verify := verifyAgentCommand(ctx, client, report.InstallDir)
	report.Checks = append(report.Checks, verify)
	if verify.Status == "error" {
		return report, fmt.Errorf("agent process verification failed: %s", verify.Detail)
	}

	report.Checks = append(report.Checks, i.installUdockerDebugHelper(ctx, client, report.InstallDir, req.UdockerBundle, report.Arch))
	return report, nil
}

// agentConfig is the configuration the install records on the node.
func (req SSHInstallRequest) agentConfig() agentconf.Config {
	conf := agentconf.Config{
		Server: req.ServerURL, Token: req.Token, ConnectMode: req.ConnectMode,
		RuntimeDriver: req.RuntimeDriver, UdockerBundle: req.UdockerBundle,
	}
	if conf.ServerDials() {
		conf.Listen = serverDialListen(req.ServerDialListen)
	}
	return conf.Normalized()
}

func (req SSHInstallRequest) supervisor() agentSupervisor {
	if req.KeepSupervisor {
		return supervisorKeep
	}
	return installSupervisor(req.NoSystemd)
}

func notifyInstallPhase(req SSHInstallRequest, phase string) error {
	if req.OnPhase == nil {
		return nil
	}
	if err := req.OnPhase(phase); err != nil {
		return fmt.Errorf("record install phase %s: %w", phase, err)
	}
	return nil
}

// configureProviderDNS applies the provider's documented accelerated resolver
// before the agent starts. The generic one-click installer does not otherwise
// know which cloud network it is running in, so callers must identify the
// provider explicitly. This is best-effort because some images mount an
// immutable resolv.conf; agent installation should still be usable there.
func configureProviderDNS(ctx context.Context, client *ssh.Client, providerKind, region string, useSudo bool, password string) (CheckResult, bool) {
	if !strings.EqualFold(strings.TrimSpace(providerKind), "compshare") || !compshareAcceleratedRegion(region) {
		return CheckResult{}, false
	}
	const content = "nameserver 100.90.90.90\\nnameserver 100.90.90.100\\n"
	command := "printf '" + content + "' >/etc/resolv.conf"
	if useSudo {
		command = "sudo -n sh -c " + shellQuote(command)
	}
	out, err := run(ctx, client, command)
	if err != nil && !useSudo && strings.TrimSpace(password) != "" {
		// Cloud images often provide password-based sudo without enabling
		// passwordless sudo. The SSH password is already supplied by the caller;
		// feed it only to sudo's stdin and never include it in diagnostics.
		out, err = runWithInput(ctx, client, "sudo -S -p '' sh -c "+shellQuote(command), password)
	}
	if err != nil {
		return CheckResult{Name: "Compshare accelerated DNS", Status: "warn", Detail: strings.TrimSpace(out) + ": " + err.Error()}, true
	}
	resolv, verifyErr := run(ctx, client, "cat /etc/resolv.conf")
	if verifyErr != nil || !strings.Contains(resolv, "100.90.90.90") {
		detail := strings.TrimSpace(resolv)
		if verifyErr != nil {
			detail = strings.TrimSpace(out) + ": " + verifyErr.Error()
		}
		return CheckResult{Name: "Compshare accelerated DNS", Status: "warn", Detail: detail}, true
	}
	return CheckResult{Name: "Compshare accelerated DNS", Status: "ok", Detail: strings.TrimSpace(resolv)}, true
}

func compshareAcceleratedRegion(region string) bool {
	s := strings.ToLower(strings.TrimSpace(region))
	s = strings.ReplaceAll(s, " ", "")
	for _, marker := range []string{
		"华北一c", "上海二a", "上海二b", "华北二a", "华北二c",
		"cn-wlcb", "cn-sh2-a", "cn-sh2-b", "cn-bj2-a", "cn-bj2-c",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// writableUserInstallDir provides a rootless fallback for images where the
// SSH account cannot write the requested system directory. The fixed /tmp
// path avoids relying on HOME or command stdout, both of which are unreliable
// on some generated cloud SSH accounts. One agent owns one state directory per
// VM, so a stable path is preferable to a path the installer cannot recover.
func writableUserInstallDir(ctx context.Context, client *ssh.Client) (string, error) {
	const dir = "/tmp/gpuworks"
	_, err := run(ctx, client, "mkdir -p "+shellQuote(dir)+" && test -w "+shellQuote(dir))
	if err != nil {
		return "", err
	}
	return dir, nil
}

// installUdockerDebugHelper writes a convenience wrapper into the SSH login
// user's home directory so a human can run `udocker` commands directly
// against the same UDOCKER_DIR / proot / entrypoint the agent uses, without
// re-deriving them by hand (runuser does not forward UDOCKER_DIR, so a raw
// `runuser -u udocker udocker ps` silently falls back to an empty ~/.udocker).
// Best-effort: home-directory detection or upload failure only downgrades to
// a warning check, it never fails the install.
func (i *SSHInstaller) installUdockerDebugHelper(ctx context.Context, client *ssh.Client, installDir, udockerBundle, goarch string) CheckResult {
	home, homeErr := run(ctx, client, `sh -c 'cd "$HOME" 2>/dev/null && pwd || getent passwd "$(id -un)" | cut -d: -f6'`)
	debugDir := resolveDebugDir(strings.TrimSpace(home), homeErr, installDir)
	debugPath := strings.TrimRight(debugDir, "/") + "/udocker-debug"
	udockerDir, proot, entry := udockerDebugPaths(installDir, udockerBundle, goarch)
	if err := uploadDataContext(ctx, client, debugPath, []byte(udockerDebugScript(installDir, udockerBundle, goarch)), true, i.uploadTimeout()); err != nil {
		return CheckResult{Name: "runveil debug helper", Status: "warn", Detail: err.Error()}
	}
	return CheckResult{
		Name:   "runveil debug helper",
		Status: "ok",
		Detail: fmt.Sprintf("%s ready — UDOCKER_DIR=%s  proot=%s  binary=%s", debugPath, udockerDir, proot, entry),
	}
}

// resolveDebugDir picks the home directory when it was successfully
// resolved, else falls back to installDir.
func resolveDebugDir(home string, homeErr error, installDir string) string {
	if homeErr == nil && home != "" {
		return home
	}
	return installDir
}

// udockerDebugPaths derives the same UDOCKER_DIR / proot / entrypoint paths
// the agent's udocker driver resolves at runtime (see
// internal/agent/runtime/udocker/udocker.go), for the common cases: the
// embedded offline bundle (udockerBundle empty) and an explicit remote
// bundle path. It does not attempt to model the online pip-install fallback.
func udockerDebugPaths(installDir, udockerBundle, goarch string) (udockerDir, proot, entry string) {
	installDir = strings.TrimRight(installDir, "/")
	udockerDir = installDir + "/udocker"
	prootArch := "x86_64"
	if accelerator.NormalizeArch(goarch) == "arm64" {
		prootArch = "arm64"
	}
	proot = udockerDir + "/bin/proot-" + prootArch
	bundleDir := strings.TrimRight(strings.TrimSpace(udockerBundle), "/")
	if bundleDir == "" {
		bundleDir = installDir + "/offline/udocker"
	}
	entry = bundleDir + "/udocker/udocker"
	return udockerDir, proot, entry
}

// udockerDebugScript renders a wrapper that runs udocker as the dedicated
// non-root "udocker" user when present (root-installed agents), or directly
// otherwise (non-root nohup installs), always with the correct UDOCKER_DIR /
// UDOCKER_USE_PROOT_EXECUTABLE set.
func udockerDebugScript(installDir, udockerBundle, goarch string) string {
	udockerDir, proot, entry := udockerDebugPaths(installDir, udockerBundle, goarch)
	return fmt.Sprintf(`#!/bin/sh
UDOCKER_DIR=%s
UDOCKER_USE_PROOT_EXECUTABLE=%s
ENTRY=%s
if id udocker >/dev/null 2>&1; then
  exec runuser -u udocker -- env UDOCKER_DIR="$UDOCKER_DIR" UDOCKER_USE_PROOT_EXECUTABLE="$UDOCKER_USE_PROOT_EXECUTABLE" "$ENTRY" --allow-root "$@"
else
  exec env UDOCKER_DIR="$UDOCKER_DIR" UDOCKER_USE_PROOT_EXECUTABLE="$UDOCKER_USE_PROOT_EXECUTABLE" "$ENTRY" --allow-root "$@"
fi
`, shellQuote(udockerDir), shellQuote(proot), shellQuote(entry))
}

func connectOnce(ctx context.Context, user, host string, port int, methods []ssh.AuthMethod, timeout time.Duration, hostKeys HostKeyStore) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            methods,
		HostKeyCallback: hostKeyCallback(hostKeys, net.JoinHostPort(host, strconv.Itoa(port))),
		Timeout:         timeout,
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	raw, err := dialTCP(ctx, addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("ssh dial: %w", err)
	}
	conn := &identCapturingConn{Conn: raw}
	// ssh.NewClientConn does not honor ClientConfig.Timeout when it is given an
	// already-open net.Conn (that field only bounds ssh.Dial's own dial+
	// handshake). Without this, a stalled remote handshake can block past both
	// the caller's ctx and this function's own timeout, which is the same
	// handshake-deadline bug fixed for SSHPrechecker.PrecheckSSH.
	handshakeDeadline := time.Now().Add(timeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	if err := conn.SetDeadline(handshakeDeadline); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set ssh handshake deadline: %w", err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, handshakeError(conn, err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		c.Close()
		return nil, fmt.Errorf("clear ssh handshake deadline: %w", err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// sshProxy is the SOCKS5 proxy (socks5://host:port) SSH sessions ride, set
// from server.yaml's ssh_proxy. A site whose direct egress to remote SSH
// endpoints is too slow for the agent upload routes the whole session —
// upload included — through it; an HTTP proxy cannot carry port 22.
var sshProxy atomic.Value

// SetSSHProxy routes later SSH dials through a SOCKS5 proxy; "" dials directly.
func SetSSHProxy(proxyURL string) { sshProxy.Store(strings.TrimSpace(proxyURL)) }

// dialTCP dials addr directly, or through the configured SOCKS5 proxy.
func dialTCP(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	proxyURL, _ := sshProxy.Load().(string)
	if proxyURL == "" {
		dialer := net.Dialer{Timeout: timeout}
		return dialer.DialContext(ctx, "tcp", addr)
	}
	host, port, err := parseSocks5Proxy(proxyURL)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: timeout}
	proxyConn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("ssh proxy dial %s: %w", proxyURL, err)
	}
	if err := socks5ConnectThrough(proxyConn, addr, timeout); err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("ssh proxy connect %s via %s: %w", addr, proxyURL, err)
	}
	return proxyConn, nil
}

func parseSocks5Proxy(raw string) (string, int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, fmt.Errorf("parse ssh_proxy: %w", err)
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "socks5" && scheme != "socks5h" {
		return "", 0, fmt.Errorf("ssh_proxy must be socks5://host:port, got %q", raw)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 {
		return "", 0, fmt.Errorf("ssh_proxy must be socks5://host:port, got %q", raw)
	}
	return u.Hostname(), port, nil
}

// socks5ConnectThrough performs the minimal no-auth SOCKS5 handshake
// (RFC 1928) and issues a CONNECT by domain name, leaving the connection in
// the tunneled state. Implemented here — rather than importing
// golang.org/x/net/proxy — because this is the only SOCKS consumer and the
// handshake is a fixed ten-line protocol.
func socks5ConnectThrough(conn net.Conn, target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("greeting reply: %w", err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		return fmt.Errorf("proxy requires auth or is not SOCKS5 (reply %x)", reply)
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("target %q: %w", target, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("target port %q: %w", portStr, err)
	}
	name := []byte(host)
	if len(name) > 255 {
		return fmt.Errorf("target host too long")
	}
	request := append([]byte{0x05, 0x01, 0x00, 0x03, byte(len(name))}, name...)
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("connect request: %w", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("connect reply: %w", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("proxy refused CONNECT (reply code %d)", head[1])
	}
	// Drain the bound-address header: ATYP (1) + addr (4 or 16) + port (2).
	switch head[3] {
	case 0x01:
		if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
			return fmt.Errorf("bound address: %w", err)
		}
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return fmt.Errorf("bound domain length: %w", err)
		}
		if _, err := io.ReadFull(conn, make([]byte, int(length[0])+2)); err != nil {
			return fmt.Errorf("bound domain: %w", err)
		}
	case 0x04:
		if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
			return fmt.Errorf("bound address v6: %w", err)
		}
	default:
		return fmt.Errorf("unknown bound-address type %d", head[3])
	}
	return nil
}

// needsSudo reports whether the connected SSH user is non-root but has
// passwordless sudo available, which is required to create/own the install
// directory and manage the systemd unit.
func needsSudo(ctx context.Context, client *ssh.Client) bool {
	out, err := run(ctx, client, "id -u")
	if err != nil || strings.TrimSpace(out) == "0" {
		return false
	}
	_, err = run(ctx, client, "sudo -n true")
	return err == nil
}

// claimInstallDirWithSudo creates the install directory (if needed) via sudo
// and chowns it to the connected SSH user, so subsequent uploads/commands run
// without sudo.
func claimInstallDirWithSudo(ctx context.Context, client *ssh.Client, dir string) error {
	uid, err := run(ctx, client, "id -u")
	if err != nil {
		return fmt.Errorf("resolve uid: %w", err)
	}
	gid, err := run(ctx, client, "id -g")
	if err != nil {
		return fmt.Errorf("resolve gid: %w", err)
	}
	// Keep each path/identifier as an individual sudo argument. Nesting these
	// already-quoted values inside `sh -c '...'` breaks the command at the
	// inner quote and is rejected by otherwise-valid cloud images.
	cmd := claimInstallDirCommand(dir, uid, gid)
	if out, err := run(ctx, client, cmd); err != nil {
		detail := strings.TrimSpace(out)
		if detail != "" {
			return fmt.Errorf("claim install dir via sudo: %w: %s", err, detail)
		}
		return fmt.Errorf("claim install dir via sudo: %w", err)
	}
	// sudo -n true and sudo mkdir/chown are not sufficient evidence that the
	// SSH account can upload into the target mount. Verify the exact unprivileged
	// operation used by uploadData before proceeding; this catches read-only or
	// policy-restricted cloud mounts early and lets the caller use its fallback.
	probeCommand := installDirWriteProbeCommand(dir, time.Now().UnixNano())
	if _, err := run(ctx, client, probeCommand); err != nil {
		return fmt.Errorf("verify install dir writable: %w", err)
	}
	return nil
}

func installDirWriteProbeCommand(dir string, nonce int64) string {
	probe := filepath.Join(dir, fmt.Sprintf(".gpuworks-write-test-%d", nonce))
	return "touch " + shellQuote(probe) + " && rm -f " + shellQuote(probe)
}

func claimInstallDirCommand(dir, uid, gid string) string {
	return fmt.Sprintf("sudo -n mkdir -p %s && sudo -n chown %s:%s %s",
		shellQuote(dir), shellQuote(strings.TrimSpace(uid)), shellQuote(strings.TrimSpace(gid)), shellQuote(dir))
}

// uploadDataContext streams data over SSH stdin and installs it at path.
// Payloads that are not already compressed are gzipped on the wire: the CI
// runners push the ~25 MiB agent binary over an international SSH path that
// can sag below 50 KiB/s, and gzip alone cuts the transfer by a third. The
// sha256 is always verified against the DECOMPRESSED bytes, so the digest
// contract is unchanged for callers.
func uploadDataContext(ctx context.Context, client *ssh.Client, path string, data []byte, executable bool, timeout time.Duration) error {
	return uploadStream(ctx, client, path, data, executable, timeout, true)
}

// uploadShellCommand assembles the remote-side install command. In compress
// mode the staged file arrives gzipped (fixture images keep gunzip — the
// offline bundles already depend on it) and is decompressed BEFORE the digest
// check, so the sha256 always verifies the payload, not the transport
// encoding.
func uploadShellCommand(path, staged, digest string, executable, compress bool) string {
	stageIn := "cat > " + shellQuote(staged)
	if compress {
		stagedGz := staged + ".gz"
		stageIn = fmt.Sprintf("cat > %s && gunzip -c %s > %s && rm -f %s", shellQuote(stagedGz), shellQuote(stagedGz), shellQuote(staged), shellQuote(stagedGz))
	}
	cmd := fmt.Sprintf("mkdir -p %s && %s && printf '%%s  %%s\\n' '%s' %s | sha256sum -c - && mv -f %s %s", shellQuote(dirName(path)), stageIn, digest, shellQuote(staged), shellQuote(staged), shellQuote(path))
	if executable {
		cmd += " && chmod +x " + shellQuote(path)
	}
	return cmd
}

func uploadStream(ctx context.Context, client *ssh.Client, path string, data []byte, executable bool, timeout time.Duration, compress bool) error {
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	wire := data
	if compress {
		var packed bytes.Buffer
		gz := gzip.NewWriter(&packed)
		if _, err := gz.Write(data); err != nil {
			return err
		}
		if err := gz.Close(); err != nil {
			return err
		}
		wire = packed.Bytes()
	}
	sess.Stdin = bytes.NewReader(wire)
	out := captureSSHSessionOutput(sess)
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	staged := fmt.Sprintf("%s.new.%d", path, time.Now().UnixNano())
	cmd := uploadShellCommand(path, staged, digest, executable, compress)
	execution := commandlog.Start(cmd)
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			// ctx may already be canceled/expired at this point (the upload
			// goroutine's failure and ctx's own deadline can race), so clean up
			// the staged file on a context detached from ctx rather than risk
			// reusing one that aborts "rm -f" before it runs.
			cleanupCtx, cancelCleanup := boundedDetached(ctx, 15*time.Second)
			_, _ = run(cleanupCtx, client, "rm -f "+shellQuote(staged))
			cancelCleanup()
			execution.Finish(out.String(), err)
			return fmt.Errorf("upload %s: %w: %s", path, err, strings.TrimSpace(out.String()))
		}
	case <-ctx.Done():
		_ = sess.Close()
		// ctx is already done here - reusing it for the cleanup command would
		// abort "rm -f" immediately and leak the staged file on the host.
		cleanupCtx, cancelCleanup := boundedDetached(ctx, 15*time.Second)
		_, _ = run(cleanupCtx, client, "rm -f "+shellQuote(staged))
		cancelCleanup()
		err := fmt.Errorf("upload %s canceled: %w", path, ctx.Err())
		execution.Finish(out.String(), err)
		return err
	case <-timer.C:
		_ = sess.Close()
		// Same reasoning as the ctx.Done() branch: ctx may still be live here,
		// but detach anyway so this cleanup never depends on the caller's ctx.
		cleanupCtx, cancelCleanup := boundedDetached(ctx, 15*time.Second)
		_, _ = run(cleanupCtx, client, "rm -f "+shellQuote(staged))
		cancelCleanup()
		err := fmt.Errorf("upload %s timed out after %s", path, timeout)
		execution.Finish(out.String(), err)
		return err
	}
	execution.Finish(out.String(), nil)
	return nil
}

func (i *SSHInstaller) installUdockerBundle(ctx context.Context, client *ssh.Client, remoteBundle, installDir string) (CheckResult, error) {
	source := strings.TrimSpace(i.UdockerBundleSource)
	if source == "" {
		return CheckResult{Name: "udocker offline bundle", Status: "warn", Detail: "udocker_bundle_source is not set in server.yaml; remote bundle path will be passed through only"}, nil
	}
	archive, err := udockerBundleArchive(source)
	if err != nil {
		return CheckResult{Name: "udocker offline bundle", Status: "error", Detail: err.Error()}, err
	}
	remoteArchive := strings.TrimRight(installDir, "/") + "/udocker-bundle.tar.gz"
	if err := uploadStream(ctx, client, remoteArchive, archive, false, i.uploadTimeout(), false); err != nil {
		return CheckResult{Name: "udocker offline bundle", Status: "error", Detail: err.Error()}, err
	}
	cmd := fmt.Sprintf("rm -rf %s && mkdir -p %s && gzip -dc %s | tar -xf - -C %s && rm -f %s",
		shellQuote(remoteBundle), shellQuote(remoteBundle), shellQuote(remoteArchive), shellQuote(remoteBundle), shellQuote(remoteArchive))
	if out, err := run(ctx, client, cmd); err != nil {
		detail := strings.TrimSpace(out)
		if detail == "" {
			detail = err.Error()
		} else {
			detail += ": " + err.Error()
		}
		return CheckResult{Name: "udocker offline bundle", Status: "error", Detail: detail}, fmt.Errorf("extract udocker offline bundle: %w", err)
	}
	return CheckResult{Name: "udocker offline bundle", Status: "ok", Detail: fmt.Sprintf("uploaded %d bytes to %s", len(archive), remoteBundle)}, nil
}

func udockerBundleArchive(source string) ([]byte, error) {
	st, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("stat udocker bundle source: %w", err)
	}
	if !st.IsDir() {
		return os.ReadFile(source)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	closeErr := tw.Close()
	gzErr := gz.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if gzErr != nil {
		return nil, gzErr
	}
	return buf.Bytes(), nil
}
