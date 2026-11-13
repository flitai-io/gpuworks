//go:build linux

package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
	"golang.org/x/sys/unix"
)

type sqlitePeerRequest struct {
	Operation    string
	Path         string
	DeploymentID uint
	ModelID      uint
}

type sqlitePeerReply struct {
	PID   int
	Locks []sqlitePeerLock
	Rows  int64
}

type sqlitePeerLock struct {
	Path     string
	Device   uint64
	Inode    uint64
	Type     int16
	OwnerPID int32
}

// runSQLitePeer uses another process because F_GETLK does not report a
// process's own traditional record locks. SQL operations also need an
// independent SQLite connection lifecycle, rather than another Store pool.
func runSQLitePeer(t *testing.T, request sqlitePeerRequest) sqlitePeerReply {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSQLitePeerProcess$")
	cmd.Env = append(os.Environ(), "GPUWORKS_TEST_SQLITE_PEER=1")
	cmd.Stdin = bytes.NewReader(input)
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("SQLite peer %q failed: %v (context: %v)\nstdout: %s\nstderr: %s", request.Operation, err, ctx.Err(), output, stderr.String())
	}
	var reply sqlitePeerReply
	if err := json.Unmarshal(output, &reply); err != nil {
		t.Fatalf("decode SQLite peer reply: %v\nstdout: %s\nstderr: %s", err, output, stderr.String())
	}
	if reply.PID <= 0 || reply.PID == os.Getpid() || reply.PID != cmd.Process.Pid {
		t.Fatalf("SQLite peer PID mismatch: parent=%d child=%d reply=%+v", os.Getpid(), cmd.Process.Pid, reply)
	}
	if stderr.Len() != 0 {
		t.Logf("SQLite peer stderr: %s", stderr.String())
	}
	return reply
}

// TestSQLitePeerProcess is the child-process entry point. It closes all
// descriptors and SQL connections before replying, so the parent observes
// the effects of an external connection's completed close.
func TestSQLitePeerProcess(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_SQLITE_PEER") != "1" {
		return
	}
	var request sqlitePeerRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		fmt.Fprintln(os.Stderr, "decode SQLite peer request:", err)
		os.Exit(2)
	}
	reply, err := performSQLitePeerRequest(request)
	if err != nil {
		fmt.Fprintf(os.Stderr, "SQLite peer %q: %v\n", request.Operation, err)
		os.Exit(2)
	}
	if err := json.NewEncoder(os.Stdout).Encode(reply); err != nil {
		fmt.Fprintln(os.Stderr, "encode SQLite peer reply:", err)
		os.Exit(2)
	}
	// Avoid appending the test framework's PASS line to the JSON protocol.
	os.Exit(0)
}

func performSQLitePeerRequest(request sqlitePeerRequest) (sqlitePeerReply, error) {
	reply := sqlitePeerReply{PID: os.Getpid()}
	if request.Path == "" {
		return reply, errors.New("database path is required")
	}
	if request.Operation == "locks" {
		for _, path := range []string{request.Path, request.Path + "-shm"} {
			lock, err := observeSQLitePeerLock(path)
			if err != nil {
				return reply, fmt.Errorf("probe %s: %w", path, err)
			}
			reply.Locks = append(reply.Locks, lock)
		}
		return reply, nil
	}
	if request.Operation != "read" && request.Operation != "count_deployments" && request.Operation != "delete_deployment" {
		return reply, fmt.Errorf("unknown operation %q", request.Operation)
	}
	var err error
	reply.Rows, err = executeSQLitePeerSQL(request)
	return reply, err
}

func observeSQLitePeerLock(path string) (result sqlitePeerLock, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return result, err
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: int16(io.SeekStart)}
	if err := unix.FcntlFlock(f.Fd(), unix.F_GETLK, &lock); err != nil {
		return result, err
	}
	return sqlitePeerLock{Path: path, Device: uint64(stat.Dev), Inode: stat.Ino, Type: lock.Type, OwnerPID: lock.Pid}, nil
}

func executeSQLitePeerSQL(request sqlitePeerRequest) (rows int64, err error) {
	// mode=rw refuses to create a different database if the fixture path is
	// wrong. URI escaping preserves spaces and literal query characters.
	dsn := "file:" + (&url.URL{Path: request.Path}).EscapedPath() + "?mode=rw&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return 0, err
	}
	db.SetMaxOpenConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()
	if request.Operation == "read" {
		err = db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&rows)
		return rows, err
	}
	if request.Operation == "count_deployments" {
		err = db.QueryRow("SELECT count(*) FROM deployments WHERE model_id = ?", request.ModelID).Scan(&rows)
		return rows, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	result, err := tx.Exec("DELETE FROM deployments WHERE id = ?", request.DeploymentID)
	if err != nil {
		return 0, err
	}
	rows, err = result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return rows, tx.Commit()
}
