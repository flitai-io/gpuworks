package engine

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A subprocess confines the process-wide umask change, including under -race.
func TestHygonUdockerConfigTraversalWithRestrictiveUmask(t *testing.T) {
	const childEnv = "GPUWORKS_TEST_HYGON_UMASK_CHILD"
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
	// Keep the Docker snapshot assertion faithful to the production helper.
	TestHygonCreatesVendorVdevAndCleansOnlyOwnedID(t)
	e, h, fake := hygonTestEngine(t)
	e.driver = hygonTestDriver{name: "udocker"}
	if err := os.Chmod(e.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(e.stateDir, "identity.json")
	if err := os.WriteFile(private, []byte("private identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(e.stateDir, "operator-private")
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := proto.InstanceDesiredState{UUID: "restricted-umask", GPUIndexes: []int{0}, VRAMLimitBytes: 1536 << 20}
	apply := func() *agentruntime.WorkloadSpec {
		t.Helper()
		ws := &agentruntime.WorkloadSpec{}
		if ok, reason := h.apply(e, t.Context(), ws, spec); !ok {
			t.Fatal(reason)
		}
		return ws
	}
	ws := apply()
	dir := hygonInstanceDir(e, spec.UUID)
	parents := []string{filepath.Join(e.stateDir, partitionStateRoot), filepath.Dir(dir)}
	runtimeOwner := agentruntime.MountOwner{UID: 54321, GID: 54322}
	check := func() {
		t.Helper()
		for _, parent := range parents {
			info, err := os.Stat(parent)
			if err != nil {
				t.Fatalf("dedicated runtime cannot search managed parent %s: %v", parent, err)
			}
			if ok, _ := agentruntime.OwnerCanAccess(parent, info, runtimeOwner, 0o1); !ok || info.Mode().Perm() != 0o711 {
				t.Fatalf("dedicated runtime cannot search managed parent %s: %v", parent, info)
			}
		}
		assertHygonUdockerView(t, ws, dir, 4)
		for path, want := range map[string]os.FileMode{
			e.stateDir: 0o755, unrelated: 0o700, private: 0o600,
			dir: 0o755, filepath.Join(dir, "vdev4.conf"): 0o644,
			filepath.Join(dir, hygonVdevLedgerName): 0o600,
		} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != want {
				t.Fatalf("permissions outside the search grant changed: %s/%v/%v, want %04o", path, info, err, want)
			}
		}
		for _, path := range []string{private, filepath.Join(dir, hygonVdevLedgerName)} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("dedicated runtime can read private file: %s/%v", path, err)
			}
			if canRead, _ := agentruntime.OwnerCanAccess(path, info, runtimeOwner, 0o4); canRead {
				t.Fatalf("dedicated runtime can read private file: %s/%v", path, info)
			}
		}
	}
	check()
	ledger, err := os.ReadFile(filepath.Join(dir, hygonVdevLedgerName))
	if err != nil {
		t.Fatal(err)
	}
	// Repeat a launch after an operator resets only the managed parents.
	for _, parent := range parents {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ws = apply()
	check()
	if after, err := os.ReadFile(filepath.Join(dir, hygonVdevLedgerName)); err != nil || !bytes.Equal(ledger, after) || fake.nextID != 5 {
		t.Fatalf("retry replaced native ownership: %q/%v/next=%d", after, err, fake.nextID)
	}
	// A later unsafe-parent failure must retain the existing ownership record.
	backup := parents[1] + ".saved"
	if err := os.Rename(parents[1], backup); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, parents[1]); err != nil {
		t.Fatal(err)
	}
	if ok, reason := h.apply(e, t.Context(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "managed directory") {
		t.Fatalf("replaced parent accepted: %v/%s", ok, reason)
	}
	if after, err := os.ReadFile(filepath.Join(backup, spec.UUID, hygonVdevLedgerName)); err != nil || !bytes.Equal(ledger, after) || !fake.owned[4] || fake.nextID != 5 {
		t.Fatalf("failed retry lost owned resource: %q/%v/owned=%v next=%d", after, err, fake.owned, fake.nextID)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("failed retry followed unsafe parent: %v/%v", entries, err)
	}
	if err := os.Remove(parents[1]); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, parents[1]); err != nil {
		t.Fatal(err)
	}
	if err := h.cleanupInstance(e, spec.UUID); err != nil {
		t.Fatal(err)
	}
}

func TestHygonUdockerRejectsUnsafeManagedParentsBeforeAllocation(t *testing.T) {
	for _, component := range []string{partitionStateRoot, filepath.Join(partitionStateRoot, "hygon-instances")} {
		for _, kind := range []string{"symlink", "writable", "file"} {
			t.Run(component+"/"+kind, func(t *testing.T) {
				e, h, fake := hygonTestEngine(t)
				e.driver = hygonTestDriver{name: "udocker"}
				path := filepath.Join(e.stateDir, component)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				outside := t.TempDir()
				if err := os.Chmod(outside, 0o700); err != nil {
					t.Fatal(err)
				}
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(outside, path)
				case "writable":
					if err = os.Mkdir(path, 0o755); err == nil {
						err = os.Chmod(path, 0o777)
					}
				case "file":
					err = os.WriteFile(path, []byte("not a directory"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
				ok, reason := h.apply(e, t.Context(), &agentruntime.WorkloadSpec{}, proto.InstanceDesiredState{UUID: "blocked-parent", GPUIndexes: []int{0}, VRAMLimitBytes: 1536 << 20})
				if ok || !strings.Contains(reason, "Hygon") || fake.nextID != 4 || len(fake.owned) != 0 {
					t.Fatalf("unsafe ancestor reached native allocation: %v/%s/next=%d owned=%v", ok, reason, fake.nextID, fake.owned)
				}
				if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
					t.Fatalf("followed an external directory: %v/%v", entries, err)
				}
				if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o700 {
					t.Fatalf("changed external ancestor permissions: %v/%v", info, err)
				}
			})
		}
	}
}
