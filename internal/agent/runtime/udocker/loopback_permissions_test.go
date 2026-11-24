package udocker

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestHostRootLoopbackRestrictiveUmask(t *testing.T) {
	const childEnv = "GPUWORKS_TEST_LOOPBACK_UMASK_CHILD"
	if os.Getenv(childEnv) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(executable, "-test.run=^"+t.Name()+"$", "-test.count=1")
		command.Env = append(os.Environ(), childEnv+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("umask subprocess failed: %v\n%s", err, output)
		}
		return
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	outer, err := os.MkdirTemp("/tmp", "gpuworks-loopback-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outer) })
	outer, err = filepath.EvalSymlinks(outer)
	if err != nil {
		t.Fatal(err)
	}
	d := New(filepath.Join(outer, "state"))
	root := filepath.Join(d.udockerDir, "containers", "real-id", "ROOT")
	if err := os.MkdirAll(filepath.Join(root, "usr", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Host-side fixture traversal is independent of the mirrored guest path.
	for path := filepath.Join(root, "usr", "lib"); ; path = filepath.Dir(path) {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if path == outer {
			break
		}
	}
	const content = "loaded HIP library bytes\x00\x01\xff\n"
	library := filepath.Join(root, "usr", "lib", "libhip-fixture.so")
	if err := os.WriteFile(library, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(library, 0o644); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(outer, "private-ledger")
	if err := os.WriteFile(private, []byte("private ownership token"), 0o600); err != nil {
		t.Fatal(err)
	}
	loopback := filepath.Join(root, strings.TrimPrefix(root, "/"))
	mirroredLibrary := filepath.Join(loopback, "usr", "lib", "libhip-fixture.so")
	readAsRuntime := func(path string) ([]byte, error) {
		t.Helper()
		cat, err := exec.LookPath("cat")
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(cat, path)
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 54321, Gid: 54322, Groups: []uint32{}}}
		return command.CombinedOutput()
	}
	check := func() {
		t.Helper()
		// Root acceptance executes the read under a genuinely different UID;
		// unprivileged unit runs still check actual metadata for that identity.
		if os.Geteuid() == 0 {
			if output, err := readAsRuntime(mirroredLibrary); err != nil || !bytes.Equal(output, []byte(content)) {
				t.Fatalf("runtime cannot read library through mirrored host path: %q/%v", output, err)
			}
			if output, err := readAsRuntime(private); err == nil {
				t.Fatalf("runtime gained private ledger access: %q", output)
			}
		}
		for path := filepath.Dir(loopback); path != root; path = filepath.Dir(path) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("mirror parent is not searchable without listing/writes: %s/%v", path, err)
			}
			if ok, _ := runtime.OwnerCanAccess(path, info, runtime.MountOwner{UID: 54321, GID: 54322}, 0o1); !ok || info.Mode().Perm() != 0o711 {
				t.Fatalf("mirror parent is not searchable without listing/writes: %s/%v", path, info)
			}
		}
		if target, err := os.Readlink(loopback); err != nil || target != root {
			t.Fatalf("canonical loopback changed: %q/%v", target, err)
		}
		if info, err := os.Stat(private); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private permissions changed: %v/%v", info, err)
		}
	}
	if err := d.ensureHostRootLoopback(root); err != nil {
		t.Fatal(err)
	}
	check()
	for path := filepath.Dir(loopback); path != root; path = filepath.Dir(path) {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A correct existing symlink must not bypass repair of its parents.
	if err := d.ensureHostRootLoopback(root); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestHostRootLoopbackRejectsEscapingImageSymlinks(t *testing.T) {
	for _, kind := range []string{"absolute", "relative", "chain"} {
		t.Run(kind, func(t *testing.T) {
			d := New(t.TempDir())
			root := filepath.Join(d.udockerDir, "containers", "real-id", "ROOT")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			var err error
			root, err = filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.Chmod(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(outside, "keep")
			if err := os.WriteFile(marker, []byte("host data"), 0o600); err != nil {
				t.Fatal(err)
			}
			target := outside
			if kind == "relative" {
				var err error
				target, err = filepath.Rel(root, outside)
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "chain" {
				if err := os.Symlink(outside, filepath.Join(root, "mirror")); err != nil {
					t.Fatal(err)
				}
				target = "mirror"
			}
			// The first host-path component is untrusted image data in ROOT.
			first := strings.Split(strings.TrimPrefix(root, "/"), "/")[0]
			if err := os.Symlink(target, filepath.Join(root, first)); err != nil {
				t.Fatal(err)
			}
			if err := d.ensureHostRootLoopback(root); err == nil {
				t.Fatal("image symlink redirected loopback construction outside ROOT")
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 1 || entries[0].Name() != "keep" {
				t.Fatalf("created host files through image symlink: %v/%v", entries, err)
			}
			if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("changed host directory mode: %v/%v", info, err)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "host data" {
				t.Fatalf("changed host marker: %q/%v", data, err)
			}
		})
	}
}

func TestHostRootLoopbackPreservesSearchableImageModes(t *testing.T) {
	d := New(t.TempDir())
	root := filepath.Join(d.udockerDir, "containers", "real-id", "ROOT")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	loopback := filepath.Join(root, strings.TrimPrefix(root, "/"))
	if err := os.MkdirAll(filepath.Dir(loopback), 0o755); err != nil {
		t.Fatal(err)
	}
	want := map[string]os.FileMode{}
	for parent := filepath.Dir(loopback); parent != root; parent = filepath.Dir(parent) {
		mode := os.FileMode(0o755)
		if parent == filepath.Dir(loopback) {
			mode = 0o751
		} else if filepath.Dir(parent) == root {
			mode = os.ModeSticky | 0o777
		}
		if err := os.Chmod(parent, mode); err != nil {
			t.Fatal(err)
		}
		want[parent] = mode
	}
	for retry := 0; retry < 2; retry++ {
		if err := d.ensureHostRootLoopback(root); err != nil {
			t.Fatal(err)
		}
		for path, mode := range want {
			info, err := os.Stat(path)
			if err != nil || info.Mode()&(os.ModePerm|os.ModeSticky) != mode {
				t.Fatalf("searchable image mode changed: %s/%v/%v, want %v", path, info, err, mode)
			}
		}
	}
}
