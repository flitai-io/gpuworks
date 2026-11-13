package selfinstall

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// minFreeBytes is the space install insists on before it starts copying: the
// binary plus room for a database and a few model catalog entries. It only
// guards the obvious failure (a full disk part-way through an install).
const minFreeBytes = 2 << 30 // 2 GiB

// Privileges describes how the installer can act as root.
type Privileges struct {
	Root bool // already root
	Sudo bool // a sudo binary is available for the privileged commands
}

// Usable reports whether privileged work is possible.
func (p Privileges) Usable() bool { return p.Root || p.Sudo }

// Preflight is what install learned about the host before touching anything.
type Preflight struct {
	Privileges  Privileges
	Existing    bool   // an installation (binary or server.yaml) is already there
	Mode        string // systemd or nohup
	ServiceUser string // resolved account for the unit
	Warnings    []string
}

// checkPrivileges establishes whether privileged work is possible. The
// installer runs as the operator; privileged steps are individual sudo
// commands, so the capability is "a sudo binary exists" — sudo itself asks
// for the password on its first call and caches the ticket.
func (i *Installer) checkPrivileges() (Privileges, error) {
	if i.deps.Euid() == 0 {
		return Privileges{Root: true}, nil
	}
	if !i.deps.hasBinary("sudo") {
		return Privileges{}, nil
	}
	return Privileges{Sudo: true}, nil
}

// preflight runs every check that must pass before the first write, and decides
// how the service will be supervised.
func (i *Installer) preflight(ctx context.Context, req InstallRequest, listen serverconfig.Listen) (Preflight, error) {
	priv, err := i.checkPrivileges()
	if err != nil {
		return Preflight{}, err
	}
	pf := Preflight{
		Privileges:  priv,
		Existing:    fileExists(i.layout.Binary()) || fileExists(i.layout.ConfigPath()),
		ServiceUser: strings.TrimSpace(req.ServiceUser),
	}
	manager := i.serviceManager()
	if !priv.Usable() {
		if err := i.checkUserLevelDir(); err != nil {
			return Preflight{}, err
		}
		pf.Warnings = append(pf.Warnings,
			fmt.Sprintf("installing without root: the service cannot be managed by %s and will not come back after a reboot", manager))
	}
	pf.Mode = i.serviceMode(pf, req)
	switch {
	case pf.Mode == ModeLaunchd:
		pf.ServiceUser, pf.Warnings = launchdUser(req, currentUserName(), pf.Warnings)
	case pf.Mode == ModeNohup && pf.ServiceUser != "" && pf.ServiceUser != "root":
		if !priv.Usable() {
			pf.ServiceUser = ""
		} else if req.NoSystemd {
			// The default account is no request of the operator's; naming
			// one and then running detached is.
			if req.ServiceUserSet {
				pf.Warnings = append(pf.Warnings,
					fmt.Sprintf("--service-user %s is ignored without %s: the detached process runs as the invoking user", pf.ServiceUser, manager))
			}
			pf.ServiceUser = ""
		} else {
			pf.Warnings = append(pf.Warnings,
				fmt.Sprintf("%s is unavailable; ignoring --service-user %s and running as the invoking user", manager, pf.ServiceUser))
			pf.ServiceUser = ""
		}
	}
	for _, b := range serverconfig.Bindings(listen) {
		if err := skippable(&pf, req, i.checkPort(ctx, pf, b.Addr)); err != nil {
			return Preflight{}, err
		}
	}
	if err := skippable(&pf, req, i.checkDisk()); err != nil {
		return Preflight{}, err
	}
	return pf, nil
}

// skippable returns a failed host check, or under --skip-check records it as a
// warning and lets the install go on. Only checks that predict a broken
// server, not ones that protect the host, go through here.
func skippable(pf *Preflight, req InstallRequest, err error) error {
	if err == nil {
		return nil
	}
	if !req.SkipCheck {
		return fmt.Errorf("%w (pass --skip-check to install anyway)", err)
	}
	pf.Warnings = append(pf.Warnings, fmt.Sprintf("ignored because of --skip-check: %v", err))
	return nil
}

// serviceMode picks the supervisor. systemd is used only when it is really
// running (a `systemctl` in a container without systemd is not enough);
// launchd is macOS's.
func (i *Installer) serviceMode(pf Preflight, req InstallRequest) string {
	if !pf.Privileges.Usable() || req.NoSystemd {
		return ModeNohup
	}
	if i.deps.systemdRunning() {
		return ModeSystemd
	}
	if i.deps.launchdRunning() {
		return ModeLaunchd
	}
	return ModeNohup
}

// serviceManager names the service manager this host would use, for messages.
func (i *Installer) serviceManager() string {
	if i.deps.launchdRunning() {
		return "launchd"
	}
	return "systemd"
}

// launchdUser is the account the launch daemon runs as: the person installing
// (the account sudo was run from), as the agent's launch daemon does. A Mac
// has no dedicated service accounts to create, and a server that listens on
// the network should not run as root; it runs as root only when root itself
// installs it.
func launchdUser(req InstallRequest, operator string, warnings []string) (string, []string) {
	if operator == "" || operator == "root" || operator == "unknown" {
		return "", append(warnings, "installing as root: the launch daemon runs as root; install from your own account (with sudo) to run it as that account")
	}
	if req.ServiceUserSet && req.ServiceUser != operator {
		warnings = append(warnings, fmt.Sprintf("on macOS the server runs as the account that installs it (%s); ignoring --service-user %s", operator, req.ServiceUser))
	}
	return operator, warnings
}

// checkUserLevelDir confirms a rootless install has somewhere to write.
func (i *Installer) checkUserLevelDir() error {
	probe := i.layout.Dir
	for !fileExists(probe) {
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	if err := os.MkdirAll(i.layout.Dir, dirMode); err != nil {
		return fmt.Errorf("%w: create %s needs root (try --dir ~/gpuworks/server, or run with sudo)",
			err, i.layout.Dir)
	}
	tmp, err := os.CreateTemp(i.layout.Dir, ".write-probe*")
	if err != nil {
		return fmt.Errorf("cannot write to %s and sudo is unavailable (try --dir ~/gpuworks/server, or run with sudo): %w", i.layout.Dir, err)
	}
	name := tmp.Name()
	tmp.Close()
	_ = os.Remove(name)
	return nil
}

// checkPort fails when something else already listens on the configured
// address. The running installation itself is the one acceptable holder: it is
// about to be restarted.
func (i *Installer) checkPort(ctx context.Context, pf Preflight, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		_ = ln.Close()
		return nil
	}
	if pf.Existing && i.serviceActive(ctx) {
		i.io.Say("address %s is held by the running installation; it will be restarted", addr)
		return nil
	}
	return fmt.Errorf("cannot listen on %s: %w", addr, err)
}

// checkDisk verifies there is room for the binary and a database.
func (i *Installer) checkDisk() error {
	probe := i.layout.Dir
	for !fileExists(probe) {
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	free, err := i.deps.FreeBytes(probe)
	if err != nil {
		// Space is a helpful hint, not a correctness check: an unsupported
		// filesystem must not block the install.
		return nil //nolint:nilerr // Free-space measurement is advisory on unsupported filesystems.
	}
	if free < minFreeBytes {
		return fmt.Errorf("%s has %.1f GiB free; at least 2 GiB is required", probe, float64(free)/(1<<30))
	}
	return nil
}

// launchdAvailable reports whether launchd is the running init: on macOS it
// always is. It is the default behind Deps.LaunchdRunning.
func launchdAvailable() bool {
	return runtime.GOOS == "darwin" && hasBinary("launchctl")
}

// systemdAvailable reports whether systemd is the running init. It is the
// default behind Deps.SystemdRunning.
func systemdAvailable() bool {
	if !hasBinary("systemctl") {
		return false
	}
	info, err := os.Stat("/run/systemd/system")
	return err == nil && info.IsDir()
}
