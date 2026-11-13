//go:build linux

package selfinstall

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUninstallNohupStopsItsRealProcessBeforeDeletingProgram(t *testing.T) {
	h := uninstallFixture(t)
	h.supervisedByNohup()
	installUninstallProcessHelper(t, h.layout.Binary())
	cmd := startUninstallProcessHelper(t, h.layout.Binary(), h.layout.Dir)
	if err := writePID(h.layout.PIDFile(), cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	// Allow the kernel and exec.Cmd.Wait to observe the real child exit.
	h.installer.deps.Sleep = func(_ context.Context, duration time.Duration) { time.Sleep(duration / 10) }
	report, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.Mode != ModeNohup || processAlive(cmd.Process.Pid) {
		t.Fatalf("uninstall left its process running: mode=%s pid=%d", report.Mode, cmd.Process.Pid)
	}
	assertUninstallAbsent(t, h.layout.Binary(), h.layout.PIDFile())
	if _, err := os.Stat(h.layout.ConfigPath()); err != nil {
		t.Fatalf("ordinary uninstall lost retained configuration: %v", err)
	}
}

func TestUninstallNohupLeavesReusedPIDRunning(t *testing.T) {
	h := uninstallFixture(t)
	h.supervisedByNohup()
	pid := startSleeper(t)
	if err := writePID(h.layout.PIDFile(), pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.installer.Uninstall(context.Background(), UninstallRequest{Resources: emptyUninstallResources, Yes: true}, nil); err != nil {
		t.Fatal(err)
	}
	if !processAlive(pid) {
		t.Fatal("uninstall signalled an unrelated process from a stale PID file")
	}
	assertUninstallAbsent(t, h.layout.Binary(), h.layout.PIDFile())
}

func TestUninstallRefusesLiveExecutableThroughOtherNames(t *testing.T) {
	for _, name := range []string{"relative", "symlink", "hardlink", "unlinked hardlink"} {
		t.Run(name, func(t *testing.T) {
			h := uninstallFixture(t)
			h.supervisedByNohup()
			installUninstallProcessHelper(t, h.layout.Binary())
			var err error
			program := "./gpuworks"
			alias := filepath.Join(h.layout.Dir, "other-name")
			if name == "symlink" {
				err = os.Symlink(h.layout.Binary(), alias)
				program = alias
			} else if name == "hardlink" || name == "unlinked hardlink" {
				err = os.Link(h.layout.Binary(), alias)
				program = alias
			}
			if err != nil {
				t.Fatal(err)
			}
			// A real process gives the kernel executable identity and ps the
			// actual argv. There is no PID file or configured listener to help.
			cmd := startUninstallProcessHelper(t, program, h.layout.Dir)
			if name == "unlinked hardlink" {
				if err := os.Remove(alias); err != nil {
					t.Fatal(err)
				}
			}
			h.run.output["ps -ww -axo pid=,command="] = childProcessTable(t, cmd.Process.Pid)
			if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Resources: emptyUninstallResources, Yes: true, Purge: true}, nil); err == nil {
				t.Fatal("purge deleted a running executable's installation")
			}
			if !processAlive(cmd.Process.Pid) {
				t.Fatal("uninstall signalled an unmanaged process")
			}
			if _, err := os.Stat(h.layout.Binary()); err != nil {
				t.Fatalf("program was deleted: %v", err)
			}
			if readFile(t, filepath.Join(h.layout.DataDir, "gpuworks.db")) != "old database" {
				t.Fatal("data was deleted")
			}
		})
	}
}

func TestUninstallIgnoresKnownOtherExecutableWithServeArgument(t *testing.T) {
	h := uninstallFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "serve"), []byte("read value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = output.Close() })
	cmd := exec.Command("/bin/sh", "serve")
	cmd.Args[0] = "sh"
	cmd.Dir, cmd.Stdin = dir, input
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	table := childProcessTable(t, cmd.Process.Pid)
	if fields := strings.Fields(table); len(fields) != 3 || fields[1] != "sh" || fields[2] != "serve" {
		t.Fatalf("fixture did not expose its real relative serve argv: %q", table)
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "exe")); err != nil {
		t.Fatalf("fixture executable identity is not readable: %v", err)
	}
	h.run.output["ps -ww -axo pid=,command="] = table
	if err := h.installer.checkUninstallProcesses(t.Context()); err != nil {
		t.Fatalf("unrelated executable blocked uninstall: %v", err)
	}
	if !h.run.ran("ps -ww -axo pid=,command=") || !processAlive(cmd.Process.Pid) {
		t.Fatal("process check did not inspect the live fixture")
	}
	// A known foreign executable is safe to ignore, but it must not make an
	// unidentified relative serve safe. This PID cannot exist on Linux.
	h.run.output["ps -ww -axo pid=,command="] = table + "2147483647 ./gpuworks serve --addr 127.0.0.1:8802\n"
	if err := h.installer.checkUninstallProcesses(t.Context()); err == nil || !strings.Contains(err.Error(), "cannot identify relative server command for pid 2147483647") {
		t.Fatalf("unknown relative serve was not refused: %v", err)
	}
}

// Keep real ps argv and /proc executable identity for the child under test,
// while the Runner seam excludes unrelated sessions on a shared test host.
func childProcessTable(t *testing.T, pid int) string {
	t.Helper()
	table, err := (ExecRunner{}).Output(t.Context(), Command{Path: "ps", Args: []string{"-ww", "-p", strconv.Itoa(pid), "-o", "pid=,command="}})
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(table)
	if len(strings.Split(strings.TrimSpace(table), "\n")) != 1 || len(fields) < 2 || fields[0] != strconv.Itoa(pid) {
		t.Fatalf("ps did not return exactly the live fixture: %q", table)
	}
	return strings.TrimSpace(table) + "\n"
}

// Execute our own binary rather than a renamed system utility: multicall
// coreutils dispatch by argv[0], so copying sleep as gpuworks can exit at once.
func installUninstallProcessHelper(t *testing.T, path string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestUninstallProcessHelper(t *testing.T) {
	if os.Getenv("GPUWORKS_UNINSTALL_PROCESS_HELPER") != "1" {
		return
	}
	_, _ = os.Stdout.Write([]byte("ready\n"))
	time.Sleep(time.Minute)
	os.Exit(0)
}

// Readiness is observed before any uninstall or unlink, and Wait has one
// owner even when uninstall itself kills the child. Cleanup is scoped to it.
func startUninstallProcessHelper(t *testing.T, program, dir string) *exec.Cmd {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	cmd := exec.Command(program, "-test.run=^TestUninstallProcessHelper$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GPUWORKS_UNINSTALL_PROCESS_HELPER=1")
	cmd.Stdout = w
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("fixture process was not reaped")
		}
	})
	_ = w.Close()
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(r, ready); err != nil {
		t.Fatalf("fixture did not become ready: %v", err)
	}
	if string(ready) != "ready\n" || !processAlive(cmd.Process.Pid) {
		t.Fatal("fixture is not alive and ready")
	}
	return cmd
}
