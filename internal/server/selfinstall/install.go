package selfinstall

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/server/nodeaddr"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/version"
)

// Install defaults, applied when the command line leaves a setting out.
const (
	DefaultAdminUser      = "admin"
	DefaultLogMaxSize     = "10MB"
	DefaultLogKeep        = 20
	DefaultTrustedProxies = "127.0.0.1/32,::1/128"
)

// InstallRequest is a parsed `gpuworks install` command line.
type InstallRequest struct {
	// Addr is one address serving every function; empty with AddrSet unset
	// means the address the default route leaves from, port 8800.
	Addr           string
	TrustedProxies string
	ServiceUser    string
	Password       string
	LogMaxSize     string
	LogKeep        int
	Quiet          bool
	NoSystemd      bool
	// SkipCheck turns the host checks that predict whether the server will
	// run (address in use, free space, service account reach) into warnings.
	// The guards that keep install from damaging the host still refuse.
	SkipCheck bool

	// The *Set fields record which settings the operator named explicitly. When
	// one was left out, an existing installation's server.yaml wins, so running
	// install again repairs an installation instead of resetting it.
	DataDirSet        bool
	LogsDirSet        bool
	AddrSet           bool
	TrustedProxiesSet bool
	ServiceUserSet    bool
	LogMaxSizeSet     bool
	LogKeepSet        bool
}

// withDefaults fills in the settings a blank flag means.
func (r InstallRequest) withDefaults() InstallRequest {
	if strings.TrimSpace(r.ServiceUser) == "" {
		r.ServiceUser = DefaultServiceUser
	}
	if strings.TrimSpace(r.LogMaxSize) == "" {
		r.LogMaxSize = DefaultLogMaxSize
	}
	if r.LogKeep <= 0 {
		r.LogKeep = DefaultLogKeep
	}
	if strings.TrimSpace(r.TrustedProxies) == "" {
		r.TrustedProxies = DefaultTrustedProxies
	}
	return r
}

// InstallReport is what the command prints when it is done.
type InstallReport struct {
	Layout         Layout
	Mode           string
	ServiceUser    string
	AdminUser      string
	Password       string
	PasswordSource string
	URL            string
	HealthURL      string
	Credentials    string
	Adopted        bool
	GroupHint      string
	ReleaseID      string
	Listen         serverconfig.Listen
}

// Install copies this binary into place, writes server.yaml and the service
// definition, starts it, and waits until it answers.
//
// It never touches an existing database, master key or admin account: running
// it again on an existing installation repairs the binary and the service
// definition around the data that is already there.
func (i *Installer) Install(ctx context.Context, req InstallRequest) (InstallReport, error) {
	req = req.withDefaults()
	previous, hasPrevious := i.previousInstall(ctx)
	if hasPrevious {
		// An installation says where it lives; keep that unless the operator
		// named somewhere else on this command line.
		i.adoptLayout(previous.Config, req.DataDirSet, req.LogsDirSet)
	}
	// Adoption is about this layout: server.yaml or a stray binary here means
	// an installation exists and its data must be left alone.
	adopted := hasPrevious || fileExists(i.layout.Binary())

	config, err := i.installConfig(req, previous.Config)
	if err != nil {
		return InstallReport{}, err
	}
	pf, err := i.preflight(ctx, req, config.Listen)
	if err != nil {
		return InstallReport{}, err
	}
	// Reject deterministic permission-policy errors before replacing a working
	// installation's binary, configuration or service definition. chownTree
	// validates again before applying permissions and starting the service.
	if pf.ServiceUser != "" && pf.ServiceUser != "root" {
		roots, err := i.permissionRoots()
		if err != nil {
			return InstallReport{}, err
		}
		if err := skippable(&pf, req, i.checkServiceUserReach(pf, roots)); err != nil {
			return InstallReport{}, err
		}
		if i.installedInSharedRoot() {
			pf.Warnings = append(pf.Warnings, fmt.Sprintf(
				"%s is shared with the node agent and stays owned by root: the console cannot replace the binary there, so upgrade with `sudo %s upgrade`, or reinstall into a dedicated directory",
				sharedApplicationRoot, i.layout.Binary()))
		}
	}
	if err := i.preflightSensitiveFiles(ctx); err != nil {
		return InstallReport{}, err
	}
	if err := i.preflightGroupSharing(pf); err != nil {
		return InstallReport{}, err
	}
	for _, warning := range pf.Warnings {
		i.io.Warn("warning: %s", warning)
	}

	// Resolving the password may prompt, so it happens before anything is
	// written: aborting here leaves the host untouched. A database that
	// already has an admin account keeps it, so the question is skipped there
	// entirely — asking for a password that would then be ignored would be
	// misleading; an explicit --password still earns the warning below.
	var password, source string
	if fileExists(filepath.Join(i.layout.DataDir, "gpuworks.db")) {
		if strings.TrimSpace(req.Password) != "" {
			i.io.Warn("warning: --password is ignored: the database in %s already has an admin account (use gpuworks reset-password to change it)", i.layout.DataDir)
		}
		source = PasswordUnchanged
	} else {
		password, source, err = i.resolvePassword(req)
		if err != nil {
			return InstallReport{}, err
		}
	}

	if err := i.prepareDirs(ctx); err != nil {
		return InstallReport{}, err
	}
	if err := i.preflightGroupSharing(pf); err != nil {
		return InstallReport{}, err
	}
	self, err := i.selfPath()
	if err != nil {
		return InstallReport{}, err
	}
	if err := i.copyInstalled(ctx, self, i.layout.Binary(), binaryMode); err != nil {
		return InstallReport{}, fmt.Errorf("install %s: %w", i.layout.Binary(), err)
	}
	if pf.Mode == ModeSystemd && pf.ServiceUser != "" {
		if err := i.ensureServiceUser(ctx, pf.ServiceUser); err != nil {
			return InstallReport{}, err
		}
	}
	groupHint := i.addOperatorToGroup(ctx, pf)
	if password != "" {
		payload, err := installcred.InitialCredentialsPayload(DefaultAdminUser, password)
		if err != nil {
			return InstallReport{}, fmt.Errorf("record initial credentials: %w", err)
		}
		if err := i.writeInstalled(ctx, installcred.CredentialsPath(i.layout.DataDir), payload, securefile.OwnerMode); err != nil {
			return InstallReport{}, fmt.Errorf("record initial credentials: %w", err)
		}
	}
	payload, err := serverconfig.Render(config)
	if err != nil {
		return InstallReport{}, err
	}
	// 0664 and the service group: the server edits its listen addresses here
	// when the operator changes them in the console.
	if err := i.writeInstalled(ctx, i.layout.ConfigPath(), payload, 0o664); err != nil {
		return InstallReport{}, fmt.Errorf("write %s: %w", i.layout.ConfigPath(), err)
	}
	cfg := i.unitConfig(pf.ServiceUser, pf.Mode)
	svc := i.service(pf.Mode, pf.ServiceUser, cfg)
	// A previous run may have supervised the server the other way (installed
	// with --no-systemd, repaired after systemd appeared). Stop that process
	// before the new one tries to bind the same port; a unit or plist that no
	// longer applies is removed, since its presence is what marks the mode.
	if hasPrevious && previous.Mode != pf.Mode {
		i.io.Say("stopping the previous %s installation", previous.Mode)
		old := i.service(previous.Mode, "", cfg)
		if err := old.Stop(ctx); err != nil {
			return InstallReport{}, err
		}
		if previous.Mode != ModeNohup {
			if err := old.RemoveUnit(ctx); err != nil {
				return InstallReport{}, err
			}
		}
	}
	if err := svc.InstallUnit(ctx); err != nil {
		return InstallReport{}, err
	}
	if previous.FromInstallJSON {
		i.removeInstallJSON(ctx)
	}
	if err := i.chownTree(ctx, pf.ServiceUser); err != nil {
		return InstallReport{}, err
	}
	if err := svc.Start(ctx); err != nil {
		return InstallReport{}, err
	}
	probe, err := healthURL(probeAddr(config.Listen))
	if err != nil {
		return InstallReport{}, err
	}
	if err := i.waitHealthy(ctx, svc, probe, ""); err != nil {
		i.io.Warn("the service did not become healthy; check %s", i.layout.LogFile())
		return InstallReport{}, err
	}
	return i.finishInstall(ctx, config, pf, password, source, adopted, groupHint, probe)
}

// installConfig is server.yaml after this install: the previous file, with
// what the command line named and what install owns (where the data and the
// log live) written over it.
func (i *Installer) installConfig(req InstallRequest, previous serverconfig.File) (serverconfig.File, error) {
	cfg := previous
	listen, err := i.listenOf(req, previous)
	if err != nil {
		return serverconfig.File{}, err
	}
	cfg.Listen = listen
	cfg.DataDir = i.layout.DataDir
	cfg.Log.File = i.layout.LogFile()
	if req.LogMaxSizeSet || cfg.Log.MaxSize == "" {
		cfg.Log.MaxSize = req.LogMaxSize
	}
	if req.LogKeepSet || cfg.Log.Keep <= 0 {
		cfg.Log.Keep = req.LogKeep
	}
	if req.TrustedProxiesSet || len(cfg.TrustedProxies) == 0 {
		cfg.TrustedProxies = splitList(req.TrustedProxies)
	}
	return cfg, nil
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// finishInstall records the installation and assembles the summary.
func (i *Installer) finishInstall(ctx context.Context, config serverconfig.File, pf Preflight, password, source string, adopted bool, groupHint, probe string) (InstallReport, error) {
	policy, policyErr := i.sensitiveFilePolicy()
	if policyErr != nil {
		return InstallReport{}, policyErr
	}
	if err := i.chownTree(ctx, pf.ServiceUser); err != nil {
		return InstallReport{}, fmt.Errorf("protect completed installation: %w", err)
	}
	if err := i.appendInstalled(ctx, i.layout.HistoryPath(), HistoryEntry{
		Action: ActionInstall, Result: "ok", ReleaseID: version.Version,
		Mode: pf.Mode, By: currentUserName(), At: stampNow(i.deps.now()),
	}, policy.Mode()); err != nil {
		i.io.Warn("warning: could not append to the install history: %v", err)
	}
	report := InstallReport{
		Layout:         i.layout,
		Mode:           pf.Mode,
		ServiceUser:    pf.ServiceUser,
		GroupHint:      groupHint,
		AdminUser:      DefaultAdminUser,
		Password:       password,
		PasswordSource: source,
		HealthURL:      probe,
		Credentials:    i.layout.CredentialsPath(),
		Adopted:        adopted,
		ReleaseID:      version.Version,
		URL:            strings.TrimSuffix(probe, "/healthz"),
		Listen:         config.Listen,
	}
	if base := consoleURL(config.Listen); base != "" {
		report.URL = base
	}
	return report, nil
}

// consoleURL is where other machines open the console: its first address, or
// for a wildcard the address the default route leaves from.
func consoleURL(l serverconfig.Listen) string {
	for _, addr := range l.Of(serverconfig.FunctionConsole) {
		ip, port, err := serverconfig.SplitAddr(addr)
		if err != nil {
			continue
		}
		if ip.IsUnspecified() {
			return nodeaddr.LocalBaseURL(addr)
		}
		return "http://" + net.JoinHostPort(ip.String(), port)
	}
	return ""
}

// prepareDirs creates the directory tree the install owns. Directories the
// operator can make are created directly; a privileged location (a fresh
// /opt/gpuworks) goes through sudo — the one time the path itself needs root.
// The modes are permissive on purpose: the tree is shared between the service
// account and whoever operates on it.
func (i *Installer) prepareDirs(ctx context.Context) error {
	for _, dir := range []string{i.layout.Dir, i.layout.DataDir, i.layout.LogsDir,
		i.layout.BackupsDir, i.layout.TmpDir()} {
		err := mkdirAll(dir, dirMode)
		if err == nil {
			continue
		}
		if i.deps.Euid() != 0 && !i.deps.hasBinary("sudo") {
			return err
		}
		if err := i.deps.Run.Run(ctx, Command{Path: "mkdir", Args: []string{"-p", dir}, Sudo: true}); err != nil {
			return err
		}
		// A directory created by root would otherwise lock the operator out;
		// the tree's access comes from its modes, not its owner.
		if err := i.deps.Run.Run(ctx, Command{Path: "chmod", Args: []string{fmt.Sprintf("%04o", dirMode), dir}, Sudo: true}); err != nil {
			return err
		}
	}
	return nil
}

// previousInstall returns the installation already in the directory.
func (i *Installer) previousInstall(ctx context.Context) (installation, bool) {
	inst, err := i.readInstallation(ctx)
	if err != nil {
		return installation{}, false
	}
	return inst, true
}
