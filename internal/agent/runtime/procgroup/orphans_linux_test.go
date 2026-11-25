//go:build linux

package procgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// waitForStat polls one process's status until want reports it is the way the
// test needs it, and names what it was last seen as when it never is.
func waitForStat(t *testing.T, pid int, what string, want func(procStat) bool) procStat {
	t.Helper()
	var last procStat
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		stat, err := readProcStat("/proc", strconv.Itoa(pid))
		if err != nil {
			t.Fatalf("process %d is gone before it was %s: %v", pid, what, err)
		}
		last = stat
		if want(stat) {
			return stat
		}
	}
	t.Fatalf("process %d never became %s: state %c ppid %d pgid %d", pid, what, last.state, last.ppid, last.pgid)
	return last
}

// assertGroupGone fails while any process of the group is still in the
// process table, a zombie included: signal 0 reaches those too.
func assertGroupGone(t *testing.T, pgid int) {
	t.Helper()
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		var left []string
		entries, _ := os.ReadDir("/proc")
		for _, entry := range entries {
			stat, statErr := readProcStat("/proc", entry.Name())
			if statErr == nil && stat.pgid == pgid {
				left = append(left, fmt.Sprintf("%s(state %c ppid %d)", entry.Name(), stat.state, stat.ppid))
			}
		}
		t.Fatalf("process group %d is not gone: kill(0) = %v, members %v", pgid, err, left)
	}
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			continue
		}
		pid, err := strconv.Atoi(string(raw))
		if err == nil && pid > 0 {
			return pid
		}
	}
	t.Fatalf("%s never named a pid", path)
	return 0
}

// A workload's own processes outlive their parents. The engine starts workers
// and the launcher kills everything it traced as it goes down, in no order,
// so a worker whose parent died first is handed to this process (it claims
// them: adoptOrphans) and only this process can wait for it. Stopping the
// workload therefore has to reap it; otherwise it stays <defunct> for as long
// as the node lives, and every deployment leaves another batch.
func TestStopReapsADescendantOrphanedByItsParent(t *testing.T) {
	if !procShowsAllPIDs() {
		t.Skip("unrestricted procfs required")
	}
	tr := &Tracker{Dir: t.TempDir(), KillWait: 2 * time.Second}
	dir := t.TempDir()
	grandchildFile, goAhead := filepath.Join(dir, "grandchild"), filepath.Join(dir, "go")
	// The middle process starts the grandchild and exits at once, which is
	// what orphans it; the leader keeps the group alive meanwhile. It waits
	// for the go-ahead so the orphan is made after this workload has been
	// tracked, the way a running engine starts and loses workers long after
	// its launch.
	leader := start(t, tr, "serving", fmt.Sprintf(`while [ ! -e %s ]; do sleep 0.02; done; /bin/sh -c 'sleep 300 & printf %%s "$!" > %s' ; exec sleep 300`, goAhead, grandchildFile))
	if err := os.WriteFile(goAhead, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	grandchild := readPIDFile(t, grandchildFile)

	// Adoption is the half that keeps it reachable: without it the kernel
	// would hand the orphan to init, where nothing this agent does can
	// reclaim it.
	adopted := waitForStat(t, grandchild, "an orphan of this process", func(s procStat) bool { return s.ppid == os.Getpid() })
	if adopted.pgid != leader.Process.Pid {
		t.Fatalf("the orphan left the workload's process group: pgid %d, want %d", adopted.pgid, leader.Process.Pid)
	}

	if err := tr.Stop(context.Background(), "serving", 500*time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Signal 0 reaches a zombie, so this fails while one is left behind.
	if err := syscall.Kill(grandchild, 0); !errors.Is(err, syscall.ESRCH) {
		stat, statErr := readProcStat("/proc", strconv.Itoa(grandchild))
		t.Fatalf("the workload's orphan is still in the process table after its stop: kill(0) = %v, state %c ppid %d (%v)", err, stat.state, stat.ppid, statErr)
	}
	// Its own leader is reaped by the Track that started it.
	if exists, code := waitExited(t, tr, "serving"); !exists || code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exists=%v code=%d, want the SIGTERM docker reports as 143", exists, code)
	}
	assertGroupGone(t, leader.Process.Pid)
}

// A graceful stop is a window, not a promise. An engine that blocks SIGTERM
// is killed when it closes, and the group must still come apart completely:
// the leader's status is the kill (what tells an out-of-memory kill from a
// clean exit) and nothing of it may be left to reap.
func TestStopKillsAWorkloadThatIgnoresSIGTERMAndStillReapsIt(t *testing.T) {
	if !procShowsAllPIDs() {
		t.Skip("unrestricted procfs required")
	}
	tr := &Tracker{Dir: t.TempDir(), KillWait: 5 * time.Second}
	dir := t.TempDir()
	workerFile, goAhead := filepath.Join(dir, "worker"), filepath.Join(dir, "go")
	leader := start(t, tr, "stubborn", fmt.Sprintf(`trap '' TERM; while [ ! -e %s ]; do sleep 0.02; done; /bin/sh -c 'trap "" TERM; sleep 300 & printf %%s "$!" > %s' ; exec sleep 300`, goAhead, workerFile))
	if err := os.WriteFile(goAhead, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	worker := readPIDFile(t, workerFile)
	waitForStat(t, worker, "an orphan of this process", func(s procStat) bool { return s.ppid == os.Getpid() })

	started := time.Now()
	if err := tr.Stop(context.Background(), "stubborn", 200*time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("the stop took %s: the graceful window was not waited out before the kill", elapsed)
	}
	// Check the Stop boundary before waiting for the leader's exit record.
	if err := syscall.Kill(worker, 0); !errors.Is(err, syscall.ESRCH) {
		stat, statErr := readProcStat("/proc", strconv.Itoa(worker))
		t.Fatalf("worker %d still present after Stop: kill(0)=%v, state=%q ppid=%d pgid=%d (%v)", worker, err, stat.state, stat.ppid, stat.pgid, statErr)
	}
	if exists, code := waitExited(t, tr, "stubborn"); !exists || code != 128+int(syscall.SIGKILL) {
		t.Fatalf("exists=%v code=%d, want the SIGKILL docker reports as 137", exists, code)
	}
	assertGroupGone(t, leader.Process.Pid)
}

// The reaper may only ever take a workload group's own descendants. Any other
// exited child of this process is a command somebody is about to read the
// exit status of, and a reaper that takes it hands that caller "no child
// processes" instead of what the command did. The group leader is the same
// case: Track waits for it.
func TestReapingLeavesExitedChildrenOfOtherCallersAlone(t *testing.T) {
	if !procShowsAllPIDs() {
		t.Skip("unrestricted procfs required")
	}
	command := exec.Command("/bin/sh", "-c", "exit 7")
	// Its own group, as every command this driver starts detached has.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	waitForStat(t, pid, "exited", func(s procStat) bool { return s.state == 'Z' })

	// A workload stop somewhere else on the node, and the pathological case:
	// a stop of the very group this command leads.
	if reaped := reapOrphans(pid + 1); reaped != 0 {
		t.Fatalf("a stop of another group reaped %d of this caller's children", reaped)
	}
	if reaped := reapOrphans(pid); reaped != 0 {
		t.Fatalf("a stop reaped the group leader (%d) its own caller waits for", pid)
	}
	err := command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("the caller's own Wait = %v, want exit status 7", err)
	}
}

// A leftover that shares its process group with this agent is signalled on
// its own: signalling the group would signal the agent. The test runs in that
// group, so its own survival is the assertion.
func TestDrainProcessSignalsOnlyThatProcess(t *testing.T) {
	stubborn := exec.Command("/bin/sh", "-c", `trap '' TERM; exec sleep 300`)
	if err := stubborn.Start(); err != nil {
		t.Fatal(err)
	}
	pid := stubborn.Process.Pid
	t.Cleanup(func() { _ = stubborn.Process.Kill(); _ = stubborn.Wait() })
	own, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatal(err)
	}
	group, err := syscall.Getpgid(pid)
	if err != nil || group != own {
		t.Fatalf("process group of the leftover = %d (%v), want this process's own %d", group, err, own)
	}
	if err := DrainProcess(context.Background(), pid, 100*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("DrainProcess: %v", err)
	}
	if err := stubborn.Wait(); err == nil {
		t.Fatal("the leftover exited cleanly; it was supposed to be killed")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the leftover is still there: kill(0) = %v", err)
	}
}
