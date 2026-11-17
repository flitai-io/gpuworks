package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestAcquireAgentLockRefusesASecondHolder: two gpuworks agent processes
// must never run one node's workload at once, and the refusal names the
// process that holds the state directory.
func TestAcquireAgentLockRefusesASecondHolder(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireAgentLock(dir)
	if err != nil {
		t.Fatalf("first lock = %v", err)
	}
	defer first.Close()
	_, err = acquireAgentLock(dir)
	if !errors.Is(err, errAgentAlreadyRunning) {
		t.Fatalf("second lock on the same state directory = %v, want errAgentAlreadyRunning", err)
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("refusal = %q, want it to name %q and the state directory", err, want)
	}
}

// TestAcquireAgentLockReleasesWhenTheFdCloses: the lock is a flock on an
// open file description, so closing the file that holds it (as the process
// exiting does) frees it for the next holder.
func TestAcquireAgentLockReleasesWhenTheFdCloses(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireAgentLock(dir)
	if err != nil {
		t.Fatalf("first lock = %v", err)
	}
	if _, err := acquireAgentLock(dir); err == nil {
		t.Fatal("a second holder was not refused while the first is open")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireAgentLock(dir)
	if err != nil {
		t.Fatalf("lock after release = %v", err)
	}
	defer second.Close()
}

// TestAgentLockIsReleasedByExec: an in-place re-exec (self-upgrade,
// rollback, accelerator-choice restart) must hand the lock to the new
// image, which retakes it; that holds only if the descriptor is close-on-exec.
func TestAgentLockIsReleasedByExec(t *testing.T) {
	f, err := acquireAgentLock(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("agent.lock is not close-on-exec; an in-place upgrade would lock out its own new image")
	}
}

// TestAcquireAgentLockWaitingTakesOverFromAnExitingHolder: a service
// restart starts the new agent while the old one is still shutting down.
func TestAcquireAgentLockWaitingTakesOverFromAnExitingHolder(t *testing.T) {
	dir := t.TempDir()
	old, err := acquireAgentLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(300*time.Millisecond, func() { _ = old.Close() })
	f, err := acquireAgentLockWaiting(dir, 5*time.Second)
	if err != nil {
		t.Fatalf("waiting lock = %v, want it once the old holder exits", err)
	}
	defer f.Close()
	if got := lockHolderPID(filepath.Join(dir, agentLockFile)); got != os.Getpid() {
		t.Fatalf("holder pid = %d, want %d", got, os.Getpid())
	}
}

// TestAcquireAgentLockWaitingGivesUpOnAResidentHolder: a second agent beside
// a running one exits instead of joining it.
func TestAcquireAgentLockWaitingGivesUpOnAResidentHolder(t *testing.T) {
	dir := t.TempDir()
	resident, err := acquireAgentLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer resident.Close()
	start := time.Now()
	if _, err := acquireAgentLockWaiting(dir, 400*time.Millisecond); !errors.Is(err, errAgentAlreadyRunning) {
		t.Fatalf("waiting lock beside a resident agent = %v, want errAgentAlreadyRunning", err)
	}
	if waited := time.Since(start); waited < 400*time.Millisecond {
		t.Fatalf("gave up after %v, before the handover wait", waited)
	}
}

// TestAcquireAgentLockReportsOtherFailures: a state directory that cannot be
// created is not mistaken for a running agent.
func TestAcquireAgentLockReportsOtherFailures(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := acquireAgentLockWaiting(filepath.Join(file, "state"), time.Minute)
	if err == nil || errors.Is(err, errAgentAlreadyRunning) {
		t.Fatalf("lock under a file = %v, want an I/O error, not a running agent", err)
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("lock under a file = %v, want ENOTDIR", err)
	}
}

// TestUpgradeRefusesBesideAResidentAgent: `gpuworks-agent upgrade` replaces
// itself with `run` on the state directory, which beside a resident agent
// would leave two agents sharing one node identity (GitHub issue 19). It
// refuses before downloading anything — the server URL here answers nothing.
func TestUpgradeRefusesBesideAResidentAgent(t *testing.T) {
	dir := t.TempDir()
	resident, err := acquireAgentLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer resident.Close()
	err = upgrade(discardLog(), []string{"--server", "http://127.0.0.1:1", "--token", "tok", "--state", dir})
	if !errors.Is(err, errAgentAlreadyRunning) {
		t.Fatalf("upgrade beside a resident agent = %v, want errAgentAlreadyRunning", err)
	}
	if !strings.Contains(err.Error(), "server console") {
		t.Fatalf("refusal = %q, want it to point at the server console", err)
	}
}

// TestUpgradeWithoutALockableStateDirStillProceeds: a file system that
// cannot lock only loses the guard; upgrade goes on to the download (which
// fails here because the server URL answers nothing).
func TestUpgradeWithoutALockableStateDirStillProceeds(t *testing.T) {
	if err := lockForUpgrade(discardLog(), filepath.Join(t.TempDir(), "f", "state")); err != nil {
		t.Fatalf("lockForUpgrade on a creatable state directory = %v", err)
	}
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := lockForUpgrade(discardLog(), filepath.Join(file, "state")); err != nil {
		t.Fatalf("lockForUpgrade where no lock can be taken = %v, want it to proceed without the guard", err)
	}
}

// TestRecordLockRefusalLeavesATraceInTheAgentLog: a nohup agent's stderr is
// /dev/null, so the refusal must reach <state>/agent.log, appended after
// what the running agent already wrote there.
func TestRecordLockRefusalLeavesATraceInTheAgentLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(path, []byte("level=INFO msg=\"agent starting\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resident, err := acquireAgentLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer resident.Close()
	_, refusal := acquireAgentLock(dir)
	recordLockRefusal(dir, refusal)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "agent starting") ||
		!strings.Contains(lines[1], "level=ERROR") || !strings.Contains(lines[1], "already runs with this state directory") {
		t.Fatalf("agent.log = %q, want the resident's line then the refusal", raw)
	}
}

// TestAcquireAgentLockCreatesAPrivateStateDirectory: the state directory
// holds the node token, so a first run creating it keeps it 0700, as
// configure and the file logger do.
func TestAcquireAgentLockCreatesAPrivateStateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	f, err := acquireAgentLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("state directory mode = %o, want 700", perm)
	}
}

// TestAgentLockSurvivesGarbageCollection: run and upgrade discard the file
// acquireAgentLock returns, so the lock must not depend on the caller's
// reference. os.File closes its descriptor from a finalizer the GC runs once
// nothing refers to the file; a dropped lock let a second agent start beside
// the first a few seconds in.
func TestAgentLockSurvivesGarbageCollection(t *testing.T) {
	dir := t.TempDir()
	if _, err := acquireAgentLock(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = heldAgentLock.Close() })
	for i := 0; i < 5; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := acquireAgentLock(dir); !errors.Is(err, errAgentAlreadyRunning) {
		t.Fatalf("lock after GC = %v, want it still held by the discarded first acquisition", err)
	}
}
