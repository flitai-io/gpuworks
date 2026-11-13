package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// installCommand runs `gpuworks install`: copy this binary into place, write the
// service definition, start it, and wait until it answers.
func installCommand(ctx context.Context, args []string) error {
	req := selfinstall.InstallRequest{}
	var dir, dataDir, logsDir string
	var autostart bool
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&dir, "dir", selfinstall.DefaultDir, "install directory")
	fs.StringVar(&dataDir, "data", "", "data directory (default <dir>/data)")
	fs.StringVar(&logsDir, "logs", "", "log directory (default <dir>/logs)")
	fs.StringVar(&req.Addr, "addr", "", "listen IP:port for every function (default: the address of the default route, port 8800; :PORT means every address)")
	fs.StringVar(&req.TrustedProxies, "trusted-proxies", selfinstall.DefaultTrustedProxies,
		"comma-separated proxy IPs/CIDRs allowed to supply X-Forwarded-* headers")
	fs.StringVar(&req.Password, "password", "", "admin password (default: prompt, or random when unattended)")
	fs.StringVar(&req.ServiceUser, "service-user", selfinstall.DefaultServiceUser,
		"account the systemd service runs as (root opts out of the dedicated account); on macOS the launch daemon runs as the installing account")
	fs.StringVar(&req.LogMaxSize, "log-max-size", selfinstall.DefaultLogMaxSize, "rotate the log after this size (KB/MB/GB, 1024-based)")
	fs.IntVar(&req.LogKeep, "log-keep", selfinstall.DefaultLogKeep, "rotated log files to keep")
	fs.BoolVar(&req.Quiet, "quiet", false, "never prompt; generate a password unless one was given")
	fs.BoolVar(&req.SkipCheck, "skip-check", false, "install even when the address is in use, space is short or the service account cannot reach the install directory (the run will likely fail)")
	fs.BoolVar(&req.NoSystemd, "no-systemd", false, "run detached instead of installing a systemd unit (a launch daemon on macOS)")
	fs.BoolVar(&autostart, "autostart", false, "install a systemd unit (a launch daemon on macOS) that starts the server at boot; the default on Linux, while macOS runs it detached unless this is given")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	autostartSet := flagProvided(fs, "autostart") || flagProvided(fs, "no-systemd")
	if flagProvided(fs, "autostart") && flagProvided(fs, "no-systemd") && autostart == req.NoSystemd {
		return fmt.Errorf("--autostart and --no-systemd contradict each other; pass one")
	}
	if flagProvided(fs, "autostart") {
		req.NoSystemd = !autostart
	}
	// A setting left out is not the same as one set to its default: an existing
	// installation's layout wins over the default, but not over an explicit flag.
	req.DataDirSet = flagProvided(fs, "data")
	req.LogsDirSet = flagProvided(fs, "logs")
	req.AddrSet = flagProvided(fs, "addr")
	req.TrustedProxiesSet = flagProvided(fs, "trusted-proxies")
	req.ServiceUserSet = flagProvided(fs, "service-user")
	req.LogMaxSizeSet = flagProvided(fs, "log-max-size")
	req.LogKeepSet = flagProvided(fs, "log-keep")

	io := selfinstall.NewIO()
	probe := selfinstall.NewLayout(dir, dataDir, logsDir)
	printInstallIntro(io, req.Quiet)

	// An earlier installation's server.yaml gives the defaults the questions
	// offer, so pressing enter on a repair keeps it instead of resetting it.
	prev, hasPrev := selfinstall.New(probe, selfinstall.NewDeps(os.Stderr), io).Existing(ctx)
	if !autostartSet {
		req.NoSystemd = !defaultAutostart(runtime.GOOS, hasPrev, prev)
	}
	if !req.Quiet {
		if err := askInstallPrompts(io, installPrompts{
			dir: &dir, addr: &req.Addr, addrSet: &req.AddrSet, noSystemd: &req.NoSystemd,
			skipDir: flagProvided(fs, "dir"), skipAddr: req.AddrSet,
			skipAutostart: autostartSet,
			hasPrev:       hasPrev, prev: prev,
		}); err != nil {
			return err
		}
	}

	layout := selfinstall.NewLayout(dir, dataDir, logsDir)
	inst := selfinstall.New(layout, selfinstall.NewDeps(os.Stderr), io)
	report, err := inst.Install(ctx, req)
	if err != nil {
		return err
	}
	printInstallReport(io, report)
	return nil
}

// defaultAutostart says whether an install that names neither --autostart nor
// --no-systemd registers a boot-time service. On Linux the control plane is a
// server and starts at boot (a detached installation that systemd has since
// appeared under is moved over to it). A Mac is the operator's own machine:
// the server runs when they start it, unless an earlier install registered a
// launch daemon, which a repair keeps.
func defaultAutostart(goos string, hasPrev bool, prev selfinstall.Existing) bool {
	if goos == "darwin" {
		return hasPrev && prev.Mode == selfinstall.ModeLaunchd
	}
	return true
}

// printInstallIntro opens the command with the banner. --quiet is what makes
// an install scriptable, so it drops the banner along with the prompts; the
// summary printed at the end survives it, because that is where a fresh
// installation's password is handed over.
func printInstallIntro(io *selfinstall.IO, quiet bool) {
	if !quiet {
		io.Say("")
		io.Say("")
		io.Say("%s", installBanner(io.Out))
		io.Say("")
		io.Say("")
	}
}

// installPrompts collects the interactive answers that configure a fresh
// install. Answers are only asked for settings the operator did not already
// name on the command line; every question offers the effective default, so
// pressing enter keeps it.
type installPrompts struct {
	dir, addr     *string
	addrSet       *bool
	noSystemd     *bool
	skipDir       bool
	skipAddr      bool
	skipAutostart bool
	hasPrev       bool
	prev          selfinstall.Existing
	// detect finds the address of the default route; nil means this machine.
	detect func() (net.IP, error)
}

// askInstallPrompts runs the install questions on an interactive terminal. The
// password is not asked here: it is the last step of the install flow itself,
// after the elevation, so it never has to cross a sudo boundary.
func askInstallPrompts(io *selfinstall.IO, p installPrompts) error {
	if !io.Terminal() {
		return nil
	}
	if !p.skipDir || !p.skipAddr || !p.skipAutostart {
		io.Say("Press Enter to take the default in brackets, or type your own value and press Enter.")
	}

	if !p.skipDir {
		def := *p.dir
		answer, err := io.Prompt(fmt.Sprintf("install directory (default: %s): ", tildePath(def)))
		if err != nil {
			return err
		}
		if answer != "" {
			def = selfinstall.ExpandHome(answer)
		}
		*p.dir = def
	}

	if !p.skipAddr {
		def := defaultListenAddr(p)
		for {
			answer, err := io.Prompt(fmt.Sprintf("listen address (default: %s): ", def))
			if err != nil {
				return err
			}
			addr, err := listenAnswer(def, answer)
			if err != nil {
				io.Warn("%v", err)
				continue
			}
			*p.addr = addr
			break
		}
		*p.addrSet = true
	}

	if !p.skipAutostart {
		// The question offers what *noSystemd already holds (the platform's
		// default, see defaultAutostart), or what an earlier installation did.
		def := "yes"
		if *p.noSystemd || (p.hasPrev && p.prev.Mode == selfinstall.ModeNohup) {
			def = "no"
		}
		for {
			answer, err := io.Prompt(fmt.Sprintf("start the service automatically (default: %s)? [yes/no]: ", def))
			if err != nil {
				return err
			}
			if answer == "" {
				answer = def
			}
			switch strings.ToLower(strings.TrimSpace(answer)) {
			case "yes", "y":
				*p.noSystemd = false
			case "no", "n":
				*p.noSystemd = true
			default:
				io.Warn("answer yes or no")
				continue
			}
			break
		}
	}
	return nil
}

// printInstallReport tells the operator what was created and how to reach it.
func printInstallReport(io *selfinstall.IO, r selfinstall.InstallReport) {
	io.Say("")
	io.Say("gpuworks %s installed", r.ReleaseID)
	io.Say("  url         %s", r.URL)
	io.Say("  config      %s", r.Layout.ConfigPath())
	io.Say("  service     %s (%s)", r.Mode, describeServiceUser(r))
	io.Say("  binary      %s", r.Layout.Binary())
	io.Say("  data        %s", r.Layout.DataDir)
	io.Say("  logs        %s", r.Layout.LogFile())
	io.Say("  backups     %s", r.Layout.BackupsDir)
	io.Say("")
	io.Say("  admin user  %s", r.AdminUser)
	switch r.PasswordSource {
	case selfinstall.PasswordUnchanged:
		io.Say("  password    unchanged (the existing database keeps its admin account)")
	case "":
		io.Say("  password    %s", r.Password)
	default:
		io.Say("  password    %s (%s)", r.Password, r.PasswordSource)
	}
	if r.Password != "" {
		io.Say("")
		io.Say("The password is recorded in %s (0664).", r.Credentials)
		io.Say("Log in and change it, then delete that file.")
	}
	if r.Mode == selfinstall.ModeNohup {
		io.Say("")
		io.Say("gpuworks runs behind its own supervisor (serve --supervise), which starts it again when it")
		io.Say("exits, but nothing starts it after a reboot or a container restart.")
		io.Say("Start it again with: %s restart", r.Layout.Binary())
		io.Say("Stop it with:        %s stop", r.Layout.Binary())
		if runtime.GOOS == "darwin" {
			io.Say("Start it at boot:    %s install --autostart", r.Layout.Binary())
		} else {
			io.Say("As a container's entry point: %s serve --supervise --config %s", r.Layout.Binary(), r.Layout.ConfigPath())
		}
	}
	if r.Adopted {
		io.Say("")
		io.Say("An installation was already present; its data, master key and admin account were left alone.")
	}
	if r.GroupHint != "" {
		io.Say("")
		io.Say("%s", r.GroupHint)
	}
}

// describeServiceUser names the account the service runs as, or says that
// nothing supervises it.
func describeServiceUser(r selfinstall.InstallReport) string {
	if r.Mode == selfinstall.ModeNohup {
		return "detached process"
	}
	manager := "systemd"
	if r.Mode == selfinstall.ModeLaunchd {
		manager = "launch daemon"
	}
	if strings.TrimSpace(r.ServiceUser) == "" {
		return manager + ", root"
	}
	return manager + ", user " + r.ServiceUser
}

// upgradeCommand runs `gpuworks upgrade`: fetch the release the distribution
// channel offers (or read a release site, or unpack a local archive), back the
// installation up, replace the binary and roll back if the new one does not
// come up.
func upgradeCommand(ctx context.Context, args []string) error {
	req := selfinstall.UpgradeRequest{}
	var dir, dataDir, logsDir string
	var sources stringList
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&dir, "dir", selfinstall.DefaultDir, installedDirUsage)
	fs.StringVar(&dataDir, "data", "", "data directory (default: the one recorded at install)")
	fs.StringVar(&logsDir, "logs", "", "log directory (default: the one recorded at install)")
	fs.StringVar(&req.Channel, "channel", "", "distribution channel to upgrade from, nightly or release (default: update_channel in server.yaml, else the channel this build follows)")
	fs.Var(&sources, "source", "distribution source base URL replacing the built-in ones (repeatable)")
	fs.StringVar(&req.BaseURL, "base-url", "", "read a release site (data/release.json + downloads/) instead of the channel, such as the internal one")
	fs.StringVar(&req.Archive, "archive", "", "upgrade from a local release archive (gpuworks-<release>-<os>-<arch>.tar.gz)")
	fs.StringVar(&req.Version, "version", "", "expected release id (must match what the channel or site offers, or the archive holds)")
	fs.IntVar(&req.Keep, "keep", selfinstall.DefaultBackupKeep, "pre-upgrade backups to keep")
	fs.BoolVar(&req.Force, "force", false, "replace a build without a release id (dev/dirty)")
	fs.BoolVar(&req.Check, "check", false, "report whether an update is available and exit")
	fs.BoolVar(&req.Quiet, "quiet", false, "print only warnings and errors")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	named := 0
	for _, name := range []string{"source", "base-url", "archive"} {
		if flagProvided(fs, name) {
			named++
		}
	}
	if named > 1 {
		return fmt.Errorf("--source, --base-url and --archive name different places to upgrade from; pass one")
	}
	if flagProvided(fs, "archive") && strings.TrimSpace(req.Archive) == "" {
		return fmt.Errorf("--archive needs a path")
	}
	if flagProvided(fs, "base-url") && strings.TrimSpace(req.BaseURL) == "" {
		return fmt.Errorf("--base-url needs a URL")
	}
	if flagProvided(fs, "channel") && (flagProvided(fs, "archive") || flagProvided(fs, "base-url")) {
		return fmt.Errorf("--channel picks a distribution channel; it does not apply to --base-url or --archive")
	}
	if !flagProvided(fs, "dir") {
		dir = selfinstall.RunningDir()
	}
	req.Sources = sources
	req.DataDirSet = flagProvided(fs, "data")
	req.LogsDirSet = flagProvided(fs, "logs")

	io := selfinstall.NewIO()
	layout := selfinstall.NewLayout(dir, dataDir, logsDir)
	// An unattended upgrade has nothing to prompt for; --quiet silences the
	// progress narration and the success summary, and leaves warnings and
	// errors on stderr.
	io.Quiet = req.Quiet
	inst := selfinstall.New(layout, selfinstall.NewDeps(os.Stderr), io)
	report, err := inst.Upgrade(ctx, req)
	if err != nil {
		// A rollback already printed its own warnings; the report still tells
		// the operator which snapshot holds the pre-upgrade state.
		if report.Backup != "" {
			io.Warn("the pre-upgrade backup is at %s", report.Backup)
		}
		return err
	}
	printUpgradeReport(io, report)
	return nil
}

func printUpgradeReport(io *selfinstall.IO, r selfinstall.UpgradeReport) {
	switch {
	case r.AheadOfChannel:
		io.Say("gpuworks %s is newer than %s, the release the channel offers; pass --force to install it anyway", r.From, r.To)
	case r.UpToDate:
		io.Say("gpuworks is already at %s", r.To)
	case r.Checked:
		io.Say("update available: %s -> %s (%s, built %s)", r.From, r.To, r.Commit, r.BuiltAt)
		if r.Source != "" {
			io.Say("  source      %s", r.Source)
		}
	default:
		io.Say("upgraded %s -> %s", r.From, r.To)
		if r.Source != "" {
			io.Say("  source      %s", r.Source)
		}
		if r.Backup != "" {
			// A rollback happens automatically when the new binary does not come
			// up; this snapshot is the manual way back if it fails later.
			io.Say("  backup      %s", r.Backup)
		}
	}
}

// defaultListenAddr is the address the question offers: what the installation
// binds, else the address of the default route.
func defaultListenAddr(p installPrompts) string {
	if p.hasPrev {
		if addrs := p.prev.Config.Listen.Of(serverconfig.FunctionConsole); len(addrs) > 0 {
			return addrs[0]
		}
	}
	detect := p.detect
	if detect == nil {
		detect = func() (net.IP, error) { return serverconfig.DetectIP(serverconfig.SystemHost()) }
	}
	if ip, err := detect(); err == nil {
		return net.JoinHostPort(ip.String(), serverconfig.DefaultPort)
	}
	return "0.0.0.0:" + serverconfig.DefaultPort
}

// listenAnswer reads an answer to the listen question: empty keeps the
// default, a bare port keeps its address, anything else must be IP:port.
func listenAnswer(def, answer string) (string, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(answer); err == nil {
		if n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid port %q; enter a number between 1 and 65535", answer)
		}
		host, _, _ := net.SplitHostPort(def)
		return net.JoinHostPort(host, answer), nil
	}
	if _, _, err := serverconfig.SplitAddr(answer); err != nil {
		return "", fmt.Errorf("enter a port or an IP:port address: %v", err)
	}
	return answer, nil
}

// restartCommand runs `gpuworks restart`: stop and start the installed server
// the way it is supervised, so a changed server.yaml takes effect.
func restartCommand(ctx context.Context, args []string) error {
	var dir string
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&dir, "dir", selfinstall.DefaultDir, installedDirUsage)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if !flagProvided(fs, "dir") {
		dir = selfinstall.RunningDir()
	}
	io := selfinstall.NewIO()
	mode, err := selfinstall.New(selfinstall.NewLayout(dir, "", ""), selfinstall.NewDeps(os.Stderr), io).Restart(ctx)
	if err != nil {
		return err
	}
	io.Say("gpuworks restarted (%s)", mode)
	return nil
}

// stopCommand runs `gpuworks stop`: shut the installed server down. A
// service manager keeps it installed, so it comes back at the next boot.
func stopCommand(ctx context.Context, args []string) error {
	var dir string
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&dir, "dir", selfinstall.DefaultDir, installedDirUsage)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if !flagProvided(fs, "dir") {
		dir = selfinstall.RunningDir()
	}
	io := selfinstall.NewIO()
	mode, err := selfinstall.New(selfinstall.NewLayout(dir, "", ""), selfinstall.NewDeps(os.Stderr), io).Stop(ctx)
	if err != nil {
		return err
	}
	if mode == selfinstall.ModeNohup {
		io.Say("gpuworks stopped (%s); start it again with gpuworks restart", mode)
	} else {
		io.Say("gpuworks stopped (%s); it starts again at the next boot, or now with gpuworks restart", mode)
	}
	return nil
}

// installedDirUsage describes --dir for the commands that act on an existing
// installation: without it they act on the one this binary belongs to.
var installedDirUsage = "install directory (default: the installation this binary belongs to, else " + selfinstall.DefaultDir + ")"

// installUsage is the help text for the installation subcommands.
const installUsage = `  gpuworks install [--dir DIR] [--data DIR] [--logs DIR] [--addr IP:PORT]
                   [--trusted-proxies CIDRS] [--password PW]
                   [--service-user gpuworks] [--log-max-size 10MB] [--log-keep 20]
                   [--quiet] [--autostart | --no-systemd] [--skip-check]
  gpuworks upgrade [--dir DIR] [--channel nightly|release]
                   [--source URL ... | --base-url URL | --archive FILE] [--version ID]
                   [--keep 3] [--force] [--check] [--quiet]
  gpuworks restart [--dir DIR]
  gpuworks stop [--dir DIR]
  gpuworks uninstall [--dir DIR] [--purge] [--keep-resources] [--yes|-y]
                     [--dry-run] [--json] [--quiet]

install defaults to /opt/gpuworks/server and a systemd unit on Linux, and to ~/gpuworks/server
run detached (no launch daemon unless --autostart) on macOS.
upgrade, restart, stop and uninstall act on the installation the binary belongs to (the one
whose directory holds it and its server.yaml), else the default directory.
uninstall retains configuration and data by default; --purge deletes them.
Unattended uninstall requires --yes; --dry-run only displays the checklist.
Active or unverified resources block uninstall unless --keep-resources is explicit;
--yes never bypasses resource or installation ownership checks.
`

// tildePath shows a directory under the operator's home as ~/..., the way the
// operator would type it back.
func tildePath(dir string) string {
	home := selfinstall.ExpandHome("~")
	if home == "~" || home == "/" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(dir, home+string(filepath.Separator)); ok {
		return "~/" + rel
	}
	return dir
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("needs a URL")
	}
	*l = append(*l, value)
	return nil
}
