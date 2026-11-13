package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/launchd"
	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

type fixtureLaunchctlExit int

func (e fixtureLaunchctlExit) Error() string { return fmt.Sprintf("exit status %d", e) }
func (e fixtureLaunchctlExit) ExitCode() int { return int(e) }

func TestAgentUninstallCLIMacOwnershipAndShutdown(t *testing.T) {
	for _, behavior := range []string{"success", "bootout_failure", "foreign_binary", "foreign_label", "foreign_command", "foreign_program", "foreign_live_program", "process_table_failure", "process_table_partial", "job_absent_with_live_agent", "no_plist_live_agent", "no_plist_alias_live_agent", "no_plist_process_table_failure", "no_plist_empty"} {
		t.Run(behavior, func(t *testing.T) {
			state, root := t.TempDir(), t.TempDir()
			binary := filepath.Join(state, "gpuworks-agent")
			h := &launchd.Host{Root: root, Admin: true, Self: launchd.Account{UID: 501, Home: "/Users/fixture"}, Wait: 10 * time.Millisecond, Tick: time.Millisecond}
			spec := launchd.Spec{Label: launchd.AgentLabel, Args: []string{binary, "run", "--state", state}}
			switch behavior {
			case "foreign_binary":
				spec.Args[0] = "/foreign/gpuworks-agent"
			case "foreign_label":
				spec.Label = "foreign.application"
			case "foreign_command":
				spec.Args[1] = "unrelated-daemon"
			case "foreign_program":
				spec.Program = "/foreign/program"
			}
			plist := filepath.Join(root, launchd.DaemonPlist())
			if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plist, spec.Plist(), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"identity.json", agentconf.FileName, "model.bin", "agent.log"} {
				if err := os.WriteFile(filepath.Join(state, name), []byte("secret-retention-canary"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			noPlist := strings.HasPrefix(behavior, "no_plist_")
			if noPlist {
				if err := os.Remove(plist); err != nil {
					t.Fatal(err)
				}
			}
			processState := state
			if behavior == "no_plist_alias_live_agent" {
				processState = filepath.Join(root, "state-alias")
				if err := os.Symlink(state, processState); err != nil {
					t.Fatal(err)
				}
			}
			loaded, bootouts := behavior != "job_absent_with_live_agent" && !noPlist, 0
			h.ProcessAlive = func(int) (bool, error) { return loaded, nil }
			h.ProcessTable = func(context.Context) (string, error) {
				if strings.Contains(behavior, "process_table_failure") {
					return "", errors.New("secret-process-table-canary")
				}
				if behavior == "process_table_partial" {
					return "1 /sbin/launchd\npartial-secret-row", nil
				}
				if loaded || behavior == "job_absent_with_live_agent" || behavior == "no_plist_live_agent" || behavior == "no_plist_alias_live_agent" {
					return "1 /sbin/launchd\n4242 " + binary + " run --state " + processState + "\n", nil
				}
				return "1 /sbin/launchd\n", nil
			}
			h.Launchctl = func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "print":
					if !loaded {
						return `Could not find service "ai.gpuworks.agent" in domain`, fixtureLaunchctlExit(113)
					}
					program := binary
					if behavior == "foreign_live_program" {
						program = "/foreign/program"
					}
					return "system/ai.gpuworks.agent = {\n state = running\n program = " + program + "\n pid = 4242\n}", nil
				case "bootout":
					bootouts++
					if behavior == "bootout_failure" {
						return "secret-launchctl-canary", context.DeadlineExceeded
					}
					loaded = false
					return "", nil
				default:
					return "unexpected launchctl operation", fixtureLaunchctlExit(91)
				}
			}
			var out bytes.Buffer
			d := agentUninstallDeps{resolve: func(string) ([]string, error) {
				dirs, err := agentLaunchdStates(h, binary)
				if err == nil && len(dirs) == 0 {
					dirs = []string{state}
				}
				return dirs, err
			}, observe: func(ctx context.Context, state string) []uninstallcheck.Item {
				return agentLaunchdChecklistForHost(ctx, state, h, binary)
			}, remove: func(ctx context.Context, dirs []string) error { return uninstallLaunchdStates(ctx, dirs, h, binary) }}
			err := runAgentUninstall(context.Background(), []string{"--yes", "--keep-resources", "--json"}, strings.NewReader(""), &out, false, d)
			success := behavior == "success" || behavior == "no_plist_empty"
			if (err == nil) != success {
				t.Fatalf("behavior=%s err=%v output=%s", behavior, err, &out)
			}
			if (strings.HasPrefix(behavior, "foreign_") || strings.HasPrefix(behavior, "process_table_") || behavior == "job_absent_with_live_agent" || noPlist) && bootouts != 0 {
				t.Fatalf("foreign job was stopped: %d", bootouts)
			}
			if success {
				for _, path := range []string{plist, filepath.Join(state, "identity.json"), agentconf.Path(state)} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("successful removal retained %s: %v", path, err)
					}
				}
			} else {
				paths := []string{filepath.Join(state, "identity.json"), agentconf.Path(state)}
				if !noPlist {
					paths = append(paths, plist)
				}
				for _, path := range paths {
					if _, err := os.Stat(path); err != nil {
						t.Fatalf("failed/foreign removal changed %s: %v", path, err)
					}
				}
			}
			for _, name := range []string{"model.bin", "agent.log"} {
				if _, err := os.Stat(filepath.Join(state, name)); err != nil {
					t.Fatalf("workload data removed: %v", err)
				}
			}
			if strings.Contains(out.String(), "secret-") || strings.Contains(out.String(), "/foreign/") {
				t.Fatalf("untrusted details leaked: %s", &out)
			}
		})
	}
}

func TestAgentUninstallCLIMacTwoJobsRemainDiscoverableAfterPartialStop(t *testing.T) {
	root, stateA, stateB := t.TempDir(), t.TempDir(), t.TempDir()
	binary := "/fixture/gpuworks-agent"
	h := &launchd.Host{Root: root, Admin: true, Self: launchd.Account{UID: 501, Home: "/Users/fixture"}, Wait: 10 * time.Millisecond, Tick: time.Millisecond}
	services := []string{"system/" + launchd.AgentLabel, "gui/501/" + launchd.AgentLabel}
	states, plists := []string{stateA, stateB}, agentLaunchdPaths(h)
	loaded, failSecond := []bool{true, true}, true
	for i, path := range plists {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, (launchd.Job{Binary: binary, StateDir: states[i]}).Plist(), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"identity.json", agentconf.FileName} {
			if err := os.WriteFile(filepath.Join(states[i], name), []byte("keep until stopped"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.ProcessAlive = func(pid int) (bool, error) { return loaded[pid-700], nil }
	h.ProcessTable = func(context.Context) (string, error) {
		out := "1 /sbin/launchd\n"
		for i, running := range loaded {
			if running {
				out += fmt.Sprintf("%d %s run --state %s\n", 700+i, binary, states[i])
			}
		}
		return out, nil
	}
	h.Launchctl = func(_ context.Context, args ...string) (string, error) {
		i := 0
		if args[1] == services[1] {
			i = 1
		}
		if args[0] == "print" {
			if !loaded[i] {
				return "Could not find service", fixtureLaunchctlExit(113)
			}
			return fmt.Sprintf("state = running\nprogram = %s\npid = %d\n", binary, 700+i), nil
		}
		if args[0] == "bootout" {
			if i == 1 && failSecond {
				return "", context.DeadlineExceeded
			}
			loaded[i] = false
			return "", nil
		}
		return "", fixtureLaunchctlExit(91)
	}
	d := agentUninstallDeps{resolve: func(string) ([]string, error) { return agentLaunchdStates(h, binary) }, observe: func(ctx context.Context, state string) []uninstallcheck.Item {
		return agentLaunchdChecklistForHost(ctx, state, h, binary)
	}, remove: func(ctx context.Context, dirs []string) error { return uninstallLaunchdStates(ctx, dirs, h, binary) }}
	var out bytes.Buffer
	if err := runAgentUninstall(context.Background(), []string{"--yes", "--keep-resources", "--json"}, strings.NewReader(""), &out, false, d); err == nil {
		t.Fatal("second job stop failure was ignored")
	}
	for _, path := range plists {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("partial stop lost a discoverable installation: %v", err)
		}
	}
	failSecond = false
	out.Reset()
	if err := runAgentUninstall(context.Background(), []string{"--yes", "--keep-resources", "--json"}, strings.NewReader(""), &out, false, d); err != nil {
		t.Fatalf("retry failed: %v output=%s", err, &out)
	}
	for _, state := range states {
		for _, name := range []string{"identity.json", agentconf.FileName} {
			if _, err := os.Stat(filepath.Join(state, name)); !os.IsNotExist(err) {
				t.Fatalf("successful retry retained credentials: %v", err)
			}
		}
	}
}
