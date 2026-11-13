package selfinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// In-service upgrade (ADR 0040): the console asks the running server to
// upgrade itself. A process cannot stop and back itself up the way
// `gpuworks upgrade` stops the service, so the order changes but the pieces
// do not: the archive is staged and checked like --archive, the snapshot uses
// the backup format the CLI writes (with a live copy of the database in place
// of the stopped files), and the binary is swapped by rename. The server then
// exits for its service manager to start the new binary. That start counts
// itself against a marker; once the new version answers it confirms the
// upgrade, and when it has failed to come up maxUpgradeStarts times the next
// start restores the snapshot and the previous binary instead.

const (
	pendingUpgradeFile = "upgrade-pending.json"
	// maxUpgradeStarts is how many starts the new binary gets. With the unit's
	// RestartSec=3 they fit well inside systemd's default start limit (5 in
	// 10s is never reached), so the restored binary still gets started.
	maxUpgradeStarts = 3
	// maxUploadedArchiveBytes bounds an archive staged for an in-service
	// upgrade; a release archive is about 40 MiB.
	maxUploadedArchiveBytes = 512 << 20

	// ConsoleUpgradeProtocolCommand is the subcommand a release answers with
	// the console upgrade protocol it speaks: counting its starts against the
	// marker, confirming itself and rolling back. A release from before
	// console upgrades does not know it and exits non-zero.
	ConsoleUpgradeProtocolCommand = "console-upgrade-protocol"
	// ConsoleUpgradeProtocol is the protocol this build speaks.
	ConsoleUpgradeProtocol = 1
)

// ErrAlreadyAtRelease is an archive holding the release that is running.
var ErrAlreadyAtRelease = errors.New("the archive holds the release this server already runs")

// ArchiveError is an archive that cannot be installed: not a release archive,
// built for another platform, or a local build without a release id.
type ArchiveError struct{ Err error }

func (e *ArchiveError) Error() string { return e.Err.Error() }
func (e *ArchiveError) Unwrap() error { return e.Err }

// PendingUpgrade is the marker an in-service upgrade leaves for the starts
// that follow it.
type PendingUpgrade struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Backup    string `json:"backup"`
	By        string `json:"by,omitempty"`
	StartedAt string `json:"started_at"`
	// Starts counts the starts of the new binary so far.
	Starts int `json:"starts"`
}

// PendingUpgradePath is where the marker lives.
func (l Layout) PendingUpgradePath() string { return filepath.Join(l.TmpDir(), pendingUpgradeFile) }

// InServiceUpgrade describes a staged upgrade waiting for the restart.
type InServiceUpgrade struct {
	From    string
	To      string
	Commit  string
	BuiltAt string
	Backup  string
}

// StageInService prepares the running server's next start to be release in
// archive: it stages and checks the binary, snapshots the installation
// (snapshotDB writes a consistent copy of the live database to the path it is
// given), leaves the marker and swaps the binary. The caller restarts the
// process afterwards. Nothing is changed when an error is returned.
func (i *Installer) StageInService(ctx context.Context, from string, archive io.Reader, snapshotDB func(dest string) error, by string) (InServiceUpgrade, error) {
	unlock, err := i.lockUpgrade(ctx)
	if err != nil {
		return InServiceUpgrade{}, err
	}
	defer unlock()
	if _, err := os.Stat(i.layout.PendingUpgradePath()); err == nil {
		return InServiceUpgrade{}, errors.New("an upgrade is already waiting for the server to restart")
	}
	if err := mkdirAll(i.layout.TmpDir(), dirMode); err != nil {
		return InServiceUpgrade{}, err
	}
	archivePath, err := saveArchive(i.layout.TmpDir(), archive)
	if err != nil {
		return InServiceUpgrade{}, err
	}
	defer func() { _ = os.Remove(archivePath) }()
	staged := i.stagedBinary()
	defer func() { _ = os.Remove(staged) }()
	build, err := i.checkArchive(ctx, archivePath, staged)
	if err != nil {
		return InServiceUpgrade{}, err
	}
	if build.ReleaseID == from {
		return InServiceUpgrade{}, ErrAlreadyAtRelease
	}
	if err := i.checkConsoleProtocol(ctx, staged, build.ReleaseID); err != nil {
		return InServiceUpgrade{}, err
	}

	at := i.deps.now()
	snapshot := filepath.Join(i.layout.TmpDir(), "gpuworks.db.snapshot")
	_ = os.Remove(snapshot)
	defer func() { _ = os.Remove(snapshot) }()
	if err := snapshotDB(snapshot); err != nil {
		return InServiceUpgrade{}, fmt.Errorf("snapshot the database: %w", err)
	}
	if err := mkdirAll(i.layout.BackupsDir, dirMode); err != nil {
		return InServiceUpgrade{}, err
	}
	backup := i.layout.BackupPath(from, at)
	if err := i.writeBackupEntries(backup, at, []backupEntry{
		{path: snapshot, name: backupDataPrefix + "gpuworks.db"},
		{path: filepath.Join(i.layout.DataDir, "master.key"), name: backupDataPrefix + "master.key"},
		{path: i.layout.ConfigPath(), name: archiveName(i.layout, i.layout.ConfigPath())},
		{path: i.layout.Binary(), name: binaryPath()},
	}); err != nil {
		return InServiceUpgrade{}, fmt.Errorf("create the pre-upgrade backup: %w", err)
	}

	pending := PendingUpgrade{From: from, To: build.ReleaseID, Backup: backup, By: by, StartedAt: stampNow(at)}
	if err := writePending(i.layout, pending); err != nil {
		return InServiceUpgrade{}, err
	}
	if err := replaceBinary(staged, i.layout.Binary()); err != nil {
		_ = os.Remove(i.layout.PendingUpgradePath())
		return InServiceUpgrade{}, fmt.Errorf("replace the binary: %w", err)
	}
	return InServiceUpgrade{From: from, To: build.ReleaseID, Commit: build.Commit, BuiltAt: build.BuiltAt, Backup: backup}, nil
}

// checkArchive unpacks the binary to staged and asks it what it is, refusing
// what --archive refuses.
func (i *Installer) checkArchive(ctx context.Context, archivePath, staged string) (binaryBuild, error) {
	if err := extractBinary(archivePath, staged); err != nil {
		return binaryBuild{}, &ArchiveError{Err: err}
	}
	goos, goarch := currentPlatform()
	if err := checkBinaryMachine(staged, goarch); err != nil {
		return binaryBuild{}, &ArchiveError{Err: fmt.Errorf("the gpuworks binary is not for this %s/%s host: %w", goos, goarch, err)}
	}
	build, err := i.binaryBuild(ctx, staged)
	if err != nil {
		return binaryBuild{}, &ArchiveError{Err: fmt.Errorf("the gpuworks binary does not run on this %s/%s host: %w", goos, goarch, err)}
	}
	if err := checkArchiveVersion(build.ReleaseID); err != nil {
		return binaryBuild{}, &ArchiveError{Err: err}
	}
	return build, nil
}

// checkConsoleProtocol refuses a release that cannot take part in an
// in-service upgrade. Nothing in such a release reads the marker, so it would
// never confirm itself or roll back, and the marker it leaves behind would be
// read by whatever release starts next. `gpuworks upgrade` installs it
// instead: the CLI watches the new release itself.
func (i *Installer) checkConsoleProtocol(ctx context.Context, staged, releaseID string) error {
	out, err := i.deps.Run.Output(ctx, Command{Path: staged, Args: []string{ConsoleUpgradeProtocolCommand}})
	if err == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(out)); convErr == nil && n >= ConsoleUpgradeProtocol {
			return nil
		}
	}
	return &ArchiveError{Err: fmt.Errorf("release %s cannot be installed from the console: it predates console upgrades and could not "+
		"confirm or roll back its own start; run `gpuworks upgrade --archive <file>` on the server host", releaseID)}
}

// saveArchive writes an uploaded archive to dir, bounded in size.
func saveArchive(dir string, archive io.Reader) (string, error) {
	f, err := os.CreateTemp(dir, "upgrade-*.tar.gz")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(archive, maxUploadedArchiveBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n > maxUploadedArchiveBytes {
		err = &ArchiveError{Err: fmt.Errorf("the archive is larger than %d MiB", maxUploadedArchiveBytes>>20)}
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// StartGuard is what GuardStart found.
type StartGuard struct {
	// Pending is the upgrade this start belongs to, nil when there is none.
	Pending *PendingUpgrade
	// RolledBack means the snapshot was restored: the caller exits so the
	// service manager starts the previous binary.
	RolledBack bool
	// Discarded is a marker left for another release, removed without
	// counting this start.
	Discarded *PendingUpgrade
}

// GuardStart runs at every start of the server, before it opens the
// database; running is the release starting and mode the service mode
// (systemd or nohup) the install history records. Without a marker it does
// nothing. A marker for another release is stale (the binary was replaced
// some other way, by `gpuworks upgrade` or by hand) and is removed: counting
// against it would roll back a release that never failed. Otherwise it counts
// this start; when the new binary has used up its starts it restores the
// snapshot (the database, the master key, server.yaml and the previous
// binary), records the failed upgrade and removes the marker.
func GuardStart(layout Layout, running, mode string, now time.Time) (StartGuard, error) {
	return guardStart(layout, running, mode, now, osuser.LookupGroup)
}

func guardStart(layout Layout, running, mode string, now time.Time, lookupGroup func(string) (*osuser.Group, error)) (StartGuard, error) {
	pending, err := readPending(layout)
	if err != nil || pending == nil {
		return StartGuard{}, err
	}
	if pending.To != running {
		if err := os.Remove(layout.PendingUpgradePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return StartGuard{}, err
		}
		return StartGuard{Discarded: pending}, nil
	}
	pending.Starts++
	if pending.Starts <= maxUpgradeStarts {
		if err := writePending(layout, *pending); err != nil {
			return StartGuard{}, err
		}
		return StartGuard{Pending: pending}, nil
	}
	installer := &Installer{layout: layout, deps: Deps{LookupGroup: lookupGroup}}
	restoreErr := installer.restoreBackup(pending.Backup)
	cause := fmt.Sprintf("%s did not come up after %d starts", pending.To, maxUpgradeStarts)
	if restoreErr != nil {
		cause += "; restoring " + pending.Backup + " failed: " + restoreErr.Error()
	}
	_ = AppendHistory(layout.HistoryPath(), HistoryEntry{
		Action: ActionUpgrade, Result: "failed", ReleaseID: pending.To, PreviousReleaseID: pending.From,
		Backup: pending.Backup, Mode: mode, By: pending.By, At: stampNow(now), Error: cause,
	}, 0o600)
	if restoreErr != nil {
		// Leave the marker counting: the operator restores by hand, and the
		// record above says from which archive.
		return StartGuard{Pending: pending}, fmt.Errorf("roll back the upgrade to %s: %w", pending.To, restoreErr)
	}
	_ = os.Remove(layout.PendingUpgradePath())
	return StartGuard{Pending: pending, RolledBack: true}, nil
}

// ErrHistoryNotRecorded is returned, wrapped, by a confirmation that
// succeeded but could not append its audit line. The upgrade stands: like
// every other history write, a missing record must not undo working software.
var ErrHistoryNotRecorded = errors.New("upgrade confirmed, but the install history could not be appended")

// ConfirmStart records that the pending upgrade came up: the marker goes, the
// install history gains the upgrade, and old snapshots beyond keep are pruned.
// Only failing to remove the marker fails the confirmation; a history that
// cannot be written (a log an older install left to root) is reported as
// ErrHistoryNotRecorded after the upgrade is confirmed.
func ConfirmStart(layout Layout, pending PendingUpgrade, mode string, now time.Time, keep int) error {
	historyErr := AppendHistory(layout.HistoryPath(), HistoryEntry{
		Action: ActionUpgrade, Result: "ok", ReleaseID: pending.To, PreviousReleaseID: pending.From,
		Backup: pending.Backup, Mode: mode, By: pending.By, At: stampNow(now),
	}, 0o600)
	if err := os.Remove(layout.PendingUpgradePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	(&Installer{layout: layout}).pruneBackups(keep, pending.Backup)
	if historyErr != nil {
		return fmt.Errorf("%w: %w", ErrHistoryNotRecorded, historyErr)
	}
	return nil
}

// AbandonStart uses up the pending upgrade's starts, so the next start rolls
// back. A new binary that runs but never becomes healthy calls it before it
// exits; without it nothing would ever restart it.
func AbandonStart(layout Layout) error {
	pending, err := readPending(layout)
	if err != nil || pending == nil {
		return err
	}
	pending.Starts = maxUpgradeStarts
	return writePending(layout, *pending)
}

// WaitConfirmed polls the new server until it answers as release want, then
// confirms the upgrade. If it never does within timeout, the upgrade is
// abandoned and the returned error says so; the caller exits so the next
// start restores the snapshot.
func WaitConfirmed(ctx context.Context, layout Layout, pending PendingUpgrade, mode, probeURL string, timeout time.Duration, keep int) error {
	deadline := time.Now().Add(timeout)
	for {
		got, err := probeHealth(ctx, probeURL)
		if err == nil && (got == "" || strings.TrimSpace(got) == pending.To) {
			return ConfirmStart(layout, pending, mode, time.Now(), keep)
		}
		if time.Now().After(deadline) {
			if abandonErr := AbandonStart(layout); abandonErr != nil {
				return fmt.Errorf("%s did not answer within %s, and marking the upgrade failed did not work: %w", pending.To, timeout, abandonErr)
			}
			return fmt.Errorf("%s did not answer within %s", pending.To, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func readPending(layout Layout) (*PendingUpgrade, error) {
	data, err := os.ReadFile(layout.PendingUpgradePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending PendingUpgrade
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("read %s: %w", layout.PendingUpgradePath(), err)
	}
	return &pending, nil
}

func writePending(layout Layout, pending PendingUpgrade) error {
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return err
	}
	tmp := layout.PendingUpgradePath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, layout.PendingUpgradePath())
}
