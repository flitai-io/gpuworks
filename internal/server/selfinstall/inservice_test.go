package selfinstall

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSnapshot(content string) func(string) error {
	return func(dest string) error { return os.WriteFile(dest, []byte(content), 0o600) }
}

// Online fixtures represent a server that applied its database policy at
// startup. Offline fixtures retain legacy modes to exercise permission repair.
func (h *harness) prepareInServiceDatabase() {
	h.t.Helper()
	policy, err := h.installer.sensitiveFilePolicy()
	if err != nil {
		h.t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		err := os.Chmod(filepath.Join(h.layout.DataDir, "gpuworks.db")+suffix, policy.Mode())
		if err != nil && !os.IsNotExist(err) {
			h.t.Fatal(err)
		}
	}
}

func openArchive(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func pendingOf(t *testing.T, layout Layout) *PendingUpgrade {
	t.Helper()
	pending, err := readPending(layout)
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

func TestSelectedArchiveMustReportItsPinnedVersionBeforeSnapshot(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.prepareInServiceDatabase()
	archive := openArchive(t, h.writeArchive("v2.0.0", []byte("new binary")))
	_, err := h.installer.StageSelectedInService(context.Background(), "v1.0.0", archive, func(string) error { t.Fatal("snapshot taken for the wrong binary"); return nil }, "admin", "v3.0.0", "attempt")
	if err == nil || readFile(t, h.layout.Binary()) != "old binary" {
		t.Fatalf("mismatched archive: %v", err)
	}
}

func TestStageInServiceSwapsTheBinaryBehindASnapshot(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.prepareInServiceDatabase()
	archive := h.writeArchive("v2.0.0", []byte("new binary"))

	up, err := h.installer.StageInService(context.Background(), "v1.0.0", openArchive(t, archive), writeSnapshot("live snapshot"), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if up.From != "v1.0.0" || up.To != "v2.0.0" || up.Commit != "cafe1234" || up.Backup == "" {
		t.Fatalf("upgrade = %+v", up)
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q", got)
	}
	// The snapshot is the CLI's backup format, with the live copy of the
	// database in place of the file on disk.
	members := readArchive(t, up.Backup)
	if members["bin/gpuworks"] != "old binary" || members["data/gpuworks.db"] != "live snapshot" || members["data/master.key"] != "old key" {
		t.Fatalf("backup members = %v", members)
	}
	if _, ok := members["server.yaml"]; !ok {
		t.Fatalf("backup lacks server.yaml: %v", members)
	}
	pending := pendingOf(t, h.layout)
	if pending == nil || pending.From != "v1.0.0" || pending.To != "v2.0.0" || pending.Backup != up.Backup || pending.By != "admin" || pending.Starts != 0 {
		t.Fatalf("pending = %+v", pending)
	}
	assertNotStaged(t, h)
	if leftovers, _ := filepath.Glob(filepath.Join(h.layout.TmpDir(), "upgrade-*.tar.gz")); len(leftovers) != 0 {
		t.Fatalf("the uploaded archive was left behind: %v", leftovers)
	}
	if _, err := os.Stat(filepath.Join(h.layout.TmpDir(), "gpuworks.db.snapshot")); !os.IsNotExist(err) {
		t.Fatal("the database snapshot was left behind")
	}
}

func TestStageInServiceRefusesAndChangesNothing(t *testing.T) {
	cases := map[string]struct {
		archive  func(h *harness) []byte
		snapshot func(string) error
		prepare  func(h *harness)
		check    func(t *testing.T, err error)
	}{
		"same release": {
			archive: func(h *harness) []byte { return []byte(readFile(h.t, h.writeArchive("v1.0.0", []byte("same")))) },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrAlreadyAtRelease) {
					t.Fatalf("err = %v", err)
				}
			},
		},
		"local build": {
			archive: func(h *harness) []byte {
				path := h.writeArchive("v2.0.0", []byte("dev"))
				h.stagedReports("gpuworks dev (none, unknown)\n")
				return []byte(readFile(h.t, path))
			},
			check: func(t *testing.T, err error) {
				var archiveErr *ArchiveError
				if !errors.As(err, &archiveErr) || !strings.Contains(err.Error(), "local build") {
					t.Fatalf("err = %v", err)
				}
			},
		},
		"not an archive": {
			archive: func(*harness) []byte { return []byte("<html>not a release</html>") },
			check: func(t *testing.T, err error) {
				var archiveErr *ArchiveError
				if !errors.As(err, &archiveErr) {
					t.Fatalf("err = %v", err)
				}
			},
		},
		"snapshot fails": {
			archive:  func(h *harness) []byte { return []byte(readFile(h.t, h.writeArchive("v2.0.0", []byte("new")))) },
			snapshot: func(string) error { return errors.New("database is locked") },
			check: func(t *testing.T, err error) {
				if err == nil || !strings.Contains(err.Error(), "database is locked") {
					t.Fatalf("err = %v", err)
				}
			},
		},
		"release from before console upgrades": {
			archive: func(h *harness) []byte {
				path := h.writeArchive("v0.9.0", []byte("old release"))
				h.stagedPredatesConsoleUpgrades()
				return []byte(readFile(h.t, path))
			},
			check: func(t *testing.T, err error) {
				var archiveErr *ArchiveError
				if !errors.As(err, &archiveErr) || !strings.Contains(err.Error(), "predates console upgrades") ||
					!strings.Contains(err.Error(), "gpuworks upgrade --archive") {
					t.Fatalf("err = %v", err)
				}
			},
		},
		"protocol answer it cannot use": {
			archive: func(h *harness) []byte {
				path := h.writeArchive("v2.0.0", []byte("new"))
				h.run.mu.Lock()
				h.run.output[h.installer.stagedBinary()+" "+ConsoleUpgradeProtocolCommand] = "usage: gpuworks <subcommand>\n"
				h.run.mu.Unlock()
				return []byte(readFile(h.t, path))
			},
			check: func(t *testing.T, err error) {
				var archiveErr *ArchiveError
				if !errors.As(err, &archiveErr) || !strings.Contains(err.Error(), "predates console upgrades") {
					t.Fatalf("err = %v", err)
				}
			},
		},
		"already pending": {
			archive: func(h *harness) []byte { return []byte(readFile(h.t, h.writeArchive("v2.0.0", []byte("new")))) },
			prepare: func(h *harness) {
				if err := os.MkdirAll(h.layout.TmpDir(), 0o755); err != nil {
					h.t.Fatal(err)
				}
				if err := writePending(h.layout, PendingUpgrade{From: "v1.0.0", To: "v1.5.0"}); err != nil {
					h.t.Fatal(err)
				}
			},
			check: func(t *testing.T, err error) {
				if err == nil || !strings.Contains(err.Error(), "already waiting") {
					t.Fatalf("err = %v", err)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.writeExistingInstall("v1.0.0")
			h.prepareInServiceDatabase()
			if tc.prepare != nil {
				tc.prepare(h)
			}
			snapshot := tc.snapshot
			if snapshot == nil {
				snapshot = writeSnapshot("snap")
			}
			_, err := h.installer.StageInService(context.Background(), "v1.0.0", bytes.NewReader(tc.archive(h)), snapshot, "admin")
			tc.check(t, err)
			if got := readFile(t, h.layout.Binary()); got != "old binary" {
				t.Fatalf("binary = %q", got)
			}
			if name != "already pending" && pendingOf(t, h.layout) != nil {
				t.Fatal("a refused upgrade left a marker")
			}
			if backups, _ := filepath.Glob(filepath.Join(h.layout.BackupsDir, "*.tar.gz")); len(backups) != 0 {
				t.Fatalf("a refused upgrade left backups: %v", backups)
			}
			assertNotStaged(t, h)
		})
	}
}

// stageForGuard leaves the installation the way an in-service upgrade does
// right before the restart.
func stageForGuard(t *testing.T) (*harness, InServiceUpgrade) {
	t.Helper()
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.prepareInServiceDatabase()
	up, err := h.installer.StageInService(context.Background(), "v1.0.0", openArchive(t, h.writeArchive("v2.0.0", []byte("new binary"))), writeSnapshot("pre-upgrade database"), "admin")
	if err != nil {
		t.Fatal(err)
	}
	// The new version starts and writes its own database and log.
	writeFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db"), "migrated database", 0o600)
	writeFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db-wal"), "newer wal", 0o600)
	return h, up
}

func TestGuardStartRollsBackAfterTheStartsAreUsed(t *testing.T) {
	h, up := stageForGuard(t)
	now := time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC)
	for start := 1; start <= maxUpgradeStarts; start++ {
		guard, err := GuardStart(h.layout, "v2.0.0", ModeSystemd, now)
		if err != nil || guard.RolledBack || guard.Pending == nil || guard.Pending.Starts != start {
			t.Fatalf("start %d: guard = %+v, %v", start, guard, err)
		}
		if got := readFile(t, h.layout.Binary()); got != "new binary" {
			t.Fatalf("start %d replaced the binary early: %q", start, got)
		}
	}
	guard, err := guardStart(h.layout, "v2.0.0", ModeSystemd, now, credentialGroupFixture)
	if err != nil || !guard.RolledBack {
		t.Fatalf("rollback start: %+v, %v", guard, err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary after rollback = %q", got)
	}
	if got := readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")); got != "pre-upgrade database" {
		t.Fatalf("database after rollback = %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.layout.DataDir, "gpuworks.db-wal")); !os.IsNotExist(err) {
		t.Fatal("the newer version's write-ahead log survived the rollback")
	}
	if pendingOf(t, h.layout) != nil {
		t.Fatal("the marker survived the rollback")
	}
	entries, err := ReadHistory(h.layout.HistoryPath())
	if err != nil || len(entries) != 1 {
		t.Fatalf("history = %+v, %v", entries, err)
	}
	if e := entries[0]; e.Result != "failed" || e.ReleaseID != "v2.0.0" || e.PreviousReleaseID != "v1.0.0" || e.Backup != up.Backup || !strings.Contains(e.Error, "did not come up") {
		t.Fatalf("history entry = %+v", e)
	}
	// A start without a marker is ordinary.
	if guard, err := GuardStart(h.layout, "v2.0.0", ModeSystemd, now); err != nil || guard.Pending != nil || guard.RolledBack {
		t.Fatalf("after rollback: %+v, %v", guard, err)
	}
}

// A marker names the release it waits for. Another release starting means the
// binary was replaced some other way (gpuworks upgrade, by hand, or a release
// from before console upgrades that never read the marker): the marker is
// stale, and counting against it would roll back a release that never failed.
func TestGuardStartDiscardsAMarkerForAnotherRelease(t *testing.T) {
	h, _ := stageForGuard(t)
	now := time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC)
	for start := 1; start <= maxUpgradeStarts+1; start++ {
		guard, err := GuardStart(h.layout, "v3.0.0", ModeSystemd, now)
		if err != nil || guard.Pending != nil || guard.RolledBack {
			t.Fatalf("start %d: guard = %+v, %v", start, guard, err)
		}
		if start == 1 && (guard.Discarded == nil || guard.Discarded.To != "v2.0.0") {
			t.Fatalf("the stale marker was not reported: %+v", guard)
		}
	}
	if pendingOf(t, h.layout) != nil {
		t.Fatal("the stale marker is still there")
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q; nothing may be restored", got)
	}
	if got := readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")); got != "migrated database" {
		t.Fatalf("database = %q; nothing may be restored", got)
	}
	if entries, _ := ReadHistory(h.layout.HistoryPath()); len(entries) != 0 {
		t.Fatalf("history = %+v", entries)
	}
}

func TestConfirmStartRecordsTheUpgrade(t *testing.T) {
	h, up := stageForGuard(t)
	guard, err := GuardStart(h.layout, "v2.0.0", ModeSystemd, time.Now())
	if err != nil || guard.Pending == nil {
		t.Fatal(guard, err)
	}
	if err := ConfirmStart(h.layout, *guard.Pending, ModeSystemd, time.Now(), DefaultBackupKeep); err != nil {
		t.Fatal(err)
	}
	if pendingOf(t, h.layout) != nil {
		t.Fatal("the marker survived the confirmation")
	}
	entries, _ := ReadHistory(h.layout.HistoryPath())
	if len(entries) != 1 || entries[0].Result != "ok" || entries[0].ReleaseID != "v2.0.0" || entries[0].Backup != up.Backup || entries[0].By != "admin" {
		t.Fatalf("history = %+v", entries)
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q", got)
	}
}

func TestWaitConfirmedConfirmsOrAbandons(t *testing.T) {
	answer := "v2.0.0"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","version":"` + answer + `"}`))
	}))
	defer server.Close()

	h, _ := stageForGuard(t)
	guard, _ := GuardStart(h.layout, "v2.0.0", ModeSystemd, time.Now())
	if err := WaitConfirmed(context.Background(), h.layout, *guard.Pending, ModeSystemd, server.URL+"/healthz", time.Minute, DefaultBackupKeep); err != nil {
		t.Fatal(err)
	}
	if pendingOf(t, h.layout) != nil {
		t.Fatal("a healthy new version was not confirmed")
	}

	// A new binary that keeps answering as the old release never confirms;
	// the next start rolls back.
	answer = "v1.0.0"
	h2, _ := stageForGuard(t)
	guard2, _ := GuardStart(h2.layout, "v2.0.0", ModeSystemd, time.Now())
	err := WaitConfirmed(context.Background(), h2.layout, *guard2.Pending, ModeSystemd, server.URL+"/healthz", 10*time.Millisecond, DefaultBackupKeep)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err = %v", err)
	}
	if next, err := guardStart(h2.layout, "v2.0.0", ModeSystemd, time.Now(), credentialGroupFixture); err != nil || !next.RolledBack {
		t.Fatalf("the start after an abandoned upgrade = %+v, %v", next, err)
	}
	if got := readFile(t, h2.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
}

func TestGuardStartKeepsTheUpgradeIntactWhenCredentialGroupLookupFails(t *testing.T) {
	wantErr := errors.New("authorized credential group is unavailable")
	for _, tc := range []struct {
		name   string
		lookup func(string) (*osuser.Group, error)
	}{
		{name: "missing resolver"},
		{name: "failed resolver", lookup: func(string) (*osuser.Group, error) {
			return nil, wantErr
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := stageForGuard(t)
			pending := pendingOf(t, h.layout)
			pending.Starts = maxUpgradeStarts
			if err := writePending(h.layout, *pending); err != nil {
				t.Fatal(err)
			}
			lookup := tc.lookup
			var queried string
			if lookup != nil {
				lookup = func(name string) (*osuser.Group, error) {
					queried = name
					return tc.lookup(name)
				}
			}
			guard, err := guardStart(h.layout, "v2.0.0", ModeSystemd, time.Now(), lookup)
			if err == nil || guard.RolledBack || guard.Pending == nil {
				t.Fatalf("unresolved authorization was silently downgraded: guard=%+v err=%v", guard, err)
			}
			if tc.lookup != nil && !errors.Is(err, wantErr) {
				t.Fatalf("lookup failure was not propagated: %v", err)
			}
			if tc.lookup != nil && queried != DefaultCredentialGroup {
				t.Fatalf("wrong credential group was resolved: %s", queried)
			}
			if readFile(t, h.layout.Binary()) != "new binary" || readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")) != "migrated database" {
				t.Fatal("failed authorization partially restored the installation")
			}
			if pendingOf(t, h.layout) == nil {
				t.Fatal("failed authorization discarded the rollback marker")
			}
		})
	}
}
