package selfinstall

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestUninstallSharedApplicationRootPreservesAgent(t *testing.T) {
	for _, mode := range []string{ModeSystemd, ModeNohup} {
		for _, operation := range []string{"ordinary", "purge", "preview"} {
			t.Run(mode+"/"+operation, func(t *testing.T) {
				root, agent := withSharedRoot(t)
				h := newHarness(t, root)
				h.writeExistingInstall("previous")
				if mode == ModeNohup {
					h.supervisedByNohup()
				}
				for _, rel := range []string{"gpuworks-agent", "gpuworks-agent.conf", "gpuworks-agent.pid", "agent-restart.sh", "operator-notes"} {
					path := filepath.Join(root, rel)
					writeFile(t, path, "keep "+rel, 0o600)
					agent = append(agent, path)
				}
				writeFile(t, h.layout.LogFile(), "server logs", 0o600)
				writeFile(t, filepath.Join(h.layout.BackupsDir, "snapshot.tar.gz"), "server backup", 0o600)
				writeFile(t, filepath.Join(h.layout.TmpDir(), "partial"), "server download", 0o600)
				before := uninstallSnapshot(t, root, h.layout.UnitDir)
				preserved := uninstallSnapshot(t, agent...)
				req := UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: operation != "ordinary", DryRun: operation == "preview"}
				report, err := h.installer.Uninstall(t.Context(), req, nil)
				if err != nil {
					t.Fatalf("historical shared-root uninstall rejected: %v", err)
				}
				if report.Mode != mode || slices.Contains(report.Removed, root) {
					t.Fatalf("incorrect uninstall report: %+v", report)
				}
				if after := uninstallSnapshot(t, agent...); !reflect.DeepEqual(preserved, after) {
					t.Fatal("uninstall changed agent state or unrelated files")
				}
				if req.DryRun {
					if after := uninstallSnapshot(t, root, h.layout.UnitDir); !reflect.DeepEqual(before, after) {
						t.Fatal("preview changed the shared installation")
					}
					return
				}
				assertUninstallAbsent(t, h.layout.Binary(), h.layout.UnitPath(), h.layout.TmpDir())
				if req.Purge {
					assertUninstallAbsent(t, h.layout.ConfigPath(), h.layout.DataDir, h.layout.LogFile(), h.layout.BackupsDir)
					if !slices.Contains(report.Kept, root) {
						t.Fatal("purge report omitted the retained shared root")
					}
				} else {
					for _, path := range []string{h.layout.ConfigPath(), filepath.Join(h.layout.DataDir, "gpuworks.db"), filepath.Join(h.layout.DataDir, "master.key"), h.layout.LogFile(), filepath.Join(h.layout.BackupsDir, "snapshot.tar.gz")} {
						if after := uninstallSnapshot(t, path); after[path] != before[path] {
							t.Errorf("ordinary uninstall changed recovery file %s", path)
						}
					}
				}
				if _, err := os.Stat(h.layout.LockPath()); err != nil {
					t.Fatalf("uninstall lost the shared lock: %v", err)
				}
				if _, err := h.installer.Uninstall(t.Context(), req, nil); err != nil {
					t.Fatalf("repeating shared-root uninstall: %v", err)
				}
			})
		}
	}
}

func TestUninstallSharedApplicationRootRefusesRecursiveRootCleanup(t *testing.T) {
	root, _ := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("previous")
	cfg := h.config()
	cfg.DataDir = root
	h.writeConfig(cfg)
	before := uninstallSnapshot(t, root, h.layout.UnitDir)
	called := false
	_, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, func(context.Context, Layout) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "refusing recursive changes to broad directory") || called {
		t.Fatalf("shared-root data cleanup accepted: err=%v callback=%v", err, called)
	}
	if after := uninstallSnapshot(t, root, h.layout.UnitDir); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected shared-root cleanup changed files")
	}
}

func TestUninstallSharedApplicationRootRejectsSymlinkAlias(t *testing.T) {
	root, _ := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("previous")
	alias := filepath.Join(t.TempDir(), "install-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	h.installer.layout.Dir = alias
	before := uninstallSnapshot(t, root, h.layout.UnitDir)
	called := false
	_, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, func(context.Context, Layout) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "resolves through a symlink") || called {
		t.Fatalf("shared-root alias accepted: err=%v callback=%v", err, called)
	}
	if after := uninstallSnapshot(t, root, h.layout.UnitDir); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected symlink alias changed files")
	}
}
