//go:build linux || darwin

package terminal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// readUntil reads from the session until want appears or the deadline passes.
func readUntil(t *testing.T, s *Session, want string, timeout time.Duration) string {
	t.Helper()
	var got []byte
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = s.pty.master.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 4096)
		n, err := s.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
			if strings.Contains(string(got), want) {
				return string(got)
			}
		}
		if err != nil && !os.IsTimeout(err) {
			break
		}
	}
	t.Fatalf("timed out waiting for %q; got %q", want, got)
	return ""
}

func TestSessionEchoRoundTrip(t *testing.T) {
	s, err := Open(Spec{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// A pty echoes its input, so a marker written literally in the command line
	// shows up before the command has run. Assembling it from shell variables
	// means the echoed text cannot contain the string being awaited.
	if _, err := s.Write([]byte("A=gpuworks; B=ok; printf '%s-%s\\n' \"$A\" \"$B\"\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, s, "gpuworks-ok", 10*time.Second)
}

// A login shell is what puts conda and CUDA on PATH via /etc/profile.d, so the
// session must not silently degrade to a non-login shell.
func TestSessionRunsLoginShell(t *testing.T) {
	argv := loginArgv(detectShell())
	if len(argv) != 2 || argv[1] != "-l" {
		t.Fatalf("loginArgv = %v, want a -l login shell", argv)
	}
}

func TestSessionInfoReportsIdentity(t *testing.T) {
	s, err := Open(Spec{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	info := s.Info()
	if info.UID != os.Getuid() {
		t.Errorf("uid = %d, want %d", info.UID, os.Getuid())
	}
	if info.Shell == "" {
		t.Error("shell is empty")
	}
	if info.Cwd == "" {
		t.Error("cwd is empty")
	}
}

// The pty must carry the requested size before the shell starts, otherwise
// full-screen programs draw one frame at 80x24 first.
func TestSessionInitialWindowSize(t *testing.T) {
	s, err := Open(Spec{Rows: 40, Cols: 132})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	rows, cols, err := s.pty.winsize()
	if err != nil {
		t.Fatalf("winsize: %v", err)
	}
	if rows != 40 || cols != 132 {
		t.Errorf("winsize = %dx%d, want 40x132", rows, cols)
	}
}

// Resize must reach the process inside the pty, not just the master fd: verify
// through stty, which reads the size from its controlling terminal.
func TestSessionResizeVisibleToChild(t *testing.T) {
	s, err := Open(Spec{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, s, "24 80", 10*time.Second)

	if err := s.Resize(50, 200); err != nil {
		t.Fatalf("resize: %v", err)
	}
	rows, cols, err := s.pty.winsize()
	if err != nil {
		t.Fatalf("winsize: %v", err)
	}
	if rows != 50 || cols != 200 {
		t.Fatalf("winsize = %dx%d, want 50x200", rows, cols)
	}

	if _, err := s.Write([]byte("stty size\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, s, "50 200", 10*time.Second)
}

func TestSessionResizeIgnoresZeroDimensions(t *testing.T) {
	s, err := Open(Spec{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if err := s.Resize(0, 0); err != nil {
		t.Errorf("Resize(0,0) = %v, want nil", err)
	}
	rows, cols, err := s.pty.winsize()
	if err != nil {
		t.Fatal(err)
	}
	if rows != 24 || cols != 80 {
		t.Errorf("winsize = %dx%d, want unchanged 24x80", rows, cols)
	}
}

// waitSessionGone waits for every process in the terminal session to exit.
func waitSessionGone(sid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !sessionAlive(sid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !sessionAlive(sid)
}

// The most important test in this package.
//
// Closing a session must reap the entire process group, including children that
// survive SIGHUP. Otherwise a python process that filled VRAM keeps running
// after the operator closes the browser tab, the scheduler still counts the
// card as occupied, and the GPU is effectively lost until a human intervenes.
func TestSessionCloseReapsProcessGroupIncludingNohup(t *testing.T) {
	s, err := Open(Spec{Rows: 24, Cols: 80, Grace: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sid := s.sid

	// A child that explicitly ignores SIGHUP and SIGTERM, so only the SIGKILL
	// fallback can reap it. This is the nohup case that closing the pty alone
	// does not handle. The marker is assembled from variables so the pty's echo
	// of this command line does not match it.
	if _, err := s.Write([]byte("trap '' HUP TERM; sleep 300 & P=$!; C=child; printf '%s_%s\\n' \"$C\" \"$P\"\n")); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, s, "child_", 10*time.Second)

	childPID := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "child_"); ok {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(after)); convErr == nil {
				childPID = pid
				break
			}
		}
	}
	if childPID == 0 {
		t.Fatalf("could not parse child pid from %q", out)
	}
	if syscall.Kill(childPID, 0) != nil {
		t.Fatalf("child %d did not start", childPID)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if !waitSessionGone(sid, 5*time.Second) {
		t.Errorf("terminal session %d still alive after Close", sid)
	}
	if syscall.Kill(childPID, 0) == nil {
		t.Errorf("child %d survived Close; VRAM would stay pinned", childPID)
	}
}

// runuser may create a new session for the command it launches. udocker then
// leaves Python/proot outside the PTY leader's session, so cleanup must also
// follow the descendant tree captured before the wrapper exits.
func TestSessionCloseReapsDescendantThatEscapesWithSetsid(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is unavailable")
	}
	s, err := Open(Spec{Rows: 24, Cols: 80, Grace: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s.Write([]byte("setsid sh -c 'trap \"\" HUP TERM; E=escaped; printf \"%s_%s\\\\n\" \"$E\" \"$$\"; sleep 300' &\n")); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, s, "escaped_", 10*time.Second)
	escapedPID := 0
	for _, line := range strings.Split(out, "\n") {
		if marker := strings.Index(line, "escaped_"); marker >= 0 {
			value := line[marker+len("escaped_"):]
			end := 0
			for end < len(value) && value[end] >= '0' && value[end] <= '9' {
				end++
			}
			if end > 0 {
				escapedPID, _ = strconv.Atoi(value[:end])
			}
		}
	}
	if escapedPID == 0 {
		t.Fatalf("could not parse escaped pid from %q", out)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && procSessionID(escapedPID) == s.sid {
		time.Sleep(10 * time.Millisecond)
	}
	if got := procSessionID(escapedPID); got == s.sid || got < 0 {
		t.Fatalf("child %d did not escape terminal session %d (sid=%d)", escapedPID, s.sid, got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && syscall.Kill(escapedPID, 0) == nil {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(escapedPID, 0) == nil {
		t.Fatalf("setsid descendant %d survived terminal Close", escapedPID)
	}
}

// A model process is not a descendant of the web terminal session.  Closing
// the terminal must therefore leave that independent process untouched; the
// cleanup walker is scoped to the PTY session, its descendant tree, and its tty
// file descriptors rather than using a broad host-wide process-name kill.
func TestSessionCloseDoesNotKillIndependentModelProcess(t *testing.T) {
	s, err := Open(Spec{Rows: 24, Cols: 80, Grace: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	model := exec.Command("sleep", "300")
	model.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := model.Start(); err != nil {
		t.Fatalf("start independent model process: %v", err)
	}
	modelPID := model.Process.Pid
	defer func() {
		_ = syscall.Kill(modelPID, syscall.SIGKILL)
		_ = model.Wait()
	}()

	if err := s.Close(); err != nil {
		t.Fatalf("close terminal: %v", err)
	}
	if err := syscall.Kill(modelPID, 0); err != nil {
		t.Fatalf("independent model process %d was killed by terminal close: %v", modelPID, err)
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	s, err := Open(Spec{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

// The master must report EOF once the shell exits; if the parent kept its slave
// fd the read would block forever and sessions would never finish.
func TestSessionReadReturnsEOFAfterShellExits(t *testing.T) {
	s, err := Open(Spec{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.Write([]byte("exit 7\n")); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := s.Read(buf); err != nil {
				done <- err
				return
			}
		}
	}()

	select {
	case err := <-done:
		if err != io.EOF {
			t.Errorf("read error = %v, want io.EOF", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for EOF after shell exit")
	}

	if code := s.ExitCode(); code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}

// A signalled shell reports 128+signal so an operator can tell SIGKILL from a
// crash.
func TestSessionExitCodeForSignalledShell(t *testing.T) {
	s, err := Open(Spec{Grace: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := syscall.Kill(s.sid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if code := s.ExitCode(); code != 128+int32(syscall.SIGKILL) {
		t.Errorf("exit code = %d, want %d", code, 128+int32(syscall.SIGKILL))
	}
	_ = s.Close()
}

// The environment is constructed rather than inherited: TERM and a UTF-8 locale
// are what keep vim usable and nvidia-smi's box drawing from turning to
// mojibake, and GPUWORKS_* internals must not leak into an operator's shell.
func TestSessionEnvironment(t *testing.T) {
	t.Setenv("GPUWORKS_UDOCKER_BUNDLE", "/should/not/leak")

	s, err := Open(Spec{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// Expand the variables rather than naming their values, so the pty's echo of
	// this command line cannot satisfy the assertions below.
	if _, err := s.Write([]byte("printf '%s|%s|[%s]\\n' \"$TERM\" \"$LANG\" \"$GPUWORKS_UDOCKER_BUNDLE\"\n")); err != nil {
		t.Fatal(err)
	}
	out := readUntil(t, s, "xterm-256color", 10*time.Second)

	if !strings.Contains(out, "C.UTF-8") {
		t.Errorf("LANG not set as expected in %q", out)
	}
	if strings.Contains(out, "/should/not/leak") {
		t.Errorf("agent env leaked into the session: %q", out)
	}
}

func TestSessionExtraEnvReachesShell(t *testing.T) {
	s, err := Open(Spec{ExtraEnv: []string{"GPUWORKS_TEST_MARKER=present"}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.Write([]byte("printf 'marker|%s|\\n' \"$GPUWORKS_TEST_MARKER\"\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, s, "marker|present|", 10*time.Second)
}

// pipeRW joins two pipes into the single ReadWriter that Serve expects, standing
// in for a yamux stream.
type pipeRW struct {
	io.Reader
	io.Writer
}

// Serve is the boundary conn hands the stream to; exercise it end to end over
// frames, the way the server will.
func TestServeBridgesFramesEndToEnd(t *testing.T) {
	peerReader, agentWriter := io.Pipe() // agent -> peer
	agentReader, peerWriter := io.Pipe() // peer -> agent
	defer peerReader.Close()
	defer peerWriter.Close()

	served := make(chan error, 1)
	go func() {
		served <- Serve(pipeRW{Reader: agentReader, Writer: agentWriter}, Spec{
			Rows: 24, Cols: 80, Grace: 500 * time.Millisecond,
		})
		_ = agentWriter.Close()
	}()

	var (
		mu       sync.Mutex
		stdout   bytes.Buffer
		info     proto.TermSessionInfo
		gotInfo  bool
		exitCode int32 = -999
	)
	frames := make(chan struct{}, 64)
	go func() {
		fr := proto.NewTermFrameReader(peerReader)
		for {
			f, err := fr.Next()
			if err != nil {
				close(frames)
				return
			}
			mu.Lock()
			switch f.Type {
			case proto.TermData:
				stdout.Write(f.Payload)
			case proto.TermInfo:
				if json.Unmarshal(f.Payload, &info) == nil {
					gotInfo = true
				}
			case proto.TermExit:
				if code, _, err := proto.DecodeExit(f.Payload); err == nil {
					exitCode = code
				}
			}
			mu.Unlock()
			select {
			case frames <- struct{}{}:
			default:
			}
		}
	}()

	waitFor := func(cond func() bool, what string) {
		t.Helper()
		deadline := time.After(15 * time.Second)
		for {
			mu.Lock()
			ok := cond()
			mu.Unlock()
			if ok {
				return
			}
			select {
			case _, open := <-frames:
				if !open {
					mu.Lock()
					ok = cond()
					mu.Unlock()
					if ok {
						return
					}
					t.Fatalf("stream ended before %s", what)
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	waitFor(func() bool { return gotInfo }, "info frame")
	if info.UID != os.Getuid() {
		t.Errorf("info uid = %d, want %d", info.UID, os.Getuid())
	}

	// Keep the shell in one command until stty has read the size. Readline
	// can write back the old size while preparing the next prompt.
	// Assemble the marker in the shell so the input echo cannot satisfy the wait.
	if err := proto.WriteTermFrame(peerWriter, proto.TermData, []byte("A=served; printf '%s_ok\\n' \"$A\"; IFS= read -r reply && stty size\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(func() bool { return strings.Contains(stdout.String(), "served_ok") }, "echo output")

	const rows, cols = 45, 150
	if err := proto.WriteTermFrame(peerWriter, proto.TermResize, proto.EncodeResize(rows, cols)); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteTermFrame(peerWriter, proto.TermData, []byte("\n")); err != nil {
		t.Fatal(err)
	}
	wantSize := fmt.Sprintf("%d %d", rows, cols)
	waitFor(func() bool { return strings.Contains(stdout.String(), wantSize) }, "resized stty output")

	// An unknown frame type must be ignored rather than ending the session.
	if err := proto.WriteTermFrame(peerWriter, 0x7e, []byte("ignored")); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteTermFrame(peerWriter, proto.TermData, []byte("A=still; printf '%s_alive\\n' \"$A\"\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(func() bool { return strings.Contains(stdout.String(), "still_alive") }, "output after unknown frame")

	if err := proto.WriteTermFrame(peerWriter, proto.TermData, []byte("exit 3\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(func() bool { return exitCode != -999 }, "exit frame")
	mu.Lock()
	code := exitCode
	mu.Unlock()
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// When the peer hangs up mid-session, Serve must return and reap the group
// rather than leaking a pty and a shell per abandoned browser tab.
func TestServeReapsWhenPeerHangsUp(t *testing.T) {
	peerReader, agentWriter := io.Pipe()
	agentReader, peerWriter := io.Pipe()

	served := make(chan error, 1)
	go func() {
		served <- Serve(pipeRW{Reader: agentReader, Writer: agentWriter}, Spec{
			Rows: 24, Cols: 80, Grace: 300 * time.Millisecond,
		})
	}()
	go func() { _, _ = io.Copy(io.Discard, peerReader) }()

	if err := proto.WriteTermFrame(peerWriter, proto.TermData, []byte("trap '' HUP TERM; sleep 300 &\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	_ = peerWriter.Close() // browser tab closed

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not return after peer hung up")
	}
}

func TestDetectShellReturnsExecutable(t *testing.T) {
	sh := detectShell()
	st, err := os.Stat(sh)
	if err != nil {
		t.Fatalf("stat %s: %v", sh, err)
	}
	if st.Mode()&0o111 == 0 {
		t.Errorf("%s is not executable", sh)
	}
}
