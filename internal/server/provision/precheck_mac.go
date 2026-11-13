package provision

import (
	"context"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/installscript"
)

// macPrecheckCommands are the checks for an Apple silicon Mac: the vendor GPU
// tools, FUSE and Python the Linux checks look for do not apply.
func macPrecheckCommands() []precheckCommand {
	minimum := strconv.Itoa(accelerator.MinimumMacOSMajor)
	return []precheckCommand{
		{name: "OS/Arch", required: true, command: `arch=$(uname -m); v=$(sw_vers -productVersion 2>/dev/null); printf 'macOS %s %s\n' "$v" "$arch"
[ "$arch" = arm64 ] || { echo "macOS nodes need Apple silicon; this is an Intel Mac"; exit 1; }
[ "${v%%.*}" -ge ` + minimum + ` ] 2>/dev/null || { echo "macOS nodes need macOS ` + minimum + ` or later"; exit 1; }`},
		{name: "GPU", required: true, command: `chip=$(sysctl -n machdep.cpu.brand_string 2>/dev/null); echo "$chip"
case "$chip" in Apple*) ;; *) echo "no Apple silicon GPU found"; exit 1 ;; esac`},
	}
}

// macPrivilegeCheck states what the install will register, from the rights
// the installer will have (macPrivilege).
func macPrivilegeCheck(ctx context.Context, client *ssh.Client, auth SSHAuth) CheckResult {
	sudo := macPrivilege(ctx, client, auth)
	if sudo.admin {
		return CheckResult{Name: "Sudo", Status: "ok", Detail: sudo.detail}
	}
	return CheckResult{Name: "Sudo", Status: "warn", Detail: sudo.detail + "; sign in with the account password, or allow passwordless sudo, to install a launch daemon instead"}
}

// macInstallDir is where the installer puts the agent when the request names
// no directory: where an earlier install left one, else ~/gpuworks, with or
// without administrator rights. The rule is the install script's own
// (installscript.MacInstallDirFuncs), so what the precheck reports is where
// the agent goes.
func macInstallDir(ctx context.Context, client *ssh.Client, requested string) string {
	if strings.TrimSpace(requested) != "" {
		return installDir(requested)
	}
	out, err := run(ctx, client, installscript.MacInstallDirFuncs+"\n"+`gw_mac_install_dir "$HOME"`)
	if dir := strings.TrimSpace(out); err == nil && strings.HasPrefix(dir, "/") {
		return dir
	}
	return installDir(requested)
}

// expandRemoteHome resolves a leading ~ in an install directory against the
// target account's home: the directory reaches the remote side quoted, where
// no shell would expand it, and "~/.gpuworks" would become a directory named
// "~". Anything else is returned unchanged.
func expandRemoteHome(ctx context.Context, client *ssh.Client, dir string) string {
	dir = strings.TrimSpace(dir)
	if dir != "~" && !strings.HasPrefix(dir, "~/") {
		return dir
	}
	home, err := run(ctx, client, `printf '%s' "$HOME"`)
	if home = strings.TrimRight(strings.TrimSpace(home), "/"); err != nil || !strings.HasPrefix(home, "/") {
		return dir
	}
	return home + strings.TrimPrefix(dir, "~")
}
