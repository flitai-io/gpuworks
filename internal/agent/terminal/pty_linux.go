//go:build linux

package terminal

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// procIdentity is a process as /proc/<pid>/stat describes it; start (clock
// ticks since boot) tells a recycled pid apart.
type procIdentity struct {
	pid, ppid, sid int
	start          uint64
}

// signalSession sends sig to every process in the session led by sid.
//
// Signalling the leader's process group is not enough. An interactive shell
// enables job control, which puts each job in a process group of its own, so
// the leader's group holds nothing but the shell itself and background jobs
// survive a kill by negative pid. Every descendant does however inherit the
// session id, and only a deliberate setsid can leave it, so the session is the
// unit that actually bounds a terminal's processes.
//
// /proc is the only portable way to enumerate a session without cgo. Errors on
// individual processes are ignored: they exit on their own constantly, and a
// vanished process is exactly the outcome being asked for.
func signalSession(sid int, sig syscall.Signal) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	self := os.Getpid()
	for _, e := range entries {
		pid, convErr := strconv.Atoi(e.Name())
		// Never signal the agent itself or init, whatever /proc claims.
		if convErr != nil || pid <= 1 || pid == self {
			continue
		}
		if procSessionID(pid) != sid {
			continue
		}
		_ = syscall.Kill(pid, sig)
	}
	return nil
}

// procSessionID reads the session id from /proc/<pid>/stat, or -1.
func procSessionID(pid int) int {
	stat, ok := readProcIdentity(pid)
	if !ok {
		return -1
	}
	return stat.sid
}

func readProcIdentity(pid int) (procIdentity, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procIdentity{}, false
	}
	// Field 2 is the executable name in parentheses and may itself contain
	// spaces and parentheses, so fields are counted from the last ')': what
	// follows is state, ppid, pgrp, session.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return procIdentity{}, false
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 20 {
		return procIdentity{}, false
	}
	ppid, ppidErr := strconv.Atoi(fields[1])
	sid, sidErr := strconv.Atoi(fields[3])
	start, startErr := strconv.ParseUint(fields[19], 10, 64)
	if ppidErr != nil || sidErr != nil || startErr != nil {
		return procIdentity{}, false
	}
	return procIdentity{pid: pid, ppid: ppid, sid: sid, start: start}, true
}

// processTree snapshots root and all of its descendants, including children
// that deliberately called setsid. runuser does this on some distributions,
// so a udocker PTY cannot be bounded by terminal session id alone.
func processTree(root int) []procIdentity {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	byParent := make(map[int][]procIdentity)
	var rootIdentity procIdentity
	for _, entry := range entries {
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		identity, ok := readProcIdentity(pid)
		if !ok {
			continue
		}
		if pid == root {
			rootIdentity = identity
		}
		byParent[identity.ppid] = append(byParent[identity.ppid], identity)
	}
	if rootIdentity.pid == 0 {
		return nil
	}
	result := []procIdentity{rootIdentity}
	for i := 0; i < len(result); i++ {
		result = append(result, byParent[result[i].pid]...)
	}
	return result
}

// ttyRef names a session's pty slave. The device identity is taken while the
// slave is open: once the master closes, the /dev/pts node is gone and a
// process still holding the slave shows "/dev/pts/N (deleted)" in /proc.
type ttyRef struct {
	path string
	id   deviceID
}

// deviceID is a device node's identity: the filesystem it lives on (one per
// devpts instance) and the device it stands for.
type deviceID struct{ dev, rdev uint64 }

func ttyRefOf(slave *os.File) ttyRef {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(slave.Fd()), &st); err != nil {
		return ttyRef{}
	}
	return ttyRef{path: slave.Name(), id: deviceID{dev: uint64(st.Dev), rdev: uint64(st.Rdev)}}
}

func (r ttyRef) String() string { return r.path }

// processesUsingTTY finds processes that still hold the PTY slave on stdin,
// stdout, or stderr. It covers wrappers such as setsid/runuser that can fork,
// reparent, and leave the original process tree before teardown begins.
//
// A match is by device identity, not by path. The fd link names the slave by
// its path inside the holder's own mount namespace, so a shell in a container
// with its own /dev/pts also reads "/dev/pts/0"; it differs only in dev (one
// devpts instance per container). The path is a cheap first filter.
func processesUsingTTY(ref ttyRef) []procIdentity {
	if ref.path == "" || ref.id == (deviceID{}) {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var result []procIdentity
	for _, entry := range entries {
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil || pid <= 1 || pid == self {
			continue
		}
		usesTTY := false
		for _, fd := range []string{"0", "1", "2"} {
			link := "/proc/" + entry.Name() + "/fd/" + fd
			target, linkErr := os.Readlink(link)
			if linkErr != nil || (target != ref.path && target != ref.path+" (deleted)") {
				continue
			}
			if got, ok := deviceOf(link); ok && got == ref.id {
				usesTTY = true
				break
			}
		}
		if usesTTY {
			if identity, ok := readProcIdentity(pid); ok {
				result = append(result, identity)
			}
		}
	}
	return result
}

// deviceOf stats path, following a /proc fd link to the file it holds.
func deviceOf(path string) (deviceID, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return deviceID{}, false
	}
	return deviceID{dev: uint64(st.Dev), rdev: uint64(st.Rdev)}, true
}

// sessionAlive reports whether any process remains in the session.
func sessionAlive(sid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	self := os.Getpid()
	for _, e := range entries {
		pid, convErr := strconv.Atoi(e.Name())
		if convErr != nil || pid <= 1 || pid == self {
			continue
		}
		if procSessionID(pid) == sid {
			return true
		}
	}
	return false
}
