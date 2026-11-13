package selfinstall

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

// The pre-existing cleanup tests isolate filesystem/service behavior; their
// intentionally non-SQLite data fixture supplies an explicit empty inventory.
func emptyUninstallResources(context.Context, Layout) []uninstallcheck.Item {
	return []uninstallcheck.Item{{ID: "server.fixture", Status: uninstallcheck.Pass, Summary: "No persisted resources in the isolated cleanup fixture"}}
}

func uninstallFixture(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, filepath.Join(t.TempDir(), "server"))
	h.writeExistingInstall("previous")
	writeFile(t, h.layout.LogFile(), "log records", 0o600)
	writeFile(t, filepath.Join(h.layout.BackupsDir, "snapshot.tar.gz"), "backup", 0o600)
	writeFile(t, filepath.Join(h.layout.TmpDir(), "partial"), "download", 0o600)
	return h
}

func assertUninstallAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected %s removed, stat error = %v", path, err)
		}
	}
}

// Snapshots include directory entries and symlink targets, so previews cannot
// silently create a lock or remove an empty directory and still pass.
func uninstallSnapshot(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			value := info.Mode().String()
			if d.Type()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				value += target
			} else if !d.IsDir() {
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				value += string(raw)
			}
			out[path] = value
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestUninstallOrdinaryPreservesRecoveryFiles(t *testing.T) {
	h := uninstallFixture(t)
	keep := []string{h.layout.ConfigPath(), filepath.Join(h.layout.DataDir, "gpuworks.db"), filepath.Join(h.layout.DataDir, "master.key"), h.layout.LogFile(), filepath.Join(h.layout.BackupsDir, "snapshot.tar.gz")}
	before := map[string]string{}
	for _, path := range keep {
		before[path] = readFile(t, path)
	}
	report, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertUninstallAbsent(t, h.layout.Binary(), h.layout.UnitPath(), h.layout.TmpDir())
	for _, path := range keep {
		if got := readFile(t, path); got != before[path] {
			t.Errorf("preserved file %s changed: %q", path, got)
		}
	}
	if report.Mode != ModeSystemd || !slices.Contains(report.Removed, h.layout.UnitPath()) {
		t.Fatalf("inaccurate uninstall report: %+v", report)
	}
	// Retry after an ordinary uninstall remains useful: config and data still
	// identify this installation, although its service and program are gone.
	if _, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil); err != nil {
		t.Fatalf("repeat ordinary uninstall: %v", err)
	}
	if readFile(t, keep[1]) != before[keep[1]] {
		t.Fatal("retry changed the retained database")
	}
}

func TestUninstallPurgeUsesConfiguredPathsAndRetainsUnownedFiles(t *testing.T) {
	h := uninstallFixture(t)
	cfg := h.config()
	cfg.DataDir = filepath.Join(mustEvalSymlinks(t, t.TempDir()), "dedicated-data")
	cfg.Log.File = filepath.Join(mustEvalSymlinks(t, t.TempDir()), "shared-logs", "custom-server.log")
	h.writeConfig(cfg)
	writeFile(t, filepath.Join(cfg.DataDir, "gpuworks.db"), "external database", 0o600)
	writeFile(t, filepath.Join(cfg.DataDir, "master.key"), "external key", 0o600)
	removeLogs := []string{cfg.Log.File, cfg.Log.File + ".1", cfg.Log.File + ".12", filepath.Join(filepath.Dir(cfg.Log.File), "gpuworks-server.stderr.log")}
	for _, path := range removeLogs {
		writeFile(t, path, "owned log", 0o600)
	}
	keep := []string{cfg.Log.File + ".backup", cfg.Log.File + ".1.gz", filepath.Join(filepath.Dir(cfg.Log.File), "other.log"), filepath.Join(h.layout.Dir, "operator-notes"), filepath.Join(h.layout.DataDir, "gpuworks.db")}
	for _, path := range keep[:len(keep)-1] {
		writeFile(t, path, "operator content", 0o600)
	}
	report, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertUninstallAbsent(t, append(removeLogs, cfg.DataDir, h.layout.ConfigPath(), h.layout.Binary(), h.layout.UnitPath(), h.layout.BackupsDir)...)
	for _, path := range keep {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unowned file %s lost: %v", path, err)
		}
	}
	if report.Layout.DataDir != cfg.DataDir || report.Layout.LogsDir != filepath.Dir(cfg.Log.File) {
		t.Fatalf("report did not use configured layout: %+v", report)
	}
	retry, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil)
	if err != nil || !retry.AlreadyAbsent {
		t.Fatalf("repeat purge: report=%+v err=%v", retry, err)
	}
}

func TestUninstallDryRunAndRefusedConfirmationDoNotMutateFiles(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		terminal    bool
		req         UninstallRequest
		wantErr     string
	}{
		{name: "preview", req: UninstallRequest{Resources: emptyUninstallResources, DryRun: true, Purge: true}},
		{name: "nonterminal", wantErr: "--yes"},
		{name: "declined", terminal: true, input: "n\n", wantErr: "cancelled"},
		{name: "default no", terminal: true, input: "\n", wantErr: "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := uninstallFixture(t)
			h.terminal, h.stdin = tc.terminal, tc.input
			before := uninstallSnapshot(t, h.layout.Dir, h.layout.UnitDir, h.layout.LaunchdDir)
			called := false
			tc.req.Resources = emptyUninstallResources
			report, err := h.installer.Uninstall(context.Background(), tc.req, func(context.Context, Layout) error {
				called = true
				return nil
			})
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error=%v, expected %q", err, tc.wantErr)
			}
			if called {
				t.Error("public-entry callback ran without authorization")
			}
			if after := uninstallSnapshot(t, h.layout.Dir, h.layout.UnitDir, h.layout.LaunchdDir); !reflect.DeepEqual(before, after) {
				t.Errorf("filesystem changed: before=%v after=%v", before, after)
			}
			if tc.req.DryRun && !slices.Contains(report.Removed, h.layout.UnitPath()) {
				t.Error("preview omitted the autostart definition")
			}
		})
	}
}

func TestUninstallFailureKeepsRecoveryDataAndProgram(t *testing.T) {
	for _, stage := range []string{"entry point", "stop", "remove unit", "upgrade lock"} {
		t.Run(stage, func(t *testing.T) {
			h := uninstallFixture(t)
			failure := errors.New("injected failure")
			var callback func(context.Context, Layout) error
			switch stage {
			case "entry point":
				callback = func(context.Context, Layout) error { return failure }
			case "stop":
				h.run.fail["sudo systemctl stop"] = failure
			case "remove unit":
				h.run.fail["sudo rm -f "+h.layout.UnitPath()] = failure
			case "upgrade lock":
				unlock, err := h.installer.lockUpgrade(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			before := uninstallSnapshot(t, h.layout.Dir, h.layout.UnitDir)
			_, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, callback)
			if err == nil {
				t.Fatal("uninstall unexpectedly succeeded")
			}
			// The lock file is intentionally persistent; everything else must
			// remain available for a retry after a pre-cleanup failure.
			after := uninstallSnapshot(t, h.layout.Dir, h.layout.UnitDir)
			delete(before, h.layout.LockPath())
			delete(after, h.layout.LockPath())
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("failure destroyed recovery files: before=%v after=%v", before, after)
			}
		})
	}
}

type uninstallPromptReader struct {
	mutate func()
	read   bool
}

func (r *uninstallPromptReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	r.mutate()
	return copy(p, "y\n"), nil
}

func TestUninstallRejectsInstallationChangedDuringConfirmation(t *testing.T) {
	for _, change := range []string{"config", "mode"} {
		t.Run(change, func(t *testing.T) {
			h := uninstallFixture(t)
			h.terminal = true
			h.io.In = &uninstallPromptReader{mutate: func() {
				if change == "config" {
					cfg := h.config()
					cfg.DataDir = filepath.Join(t.TempDir(), "replacement")
					h.writeConfig(cfg)
				} else {
					h.supervisedByNohup()
				}
			}}
			called := false
			_, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Purge: true}, func(context.Context, Layout) error { called = true; return nil })
			if err == nil || !strings.Contains(err.Error(), "changed during confirmation") || called {
				t.Fatalf("stale approval accepted: err=%v callback=%v", err, called)
			}
			if readFile(t, h.layout.Binary()) != "old binary" || readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")) != "old database" {
				t.Fatal("stale approval removed original files")
			}
		})
	}
}

func TestUninstallLeavesSiblingServiceDefinition(t *testing.T) {
	h := uninstallFixture(t)
	other := filepath.Join(t.TempDir(), "sibling", "gpuworks")
	unit := RenderUnit(UnitConfig{Binary: other, ConfigPath: other + ".yaml"})
	writeFile(t, h.layout.UnitPath(), unit, 0o644)
	if _, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, h.layout.UnitPath()); got != unit {
		t.Fatal("sibling service was changed")
	}
	assertUninstallAbsent(t, h.layout.Binary())
	if h.run.ran("sudo systemctl stop") || h.run.ran("sudo systemctl disable") {
		t.Fatal("sibling service was stopped or disabled")
	}
}

func TestUninstallLaunchdRemovesOwnedPlist(t *testing.T) {
	h := uninstallFixture(t)
	h.supervisedByNohup()
	h.launchd, h.systemd = true, false
	writeFile(t, h.layout.LaunchdPath(), RenderPlist(UnitConfig{Binary: h.layout.Binary(), ConfigPath: h.layout.ConfigPath(), ServiceUser: DefaultServiceUser}), 0o644)
	h.run.output["sudo launchctl print system/ai.gpuworks.server"] = "state = running\npid = 4242"
	h.run.on = func(cmd Command) {
		if cmd.Path == "launchctl" && len(cmd.Args) > 0 && cmd.Args[0] == "bootout" {
			h.run.fail["sudo launchctl print"] = errors.New("service not found")
		}
	}
	report, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Mode != ModeLaunchd {
		t.Fatalf("incorrect mode: %+v", report)
	}
	assertUninstallAbsent(t, h.layout.Binary(), h.layout.LaunchdPath())
	if readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")) != "old database" {
		t.Fatal("launchd uninstall lost data")
	}
}

func TestUninstallLegacyInstallWithoutMigratingConfiguration(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(map[bool]string{false: "keep", true: "purge"}[purge], func(t *testing.T) {
			h := newHarness(t, filepath.Join(t.TempDir(), "server"))
			old := h.writeInstallJSONInstall("old", h.freeAddr())
			original := readFile(t, h.layout.installJSONPath())
			if _, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: purge}, nil); err != nil {
				t.Fatal(err)
			}
			assertUninstallAbsent(t, h.layout.Binary(), h.layout.UnitPath(), h.layout.ConfigPath())
			if purge {
				assertUninstallAbsent(t, h.layout.installJSONPath(), old.DataDir)
			} else if got := readFile(t, h.layout.installJSONPath()); got != original {
				t.Fatal("ordinary uninstall changed legacy configuration")
			} else if readFile(t, filepath.Join(old.DataDir, "master.key")) != "old key" {
				t.Fatal("ordinary legacy uninstall lost the master key")
			}
		})
	}
}

func TestUninstallRejectsUnsafeCleanupBeforeMutation(t *testing.T) {
	for _, scenario := range []string{"broad data", "parent data", "symlink data", "dangling data", "log directory", "foreign installation"} {
		t.Run(scenario, func(t *testing.T) {
			h := uninstallFixture(t)
			cfg := h.config()
			switch scenario {
			case "broad data":
				cfg.DataDir = "/"
			case "parent data":
				cfg.DataDir = filepath.Dir(h.layout.Dir)
			case "symlink data", "dangling data":
				link := filepath.Join(t.TempDir(), "data-link")
				target := t.TempDir()
				if scenario == "dangling data" {
					target = filepath.Join(target, "missing")
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				cfg.DataDir = link
			case "log directory":
				cfg.Log.File = filepath.Join(t.TempDir(), "directory-instead-of-file")
				writeFile(t, filepath.Join(cfg.Log.File, "unrelated"), "keep me", 0o600)
			case "foreign installation":
				writeFile(t, filepath.Join(cfg.DataDir, "gpuworks-agent"), "agent binary", 0o755)
			}
			h.writeConfig(cfg)
			before := uninstallSnapshot(t, h.layout.Dir, h.layout.UnitDir)
			called := false
			_, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, func(context.Context, Layout) error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("unsafe cleanup accepted: err=%v callback=%v", err, called)
			}
			if after := uninstallSnapshot(t, h.layout.Dir, h.layout.UnitDir); !reflect.DeepEqual(before, after) {
				t.Fatal("unsafe cleanup mutated installation before rejection")
			}
		})
	}
}

func TestUninstallMissingConfigRefusesOrphanProgram(t *testing.T) {
	h := uninstallFixture(t)
	if err := os.Remove(h.layout.ConfigPath()); err != nil {
		t.Fatal(err)
	}
	_, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "without configuration") {
		t.Fatalf("orphan program treated as safely absent: %v", err)
	}
	if readFile(t, h.layout.Binary()) != "old binary" {
		t.Fatal("orphan program removed")
	}
}
