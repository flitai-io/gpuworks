package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// stateDir returns a fresh directory with mode whose ancestors any identity
// can traverse; t.TempDir's per-test parent is 0700.
func stateDir(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runtimeIn is a runtime identity other than the agent whose primary group is
// the agent's own (sameGroup) or one the directory does not carry.
func runtimeIn(sameGroup bool) MountOwner {
	gid := os.Getegid()
	if !sameGroup {
		gid++
	}
	return MountOwner{UID: os.Geteuid() + 1, GID: gid, Username: "runtime"}
}

func assertDirMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func assertGroup(t *testing.T, path string, want int) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != os.Geteuid() || int(stat.Gid) != want {
		t.Fatalf("%s owner = %d:%d, want %d:%d", path, stat.Uid, stat.Gid, os.Geteuid(), want)
	}
}

func TestPrepareTraversalGrantsRuntimeGroupSearchOnly(t *testing.T) {
	for _, tc := range []struct {
		mode, want os.FileMode
	}{
		{0o700, 0o710}, // the fresh agent state directory
		{0o740, 0o750}, // the runtime group already reads it
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			dir := stateDir(t, tc.mode)
			private := filepath.Join(dir, "identity.json")
			if err := os.WriteFile(private, []byte("private identity"), 0o600); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := PrepareTraversal(dir, runtimeIn(true)); err != nil {
					t.Fatal(err)
				}
				assertDirMode(t, dir, tc.want)
				assertDirMode(t, private, 0o600)
				assertGroup(t, dir, os.Getegid())
			}
		})
	}
}

// A group that holds its own permissions keeps them: moving the directory to
// the runtime group would revoke that group's access or hand its read bits to
// the runtime. Others gain search instead, which still cannot list or write.
func TestPrepareTraversalFallsBackToOtherSearchWhenGroupIsInUse(t *testing.T) {
	for _, tc := range []struct {
		mode, want os.FileMode
	}{
		{0o750, 0o751},
		{0o740, 0o741},
		{0o710, 0o711},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			dir := stateDir(t, tc.mode)
			if err := PrepareTraversal(dir, runtimeIn(false)); err != nil {
				t.Fatal(err)
			}
			assertDirMode(t, dir, tc.want)
			assertGroup(t, dir, os.Getegid())
		})
	}
}

func TestPrepareTraversalLeavesTraversableDirectoriesUnchanged(t *testing.T) {
	for _, mode := range []os.FileMode{0o711, 0o755, 0o775, 0o701} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := stateDir(t, mode)
			if err := PrepareTraversal(dir, runtimeIn(false)); err != nil {
				t.Fatal(err)
			}
			assertDirMode(t, dir, mode)
		})
	}
}

func TestPrepareTraversalRejectsWritableStateDirectories(t *testing.T) {
	for _, mode := range []os.FileMode{0o720, 0o702, 0o760} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := stateDir(t, mode)
			err := PrepareTraversal(dir, runtimeIn(true))
			if err == nil || !strings.Contains(err.Error(), "not be writable by group or others") {
				t.Fatalf("writable state directory: err=%v", err)
			}
			assertDirMode(t, dir, mode)
		})
	}
}

func TestPrepareTraversalResolvesConfiguredSymlink(t *testing.T) {
	dir := stateDir(t, 0o700)
	link := filepath.Join(stateDir(t, 0o755), "state-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := PrepareTraversal(link, runtimeIn(true)); err != nil {
		t.Fatalf("configured state symlink was rejected: %v", err)
	}
	assertDirMode(t, dir, 0o710)
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("configured state symlink was replaced")
	}
}

// Ancestors are the operator's directories: an untraversable one is reported,
// never opened up, and does not stop the state directory's own grant.
func TestCheckAncestorTraversalReportsWithoutChanges(t *testing.T) {
	// The last check walks every ancestor up to /, so the chain is rooted in
	// /tmp: t.TempDir sits under the per-user 0700 $TMPDIR on macOS, which no
	// other uid may traverse. A new directory takes its parent's group there
	// (wheel, under /tmp), so base is handed the agent's group first.
	base, err := os.MkdirTemp("/tmp", "traversal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chown(base, -1, os.Getegid()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	outer := filepath.Join(base, "outer")
	if err := os.Mkdir(outer, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(outer, "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PrepareTraversal(dir, runtimeIn(true)); err != nil {
		t.Fatal(err)
	}
	assertDirMode(t, dir, 0o710)
	err = CheckAncestorTraversal(filepath.Dir(dir), runtimeIn(true))
	if err == nil || !strings.Contains(err.Error(), outer+" is not traversable") {
		t.Fatalf("untraversable ancestor: err=%v", err)
	}
	assertDirMode(t, outer, 0o700)
	if err := CheckAncestorTraversal(dir, runtimeIn(true)); err == nil {
		t.Fatal("ancestor check skipped the closed parent")
	}
	if err := os.Chmod(outer, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := CheckAncestorTraversal(dir, runtimeIn(true)); err != nil {
		t.Fatalf("traversable chain reported: %v", err)
	}
}

// The blocking directory is a typed result, not only text: the doctor reads
// the directory, its mode and owner and the fix from it.
func TestCheckAncestorTraversalReturnsATypedVerdict(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "traversal-typed-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(base, "my home")
	state := filepath.Join(private, "gpuworks")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	owner := runtimeIn(false)
	err = CheckAncestorTraversal(filepath.Dir(state), owner)
	var blocked *TraversalError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want *TraversalError", err)
	}
	if blocked.Dir != private || blocked.Mode != 0o700 || blocked.DirUID != os.Geteuid() || blocked.Owner != owner {
		t.Fatalf("verdict = %+v", blocked)
	}
	if got, want := blocked.FixCommand(), "chmod o+x '"+private+"'"; got != want {
		t.Fatalf("fix = %q, want %q", got, want)
	}
	// A directory that does not exist is a stat failure, not a closed directory.
	err = CheckAncestorTraversal(filepath.Join(base, "missing", "deeper"), owner)
	var typed *TraversalError
	if err == nil || errors.As(err, &typed) || !strings.Contains(err.Error(), "stat parent") {
		t.Fatalf("a stat failure must stay a plain error: %v", err)
	}
}

func TestPrepareTraversalRejectsFilesAndMissingPaths(t *testing.T) {
	file := filepath.Join(stateDir(t, 0o755), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareTraversal(file, runtimeIn(true)); err == nil {
		t.Fatal("file accepted as state directory")
	}
	if err := PrepareTraversal(filepath.Join(stateDir(t, 0o755), "missing"), runtimeIn(true)); err == nil {
		t.Fatal("missing state directory accepted")
	}
}

func TestHasPermissionUsesExactlyOneClass(t *testing.T) {
	dir := stateDir(t, 0o701)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The group class applies to a member even when others hold more bits.
	if hasPermission(info, MountOwner{UID: os.Geteuid() + 1, GID: os.Getegid()}, 0o1) {
		t.Fatal("group member granted search through the other class")
	}
	if !hasPermission(info, runtimeIn(false), 0o1) {
		t.Fatal("other identity denied search granted to others")
	}
	if !hasPermission(info, MountOwner{UID: os.Geteuid(), GID: os.Getegid() + 1}, 0o7) {
		t.Fatal("owner denied its own bits")
	}
}
