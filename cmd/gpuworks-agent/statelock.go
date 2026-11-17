package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// agentLockFile is the exclusive lock one gpuworks-agent process holds on its
// state directory. Two processes on one state directory share one node
// identity: each connection the server accepts replaces the other's, so the
// node's connection flaps and every stream on it — a proxied inference
// response included — is cut.
const agentLockFile = "agent.lock"

// heldAgentLock is the state directory lock this process holds; see
// acquireAgentLock for why it must stay reachable.
var heldAgentLock *os.File

// errAgentAlreadyRunning is why a lock on the state directory was refused.
var errAgentAlreadyRunning = errors.New("another gpuworks agent already runs with this state directory")

// agentLockHandoverWait is how long run waits for the previous holder to
// exit: a service restart, or an installer that has just signalled the old
// agent, starts the new one while the old one is still shutting down.
const agentLockHandoverWait = 15 * time.Second

// agentLockPoll is how often a waiting run retries the lock.
const agentLockPoll = 200 * time.Millisecond

// acquireAgentLock takes an exclusive, non-blocking flock on the state
// directory's agent.lock and records this process's PID in it, so a refused
// holder can name the one that runs. Go opens the file close-on-exec, so an
// in-place re-exec — a self-upgrade, its rollback, or an accelerator-choice
// restart, all syscall.Exec — releases it on the old process image and the
// new image retakes it on its own first line of run. The file is also kept in
// heldAgentLock, so it stays reachable for the life of the process: os.File
// closes its descriptor from a finalizer on an internal object (which
// runtime.SetFinalizer on the *os.File cannot clear), and a lock only the
// caller's discarded local refers to is dropped at the next GC. Only an
// explicit Close or the process exiting frees it.
func acquireAgentLock(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, agentLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if pid := lockHolderPID(path); pid > 0 {
			return nil, fmt.Errorf("%w (%s, pid %d)", errAgentAlreadyRunning, stateDir, pid)
		}
		return nil, fmt.Errorf("%w (%s)", errAgentAlreadyRunning, stateDir)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	heldAgentLock = f
	return f, nil
}

// acquireAgentLockWaiting is acquireAgentLock retried while another agent
// holds the lock, for up to wait: the previous holder may be exiting.
func acquireAgentLockWaiting(stateDir string, wait time.Duration) (*os.File, error) {
	deadline := time.Now().Add(wait)
	for {
		f, err := acquireAgentLock(stateDir)
		if err == nil || !errors.Is(err, errAgentAlreadyRunning) || !time.Now().Before(deadline) {
			return f, err
		}
		time.Sleep(agentLockPoll)
	}
}

// lockHolderPID is the PID the current holder wrote into the lock file, or 0.
func lockHolderPID(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// recordLockRefusal appends why run exits to <state>/agent.log: the lock is
// taken before the file logger exists (the holder owns that log and its
// rotation), and a nohup agent's stderr goes to /dev/null, so without this
// line a refused agent would vanish without a trace. The holder appends to
// the same file, so at worst this one line lands in the rotated generation.
func recordLockRefusal(stateDir string, reason error) {
	f, err := os.OpenFile(filepath.Join(stateDir, "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	slog.New(slog.NewTextHandler(f, nil)).Error("agent exited", "err", reason)
}

// lockForUpgrade takes the state directory's lock for `upgrade`, which
// replaces itself with `run` on that directory. Beside a resident agent (a
// systemd or launchd service, a nohup one) that `run` would be a second
// agent outside the service sharing the node's identity, so upgrade refuses
// before downloading anything; the resident agent is upgraded from the
// server instead. The lock held from here keeps another run from starting
// during the download and is released by the exec that starts the new binary.
// A file system that cannot lock (some NFS mounts answer ENOLCK) only loses
// the guard, as run does: it is not a reason to refuse.
func lockForUpgrade(log *slog.Logger, stateDir string) error {
	_, err := acquireAgentLock(stateDir)
	if errors.Is(err, errAgentAlreadyRunning) {
		return fmt.Errorf("%w; upgrade that agent from the server console instead of running upgrade beside it", err)
	}
	if err != nil {
		log.Warn("state directory lock unavailable; upgrading without the single-agent guard", "err", err)
	}
	return nil
}
