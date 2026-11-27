//go:build linux || darwin

package terminal

import (
	"fmt"
	"os"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/ptyio"
)

// Supported reports whether this build can allocate a pty.
func Supported() bool { return true }

// pty is an allocated pseudo-terminal pair.
type pty struct {
	master *os.File
	slave  *os.File
}

// openPTY allocates a pty pair (ptyio.Open). The master is nonblocking, so a
// pending Read returns when another goroutine calls Close: without that the
// output pump would stay parked in Read forever, leaking a pty and a shell for
// every browser tab that goes away.
func openPTY() (*pty, error) {
	master, slave, err := ptyio.Open()
	if err != nil {
		return nil, err
	}
	return &pty{master: master, slave: slave}, nil
}

// resize applies a new window size. The kernel delivers SIGWINCH to the
// foreground process group, which is how full-screen programs learn to redraw.
func (p *pty) resize(rows, cols uint16) error { return ptyio.SetSize(p.master, rows, cols) }

// winsize reads the current window size back. Only tests need this.
func (p *pty) winsize() (rows, cols uint16, err error) { return ptyio.Size(p.master) }

// closeSlave releases the parent's copy of the slave fd. It must happen once
// the child has inherited it, otherwise the master never sees EOF: the kernel
// keeps the pty open as long as any slave fd survives, so a session whose shell
// has already exited would hang forever.
func (p *pty) closeSlave() {
	if p.slave != nil {
		_ = p.slave.Close()
		p.slave = nil
	}
}

// pinTTY opens another reference to the slave at path, or returns nil. While
// it is open the kernel cannot give the pts number to a new terminal, even
// after the master closes. Close needs that: it looks for the session's
// leftovers by slave path once the master is gone, and without the pin a
// terminal opened in the meantime (another session, an SSH login, a docker pull
// the agent is running) could take the number and be killed as one of them.
func pinTTY(ref ttyRef) *os.File {
	path := fmt.Sprint(ref)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil
	}
	return f
}

func (p *pty) close() {
	p.closeSlave()
	if p.master != nil {
		_ = p.master.Close()
	}
}

// ptySysProcAttr makes the child a session leader with the pty slave as its
// controlling terminal. Setctty is what gives the child job control and makes
// the kernel send SIGHUP to its foreground group when the master closes.
// Setsid also places it in a fresh process group whose id equals its pid, which
// is what lets reap() signal the entire group by negative pid.
//
// Ctty is a descriptor number in the child, not the parent: the child's stdin,
// stdout and stderr are all the pty slave, so 0 is correct.
func ptySysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
}

// signalProcessIdentities works leaf-first and verifies each process start time
// before signalling. That avoids hitting an unrelated process if a PID is
// recycled during the terminal's grace period.
func signalProcessIdentities(processes []procIdentity, sig syscall.Signal) {
	self := os.Getpid()
	for i := len(processes) - 1; i >= 0; i-- {
		process := processes[i]
		if process.pid <= 1 || process.pid == self {
			continue
		}
		current, ok := readProcIdentity(process.pid)
		if !ok || current.start != process.start {
			continue
		}
		_ = syscall.Kill(process.pid, sig)
	}
}

// isPTYClosed reports whether err is the EIO that Linux and macOS return when
// reading a master whose last slave fd has closed. That is the normal end of a session,
// not a failure, and treating it as an error would surface a spurious message
// every time a shell exits.
func isPTYClosed(err error) bool { return ptyio.IsClosed(err) }
