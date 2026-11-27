// Package terminal runs interactive pty sessions on the agent host.
//
// The package owns everything pty-related: allocation, the login shell and its
// environment, window resizing, and reaping the process group on teardown. Its
// caller (internal/agent/conn) only reads a stream header and hands over the
// yamux stream, and never learns what a pty is.
//
// Idle timeouts, concurrency limits and audit records live on the server, which
// is the side that knows the platform settings and the operator's identity. The
// agent's only policy input is the SIGTERM-to-SIGKILL grace period, carried in
// the stream header.
package terminal

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// DefaultGrace is used when the stream header carries no grace period.
const DefaultGrace = 6 * time.Second

// ErrUnsupported is returned when this build cannot allocate a pty.
var ErrUnsupported = errors.New("terminal sessions are only supported on linux and macOS nodes")

// Spec describes a session to open.
type Spec struct {
	// Rows and Cols are the initial window size. Zero falls back to 24x80.
	Rows, Cols uint16
	// Grace is how long to wait between SIGTERM and SIGKILL when reaping the
	// process group. Zero means DefaultGrace.
	Grace time.Duration
	// Argv overrides the default host login shell. The container branch sets it
	// to the container runtime's exec command; the pty itself is allocated here
	// either way, because `docker exec -it` requires a real tty on stdin and the
	// agent's own stdin is a pipe.
	Argv []string
	// ExtraEnv is appended to the constructed environment.
	ExtraEnv []string
	// Dir overrides the starting directory (default: the user's home). The
	// container branch sets it to "/": the container runtime decides the
	// container's working directory, and the agent user's home may be
	// unreachable for the account the runtime drops to (/root under runveil).
	Dir string
	// Cleanup owns runtime resources outside the local PTY process tree. Serve
	// calls it exactly once, including Open failure and peer disconnect, and
	// completes it before emitting a terminal outcome. The owner supplies its
	// own fresh bounded context. Nil preserves the host-only lifecycle.
	Cleanup func() error
}

// Session is a running pty and the process group attached to it.
type Session struct {
	pty *pty
	cmd *exec.Cmd
	// sid is the terminal's session id. Setsid makes it equal to the shell's pid,
	// and every descendant inherits it, so it bounds the processes Close reaps.
	sid   int
	grace time.Duration
	info  proto.TermSessionInfo
	tty   ttyRef

	waited   chan struct{} // closed once wait() has stored exitCode
	exitCode int32

	closeOnce sync.Once
}

// Open allocates a pty and starts the shell (or Argv) attached to it.
func Open(spec Spec) (*Session, error) {
	if !Supported() {
		slog.Warn("terminal unsupported on this platform")
		return nil, ErrUnsupported
	}
	rows, cols := spec.Rows, spec.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	grace := spec.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}

	id := currentIdentity()
	shell := detectShell()
	argv := spec.Argv
	if len(argv) == 0 {
		argv = loginArgv(shell)
	}

	p, err := openPTY()
	if err != nil {
		return nil, err
	}
	// Size the pty before the child starts so a full-screen program never draws
	// one frame at the wrong size.
	if err := p.resize(rows, cols); err != nil {
		p.close()
		return nil, fmt.Errorf("set window size: %w", err)
	}
	tty := ttyRefOf(p.slave)

	cwd := resolveCwd(id.Home)
	if spec.Dir != "" {
		cwd = spec.Dir
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = buildEnv(id, shell, spec.ExtraEnv)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.slave, p.slave, p.slave
	cmd.SysProcAttr = ptySysProcAttr()

	if err := cmd.Start(); err != nil {
		slog.Warn("terminal shell start failed", "shell", argv, "err", err)
		p.close()
		return nil, fmt.Errorf("start %s: %w", argv[0], err)
	}
	// The child holds its own slave fd now. Releasing the parent's copy is what
	// lets the master report EOF after the shell exits; keeping it would leave
	// every finished session hanging.
	p.closeSlave()

	s := &Session{
		pty:   p,
		cmd:   cmd,
		sid:   cmd.Process.Pid, // Setsid makes the session id equal the pid
		grace: grace,
		tty:   tty,
		info: proto.TermSessionInfo{
			User: id.Name, UID: id.UID, Shell: argv[0], Cwd: cwd,
		},
		waited:   make(chan struct{}),
		exitCode: -1,
	}
	if host, err := os.Hostname(); err == nil {
		s.info.Host = host
	}
	go s.wait()
	slog.Debug("terminal shell started", "pid", s.sid, "shell", argv[0], "cwd", cwd)
	return s, nil
}

// wait reaps the leader and records its exit code. Called exactly once.
func (s *Session) wait() {
	defer close(s.waited)
	err := s.cmd.Wait()
	if err == nil {
		s.exitCode = 0
		return
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		s.exitCode = int32(ee.ExitCode())
		// A signalled process reports ExitCode() == -1; report the conventional
		// 128+signal so the operator can tell a SIGKILL from a crash.
		if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			s.exitCode = int32(128 + int(st.Signal()))
		}
	}
}

// Info describes the account and shell the session is running as.
func (s *Session) Info() proto.TermSessionInfo { return s.info }

// Read returns pty output. A closed pty reports io.EOF rather than the raw EIO
// Linux uses to signal that the last slave fd is gone.
func (s *Session) Read(p []byte) (int, error) {
	n, err := s.pty.master.Read(p)
	if err != nil && isPTYClosed(err) {
		return n, io.EOF
	}
	return n, err
}

// Write sends bytes to the pty as terminal input.
func (s *Session) Write(p []byte) (int, error) { return s.pty.master.Write(p) }

// Resize applies a new window size, which makes the kernel deliver SIGWINCH to
// the foreground process group.
func (s *Session) Resize(rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return nil // a hidden or unmeasured browser tab; ignore rather than fail
	}
	return s.pty.resize(rows, cols)
}

// ExitCode blocks until the leader has been reaped and returns its exit status.
func (s *Session) ExitCode() int32 {
	<-s.waited
	return s.exitCode
}

// Wait blocks until the leader exits.
func (s *Session) Wait() { <-s.waited }

// Close tears the session down and reaps every process in it.
//
// Closing the master makes the kernel send SIGHUP to the foreground process
// group, which is enough for an interactive shell. It is not enough in general:
// processes started under nohup, and anything that ignores SIGHUP, survive it.
//
// The fallback signals the whole terminal session, not just the leader's process
// group. An interactive shell enables job control and puts each job in its own
// process group, so the leader's group holds nothing but the shell and a kill by
// negative pid misses every background job. Descendants do inherit the session
// id, which makes the session the unit that actually bounds a terminal.
//
// Skipping this has a concrete cost on a GPU node: someone runs a python process
// that fills VRAM, closes the browser tab, and the process keeps the memory. The
// scheduler still counts the card as occupied and it is effectively lost until a
// human SSHes in.
func (s *Session) Close() error {
	slog.Debug("terminal session closing", "pid", s.sid)
	var err error
	s.closeOnce.Do(func() {
		// Snapshot and signal the complete descendant tree before closing the
		// master. runuser may put udocker's Python/proot children in another
		// session; after their parent exits they are reparented and can no longer
		// be discovered from the PTY leader.
		tree := append(processTree(s.sid), processesUsingTTY(s.tty)...)
		signalProcessIdentities(tree, syscall.SIGTERM)

		if s.sid > 0 {
			_ = signalSession(s.sid, syscall.SIGTERM)
		}
		// Keep the pts number ours until the last lookup by slave below. Once
		// the master is closed and the session's processes let go of the slave,
		// the number is free, and a terminal opened meanwhile would carry the
		// same device identity as ours and be killed with the leftovers.
		pin := pinTTY(s.tty)
		s.pty.close() // SIGHUP to the foreground group
		select {
		case <-s.waited:
		case <-time.After(s.grace):
		}
		// Unconditional: the shell may already be gone while its children linger.
		tree = append(tree, processTree(s.sid)...)
		if pin != nil {
			tree = append(tree, processesUsingTTY(s.tty)...)
		}
		signalProcessIdentities(tree, syscall.SIGKILL)
		if pin != nil {
			_ = pin.Close()
		}
		if s.sid > 0 {
			err = signalSession(s.sid, syscall.SIGKILL)
		}
		<-s.waited
	})
	return err
}
