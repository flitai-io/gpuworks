//go:build darwin

package terminal

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// procIdentity is a process as the kernel's process table describes it. start
// (microseconds since the epoch) tells a recycled pid apart; tdev is the
// controlling terminal's device number, or -1.
type procIdentity struct {
	pid, ppid, sid int
	start          uint64
	tdev           int32
}

// processes lists every process in the kernel's process table. There is no
// /proc on macOS; kern.proc.all is the cgo-free equivalent. Session ids are
// not in the table, so they are read per process with getsid(2).
func processes() []procIdentity {
	kinfos, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	out := make([]procIdentity, 0, len(kinfos))
	for i := range kinfos {
		identity := kinfoIdentity(&kinfos[i])
		if identity.pid <= 1 || identity.pid == self {
			continue
		}
		out = append(out, identity)
	}
	return out
}

func kinfoIdentity(k *unix.KinfoProc) procIdentity {
	pid := int(k.Proc.P_pid)
	sid := procSessionID(pid)
	start := uint64(k.Proc.P_starttime.Sec)*1e6 + uint64(k.Proc.P_starttime.Usec)
	return procIdentity{pid: pid, ppid: int(k.Eproc.Ppid), sid: sid, start: start, tdev: k.Eproc.Tdev}
}

func readProcIdentity(pid int) (procIdentity, bool) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(k.Proc.P_pid) != pid {
		return procIdentity{}, false
	}
	return kinfoIdentity(k), true
}

// signalSession sends sig to every process in the session led by sid; see the
// Linux variant for why the session, not the process group, bounds a terminal.
func signalSession(sid int, sig syscall.Signal) error {
	for _, p := range processes() {
		if p.sid == sid {
			_ = syscall.Kill(p.pid, sig)
		}
	}
	return nil
}

// processTree snapshots root and all of its descendants.
func processTree(root int) []procIdentity {
	byParent := make(map[int][]procIdentity)
	var rootIdentity procIdentity
	for _, p := range processes() {
		if p.pid == root {
			rootIdentity = p
		}
		byParent[p.ppid] = append(byParent[p.ppid], p)
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

// ttyRef names a session's pty slave by path.
type ttyRef string

func ttyRefOf(slave *os.File) ttyRef { return ttyRef(slave.Name()) }

// processesUsingTTY finds processes whose controlling terminal is the slave at
// path. macOS has no per-process fd table to read without libproc, so the
// controlling terminal stands in for "holds the slave on stdio"; a process that
// detached from it is still caught by processTree.
func processesUsingTTY(ref ttyRef) []procIdentity {
	path := string(ref)
	if path == "" {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return nil
	}
	var result []procIdentity
	for _, p := range processes() {
		if p.tdev == st.Rdev {
			result = append(result, p)
		}
	}
	return result
}

// sessionAlive reports whether any process remains in the session.
func sessionAlive(sid int) bool {
	for _, p := range processes() {
		if p.sid == sid {
			return true
		}
	}
	return false
}

// procSessionID reads a process's session id, or -1.
func procSessionID(pid int) int {
	sid, err := unix.Getsid(pid)
	if err != nil {
		return -1
	}
	return sid
}
