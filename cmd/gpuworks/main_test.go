package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A bare invocation prints the usage and fails; it must not panic reaching
// into arguments that were never there.
func TestRunWithoutSubcommandPrintsUsage(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(discardLog(), nil, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("the usage is missing:\n%s", stderr.String())
	}
}

func TestRunHelpExitsCleanly(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(discardLog(), []string{"help"}, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("the usage is missing:\n%s", stderr.String())
	}
}

func TestRunRejectsAnUnknownSubcommand(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(discardLog(), []string{"frobnicate"}, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown subcommand "frobnicate"`) {
		t.Fatalf("the rejection is missing:\n%s", stderr.String())
	}
}

// A server installing this release from the console asks it which console
// upgrade protocol it speaks; a release from before that rejects the
// subcommand like any unknown one (TestRunRejectsAnUnknownSubcommand).
func TestRunAnswersTheConsoleUpgradeProtocol(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	code := run(discardLog(), []string{selfinstall.ConsoleUpgradeProtocolCommand}, io.Discard)
	os.Stdout = stdout
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if code != 0 || strings.TrimSpace(string(out)) != strconv.Itoa(selfinstall.ConsoleUpgradeProtocol) {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestUpgradeTakesOneSource(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--archive", "gpuworks.tar.gz", "--base-url", "http://mirror.internal"}, "pass one"},
		{[]string{"--source", "https://mirror.example", "--base-url", "http://mirror.internal"}, "pass one"},
		{[]string{"--source", "https://mirror.example", "--archive", "gpuworks.tar.gz"}, "pass one"},
		{[]string{"--source", " "}, "needs a URL"},
		{[]string{"--base-url="}, "needs a URL"},
		{[]string{"--channel", "release", "--base-url", "http://mirror.internal"}, "does not apply"},
		{[]string{"--channel", "release", "--archive", "gpuworks.tar.gz"}, "does not apply"},
		{[]string{"--archive="}, "needs a path"},
		{[]string{"--archive", "  "}, "needs a path"},
	} {
		// Each is refused before the installation directory is even read, so
		// the default --dir need not exist.
		err := upgradeCommand(context.Background(), tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("upgrade %v: err = %v, want it to mention %q", tc.args, err, tc.want)
		}
	}
}

// A server that cannot bind writes why to its own log file: a detached
// (nohup) server's stderr is /dev/null, and the file is where install and
// upgrade point an operator whose server did not come up. The reason is
// printed once, by the server's logger, not a second time by run.
func TestServeLogsWhyItExitedToItsLogFile(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	dir := t.TempDir()
	logFile := filepath.Join(dir, "logs", "gpuworks-server.log")
	var base bytes.Buffer
	log := slog.New(slog.NewTextHandler(&base, nil))

	code := run(log, []string{"serve", "--addr", held.Addr().String(), "--data", filepath.Join(dir, "data"),
		"--log-file", logFile, "--recovery-enabled=false"}, io.Discard)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read the server log: %v", err)
	}
	if got := string(raw); !strings.Contains(got, `msg="server exited"`) || !strings.Contains(got, "address already in use") {
		t.Fatalf("the log file does not say why the server exited:\n%s", got)
	}
	if strings.Contains(base.String(), "server exited") {
		t.Fatalf("run printed the reason a second time:\n%s", base.String())
	}
}

func TestServerLoggerPreservesRedirectedStderr(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS uses a launchd stderr file")
	}
	stderrPath := filepath.Join(t.TempDir(), "stderr.log")
	original := strings.Repeat("existing log\n", 100)
	if err := os.WriteFile(stderrPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = saved; stderr.Close() })
	logPath := filepath.Join(t.TempDir(), "explicit", "server.log")
	log, closeLog := serverLogger(logPath, "1KB", 2, slog.LevelInfo)
	log.Info("redirected server record")
	closeLog()
	for _, path := range []string{stderrPath, logPath} {
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), "redirected server record") {
			t.Fatalf("%s missing record: %v", path, err)
		}
		if path == stderrPath && !strings.HasPrefix(string(data), original) {
			t.Fatal("existing redirected stderr was truncated")
		}
	}
}
