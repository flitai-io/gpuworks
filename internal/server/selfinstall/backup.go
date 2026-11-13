package selfinstall

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"

	"gpuworks.ai/gpuworks/internal/securefile"
)

// Archive member names. A backup holds exactly what an upgrade must be able to
// put back: the database (with its write-ahead log), the key that decrypts the
// credentials stored in it, and the binary that was running.
const (
	backupDataPrefix = "data/"
	backupBinPrefix  = "bin/"
)

// backupMembers lists the files a snapshot takes from the installation, in the
// order they are written.
func (i *Installer) backupMembers() []string {
	members := []string{
		filepath.Join(i.layout.DataDir, "gpuworks.db"),
		filepath.Join(i.layout.DataDir, "gpuworks.db-wal"),
		filepath.Join(i.layout.DataDir, "gpuworks.db-shm"),
		filepath.Join(i.layout.DataDir, "master.key"),
		i.layout.ConfigPath(),
		i.layout.Binary(),
	}
	return members
}

// requiredBackupMember returns why a snapshot cannot be taken without the
// archive member name, or "" when that member is optional.
//
// Only the master key is required. It is named rather than pointed at, so both
// writers of this archive share one decision: the CLI snapshot takes the file
// from the data directory, the in-service one names the same archive member,
// and each maps a path onto the name it carries inside. It is also the one
// member an upgrade cannot repair: the key decrypts the credentials sealed in
// the database, and a server that starts without the file generates a fresh
// one, which leaves that ciphertext unreadable for good.
//
// Everything else in the member list may legitimately be absent. A database
// that has never been checkpointed has no -wal/-shm, an installation made by
// hand may have no database yet, and a server.yaml can be removed by hand; a
// snapshot missing those is still restorable, so they are skipped.
func requiredBackupMember(name string) string {
	if name == backupDataPrefix+"master.key" {
		return "it decrypts the credentials stored in the database, and a server that starts without " +
			"it generates a new one, which would leave every encrypted credential undecryptable"
	}
	return ""
}

// inspectBackupMember applies the required/optional policy to one member before
// it is archived. It reports whether the member should be written: an absent or
// non-regular required member is an error, an optional one is skipped.
func inspectBackupMember(path, name string) (os.FileInfo, bool, error) {
	info, err := os.Stat(path)
	required := requiredBackupMember(name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if required != "" {
			return nil, false, fmt.Errorf("%s is missing, but %s", path, required)
		}
		return nil, false, nil
	case err != nil:
		return nil, false, err
	case !info.Mode().IsRegular():
		if required != "" {
			return nil, false, fmt.Errorf("%s is not a regular file, but %s", path, required)
		}
		return nil, false, nil
	}
	return info, true, nil
}

// checkSnapshotMembers verifies that the members a snapshot cannot do without
// are there, applying the same policy the archive writer does. It reads no
// member contents, so the upgrade path can call it before the service is
// stopped and refuse while nothing has changed.
func (i *Installer) checkSnapshotMembers() error {
	for _, member := range i.backupMembers() {
		if _, _, err := inspectBackupMember(member, archiveName(i.layout, member)); err != nil {
			return err
		}
	}
	return nil
}

// createBackup archives the current state before an upgrade replaces it and
// returns the archive path.
func (i *Installer) createBackup(ctx context.Context, releaseID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	policy, err := i.sensitiveFilePolicy()
	if err != nil {
		return "", err
	}
	if err := i.hardenSensitiveFiles(ctx, policy); err != nil {
		return "", err
	}
	if err := mkdirAll(i.layout.BackupsDir, dirMode); err != nil {
		return "", err
	}
	at := i.deps.now()
	path := i.layout.BackupPath(releaseID, at)
	if err := i.writeBackup(path, at); err != nil {
		return "", err
	}
	return path, nil
}

// backupEntry is one file a snapshot takes and the name it carries inside.
type backupEntry struct {
	path, name string
}

// writeBackup streams the installation's member files into a tar.gz.
func (i *Installer) writeBackup(path string, at time.Time) error {
	members := i.backupMembers()
	entries := make([]backupEntry, 0, len(members))
	for _, member := range members {
		entries = append(entries, backupEntry{path: member, name: archiveName(i.layout, member)})
	}
	return i.writeBackupEntries(path, at, entries)
}

// writeBackupEntries writes a snapshot through a temporary file so an
// interrupted backup never looks like a usable one.
func (i *Installer) writeBackupEntries(path string, at time.Time, entries []backupEntry) error {
	policy, err := i.sensitiveFilePolicy()
	if err != nil {
		return err
	}
	if err := i.hardenBackupFiles(policy, entries); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(i.layout.BackupsDir, filepath.Base(path)+".part*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	// Backups contain the master key and encrypted credentials together;
	// service-group membership is privileged access to that material.
	if err := policy.Apply(tmp); err != nil {
		tmp.Close()
		return err
	}
	gz := gzip.NewWriter(tmp)
	writer := tar.NewWriter(gz)
	for _, entry := range entries {
		if err := addToArchive(writer, entry.path, entry.name, at); err != nil {
			writer.Close()
			gz.Close()
			tmp.Close()
			return err
		}
	}
	if err := writer.Close(); err != nil {
		gz.Close()
		tmp.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (i *Installer) hardenBackupFiles(policy securefile.Policy, entries []backupEntry) error {
	livePath := filepath.Join(i.layout.DataDir, "gpuworks.db")
	liveInfo, err := os.Lstat(livePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	archivesLive := false
	var snapshots []string
	for _, entry := range entries {
		if entry.name != backupDataPrefix+"gpuworks.db" {
			continue
		}
		info, include, err := inspectBackupMember(entry.path, entry.name)
		if err != nil {
			return err
		}
		if !include {
			continue
		}
		if liveInfo != nil && os.SameFile(liveInfo, info) {
			if filepath.Clean(entry.path) != filepath.Clean(livePath) {
				return errors.New("database snapshot aliases the live database")
			}
			archivesLive = true
		} else {
			snapshots = append(snapshots, entry.path)
		}
	}
	var active []string
	if !archivesLive {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			active = append(active, livePath+suffix)
		}
	}
	if err := hardenFilesWithPolicy(i.layout, policy, active...); err != nil {
		return err
	}
	for _, path := range snapshots {
		if err := protectFile(path, policy); err != nil {
			return err
		}
	}
	return nil
}

// archiveName maps an installed file to the name it carries inside the archive.
func archiveName(layout Layout, member string) string {
	switch member {
	case layout.ConfigPath():
		return serverconfig.FileName
	case layout.Binary():
		return binaryPath()
	default:
		return backupDataPrefix + filepath.Base(member)
	}
}

func binaryPath() string { return backupBinPrefix + binaryName }

// addToArchive writes one member, applying the required/optional policy: an
// optional file that is not there (a database that has never been checkpointed
// has no -wal) is skipped, a required one that is not there fails the snapshot.
// Skipping a required member would hand back an archive that cannot be
// restored, so it is reported here rather than left to the restore path.
func addToArchive(writer *tar.Writer, path, name string, at time.Time) error {
	info, write, err := inspectBackupMember(path, name)
	if err != nil || !write {
		return err
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = name
	header.ModTime = at
	header.Mode = int64(info.Mode().Perm())
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(writer, file)
	return err
}

// restoreBackup puts a snapshot back. The write-ahead log is removed first:
// restoring a database while a stale -wal from the newer version is present
// would leave sqlite replaying changes that belong to a schema it no longer
// has.
//
// It restores exactly what the archive holds and verifies no required member.
// A snapshot that lacks data/master.key therefore unpacks without complaint:
// it leaves whatever key is on disk in place (so the restored database's
// encrypted credentials stay undecryptable, because the key that sealed them
// is gone) or, when there is no key at all, leaves none for the next start to
// replace with a fresh one — the same loss. That is deliberate: a rollback has
// to put the database back even from an incomplete snapshot, or the operator is
// left with neither the old version nor its data. The guard belongs on the way
// in, which is what requiredBackupMember and checkSnapshotMembers enforce, so
// every snapshot this build writes is restorable.
func (i *Installer) restoreBackup(path string) error {
	policy, err := i.sensitiveFilePolicy()
	if err != nil {
		return err
	}
	f, err := securefile.Open(path, policy)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	defer gz.Close()

	if err := i.discardDatabaseLog(); err != nil {
		return err
	}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		target, err := restoreTarget(i.layout, header.Name)
		if err != nil {
			return err
		}
		mode := header.FileInfo().Mode().Perm()
		if mode == 0 {
			// The snapshot's own files always carry a mode; a missing one is a
			// hand-built archive, which gets the conservative defaults.
			mode = 0o600
			if filepath.Base(target) == binaryName {
				mode = binaryMode
			}
		}
		if strings.HasPrefix(header.Name, backupDataPrefix) {
			mode = policy.Mode()
		} else {
			mode &^= 0002
			if _, group := policy.GroupID(); !group {
				mode &^= 0020
			}
		}
		if err := writeRestored(reader, target, mode, policy); err != nil {
			return err
		}
	}
}

// restoreTarget maps an archive member to where it belongs. A member that
// belongs nowhere is an error: silently ignoring it would restore a snapshot
// that is not the one that was taken.
func restoreTarget(layout Layout, name string) (string, error) {
	name = filepath.Clean(name)
	switch {
	case name == serverconfig.FileName:
		return layout.ConfigPath(), nil
	case name == backupBinPrefix+binaryName:
		return layout.Binary(), nil
	case strings.HasPrefix(name, backupDataPrefix):
		base := filepath.Base(name)
		if base != name[len(backupDataPrefix):] {
			return "", fmt.Errorf("backup member %q escapes the data directory", name)
		}
		return filepath.Join(layout.DataDir, base), nil
	default:
		return "", fmt.Errorf("backup member %q does not belong to an installation", name)
	}
}

// writeRestored writes one archive member, keeping the mode it had when the
// snapshot was taken, except sensitive data members are restricted by the
// caller even when restoring an older, publicly-readable backup.
func writeRestored(reader io.Reader, target string, mode os.FileMode, policy securefile.Policy) error {
	if err := mkdirAll(filepath.Dir(target), dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".restore*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, io.LimitReader(reader, maxExtractedBinaryBytes)); err != nil {
		tmp.Close()
		return err
	}
	if err := policy.Apply(tmp); err != nil {
		tmp.Close()
		return err
	}
	if mode != policy.Mode() {
		if err := tmp.Chmod(mode); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// discardDatabaseLog removes the write-ahead log of the database that is about
// to be replaced.
func (i *Installer) discardDatabaseLog() error {
	for _, suffix := range []string{"-wal", "-shm"} {
		path := filepath.Join(i.layout.DataDir, "gpuworks.db"+suffix)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// backupSnapshot is one snapshot in the backup directory and the moment it was
// taken, as its name records it.
type backupSnapshot struct {
	name string
	at   time.Time
}

// backupStampAt reads the UTC timestamp a snapshot's name ends with. The stamp
// is taken from the end of the name, not from its first dash: the release id in
// front of it may contain dashes itself (`dev-0b0801dfd`), and only the tail is
// guaranteed to be the time the snapshot was taken, because that is what
// BackupPath appends.
//
// The stamp is fixed-width, so comparing those sixteen characters would rank
// them correctly too. Parsing is preferred because it also rejects a stamp that
// is the right shape and the wrong time (a month of 13, an hour of 25): a name
// like that is not one this package wrote, and ranking it by a key nobody can
// reason about is how a snapshot gets deleted on a guess.
func backupStampAt(name string) (time.Time, bool) {
	stem, ok := strings.CutSuffix(name, backupSuffix)
	if !ok || len(stem) < len(backupStampLayout)+2 {
		return time.Time{}, false
	}
	stamp := stem[len(stem)-len(backupStampLayout):]
	if stem[len(stem)-len(backupStampLayout)-1] != '-' {
		return time.Time{}, false
	}
	at, err := time.Parse(backupStampLayout, stamp)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// pruneBackups keeps the newest keep snapshots, current never among the ones it
// removes.
//
// Ordering is by the timestamp in the name, not by the whole name and not by
// mtime. The whole name is what used to be used, and it orders by release id
// first: `aaa-…` sorts before `dev-…` and `sit-…`, so the newest snapshot of
// the newest version was the first one deleted. mtime would order correctly on
// the machine that wrote the files, but a snapshot's mtime is carried by
// nothing inside the archive and is rewritten by a copy, a restore or a tar
// round trip, so it can say a five-day-old snapshot is newer than this
// morning's; the name is what the snapshot itself recorded.
//
// A name that does not end in a readable stamp is left alone. It is not
// provably one of ours (a hand-placed copy, or one from a version that named
// its snapshots differently), and deleting the archive an operator was counting
// on because its name could not be parsed is the same mistake as deleting the
// newest one.
func (i *Installer) pruneBackups(keep int, current string) {
	if keep <= 0 {
		return
	}
	entries, err := os.ReadDir(i.layout.BackupsDir)
	if err != nil {
		return
	}
	// The archive this run just wrote is not a pruning candidate. It holds one
	// of the keep slots by definition — it is the newest snapshot by the clock
	// that wrote it — so only the rest are ranked, and nothing can remove it even
	// when the clock puts it among the oldest or ties it with an older archive.
	protected := ""
	if current != "" {
		if info, err := os.Stat(current); err == nil && info.Mode().IsRegular() {
			protected = filepath.Base(current)
		}
	}
	var snapshots []backupSnapshot
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == protected {
			continue
		}
		at, ok := backupStampAt(entry.Name())
		if !ok {
			continue
		}
		snapshots = append(snapshots, backupSnapshot{name: entry.Name(), at: at})
	}
	vacancies := keep
	if protected != "" {
		vacancies--
	}
	if len(snapshots) <= vacancies {
		return
	}
	// Newest first. Two snapshots taken within the same second carry the same
	// stamp and the name cannot tell them apart, so the name breaks the tie and
	// keeps pruning deterministic from one run to the next.
	slices.SortFunc(snapshots, func(a, b backupSnapshot) int {
		if c := b.at.Compare(a.at); c != 0 {
			return c
		}
		return strings.Compare(b.name, a.name)
	})
	for _, snapshot := range snapshots[vacancies:] {
		_ = os.Remove(filepath.Join(i.layout.BackupsDir, snapshot.name))
	}
}
