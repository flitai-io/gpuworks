package selfinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// upgradeReq is what the command line builds for `gpuworks upgrade`.
func (h *harness) upgradeReq() UpgradeRequest {
	return UpgradeRequest{BaseURL: h.site.baseURL(), Keep: DefaultBackupKeep}
}

func TestUpgradeReplacesTheBinaryAndKeepsABackup(t *testing.T) {
	h := newHarness(t, t.TempDir())
	site := h.useSite("v2.0.0", []byte("new binary"))
	existing := h.writeExistingInstall("v1.0.0")

	report, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}

	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q", got)
	}
	if report.From != "v1.0.0" || report.To != "v2.0.0" || report.RolledBack {
		t.Fatalf("report = %+v", report)
	}
	if !h.run.ranInOrder("sudo systemctl stop gpuworks", "sudo systemctl restart gpuworks") {
		t.Fatalf("the service was not stopped before it was restarted: %v", h.run.executed())
	}
	if !h.run.ran("sudo chown -R " + DefaultServiceUser + ":" + DefaultServiceUser + " " + h.layout.Dir) {
		t.Fatalf("the tree was not handed to the service account: %v", h.run.executed())
	}

	// The snapshot holds the pre-upgrade state, which is what a rollback
	// restores.
	if _, err := os.Stat(report.Backup); err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if !strings.Contains(filepath.Base(report.Backup), "v1.0.0") {
		t.Fatalf("backup name = %q, want the version it replaces", filepath.Base(report.Backup))
	}
	members := readArchive(t, report.Backup)
	if got := members["bin/gpuworks"]; got != "old binary" {
		t.Fatalf("backup binary = %q", got)
	}
	if got := members["data/gpuworks.db"]; got != "old database" {
		t.Fatalf("backup database = %q", got)
	}
	if got := members["data/master.key"]; got != "old key" {
		t.Fatalf("backup master key = %q", got)
	}

	// The upgrade leaves the configuration and the service definition alone.
	if cfg := h.config(); cfg.DataDir != h.layout.DataDir || cfg.Listen.Console[0] != existing.Addr {
		t.Fatalf("the upgrade changed server.yaml: %+v", cfg)
	}
	if h.run.ran("sudo tee " + h.layout.UnitPath()) {
		t.Fatalf("the upgrade rewrote the unit: %v", h.run.executed())
	}
	if _, err := os.Stat(filepath.Join(h.layout.Dir, "install.json")); !os.IsNotExist(err) {
		t.Fatalf("install.json must not be written: %v", err)
	}

	entries, err := ReadHistory(h.layout.HistoryPath())
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("history = %+v", entries)
	}
	entry := entries[0]
	if entry.Action != ActionUpgrade || entry.Result != "ok" || entry.ReleaseID != "v2.0.0" {
		t.Fatalf("history entry = %+v", entry)
	}
	if entry.PreviousReleaseID != "v1.0.0" || entry.Backup != report.Backup || entry.By == "" {
		t.Fatalf("history entry = %+v", entry)
	}
	if site.Release.Channel != "preview" || report.Commit != "deadbeef" {
		t.Fatalf("report = %+v", report)
	}
}

func TestUpgradeKeepsTheConfiguredServiceSettings(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.supervisedByNohup()
	cfg := h.config()
	cfg.MetricsRetention = "168h"
	cfg.Log.MaxSize, cfg.Log.Keep = "5MB", 4
	h.writeConfig(cfg)
	before := readFile(t, h.layout.ConfigPath())
	h.useSite("v2.0.0", []byte("new binary"))

	if _, err := h.installer.Upgrade(context.Background(), h.upgradeReq()); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if len(h.starts) != 1 {
		t.Fatalf("detached starts = %v", h.starts)
	}
	if joined := strings.Join(h.starts[0], " "); joined != h.layout.Binary()+" serve --supervise --config "+h.layout.ConfigPath() {
		t.Errorf("the restarted command line = %s", joined)
	}
	if after := readFile(t, h.layout.ConfigPath()); after != before {
		t.Errorf("the upgrade changed server.yaml:\n%s", after)
	}
	if h.run.ran("sudo systemctl") {
		t.Fatalf("a nohup installation must not be restarted through systemd: %v", h.run.executed())
	}
}

func TestUpgradeIsANoOpWhenAlreadyCurrent(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v2.0.0")
	h.useSite("v2.0.0", []byte("new binary"))

	report, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !report.UpToDate || report.Checked {
		t.Fatalf("report = %+v", report)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("an up-to-date installation was restarted: %v", h.run.executed())
	}
	if _, err := os.Stat(h.layout.BackupsDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an up-to-date installation was backed up: %v", err)
	}
}

func TestUpgradeCheckStopsBeforeTouchingAnything(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	req := h.upgradeReq()
	req.Check = true

	report, err := h.installer.Upgrade(context.Background(), req)
	if err != nil {
		t.Fatalf("upgrade --check: %v", err)
	}
	if !report.Checked || report.UpToDate {
		t.Fatalf("report = %+v", report)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("--check replaced the binary: %q", got)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("--check stopped the service: %v", h.run.executed())
	}
}

func TestUpgradeRefusesALocalBuildWithoutForce(t *testing.T) {
	for _, local := range []string{"", "dev", "v1.0.0-dirty"} {
		t.Run("local="+local, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.writeExistingInstall(local)
			h.useSite("v2.0.0", []byte("new binary"))

			_, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
			if err == nil || !strings.Contains(err.Error(), "--force") {
				t.Fatalf("err = %v", err)
			}
			if got := readFile(t, h.layout.Binary()); got != "old binary" {
				t.Fatalf("binary = %q", got)
			}

			// --force replaces it, which is how a locally built installation is
			// brought onto a released version.
			h2 := newHarness(t, t.TempDir())
			h2.writeExistingInstall(local)
			h2.useSite("v2.0.0", []byte("new binary"))
			req := h2.upgradeReq()
			req.Force = true
			if _, err := h2.installer.Upgrade(context.Background(), req); err != nil {
				t.Fatalf("upgrade --force: %v", err)
			}
			if got := readFile(t, h2.layout.Binary()); got != "new binary" {
				t.Fatalf("binary = %q", got)
			}
		})
	}
}

func TestUpgradeRequiresAnInstallation(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.useSite("v2.0.0", []byte("new binary"))

	_, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "gpuworks install --dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpgradeRejectsAPinnedVersionTheSiteDoesNotServe(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	req := h.upgradeReq()
	req.Version = "v1.5.0"

	_, err := h.installer.Upgrade(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "the site is at v2.0.0") {
		t.Fatalf("err = %v", err)
	}

	req.Version = "v2.0.0"
	if _, err := h.installer.Upgrade(context.Background(), req); err != nil {
		t.Fatalf("a matching --version must be accepted: %v", err)
	}
}

func TestUpgradeRejectsACorruptedDownload(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	site := h.useSite("v2.0.0", []byte("new binary"))
	name := site.Release.Files[0].Name
	// Same length, different bytes: only the checksum can catch this.
	site.corrupt(name, bytes.Repeat([]byte("x"), len(site.Bodies[name])))

	_, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("the service was stopped for a download that never verified: %v", h.run.executed())
	}
}

func TestUpgradeRejectsATruncatedDownload(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	site := h.useSite("v2.0.0", []byte("new binary"))
	site.corrupt(site.Release.Files[0].Name, []byte("short"))

	_, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "the release manifest says") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpgradeRejectsAnArchiveWithoutTheBinary(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	h.site.Release.Files[0].Size = int64(len(tarGz(t, map[string]string{"README": "nothing here"})))
	name := h.site.Release.Files[0].Name
	body := tarGz(t, map[string]string{"README": "nothing here"})
	h.site.corrupt(name, body)
	h.site.Release.Files[0].SHA256 = sha256Hex(body)
	h.site.Release.Files[0].Size = int64(len(body))

	_, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "does not contain a gpuworks binary") {
		t.Fatalf("err = %v", err)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("the service was stopped for an archive that has no binary: %v", h.run.executed())
	}
}

func TestUpgradeRollsBackWhenTheNewVersionNeverAnswers(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	// The replacement starts and then fails its readiness probe.
	h.deps.Health = func(context.Context, string) (string, error) { return "", errors.New("connection refused") }
	h.installer = New(h.layout, h.deps, h.io)

	report, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil {
		t.Fatal("upgrade reported success although the new version never answered")
	}
	if !report.RolledBack || report.Backup == "" {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(err.Error(), "rolled back") || !strings.Contains(err.Error(), report.Backup) {
		t.Fatalf("the error does not point at the rollback: %v", err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("the previous binary was not restored: %q", got)
	}
	if got := readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")); got != "old database" {
		t.Fatalf("the database was not restored: %q", got)
	}
	if !h.run.ranInOrder("sudo systemctl stop gpuworks", "sudo systemctl restart gpuworks") {
		t.Fatalf("the previous version was not started again: %v", h.run.executed())
	}
	if len(h.run.calls) == 0 {
		t.Fatal("nothing ran")
	}
	if !h.run.ran("sudo systemctl restart gpuworks") {
		t.Fatalf("the service was never restarted: %v", h.run.executed())
	}
	if got := countCommands(h, "sudo systemctl restart gpuworks"); got != 2 {
		t.Fatalf("restarts = %d, want one for the new version and one for the rollback: %v", got, h.run.executed())
	}
	// The snapshot is unpacked by root, so the database and the master key have
	// to be handed back to the service account before it is started again;
	// otherwise the restored version crashes on its own database.
	if !h.run.ranInOrder("sudo systemctl stop gpuworks",
		"sudo chown -R "+DefaultServiceUser+":"+DefaultServiceUser+" "+h.layout.Dir,
		"sudo systemctl restart gpuworks") {
		t.Fatalf("the rollback did not hand the tree back before the restart: %v", h.run.executed())
	}

	entries, err := ReadHistory(h.layout.HistoryPath())
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if len(entries) != 1 || entries[0].Result != "failed" || entries[0].ReleaseID != "v2.0.0" {
		t.Fatalf("history = %+v", entries)
	}
	if entries[0].Error == "" || entries[0].PreviousReleaseID != "v1.0.0" {
		t.Fatalf("history entry = %+v", entries[0])
	}
}

// The previous server kept the port (a stop that could not reach it): the
// new binary cannot bind, and /healthz is answered by the old version. That
// answer is not the upgrade coming up, so the upgrade rolls back instead of
// reporting success while the old release keeps serving.
func TestUpgradeRollsBackWhenTheOldVersionStillAnswers(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	h.deps.Health = func(context.Context, string) (string, error) { return "v1.0.0", nil }
	h.installer = New(h.layout, h.deps, h.io)

	report, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil {
		t.Fatal("upgrade reported success although only the old version answered")
	}
	if !report.RolledBack || !strings.Contains(err.Error(), "reports version v1.0.0, not v2.0.0") {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("the previous binary was not restored: %q", got)
	}
	// The rollback waits for the version it restored, which is the one answering.
	if !strings.Contains(h.errOut.String(), "upgrade failed") || strings.Contains(h.errOut.String(), "did not come back cleanly") {
		t.Fatalf("the restored version was not accepted: %s", h.errOut.String())
	}
}

func TestUpgradeRollsBackWhenTheBinaryCannotBeStarted(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	// The first restart fails, which is what an unexecutable binary looks like
	// from here; the rollback's restart then succeeds.
	h.run.on = func(cmd Command) {
		if cmd.String() == "sudo systemctl restart gpuworks" && countCommands(h, "sudo systemctl restart gpuworks") == 1 {
			h.run.fail["sudo systemctl restart gpuworks"] = errors.New("exit status 1")
		}
	}

	report, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil || !report.RolledBack {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
}

// A root-level installation has no service account to hand the tree to; the
// rollback must still restore and restart it, just without a chown.
func TestUpgradeRollbackWithoutAServiceAccountSkipsTheChown(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	writeFile(t, h.layout.UnitPath(), RenderUnit(UnitConfig{
		Binary: h.layout.Binary(), ConfigPath: h.layout.ConfigPath(), DataDir: h.layout.DataDir, ServiceUser: "root",
	}), 0o644)
	h.useSite("v2.0.0", []byte("new binary"))
	h.deps.Health = func(context.Context, string) (string, error) { return "", errors.New("connection refused") }
	h.installer = New(h.layout, h.deps, h.io)

	_, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil {
		t.Fatal("upgrade reported success although the new version never answered")
	}
	if h.run.ran("sudo chown") {
		t.Fatalf("a root-level installation must not be chowned: %v", h.run.executed())
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
}

func TestUpgradeKeepsOnlyTheNewestBackups(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	for day := 1; day <= 4; day++ {
		name := fmt.Sprintf("v1.0.0-2026010%dT000000Z.tar.gz", day)
		writeFile(t, filepath.Join(h.layout.BackupsDir, name), "old snapshot", 0o600)
	}
	req := h.upgradeReq()
	req.Keep = 3

	report, err := h.installer.Upgrade(context.Background(), req)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	names := backupNames(t, h.layout.BackupsDir)
	if len(names) != 3 {
		t.Fatalf("backups = %v, want the newest three", names)
	}
	if _, err := os.Stat(report.Backup); err != nil {
		t.Fatalf("the snapshot just taken was pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.layout.BackupsDir, "v1.0.0-20260101T000000Z.tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the oldest backup survived pruning")
	}
}

// GWKS-146: `--keep N` deleted the newest snapshot first when the release id
// sorted it first. The upgrade path is what the operator runs, so the guarantee
// is asserted here end to end, with the three release ids the ticket measured:
// `aaa-keep20000` sorts before `dev-…` and `sit-…`, and it is the newest.
func TestUpgradePrunesByTimeNotReleaseID(t *testing.T) {
	h := newHarness(t, t.TempDir())
	// The snapshot this run takes lands a few seconds after the two already on
	// disk, and it carries the release id that sorts first of all three.
	h.now = time.Date(2026, 10, 24, 9, 24, 15, 0, time.UTC)
	h.useSite("zzz-next-release", []byte("new binary"))
	// The installation upgraded away from aaa-keep20000 first, so the two older
	// snapshots are already there when this run takes its own.
	h.writeExistingInstall("aaa-keep20000")
	for _, name := range []string{
		"dev-0b0801dfd-20261024T092315Z.tar.gz",
		"sit-zkeep1000-20261024T092327Z.tar.gz",
	} {
		writeFile(t, filepath.Join(h.layout.BackupsDir, name), "old snapshot", 0o600)
	}
	req := h.upgradeReq()
	req.Keep = 2

	report, err := h.installer.Upgrade(context.Background(), req)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if got := filepath.Base(report.Backup); got != "aaa-keep20000-20261024T092415Z.tar.gz" {
		t.Fatalf("snapshot = %q, want it named by the release it replaced and the time it was taken", got)
	}
	got, want := backupNames(t, h.layout.BackupsDir), []string{
		"aaa-keep20000-20261024T092415Z.tar.gz",
		"sit-zkeep1000-20261024T092327Z.tar.gz",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("survivors = %v, want the newest two %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(h.layout.BackupsDir, "dev-0b0801dfd-20261024T092315Z.tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the oldest snapshot survived pruning")
	}
}

func TestUpgradeRefusesToRunTwiceAtOnce(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	if err := os.MkdirAll(h.layout.Dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(h.layout.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer f.Close()
	locked, err := lockFile(f)
	if err != nil || !locked {
		t.Fatalf("take the lock: %v", err)
	}

	_, err = h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("the locked upgrade touched the service: %v", h.run.executed())
	}
}

func TestUpgradeReportsAnUnreachableSite(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	h.site.Close()
	req := h.upgradeReq()

	_, err := h.installer.Upgrade(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "release manifest") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpgradeRestartsTheOldVersionWhenTheBackupFails(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	// A snapshot cannot be written where a directory already holds its name.
	// The timestamp is the harness's fixed clock, so this is deterministic.
	blocked := h.layout.BackupPath("v1.0.0", h.now)
	// Inject the conflict after preflight hardening, when stopping the old
	// service has already succeeded, so this still tests backup-failure recovery.
	h.run.on = func(cmd Command) {
		if cmd.String() == "sudo systemctl stop gpuworks" {
			if err := os.MkdirAll(blocked, 0755); err != nil {
				t.Fatalf("block backup after stop: %v", err)
			}
		}
	}

	report, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "pre-upgrade backup") {
		t.Fatalf("err = %v", err)
	}
	if report.Backup != "" || report.RolledBack {
		t.Fatalf("report = %+v", report)
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
	if !h.run.ran("sudo systemctl restart gpuworks") {
		t.Fatalf("the previous version was left stopped: %v", h.run.executed())
	}
	if _, err := ReadHistory(h.layout.HistoryPath()); err == nil {
		if entries, _ := ReadHistory(h.layout.HistoryPath()); len(entries) > 0 && entries[len(entries)-1].Result == "ok" {
			t.Fatalf("the history claims an upgrade that never happened: %+v", entries)
		}
	}
}

func TestUpgradeFailsBeforeStoppingWhenTheTreeIsUnwritable(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	// A file where the backup directory belongs: the upgrade notices before it
	// touches the running service. The privileged repair attempt fails too —
	// sudo cannot mkdir over a file — so the failure surfaces right here.
	writeFile(t, h.layout.BackupsDir, "not a directory", 0o600)
	h.run.fail["sudo mkdir -p "+h.layout.BackupsDir] = errors.New("exit status 1")

	_, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil {
		t.Fatal("upgrade succeeded with an unusable install tree")
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("the service was stopped for an upgrade that never started: %v", h.run.executed())
	}
}
