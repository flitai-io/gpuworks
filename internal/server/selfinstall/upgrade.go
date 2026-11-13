package selfinstall

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/version"
)

// UpgradeRequest is a parsed `gpuworks upgrade` command line. The new release
// comes from exactly one place: the distribution channel (the default, on the
// built-in sources or on Sources), a release site (BaseURL), or a local
// Archive.
type UpgradeRequest struct {
	// Channel is the distribution channel to read; empty is the channel this
	// build follows.
	Channel string
	// Sources replaces the built-in distribution sources, for a mirror that
	// copies the channel's tree.
	Sources []string
	// BaseURL reads a release site (data/release.json + downloads/) instead of
	// the channel, such as the internal one.
	BaseURL string
	// Archive is a release archive already on this machine, for hosts that
	// cannot reach any source.
	Archive string
	// Version pins the release to install. A channel or site offers one release
	// at a time, so a pin that is not what it offers is an error rather than a
	// silent upgrade to something else.
	Version string
	Force   bool
	Check   bool
	Quiet   bool
	Keep    int

	// Addr is not a flag of its own: the health check after a restart reaches
	// the address server.yaml binds.
	Addr string

	DataDirSet bool
	LogsDirSet bool
}

// withDefaults fills in the settings a blank flag means.
func (r UpgradeRequest) withDefaults() UpgradeRequest {
	if r.Keep <= 0 {
		r.Keep = DefaultBackupKeep
	}
	return r
}

// UpgradeReport is what the command prints when it is done.
type UpgradeReport struct {
	From    string
	To      string
	Commit  string
	BuiltAt string
	// Source is where the release came from: the distribution source or
	// release site that served it, empty for a local archive.
	Source   string
	Mode     string
	Backup   string
	UpToDate bool
	// AheadOfChannel means the running build is newer than what the channel
	// offers, so nothing was installed.
	AheadOfChannel bool
	Checked        bool
	RolledBack     bool
}

// Upgrade replaces the installed binary with the release the channel (or a
// release site) offers, or the one in a local archive, keeping a snapshot to
// roll back to.
//
// The upgrade window is short but not empty: the service is stopped, backed up,
// replaced and started again, which takes on the order of a minute. Writes made
// during that window are lost if the upgrade rolls back, because a rollback
// restores the snapshot taken before it started.
func (i *Installer) Upgrade(ctx context.Context, req UpgradeRequest) (UpgradeReport, error) {
	req = req.withDefaults()
	inst, err := i.readInstallation(ctx)
	if err != nil {
		return UpgradeReport{}, err
	}
	// An upgrade keeps the installation's layout unless the operator names
	// somewhere else on this command line.
	i.adoptLayout(inst.Config, req.DataDirSet, req.LogsDirSet)
	req.Addr = installedProbeAddr(inst.Config)

	unlock, err := i.lockUpgrade(ctx)
	if err != nil {
		return UpgradeReport{}, err
	}
	defer unlock()

	// Whatever the source staged is never wanted once this returns: on success
	// it has been moved into place, otherwise it must not linger.
	defer func() { _ = os.Remove(i.stagedBinary()) }()
	var src upgradeSource
	switch {
	case strings.TrimSpace(req.Archive) != "":
		src, err = i.archiveSource(ctx, req.Archive)
	case strings.TrimSpace(req.BaseURL) != "":
		src, err = i.siteSource(ctx, req.BaseURL)
	default:
		src, err = i.channelSource(ctx, req.Sources, req.Channel, inst.Config.UpdateChannel)
	}
	if err != nil {
		return UpgradeReport{}, err
	}
	target := strings.TrimSpace(req.Version)
	if target != "" && target != src.ReleaseID {
		return UpgradeReport{}, src.pinMismatch(target)
	}
	if target == "" {
		target = src.ReleaseID
	}

	local := strings.TrimSpace(inst.ReleaseID)
	if local == "" {
		local = version.Version
	}
	report := UpgradeReport{
		From: local, To: target, Commit: src.Commit, BuiltAt: src.BuiltAt,
		Source: src.Source, Mode: inst.Mode,
	}
	// The outcome ("already at", "update available", "upgraded") is reported by
	// the command that called this, so it is printed once and can be silenced
	// as a whole; what this layer narrates is progress.
	if local == target {
		report.UpToDate = true
		return report, nil
	}
	// A channel switch (nightly to release) can offer a release older than the
	// running build; that is not an upgrade, and only --force installs it.
	if target == src.ReleaseID && src.newerThan != nil && !src.newerThan(local, inst.BuiltAt) && !req.Force {
		report.UpToDate, report.AheadOfChannel = true, true
		return report, nil
	}
	if !req.Force {
		if err := checkLocalVersion(local); err != nil {
			return UpgradeReport{}, err
		}
	}
	if req.Check {
		report.Checked = true
		return report, nil
	}
	if err := i.runUpgrade(ctx, req, inst, src, &report); err != nil {
		return report, err
	}
	return report, nil
}

// checkLocalVersion refuses to upgrade an installation whose version carries no
// usable release identity, because there would be no way to tell whether the
// upgrade worked or which snapshot to go back to.
func checkLocalVersion(local string) error {
	switch {
	case local == "", local == "dev":
		return fmt.Errorf("this installation has no release id (it was built locally); pass --force to replace it anyway")
	case strings.HasSuffix(local, "-dirty"):
		return fmt.Errorf("this installation was built from a modified tree (%s); pass --force to replace it anyway", local)
	}
	return nil
}

// runUpgrade performs the replacement, rolling back when the new binary does
// not come up.
func (i *Installer) runUpgrade(ctx context.Context, req UpgradeRequest, inst installation, src upgradeSource, report *UpgradeReport) error {
	// A host can carry more than one installation; naming the one this run
	// acts on is what tells an operator they picked the wrong one.
	i.io.Say("upgrading the installation at %s (%s, %s)", i.layout.Dir, report.From, inst.Mode)
	cfg := i.unitConfig(inst.ServiceUser, inst.Mode)
	svc := i.service(inst.Mode, inst.ServiceUser, cfg)
	// A snapshot has to hold the master key: it decrypts the credentials in the
	// database, and a server that starts without it generates a fresh key,
	// leaving every encrypted credential undecryptable. That is checked here,
	// before anything on the host is written, so the refusal leaves nothing
	// changed instead of producing a backup that cannot be restored and then
	// re-keying the installation.
	if err := i.checkSnapshotMembers(); err != nil {
		return fmt.Errorf("%w; nothing was changed", err)
	}
	if err := i.preflightSensitiveFiles(ctx); err != nil {
		return err
	}
	if inst.FromInstallJSON {
		if err := i.adoptInstallJSON(ctx, inst, svc); err != nil {
			return err
		}
	}
	staged, err := src.stage(ctx)
	if err != nil {
		return err
	}

	// The service writes its log file and its database under the install tree;
	// it must be able to start even if one of those was removed by hand. An
	// upgrade runs as root, so any directory it just recreated has to be handed
	// back to the service account before the service is asked to use it.
	if err := i.prepareDirs(ctx); err != nil {
		return err
	}
	// So is a history removed by hand: recreated after the handoff by the
	// audit line below, it would be root's again (see createHistory).
	if err := i.createHistory(ctx, i.layout.HistoryPath(), 0o600); err != nil {
		i.io.Warn("warning: could not create the install history: %v", err)
	}
	if err := i.chownTree(ctx, inst.ServiceUser); err != nil {
		return err
	}
	wasActive := svc.Active(ctx)
	i.io.Say("stopping the service")
	if err := svc.Stop(ctx); err != nil {
		return fmt.Errorf("stop the service: %w", err)
	}
	// Whatever still listens on the installation's addresses now is not this
	// installation's service, and the new binary could not bind them. Starting
	// it anyway only ends in a rollback that fails the same way, with the
	// holder's /healthz answer as the only clue; refusing here, before the
	// backup, leaves nothing changed.
	if err := i.checkAddrsReleased(ctx, inst.Config, "upgrade"); err != nil {
		if wasActive {
			if startErr := svc.Start(ctx); startErr != nil {
				i.io.Warn("warning: could not restart the service: %v", startErr)
			}
		}
		return fmt.Errorf("%w; nothing was changed", err)
	}
	i.io.Say("backing up the database, master key and current binary")
	backup, err := i.createBackup(ctx, report.From)
	if err != nil {
		// Nothing has been replaced yet, so the previous version is put back in
		// charge rather than left stopped because its snapshot failed.
		if startErr := svc.Start(ctx); startErr != nil {
			i.io.Warn("warning: could not restart the service: %v", startErr)
		}
		return fmt.Errorf("create the pre-upgrade backup: %w", err)
	}
	report.Backup = backup

	cause := i.applyAndCheck(ctx, svc, staged, req.Addr, report.To)
	if cause != nil {
		i.rollback(ctx, svc, inst.ServiceUser, backup, req.Addr, report, cause)
		return fmt.Errorf("upgrade to %s failed and was rolled back to %s (backup: %s): %w",
			report.To, report.From, backup, cause)
	}
	i.finishUpgrade(ctx, req, report)
	return nil
}

// applyAndCheck replaces the binary, starts the service and waits for it to
// answer as release want. It returns nil when the new version is running.
func (i *Installer) applyAndCheck(ctx context.Context, svc Service, staged, addr, want string) error {
	if err := replaceBinary(staged, i.layout.Binary()); err != nil {
		return fmt.Errorf("replace the binary: %w", err)
	}
	if err := svc.Start(ctx); err != nil {
		return err
	}
	probe, err := healthURL(addr)
	if err != nil {
		return err
	}
	if err := i.waitHealthy(ctx, svc, probe, want); err != nil {
		i.io.Warn("the new version did not become healthy; check %s", i.layout.LogFile())
		return err
	}
	return nil
}

// rollback restores the snapshot and restarts the previous version, recording
// the failure in the audit log.
func (i *Installer) rollback(ctx context.Context, svc Service, serviceUser, backup, addr string, report *UpgradeReport, cause error) {
	i.io.Warn("upgrade failed: %v", cause)
	i.io.Warn("restoring %s", backup)
	report.RolledBack = true
	restoreErr := i.restoreBackup(backup)
	if restoreErr == nil {
		// The snapshot is unpacked by root, which would leave the database and
		// the master key unreadable for the service account that has to open
		// them; the tree is handed back before the service is started again,
		// with the history the failure is about to be recorded in.
		_ = i.createHistory(ctx, i.layout.HistoryPath(), 0o600)
		restoreErr = i.chownTree(ctx, serviceUser)
	}
	if restoreErr == nil {
		restoreErr = svc.Start(ctx)
	}
	if restoreErr == nil {
		if probe, err := healthURL(addr); err == nil {
			restoreErr = i.waitHealthy(ctx, svc, probe, report.From)
		}
	}
	if restoreErr != nil {
		i.io.Warn("the previous version did not come back cleanly: %v", restoreErr)
	}
	_ = i.appendInstalled(ctx, i.layout.HistoryPath(), HistoryEntry{
		Action: ActionUpgrade, Result: "failed", ReleaseID: report.To,
		PreviousReleaseID: report.From, Backup: backup, Mode: report.Mode,
		By: currentUserName(), At: stampNow(i.deps.now()), Error: cause.Error(),
	}, 0o600)
}

// finishUpgrade records the new version in the audit log.
func (i *Installer) finishUpgrade(ctx context.Context, req UpgradeRequest, report *UpgradeReport) {
	if err := i.appendInstalled(ctx, i.layout.HistoryPath(), HistoryEntry{
		Action: ActionUpgrade, Result: "ok", ReleaseID: report.To,
		PreviousReleaseID: report.From, Backup: report.Backup, Mode: report.Mode,
		By: currentUserName(), At: stampNow(i.deps.now()),
	}, 0o600); err != nil {
		i.io.Warn("warning: could not append to the install history: %v", err)
	}
	// The snapshot this run just took is named separately, so pruning cannot
	// delete the archive the history line above just offered as the rollback
	// point — not even when the clock ties its stamp with an older one.
	i.pruneBackups(req.Keep, report.Backup)
}

// installedProbeAddr is the address the health check reaches for an
// installation. A server.yaml whose listen section the first start has not
// filled in yet is served on the default port.
func installedProbeAddr(cfg serverconfig.File) string {
	if cfg.Listen.Empty() {
		return "127.0.0.1:" + serverconfig.DefaultPort
	}
	return probeAddr(cfg.Listen)
}

// replaceBinary moves a staged binary over the installed one. rename is atomic
// within a filesystem and, unlike writing in place, works while the old binary
// is still executing.
// replaceBinary puts the staged binary in place. The mode is deliberately not
// touched here: writeExtracted already set it on the staged file, and a chmod
// would fail when an earlier chownTree handed the tree (and with it this file)
// to the service account — rename only needs a writable directory, which is
// the one permission the operator is guaranteed to have.
func replaceBinary(staged, target string) error {
	return os.Rename(staged, target)
}
