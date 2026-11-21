package engine

import (
	"os"
	"path/filepath"
	"testing"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestRuntimeMountManagerPreparesTraversableReadOnlyAssetPath(t *testing.T) {
	state := t.TempDir()
	t.Setenv(runtimeMountRootEnv, filepath.Join(state, "runtime-mounts"))
	manager := NewRuntimeMountManager(state, nil)
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	owner, destination, err := manager.assetPath(digest, ".file")
	if err != nil {
		t.Fatal(err)
	}
	assetsDir := filepath.Dir(destination)
	for _, name := range []string{manager.Root(), assetsDir} {
		info, statErr := os.Stat(name)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode=%o, want 0755", name, info.Mode().Perm())
		}
	}
	if owner.UID != os.Geteuid() || owner.GID != os.Getegid() {
		t.Fatalf("owner=%+v, want current uid/gid", owner)
	}
	if err := os.WriteFile(destination, []byte("asset"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := manager.finalizeReadOnly(destination, owner); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("asset mode=%o, want 0444", info.Mode().Perm())
	}
}

func TestRuntimeMountManagerRejectsSymlinkAndUnsafePurposeKey(t *testing.T) {
	state := t.TempDir()
	manager := NewRuntimeMountManager(state, nil)
	if _, err := manager.PrepareInstanceDir("future-b", "x"); err == nil {
		t.Fatal("unallowlisted purpose accepted")
	}
	if _, err := manager.PrepareInstanceDir("overlays", "../escape"); err == nil {
		t.Fatal("path traversal key accepted")
	}
	if err := os.MkdirAll(manager.Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(manager.Root(), "overlays")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PrepareInstanceDir("overlays", "instance"); err == nil {
		t.Fatal("symlink purpose directory accepted")
	}
}

func TestRuntimeMountManagerRepairsManagedRootModeOnly(t *testing.T) {
	state := t.TempDir()
	root := filepath.Join(state, "runtime-mounts")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewRuntimeMountManager(state, nil)
	if _, err := manager.PrepareInstanceDir("pre-start", "instance"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("managed root mode=%o, want 0755", info.Mode().Perm())
	}
	private := filepath.Join(state, "private")
	if err := os.WriteFile(private, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	privateInfo, err := os.Stat(private)
	if err != nil {
		t.Fatal(err)
	}
	if privateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("unmanaged state mode changed to %o", privateInfo.Mode().Perm())
	}
}

func TestRuntimeMountManagerPreStartPublicationIsAtomicAndIdempotent(t *testing.T) {
	state := t.TempDir()
	manager := NewRuntimeMountManager(state, nil)
	dir, err := manager.PrepareInstanceDir("pre-start", "instance-1")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := manager.mountOwner()
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.writeReadOnlyFileAtomic(dir, "pre-start.sh", []byte("#!/bin/sh\nexec \"$@\"\n"), owner)
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.writeReadOnlyFileAtomic(dir, "pre-start.sh", []byte("#!/bin/sh\nexec \"$@\"\n"), owner)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("identical pre-start reconcile republished a valid file")
	}
	if secondInfo.Mode().Perm() != 0o444 {
		t.Fatalf("wrapper mode=%o, want 0444", secondInfo.Mode().Perm())
	}
}

// A root agent on a local filesystem can traverse and read through
// CAP_DAC_OVERRIDE where the mode bits deny it: a preflight failed on a
// 0711 state directory owned by the sudo caller's uid while the agent ran
// as root. The preflight must ask the kernel when the mount owner is the
// calling identity itself, and must keep the mode-bit verdict when the
// owner is someone else (the kernel cannot speak for a switched identity).
// Without capabilities the kernel answer matches the bits, which pins both
// directions.
func TestRuntimeMountPreflightFallsBackToKernelAccess(t *testing.T) {
	state := t.TempDir()
	manager := &RuntimeMountManager{stateDir: state, root: state, legacy: true}
	owner := agentruntime.MountOwner{UID: os.Geteuid(), GID: os.Getegid()}

	if os.Geteuid() == 0 {
		if err := os.Chown(state, 1000, 1000); err != nil {
			t.Skipf("this user namespace does not map uid 1000: %v", err)
		}
		if err := os.Chmod(state, 0o711); err != nil {
			t.Fatal(err)
		}
		if err := manager.ensureRoot(owner); err != nil {
			t.Fatalf("root agent must pass preflight the kernel allows despite mode bits: %v", err)
		}
		info, err := os.Stat(state)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o711 {
			t.Fatalf("preflight fell back to a chmod: mode=%o, want 0711 kept", info.Mode().Perm())
		}
		// The same false denial is reachable on an ancestor: a 0700
		// directory owned by another uid must not block a root agent's
		// ancestor walk, and a foreign owner must keep the denial. The
		// 0700 mode is what makes this pin the kernel probe: the mode
		// bits deny every class, so only the probe can pass the walk.
		nested := filepath.Join(state, "nested")
		if err := os.Mkdir(nested, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(nested, 1000, 1000); err != nil {
			t.Skipf("this user namespace does not map uid 1000: %v", err)
		}
		if err := agentruntime.CheckAncestorTraversal(nested, owner); err != nil {
			t.Fatalf("root agent must traverse a 0700 foreign-owned ancestor: %v", err)
		}
		if err := agentruntime.CheckAncestorTraversal(nested, agentruntime.MountOwner{UID: 4242, GID: 4242}); err == nil {
			t.Fatal("a foreign owner must keep the mode-bit denial on the ancestor")
		}
		return
	}

	denied := filepath.Join(state, "denied")
	if err := os.Mkdir(denied, 0o300); err != nil {
		t.Fatal(err)
	}
	if err := manager.preflight(denied, owner, true); err == nil {
		t.Fatal("preflight must fail when the kernel also denies search+read on 0300")
	}
	if err := os.Chmod(denied, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := manager.preflight(denied, owner, true); err != nil {
		t.Fatalf("preflight must pass when the mode bits allow: %v", err)
	}
	if err := os.Chmod(denied, 0o700); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(denied, "asset")
	if err := os.WriteFile(file, []byte("asset"), 0o200); err != nil {
		t.Fatal(err)
	}
	if err := manager.preflight(file, owner, false); err == nil {
		t.Fatal("preflight must fail when the kernel also denies read on a 0200 file")
	}
	if err := os.Chmod(file, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := manager.preflight(file, owner, false); err != nil {
		t.Fatalf("preflight must pass when the file bits allow: %v", err)
	}
}
