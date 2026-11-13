package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func uninstallFixture(t *testing.T) (linuxUninstaller, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	proc := filepath.Join(root, "proc")
	for _, dir := range []string{state, proc} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"identity.json", "model.bin", "agent.log", "gpuworks-agent.pid"} {
		if err := os.WriteFile(filepath.Join(state, file), []byte("keep-unless-uninstalled"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	u := linuxUninstaller{
		binary: filepath.Join(root, "gpuworks-agent"), procDir: proc, unitPath: filepath.Join(root, "gpuworks-agent.service"),
		run:    func(context.Context, ...string) error { return errors.New("unexpected systemctl") },
		signal: func(pid int) error { return os.RemoveAll(filepath.Join(proc, strconv.Itoa(pid))) },
	}
	return u, state
}

func addUninstallProcess(t *testing.T, u linuxUninstaller, pid int, args ...string) {
	t.Helper()
	dir := filepath.Join(u.procDir, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(args[0], filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallStopsOnlyThisInstallationsRunProcesses(t *testing.T) {
	u, state := uninstallFixture(t)
	addUninstallProcess(t, u, 101, u.binary, "run", "--state", state)
	addUninstallProcess(t, u, 102, u.binary, "run", "--state="+state)
	addUninstallProcess(t, u, 103, u.binary, "uninstall", "--state", state)
	addUninstallProcess(t, u, 104, u.binary, "run", "--state", state+"-other")
	addUninstallProcess(t, u, 105, "/another/gpuworks-agent", "run", "--state", state)
	if err := u.uninstall(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{101, 102} {
		if _, err := os.Stat(filepath.Join(u.procDir, strconv.Itoa(pid))); !os.IsNotExist(err) {
			t.Errorf("agent process %d still exists", pid)
		}
	}
	for _, pid := range []int{103, 104, 105} {
		if _, err := os.Stat(filepath.Join(u.procDir, strconv.Itoa(pid))); err != nil {
			t.Errorf("unrelated process %d was removed", pid)
		}
	}
	for _, name := range []string{"identity.json", "gpuworks-agent.pid"} {
		if _, err := os.Stat(filepath.Join(state, name)); !os.IsNotExist(err) {
			t.Errorf("%s was not removed", name)
		}
	}
	for _, name := range []string{"model.bin", "agent.log"} {
		if _, err := os.Stat(filepath.Join(state, name)); err != nil {
			t.Errorf("user data %s removed: %v", name, err)
		}
	}
	if err := u.uninstall(t.Context(), state); err != nil {
		t.Fatalf("retry after successful cleanup: %v", err)
	}
}

func TestUninstallSystemdAndFailurePreservesIdentity(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			u, state := uninstallFixture(t)
			if err := os.WriteFile(u.unitPath, []byte("[Service]\nExecStart="+u.binary+" run --state "+state+" --server http://control\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var commands []string
			u.run = func(_ context.Context, args ...string) error {
				command := strings.Join(args, " ")
				commands = append(commands, command)
				switch command {
				case "disable --now gpuworks-agent.service":
					if fail {
						return errors.New("permission denied")
					}
				case "daemon-reload":
					if _, err := os.Stat(u.unitPath); !os.IsNotExist(err) {
						t.Fatal("reloaded before removing service unit")
					}
				default:
					t.Fatalf("unexpected systemctl command %q", command)
				}
				return nil
			}
			err := u.uninstall(t.Context(), "")
			if fail {
				if err == nil {
					t.Fatal("stop failure reported as successful uninstall")
				}
				if _, err := os.Stat(filepath.Join(state, "identity.json")); err != nil {
					t.Fatalf("identity lost on failure: %v", err)
				}
				if _, err := os.Stat(u.unitPath); err != nil {
					t.Fatalf("unit lost on failure: %v", err)
				}
			} else if err != nil || !reflect.DeepEqual(commands, []string{"disable --now gpuworks-agent.service", "daemon-reload"}) {
				t.Fatalf("commands=%v err=%v", commands, err)
			}
		})
	}
}

// A unit running from another state directory is another node's agent on the
// same host: uninstalling this one stops only this directory's process and
// leaves that unit installed and running.
func TestUninstallLeavesAnotherDirectorysServiceRunning(t *testing.T) {
	u, state := uninstallFixture(t)
	unit := "ExecStart=" + u.binary + " run --state /another/state\n"
	if err := os.WriteFile(u.unitPath, []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}
	addUninstallProcess(t, u, 101, u.binary, "run", "--state", state)
	addUninstallProcess(t, u, 102, u.binary, "run", "--state", "/another/state")
	if err := u.uninstall(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(u.procDir, "101")); !os.IsNotExist(err) {
		t.Fatalf("this directory's agent still runs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(u.procDir, "102")); err != nil {
		t.Fatalf("the service's agent was stopped: %v", err)
	}
	if data, err := os.ReadFile(u.unitPath); err != nil || string(data) != unit {
		t.Fatalf("the other directory's unit changed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(state, "identity.json")); !os.IsNotExist(err) {
		t.Fatalf("this installation's identity remains: %v", err)
	}
}

// A unit that names no state directory cannot be told apart from this one.
func TestUninstallRefusesAServiceWithoutAStateDirectory(t *testing.T) {
	u, state := uninstallFixture(t)
	if err := os.WriteFile(u.unitPath, []byte("ExecStart="+u.binary+" run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := u.uninstall(t.Context(), state); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("expected ownership failure, got %v", err)
	}
}

func TestUninstallReturnsProcessAndCleanupErrors(t *testing.T) {
	for _, scenario := range []string{"signal", "timeout", "identity"} {
		t.Run(scenario, func(t *testing.T) {
			u, state := uninstallFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			switch scenario {
			case "signal":
				addUninstallProcess(t, u, 101, u.binary, "run", "--state", state)
				u.signal = func(int) error { return syscall.EPERM }
			case "timeout":
				addUninstallProcess(t, u, 101, u.binary, "run", "--state", state)
				u.signal = func(int) error { return nil }
			case "identity":
				file := filepath.Join(state, "identity.json")
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(file, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(file, "cannot-delete"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := u.uninstall(ctx, state); err == nil {
				t.Fatal("failed uninstall reported success")
			}
			if _, err := os.Stat(filepath.Join(state, "identity.json")); err != nil {
				t.Fatalf("identity lost: %v", err)
			}
		})
	}
}

func TestUninstallUnitStateParsing(t *testing.T) {
	for _, tc := range []struct{ unit, state string }{
		{"ExecStart=/bin/agent run --server http://control", ""},
		{`ExecStart=/bin/agent run --state "/path with spaces" --server http://control`, "/path with spaces"},
		{"ExecStart=/bin/agent run --state=/path/state", "/path/state"},
	} {
		if got := unitStateDir(tc.unit); got != tc.state {
			t.Errorf("state=%q want %q", got, tc.state)
		}
	}
}
