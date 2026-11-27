//go:build !linux && !darwin

package terminal

import (
	"errors"
	"os"
	"syscall"
)

// errUnsupported is returned on platforms with no pty support in this build.
// The agent ships for Linux and macOS; these stubs keep the package compiling
// for any other host a developer builds on.
var errUnsupported = errors.New("terminal sessions are only supported on linux and macOS nodes")

// Supported reports whether this build can allocate a pty.
func Supported() bool { return false }

type pty struct {
	master *os.File
	slave  *os.File
}

type procIdentity struct {
	pid, ppid, sid int
	start          uint64
}

func openPTY() (*pty, error) { return nil, errUnsupported }

func (p *pty) resize(rows, cols uint16) error { return errUnsupported }

func (p *pty) winsize() (rows, cols uint16, err error) { return 0, 0, errUnsupported }

func (p *pty) closeSlave() {}

func (p *pty) close() {}

func ptySysProcAttr() *syscall.SysProcAttr { return nil }

func signalSession(sid int, sig syscall.Signal) error { return errUnsupported }

func processTree(root int) []procIdentity { return nil }

type ttyRef string

func ttyRefOf(slave *os.File) ttyRef { return "" }

func processesUsingTTY(ref ttyRef) []procIdentity { return nil }

func pinTTY(ref ttyRef) *os.File { return nil }

func signalProcessIdentities(processes []procIdentity, sig syscall.Signal) {}

func sessionAlive(sid int) bool { return false }

func isPTYClosed(err error) bool { return false }
