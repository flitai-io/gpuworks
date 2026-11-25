package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// serverLockFile is the file in the data directory that serve holds an
// exclusive flock on for its whole life. Commands that change what a running
// server keeps in memory (the plugin lifecycle among them) take the same lock
// and refuse while a server holds it: the server would quietly undo them.
// The lock belongs to the open file, so it goes away with the process, a
// crash included; a stale file proves nothing.
const serverLockFile = "server.lock"

// serverLockWait is how long serve waits for the lock: a maintenance command
// holds it for the length of one operation, and a server that is restarting
// releases it when its process exits. Tests shorten it.
var serverLockWait = 10 * time.Second

// serverRunningError is a refusal because another process holds the data
// directory's lock. PID is the live server the lock file names, 0 when it
// names none that is alive: then the holder may as well be another
// maintenance command, and the refusal names the lock file (Path) instead.
type serverRunningError struct {
	PID  int
	Path string
}

func (e *serverRunningError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("the server (pid %d) is running; stop it first", e.PID)
	}
	return fmt.Sprintf("another gpuworks process holds %s; stop the server or wait for the command to finish", e.Path)
}

// errServerLockHeld is what tryLock reports for a lock another open file
// holds; every other failure means the question could not be answered.
var errServerLockHeld = errors.New("server lock held")

// takeServerLock takes the data directory's server lock for serve, waiting
// up to wait for a maintenance command or an exiting server to let it go, and
// writes this process's pid into the file for the refusal message. The
// returned function releases it; so does the process exiting.
func takeServerLock(dataDir string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, serverLockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if errors.Is(err, fs.ErrPermission) {
		// A file another account left behind: flock needs only a read
		// descriptor, and the refusal message just loses this pid.
		f, err = os.OpenFile(path, os.O_RDONLY, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open the server lock %s: %w", path, err)
	}
	ownLikeDataDir(f, dataDir)
	deadline := time.Now().Add(wait)
	for {
		err = tryLock(f)
		if err == nil {
			break
		}
		if !errors.Is(err, errServerLockHeld) || !time.Now().Before(deadline) {
			f.Close()
			if errors.Is(err, errServerLockHeld) {
				if pid := lockHolder(path); pid > 0 {
					return nil, fmt.Errorf("another gpuworks server (pid %d) is running on %s", pid, dataDir)
				}
				return nil, fmt.Errorf("another gpuworks process holds %s", path)
			}
			return nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return func() {
		_ = unlock(f)
		_ = f.Close()
	}, nil
}

// requireServerStopped takes the server lock of dataDir without waiting, so
// that no server runs, or starts, while an offline command changes what it
// would hold in memory. It refuses with *serverRunningError when a server
// holds the lock. The returned function releases the lock.
func requireServerStopped(dataDir string) (func(), error) {
	f, err := openServerLock(dataDir, true)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		f.Close()
		if errors.Is(err, errServerLockHeld) {
			path := filepath.Join(dataDir, serverLockFile)
			return nil, &serverRunningError{PID: lockHolder(path), Path: path}
		}
		return nil, err
	}
	return func() {
		_ = unlock(f)
		_ = f.Close()
	}, nil
}

// serverRunning reports whether some process holds dataDir's lock and the
// pid of the live server the lock file names (0 when it names none), for
// read-only commands that only mention it. It never creates the file: a
// missing one is held by nobody. A lock that cannot be examined counts as
// not held: the caller only words its output.
func serverRunning(dataDir string) (bool, int) {
	f, err := openServerLock(dataDir, false)
	if err != nil {
		return false, 0
	}
	defer f.Close()
	if err := tryLock(f); err != nil {
		return errors.Is(err, errServerLockHeld), lockHolder(f.Name())
	}
	_ = unlock(f)
	return false, 0
}

// lockHolder is the pid the lock file names when that process is alive, 0
// otherwise. Only serve writes the file, so a pid that is gone belongs to a
// server that has exited, and whoever holds the lock now is another command.
func lockHolder(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 || !processAlive(pid) {
		return 0
	}
	return pid
}

// RequireServerStopped is the server lock for a command registered through
// Options.Commands that changes what a running server holds in memory: it
// takes the lock of dataDir without waiting and refuses, naming the server,
// when one holds it. The returned function releases the lock.
func RequireServerStopped(dataDir string) (func(), error) { return requireServerStopped(dataDir) }

// ServerRunning reports whether a server holds dataDir's lock, and its pid
// when the lock file names a live one, without creating the file.
func ServerRunning(dataDir string) (bool, int) { return serverRunning(dataDir) }
