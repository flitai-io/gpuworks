//go:build linux

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func openSQLiteLockStore(t *testing.T, path string, mode os.FileMode) *Store {
	t.Helper()
	var policies []securefile.Policy
	if mode == securefile.GroupMode {
		policy, err := securefile.TrustedGroup(os.Getegid())
		if err != nil {
			t.Fatal(err)
		}
		policies = append(policies, policy)
	}
	s, err := Open(path, bus.New(), policies...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode=%#o, want %#o", info.Name(), info.Mode().Perm(), mode)
		}
	}
	return s
}

// F_GETLK ignores the querying process's own POSIX locks. The observation
// must come from a distinct process and identify the live Store's inode/PID.
func assertSQLiteStoreLocks(t *testing.T, path, phase string) {
	t.Helper()
	reply := runSQLitePeer(t, sqlitePeerRequest{Operation: "locks", Path: path})
	if len(reply.Locks) != 2 {
		t.Fatalf("%s: lock observations=%+v, want main and shm", phase, reply.Locks)
	}
	for _, file := range []string{path, path + "-shm"} {
		var got *sqlitePeerLock
		for i := range reply.Locks {
			if reply.Locks[i].Path == file {
				got = &reply.Locks[i]
			}
		}
		if got == nil {
			t.Fatalf("%s: no lock observation for %s", phase, file)
		}
		var info unix.Stat_t
		if err := unix.Stat(file, &info); err != nil {
			t.Fatal(err)
		}
		if got.Device != uint64(info.Dev) || got.Inode != info.Ino {
			t.Errorf("%s: probe observed a different inode for %s: %+v", phase, file, got)
		}
		if got.Type == unix.F_UNLCK || got.OwnerPID != int32(os.Getpid()) {
			t.Errorf("%s: %s lock type=%d owner=%d, want a lock owned by live Store PID %d (observer PID %d)", phase, file, got.Type, got.OwnerPID, os.Getpid(), reply.PID)
		}
	}
}

func TestStoreOpenRetainsSQLiteFileLocks(t *testing.T) {
	for _, mode := range []os.FileMode{securefile.OwnerMode, securefile.GroupMode} {
		t.Run(fmt.Sprintf("mode=%#o", mode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "management database.db")
			openSQLiteLockStore(t, path, mode)
			assertSQLiteStoreLocks(t, path, "after Open")
		})
	}
}

func TestExternalSQLiteReadPreservesLiveStoreSidecars(t *testing.T) {
	for _, mode := range []os.FileMode{securefile.OwnerMode, securefile.GroupMode} {
		t.Run(fmt.Sprintf("mode=%#o", mode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "management.db")
			openSQLiteLockStore(t, path, mode)
			before := make(map[string]os.FileInfo)
			for _, suffix := range []string{"-wal", "-shm"} {
				info, err := os.Stat(path + suffix)
				if err != nil {
					t.Fatal(err)
				}
				before[suffix] = info
			}
			reply := runSQLitePeer(t, sqlitePeerRequest{Operation: "read", Path: path})
			if reply.Rows == 0 {
				t.Fatal("external SQLite connection did not read the migrated schema")
			}
			for suffix, original := range before {
				after, err := os.Stat(path + suffix)
				if err != nil {
					t.Errorf("active %s disappeared after external read/close: %v", suffix, err)
					continue
				}
				if !os.SameFile(original, after) {
					t.Errorf("active %s was replaced after external read/close", suffix)
				}
				if after.Mode().Perm() != mode {
					t.Errorf("active %s mode=%#o, want %#o", suffix, after.Mode().Perm(), mode)
				}
			}
		})
	}
}

func TestSecondStoreOpenPreservesExistingSQLiteFileLocks(t *testing.T) {
	for _, mode := range []os.FileMode{securefile.OwnerMode, securefile.GroupMode} {
		t.Run(fmt.Sprintf("mode=%#o", mode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "management.db")
			openSQLiteLockStore(t, path, mode)
			assertSQLiteStoreLocks(t, path, "after first Open")
			second := openSQLiteLockStore(t, path, mode)
			assertSQLiteStoreLocks(t, path, "after second Open")
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
			assertSQLiteStoreLocks(t, path, "after second Close")
		})
	}
}

// A real external commit must update the reference guard. Both active and
// stopped deployments remain references until their rows are removed.
func TestModelDeletionSeesExternalDeploymentCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "management.db")
	s := openSQLiteLockStore(t, path, securefile.OwnerMode)
	model, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "fixture/external-delete", Revision: "main", Name: "external-delete"})
	if err != nil {
		t.Fatal(err)
	}
	var deployments []*Deployment
	for _, state := range []DeploymentState{DeployActive, DeployStopped} {
		dep, err := s.CreateDeployment(&Deployment{ModelID: model.ID, State: state})
		if err != nil {
			t.Fatal(err)
		}
		deployments = append(deployments, dep)
	}
	for _, dep := range deployments {
		if err := s.DeleteModel(model.ID); !errors.Is(err, ErrReferenced) {
			t.Fatalf("model with %s deployment: got %v, want ErrReferenced", dep.State, err)
		}
		reply := runSQLitePeer(t, sqlitePeerRequest{Operation: "delete_deployment", Path: path, DeploymentID: dep.ID})
		if reply.Rows != 1 {
			t.Fatalf("external deletion affected %d rows, want 1", reply.Rows)
		}
	}
	deleteErr := s.DeleteModel(model.ID)
	// Observe after the attempted deletion so another connection cannot heal
	// the server's stale view before the operation under test runs.
	remaining := runSQLitePeer(t, sqlitePeerRequest{Operation: "count_deployments", Path: path, ModelID: model.ID})
	if remaining.Rows != 0 {
		t.Fatalf("external deployment count=%d, want 0 after both commits", remaining.Rows)
	}
	if deleteErr != nil {
		t.Fatalf("external deployments=0 but live Store refused model deletion: %v", deleteErr)
	}
	if _, err := s.GetModel(model.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted model: got %v, want ErrNotFound", err)
	}
}

func TestExternalDeploymentRemovalPreservesModelReferencePolicy(t *testing.T) {
	for _, state := range []DeploymentState{DeployActive, DeployStopped} {
		t.Run(string(state), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "management.db")
			s := openSQLiteLockStore(t, path, securefile.OwnerMode)
			model, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "fixture/single-reference", Revision: "main", Name: "single-reference"})
			if err != nil {
				t.Fatal(err)
			}
			dep, err := s.CreateDeployment(&Deployment{ModelID: model.ID, State: state})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteModel(model.ID); !errors.Is(err, ErrReferenced) {
				t.Fatalf("%s deployment must keep its model: %v", state, err)
			}
			removed := runSQLitePeer(t, sqlitePeerRequest{Operation: "delete_deployment", Path: path, DeploymentID: dep.ID})
			if removed.Rows != 1 {
				t.Fatalf("external deletion affected %d rows, want 1", removed.Rows)
			}
			if err := s.DeleteModel(model.ID); err != nil {
				t.Fatalf("model without its %s reference: %v", state, err)
			}
		})
	}
}
