//go:build docker

package docker

// Explicit real-runtime check (never selected by the offline suite):
// go test -tags=docker -run TestDockerTerminalSessionsRemainIsolatedOnClose -v ./internal/agent/runtime/docker
// Requires a dedicated gpuworks-e2e Unix daemon and a cached shell image via
// GPUWORKS_L3_SHELL_BASE_IMAGE (default alpine:3.20).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/terminal"
)

// dockerCLITimeout bounds one docker CLI call; terminalStepTimeout bounds a
// wait for the container to show an effect of what the terminal wrote or an
// owned session close. Both are ceilings for a hung daemon: every wait returns
// as soon as its condition holds, so a healthy run is unaffected.
const (
	dockerCLITimeout    = 2 * time.Minute
	terminalStepTimeout = time.Minute
)

func TestDockerTerminalSessionsRemainIsolatedOnClose(t *testing.T) {
	host, ok := localDockerHost()
	if !ok || !strings.Contains(host, "gpuworks-e2e") {
		t.Fatal("explicit real test requires a dedicated gpuworks-e2e Unix daemon")
	}
	// Every CLI call may share the daemon with a busy CI host: in a Docker-in-
	// Docker job a single `run -d` or `rm -f` has taken longer than 15s while
	// other test binaries compiled and ran beside it. A killed client leaves
	// the container behind, so the bound only has to catch a daemon that is
	// really hung, not one that is slow.
	docker := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), dockerCLITimeout)
		defer cancel()
		return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	}
	// containerFile polls a file the shell inside the container writes. A shell
	// redirection creates the file before its content is written, so a bare
	// "cat succeeded" can observe it empty; accept only content the caller
	// recognizes as complete.
	containerFile := func(container, path string, complete func(string) bool) (string, error) {
		deadline := time.Now().Add(terminalStepTimeout)
		var last string
		var lastErr error
		for time.Now().Before(deadline) {
			output, err := docker("exec", container, "cat", path)
			last, lastErr = strings.TrimSpace(string(output)), err
			if err == nil && complete(last) {
				return last, nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return last, fmt.Errorf("%s never completed: last=%q err=%v", path, last, lastErr)
	}
	isPID := func(value string) bool {
		pid, err := strconv.Atoi(value)
		return err == nil && pid > 1
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "e2e-terminal-isolation-" + hex.EncodeToString(nonce[:])
	image := os.Getenv("GPUWORKS_L3_SHELL_BASE_IMAGE")
	if image == "" {
		image = "alpine:3.20"
	}
	// Docker can create the named container and then fail to start it. Register
	// cleanup before run so that partial creation follows the same owned path.
	container := name
	type owned struct {
		local *terminal.Session
		close func(context.Context) error
		pid   string
		start string
	}
	var sessions []*owned
	t.Cleanup(func() {
		for _, session := range sessions {
			if session.local != nil {
				if err := session.local.Close(); err != nil {
					t.Errorf("owned local close: %v", err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), terminalStepTimeout)
			if err := session.close(ctx); err != nil {
				t.Errorf("owned remote close: %v", err)
			}
			cancel()
		}
		if output, err := docker("rm", "-f", container); err != nil && !strings.Contains(string(output), "No such container:") {
			t.Errorf("reclaim only %s: %v: %s", container, err, output)
		}
		output, err := docker("ps", "-aq", "--filter", "name=^/"+name+"$")
		if err != nil || len(strings.TrimSpace(string(output))) != 0 {
			t.Errorf("owned container remains %s: %v: %s", container, err, output)
		}
	})
	output, err := docker("run", "-d", "--name", name, image, "sleep", "300")
	if err != nil {
		t.Fatalf("owned container run: %v: %s", err, output)
	}
	container = strings.TrimSpace(string(output))
	start := func(label string) *owned {
		t.Helper()
		argv, closeSession, err := New().ExecPTYSession(context.Background(), container, nil)
		if err != nil {
			t.Fatal(err)
		}
		session := &owned{close: closeSession}
		sessions = append(sessions, session)
		session.local, err = terminal.Open(terminal.Spec{Argv: argv, Dir: "/", Rows: 24, Cols: 80, Grace: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		path := "/tmp/" + name + "-" + label + ".pid"
		if _, err = session.local.Write([]byte("printf '%s' \"$$\" > " + path + "\n")); err != nil {
			t.Fatal(err)
		}
		session.pid, err = containerFile(container, path, isPID)
		if err != nil {
			t.Fatalf("owned shell PID missing: %v", err)
		}
		output, err = docker("exec", container, "cat", "/proc/"+session.pid+"/stat")
		if err != nil {
			t.Fatalf("owned shell not alive: %v: %s", err, output)
		}
		fields := strings.Fields(strings.TrimSpace(string(output[strings.LastIndexByte(string(output), ')')+1:])))
		if len(fields) < 20 || fields[0] != "S" {
			t.Fatalf("owned shell not sleeping/live: %s", output)
		}
		session.start = fields[19]
		t.Logf("owned real terminal container=%s mode=%s pid=%s start_ticks=%s", container, label, session.pid, session.start)
		return session
	}
	first, second := start("first"), start("second")
	// A terminal-owned job may create a separate session (for example, runuser
	// does this). Its parent shell must stay alive until remote teardown has
	// captured and reaped that descendant, before disconnecting the local CLI.
	escapedPath := "/tmp/" + name + "-escaped.pid"
	if _, err = first.local.Write([]byte("/bin/sh -c 'setsid sleep 300 & echo $! > " + escapedPath + "; wait' &\n")); err != nil {
		t.Fatal(err)
	}
	escapedPID, err := containerFile(container, escapedPath, isPID)
	if err != nil {
		t.Fatalf("escaped descendant PID missing: %v", err)
	}
	output, err = docker("exec", container, "cat", "/proc/"+escapedPID+"/stat")
	if err != nil {
		t.Fatalf("escaped descendant not alive: %v: %s", err, output)
	}
	escapedStat := strings.Fields(string(output[strings.LastIndexByte(string(output), ')')+1:]))
	if len(escapedStat) < 20 || escapedStat[3] == first.pid {
		t.Fatalf("fixture did not escape original terminal session: %s", output)
	}
	closeOwned := func(session *owned) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), terminalStepTimeout)
		defer cancel()
		if err := session.close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := session.local.Close(); err != nil {
			t.Fatal(err)
		}
		output, err := docker("exec", container, "cat", "/proc/"+session.pid+"/stat")
		expected := "cat: can't open '/proc/" + session.pid + "/stat': No such file or directory"
		if err == nil || strings.TrimSpace(string(output)) != expected {
			t.Fatalf("owned shell release unconfirmed: err=%v output=%s", err, output)
		}
	}
	closeOwned(first)
	if output, err = docker("exec", container, "kill", "-0", "1"); err != nil {
		t.Fatalf("container main process affected by terminal close: %v: %s", err, output)
	}
	if output, err = docker("exec", container, "test", "!", "-e", "/proc/"+escapedPID+"/stat"); err != nil {
		t.Fatalf("escaped terminal descendant survived close: %v: %s", err, output)
	}
	output, err = docker("exec", container, "cat", "/proc/"+second.pid+"/stat")
	if err != nil {
		t.Fatalf("other terminal was killed: %v: %s", err, output)
	}
	fields := strings.Fields(string(output[strings.LastIndexByte(string(output), ')')+1:]))
	if len(fields) < 20 || fields[19] != second.start || fields[0] != "S" {
		t.Fatalf("other terminal identity changed: %s", output)
	}
	marker := "independent second session " + name
	path := "/tmp/" + name + "-after-first-close.txt"
	command := fmt.Sprintf("printf 'independent second session %%s' %s > %s\n", name, path)
	if _, err = second.local.Write([]byte(command)); err != nil {
		t.Fatal(err)
	}
	if got, err := containerFile(container, path, func(value string) bool { return value == marker }); err != nil {
		t.Fatalf("unrelated shell no longer executes: %v (got %q)", err, got)
	}
	closeOwned(second)
	output, err = docker("exec", container, "/bin/sh", "-c", `set -- /tmp/gpuworks-webterm-*; [ "$1" = '/tmp/gpuworks-webterm-*' ]`)
	if err != nil {
		t.Fatalf("terminal private registration leaked: %v: %s", err, output)
	}
	t.Log("both real terminal identities gone, unrelated session survived first close, private registrations absent")
}
