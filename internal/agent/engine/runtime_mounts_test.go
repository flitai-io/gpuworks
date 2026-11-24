package engine

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// missingCapability ends a test whose premise this process lacks: the
// filesystem or namespace refuses uid 1000 as an owner, or the kernel
// does not behave toward this process the way the case needs. Strict
// mode turns the lack into a failure.
func missingCapability(t *testing.T, what string, err error) {
	t.Helper()
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatalf("GPUWORKS_TEST_STRICT=1: %s: %v", what, err)
	}
	t.Skipf("%s: %v", what, err)
}

// rootOverridesBits ends the test, as a capability lack, unless the
// kernel lets this root process search and read a directory its mode
// bits close to it: the capability the fix relies on.
func rootOverridesBits(t *testing.T) {
	t.Helper()
	closed := filepath.Join(t.TempDir(), "closed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0o200); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(closed + "/."); err != nil {
		if errors.Is(err, fs.ErrPermission) {
			missingCapability(t, "root cannot search a directory the mode bits close to it", err)
		}
		t.Fatal(err)
	}
	f, err := os.Open(closed)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			missingCapability(t, "root cannot read a directory the mode bits close to it", err)
		}
		t.Fatal(err)
	}
	f.Close()
}

// A root agent on a local filesystem can traverse and read through
// CAP_DAC_OVERRIDE where the mode bits deny it: a preflight failed on a
// 0711 state directory owned by the sudo caller's uid while the agent ran
// as root. The preflight must ask the kernel when the mount owner is the
// calling identity itself, and must keep the mode-bit verdict when the
// owner is someone else (the kernel cannot speak for a switched identity).
//
// The root branch is the proof of the fix, the one case where the kernel's
// answer differs from the mode bits; it needs root to give the directory
// another owner. The non-root branch pins the other direction -- the probe
// does not open what the kernel also denies -- and that such a denial
// carries the kernel's error: without the probe it fails on a bare
// mode-bit verdict.
func TestRuntimeMountPreflightFallsBackToKernelAccess(t *testing.T) {
	state := t.TempDir()
	manager := &RuntimeMountManager{stateDir: state, root: state, legacy: true}
	owner := agentruntime.MountOwner{UID: os.Geteuid(), GID: os.Getegid()}

	if os.Geteuid() == 0 {
		if err := os.Chmod(state, 0o711); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(state, 1000, 1000); err != nil {
			// A user namespace without uid 1000 mapped, or a root without
			// CAP_CHOWN, cannot build the shape; anything else is a fault.
			if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EPERM) {
				missingCapability(t, "cannot give the state directory to uid 1000", err)
			}
			t.Fatal(err)
		}
		// The same false denial is reachable on an ancestor. The directory
		// is a sibling of the state directory, so building and removing it
		// need no write into a directory another user owns.
		nested := filepath.Join(filepath.Dir(state), "nested")
		if err := os.Mkdir(nested, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(nested, 1000, 1000); err != nil {
			t.Fatal(err)
		}
		// 0700 gives root nothing there -- it does not own the directory
		// -- so only the probe can pass the walk; and nothing of root's
		// own reach may answer for the foreign owner.
		blocked, _ := agentruntime.CheckAncestorTraversal(nested, agentruntime.MountOwner{UID: 4242, GID: 4242}).(*agentruntime.TraversalError)
		if blocked == nil || blocked.Dir != nested || blocked.ProbeErr != nil {
			t.Fatalf("a foreign owner must keep the bare mode-bit denial on the ancestor: %v", blocked)
		}
		// What follows needs the kernel to grant root what the bits deny.
		rootOverridesBits(t)
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
		if err := agentruntime.CheckAncestorTraversal(nested, owner); err != nil {
			t.Fatalf("root agent must traverse a 0700 foreign-owned ancestor: %v", err)
		}
		return
	}

	denied := filepath.Join(state, "denied")
	if err := os.Mkdir(denied, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(denied, "asset")
	if err := os.WriteFile(file, []byte("asset"), 0o400); err != nil {
		t.Fatal(err)
	}
	// The directory is tightened below, and then holds a file its owner
	// cannot remove: put it back whatever ends the test.
	t.Cleanup(func() { _ = os.Chmod(denied, 0o700) })
	// What the mode bits allow passes with no probe.
	if err := manager.preflight(file, owner, false); err != nil {
		t.Fatalf("preflight must pass when the file bits allow: %v", err)
	}
	if err := os.Chmod(denied, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := manager.preflight(denied, owner, true); err != nil {
		t.Fatalf("preflight must pass when the mode bits allow: %v", err)
	}
	// The bits deny the read and the kernel denies it too: the verdict is
	// the kernel's error, and the message keeps the facts.
	if err := os.Chmod(denied, 0o300); err != nil {
		t.Fatal(err)
	}
	err := manager.preflight(denied, owner, true)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("preflight on 0300 must be denied with the kernel's error: %v", err)
	}
	if prefix := "runtime mount traverse/read denied for path=" + denied + " "; !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), "mode=0300") {
		t.Fatalf("the denial must name the path and the mode bits: %v", err)
	}
	if err := os.Chmod(denied, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o200); err != nil {
		t.Fatal(err)
	}
	if err := manager.preflight(file, owner, false); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("preflight must be denied with the kernel's error when the kernel also denies read on a 0200 file: %v", err)
	}
}
