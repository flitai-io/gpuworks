//go:build linux

package selfinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
	"golang.org/x/sys/unix"
)

type inServiceSQLiteLock struct {
	Path   string
	Device uint64
	Inode  uint64
	Type   int16
	Owner  int32
}

type inServiceSQLiteReply struct {
	PID    int
	Tables int64
	Locks  []inServiceSQLiteLock
}

func inServiceSQLiteDSN(path string) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=rw&_pragma=busy_timeout(5000)"
}

func inServiceSQLitePeer(t *testing.T, path, action string) inServiceSQLiteReply {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestInServiceSQLitePeerProcess$")
	cmd.Env = append(os.Environ(), "GPUWORKS_INSERVICE_PEER_PATH="+path, "GPUWORKS_INSERVICE_PEER_ACTION="+action)
	cmd.WaitDelay = 2 * time.Second
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("SQLite peer %s: %v: %s", action, err, data)
	}
	var reply inServiceSQLiteReply
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatalf("SQLite peer JSON: %v: %s", err, data)
	}
	if reply.PID != cmd.Process.Pid || reply.PID == os.Getpid() {
		t.Fatalf("independent peer PID: parent=%d child=%d reply=%d", os.Getpid(), cmd.Process.Pid, reply.PID)
	}
	return reply
}

func inspectInServiceSQLiteLock(path string) (result inServiceSQLiteLock, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	var info unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &info); err != nil {
		return result, err
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: int16(io.SeekStart)}
	if err := unix.FcntlFlock(f.Fd(), unix.F_GETLK, &lock); err != nil {
		return result, err
	}
	return inServiceSQLiteLock{path, uint64(info.Dev), info.Ino, lock.Type, lock.Pid}, nil
}

func TestInServiceSQLitePeerProcess(t *testing.T) {
	path := os.Getenv("GPUWORKS_INSERVICE_PEER_PATH")
	if path == "" {
		return
	}
	reply := inServiceSQLiteReply{PID: os.Getpid()}
	run := func() error {
		switch os.Getenv("GPUWORKS_INSERVICE_PEER_ACTION") {
		case "locks":
			for _, file := range []string{path, path + "-shm"} {
				lock, err := inspectInServiceSQLiteLock(file)
				if err != nil {
					return err
				}
				reply.Locks = append(reply.Locks, lock)
			}
			return nil
		case "read":
			db, err := sql.Open("sqlite", inServiceSQLiteDSN(path))
			if err != nil {
				return err
			}
			err = db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&reply.Tables)
			return errors.Join(err, db.Close())
		default:
			return errors.New("unknown SQLite peer action")
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := json.NewEncoder(os.Stdout).Encode(reply); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func assertInServiceSQLiteLocks(t *testing.T, path, phase string) {
	t.Helper()
	reply := inServiceSQLitePeer(t, path, "locks")
	if len(reply.Locks) != 2 {
		t.Fatalf("%s: expected main/shm locks: %+v", phase, reply)
	}
	for _, observed := range reply.Locks {
		var info unix.Stat_t
		if err := unix.Stat(observed.Path, &info); err != nil {
			t.Fatal(err)
		}
		if observed.Inode != info.Ino || observed.Device != uint64(info.Dev) {
			t.Errorf("%s: lock probe saw another inode: %+v", phase, observed)
		}
		if observed.Type == unix.F_UNLCK || observed.Owner != int32(os.Getpid()) {
			t.Errorf("%s: %s lock type=%d owner=%d, want live SQLite PID %d", phase, observed.Path, observed.Type, observed.Owner, os.Getpid())
		}
	}
}

func assertInServiceArchiveModes(t *testing.T, path string, expected map[string]int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode, ok := expected[header.Name]; ok {
			if header.Mode != mode {
				t.Errorf("archive %s mode=%#o, want %#o", header.Name, header.Mode, mode)
			}
			delete(expected, header.Name)
		}
	}
	if len(expected) != 0 {
		t.Fatalf("archive missing mode-checked members: %v", expected)
	}
}

func TestStageInServiceKeepsLiveSQLiteLocks(t *testing.T) {
	for _, scenario := range []string{"owner", "trusted_group", "owner_masked_acl", "bad_mode", "group_acl"} {
		t.Run(scenario, func(t *testing.T) {
			group := scenario == "trusted_group" || scenario == "group_acl"
			reject := scenario == "bad_mode" || scenario == "group_acl"
			h := newHarness(t, t.TempDir())
			h.writeExistingInstall("v1.0.0")
			if !group {
				h.supervisedByNohup()
			}
			policy, err := h.installer.sensitiveFilePolicy()
			if err != nil {
				t.Fatal(err)
			}
			wantMode := os.FileMode(0600)
			if group {
				wantMode = 0660
			}
			if policy.Mode() != wantMode {
				t.Fatalf("fixture policy=%#o, want %#o", policy.Mode(), wantMode)
			}
			path := filepath.Join(h.layout.DataDir, "gpuworks.db")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()
			for _, query := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE live_fixture (value TEXT)", "INSERT INTO live_fixture VALUES ('before-upgrade')"} {
				if _, err := db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			before := make(map[string]os.FileInfo)
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if err := os.Chmod(path+suffix, policy.Mode()); err != nil {
					t.Fatal(err)
				}
			}
			var originalACL []byte
			if scenario == "owner_masked_acl" || scenario == "group_acl" {
				sharingNamedACL(t, path, "system.posix_acl_access", scenario == "owner_masked_acl")
				if err := os.Chmod(path, policy.Mode()); err != nil {
					t.Fatal(err)
				}
				originalACL = sharingReadACL(t, path, "system.posix_acl_access")
			}
			if scenario == "bad_mode" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				before[suffix], err = os.Stat(path + suffix)
				if err != nil {
					t.Fatal(err)
				}
			}
			assertInServiceSQLiteLocks(t, path, "before StageInService")
			config, err := os.Stat(h.layout.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			snapshot := func(dest string) error {
				if _, err := db.Exec("VACUUM INTO ?", dest); err != nil {
					return err
				}
				info, err := os.Stat(dest)
				if err == nil && os.SameFile(before[""], info) {
					return errors.New("snapshot must have an independent inode")
				}
				return err
			}
			up, err := h.installer.StageInService(context.Background(), "v1.0.0", openArchive(t, h.writeArchive("v2.0.0", []byte("new binary"))), snapshot, "admin")
			if reject {
				if err == nil {
					t.Error("in-service upgrade accepted unsafe live database metadata")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reject && (up.Backup == "" || readArchive(t, up.Backup)["data/gpuworks.db"] == "") {
				t.Fatal("in-service upgrade did not archive the real snapshot")
			}
			if !reject {
				assertInServiceArchiveModes(t, up.Backup, map[string]int64{
					"data/gpuworks.db": int64(wantMode), "data/master.key": int64(wantMode),
					"bin/gpuworks": 0755, "server.yaml": int64(config.Mode().Perm()),
				})
			}
			if len(originalACL) > 0 && !bytes.Equal(originalACL, sharingReadACL(t, path, "system.posix_acl_access")) {
				t.Error("in-service permission handling changed the live database ACL")
			}
			assertInServiceSQLiteLocks(t, path, "after StageInService")
			if reply := inServiceSQLitePeer(t, path, "read"); reply.Tables == 0 {
				t.Fatal("external SQLite query did not read the database")
			}
			for suffix, original := range before {
				after, err := os.Stat(path + suffix)
				if err != nil {
					t.Errorf("active database%s disappeared: %v", suffix, err)
					continue
				}
				if !os.SameFile(original, after) || after.Mode().Perm() != original.Mode().Perm() {
					t.Errorf("active database%s identity/permissions changed", suffix)
				}
			}
		})
	}
}
