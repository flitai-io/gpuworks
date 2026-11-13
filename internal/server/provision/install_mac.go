package provision

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/launchd"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/installscript"
)

// remoteOS reports the target's kernel name as Go names it ("linux",
// "darwin"); anything else, or a failed probe, is returned as reported.
func remoteOS(ctx context.Context, client *ssh.Client) string {
	out, err := run(ctx, client, "uname -s")
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(out))
}

// macSudo is how the installer gains administrator rights on a Mac.
type macSudo struct {
	admin    bool   // the install runs with administrator rights
	prefix   string // prepended to the install command; "" runs it as the SSH user
	password string // fed to sudo -S on stdin (runWithInput ends the line)
	detail   string
}

// macPrivilege finds administrator rights for the install. A Mac's sudo
// normally asks for the account password; when the operator signed in with
// that password it is used for sudo too, which is what installs the agent as
// a launch daemon (running from boot as the SSH user, ADR 0044). Without
// rights the agent becomes a launch agent of the user's login session.
func macPrivilege(ctx context.Context, client *ssh.Client, auth SSHAuth) macSudo {
	if out, err := run(ctx, client, "id -u"); err == nil && strings.TrimSpace(out) == "0" {
		return macSudo{admin: true, detail: "root: installs a launch daemon"}
	}
	if _, err := run(ctx, client, "sudo -n true"); err == nil {
		return macSudo{admin: true, prefix: "sudo -n ", detail: "passwordless sudo: installs a launch daemon that runs as the SSH user"}
	}
	if auth.Password != "" {
		if _, err := runWithInput(ctx, client, "sudo -S -p '' true", auth.Password); err == nil {
			return macSudo{admin: true, prefix: "sudo -S -p '' ", password: auth.Password, detail: "sudo with the SSH password: installs a launch daemon that runs as the SSH user"}
		}
	}
	return macSudo{detail: "no administrator rights: installs a launch agent of the user's login session, which runs only while that user is logged in"}
}

// installMac installs the agent on an Apple silicon Mac. It runs the same
// installer the curl command runs (installscript.Script), handing it the
// binary this server uploads, so the two ways of adding a Mac register the
// same launchd job and the Mac need not reach the server to download. The
// installer returns once launchd keeps the agent running (launchd.Host.Install)
// and fails otherwise, so its exit status is the verdict.
func (i *SSHInstaller) installMac(ctx context.Context, client *ssh.Client, req SSHInstallRequest, report SSHInstallReport) (SSHInstallReport, error) {
	archOut, goarch, err := detectGOARCH(func(cmd string) (string, error) { return run(ctx, client, cmd) })
	if err == nil && goarch != "arm64" {
		err = fmt.Errorf("macOS nodes need Apple silicon; this Mac reports %s", strings.TrimSpace(archOut))
	}
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{Name: "Arch", Status: "error", Detail: err.Error()})
		return report, err
	}
	report.Arch = goarch
	report.BinaryName = proto.AgentBinaryName("darwin", goarch)
	report.Checks = append(report.Checks, CheckResult{Name: "Arch", Status: "ok", Detail: "Darwin " + strings.TrimSpace(archOut) + " -> darwin/" + goarch})

	if err := notifyInstallPhase(req, "artifact_check"); err != nil {
		return report, err
	}
	bin, err := i.ReadBin(report.BinaryName)
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{Name: "Agent binary", Status: "error", Detail: err.Error()})
		return report, fmt.Errorf("read agent binary %s: %w", report.BinaryName, err)
	}
	report.Checks = append(report.Checks, CheckResult{Name: "Agent binary", Status: "ok", Detail: fmt.Sprintf("%s (%d bytes)", report.BinaryName, len(bin))})

	// Stage the binary and the installer in the user's home, which is
	// writable whatever rights the account has; the installer copies the
	// binary into place.
	stage, err := run(ctx, client, `d="$HOME/.gpuworks-install" && mkdir -p "$d" && chmod 700 "$d" && printf '%s' "$d"`)
	stage = strings.TrimSpace(stage)
	if err != nil || stage == "" {
		report.Checks = append(report.Checks, CheckResult{Name: "Upload", Status: "error", Detail: fmt.Sprintf("create staging directory: %s %v", stage, err)})
		return report, fmt.Errorf("create staging directory: %w", err)
	}
	defer func() { _, _ = run(context.WithoutCancel(ctx), client, "rm -rf "+shellQuote(stage)) }()
	stamp := time.Now().UnixNano()
	stagedBin := fmt.Sprintf("%s/gpuworks-agent.%d", stage, stamp)
	stagedScript := fmt.Sprintf("%s/install.%d.sh", stage, stamp)
	if err := notifyInstallPhase(req, "uploading"); err != nil {
		return report, err
	}
	for _, file := range []struct {
		path string
		data []byte
		exec bool
	}{{stagedBin, bin, true}, {stagedScript, []byte(installscript.Script), false}} {
		if err := uploadDataContext(ctx, client, file.path, file.data, file.exec, i.uploadTimeout()); err != nil {
			report.Checks = append(report.Checks, CheckResult{Name: "Upload", Status: "error", Detail: err.Error()})
			return report, err
		}
	}
	report.Checks = append(report.Checks, CheckResult{Name: "Upload", Status: "ok", Detail: stagedBin})

	if err := notifyInstallPhase(req, "verifying"); err != nil {
		return report, err
	}
	sudo := macPrivilege(ctx, client, req.SSHAuth)
	privilege := CheckResult{Name: "Privilege", Status: "ok", Detail: sudo.detail}
	if !sudo.admin {
		privilege.Status = "warn"
	}
	report.Checks = append(report.Checks, privilege)

	if err := notifyInstallPhase(req, "replacing"); err != nil {
		return report, err
	}
	// The remaining steps change the Mac; like the Linux path they run
	// detached from the caller's cancellation (see InstallSSH).
	installCtx, cancelInstall := boundedDetached(ctx, i.timeout()+i.uploadTimeout())
	defer cancelInstall()
	ctx = installCtx

	// The directory is decided here, by the rule the precheck reported, and
	// handed to the installer rather than left to its own default.
	report.InstallDir = macInstallDir(ctx, client, req.InstallDir)
	// The same arguments as the Linux install; --supervisor means nothing to
	// launchd and the script ignores it there.
	args := scriptInstall{installDir: report.InstallDir, conf: req.agentConfig()}.args()
	cmd := sudo.prefix + "env GPUWORKS_AGENT_BINARY=" + shellQuote(stagedBin) + " sh " + shellQuote(stagedScript) + " " + args
	if err := notifyInstallPhase(req, "restarting"); err != nil {
		return report, err
	}
	var out string
	if sudo.password != "" {
		out, err = runWithInput(ctx, client, cmd, sudo.password)
	} else {
		out, err = run(ctx, client, cmd)
	}
	out = strings.TrimSpace(out)
	if err != nil {
		report.Checks = append(report.Checks, CheckResult{Name: "Start agent", Status: "error", Detail: out + ": " + err.Error()})
		// The error is all an API caller sees of a failed install: carry the
		// installer's last words, which say why.
		return report, fmt.Errorf("install agent: %w: %s", err, lastLines(out, 3))
	}
	report.Checks = append(report.Checks, CheckResult{Name: "Start agent", Status: "ok", Detail: out})
	return report, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// macJobDomainCommand prints the launchd domain the agent's job is in:
// "system" for the launch daemon, "gui/<uid>" for this user's launch agent,
// nothing when there is none.
var macJobDomainCommand = `if [ -e ` + shellQuote(launchd.DaemonPlist()) + ` ]; then echo system
elif [ -e "$HOME/` + launchd.AgentPlist("") + `" ]; then echo "gui/$(id -u)"; fi`

// restartMacAgent restarts the agent's launchd job in place: its plist keeps
// the arguments the install wrote, and launchd stays the one supervisor (a
// process started beside it would be a second agent). A job installed for
// another server is left alone, so the caller reinstalls it for this node.
func (i *SSHInstaller) restartMacAgent(ctx context.Context, client *ssh.Client, serverURL string) error {
	if found, err := detectExistingAgent(ctx, client); err == nil && found.Unit && found.UnitServer != "" && found.UnitServer != trimServer(serverURL) {
		return fmt.Errorf("the gpuworks launchd job is installed for another server (%s); not restarting it", found.UnitServer)
	}
	out, err := run(ctx, client, macJobDomainCommand)
	domain := strings.TrimSpace(out)
	if err != nil || domain == "" {
		return fmt.Errorf("no gpuworks launchd job on this Mac")
	}
	command := "launchctl kickstart -k " + domain + "/" + launchd.AgentLabel
	if domain == "system" {
		if id, _ := run(ctx, client, "id -u"); strings.TrimSpace(id) != "0" {
			if !needsSudo(ctx, client) {
				return fmt.Errorf("restarting the launch daemon needs root or passwordless sudo")
			}
			command = "sudo -n " + command
		}
	}
	// Point of no return, as in RestartAgent.
	restartCtx, cancel := boundedDetached(ctx, i.timeout())
	defer cancel()
	if out, err := run(restartCtx, client, command); err != nil {
		return fmt.Errorf("restart the agent's launchd job: %s: %w", strings.TrimSpace(out), err)
	}
	return nil
}
