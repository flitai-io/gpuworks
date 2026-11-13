package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func uninstallMacFixture(t *testing.T) (*testMac, Job, string) {
	t.Helper()
	m := newTestMac(t, true)
	job := testJob(t, "https://example.invalid")
	job.User = "alice"
	plist := filepath.Join(m.Root, DaemonPlist())
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, job.Plist(), 0o600); err != nil {
		t.Fatal(err)
	}
	m.launchd.loaded["system/"+AgentLabel] = 700
	m.launchd.programs["system/"+AgentLabel] = job.Binary
	return m, job, plist
}

func TestUninstallPreservesPlistWhenBootoutCannotStopJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"timeout", context.DeadlineExceeded},
		{"cancelled", context.Canceled},
		{"io_failure", errors.New("input/output error")},
		{"permission", errors.New("operation denied")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, job, plist := uninstallMacFixture(t)
			m.Launchctl = func(ctx context.Context, args ...string) (string, error) {
				if args[0] == "bootout" {
					return "private-output-canary", tc.err
				}
				return m.launchd.run(ctx, args...)
			}
			dirs, err := m.Uninstall(context.Background(), job.Binary, nil)
			if err == nil || len(dirs) != 0 {
				t.Fatalf("failed stop accepted: dirs=%v err=%v", dirs, err)
			}
			if strings.Contains(err.Error(), "private-output-canary") {
				t.Fatalf("raw launchctl output leaked: %v", err)
			}
			if _, err := os.Stat(plist); err != nil {
				t.Fatalf("failed stop removed plist: %v", err)
			}
		})
	}
}

func TestUninstallWaitsUntilLaunchdReleasesItsJob(t *testing.T) {
	m, job, plist := uninstallMacFixture(t)
	if _, err := m.Uninstall(context.Background(), job.Binary, nil); err != nil {
		t.Fatal(err)
	}
	if _, loaded := m.launchd.loaded["system/"+AgentLabel]; loaded {
		t.Fatal("plist removed while launchd still holds the job")
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatalf("confirmed stopped plist retained: %v", err)
	}
}

func TestUninstallRejectsForeignPlistBeforeStopping(t *testing.T) {
	for _, change := range []string{"label", "command", "program_override", "executable", "relative_state", "duplicate_state"} {
		t.Run(change, func(t *testing.T) {
			m, job, plist := uninstallMacFixture(t)
			spec, err := ReadSpec(job.Plist())
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "label":
				spec.Label = "other.application"
			case "command":
				spec.Args[1] = "unrelated-daemon"
			case "program_override":
				spec.Program = "/foreign/program"
			case "executable":
				spec.Args[0] = "/foreign/gpuworks-agent"
			case "relative_state":
				spec.Args[3] = "relative/state"
			case "duplicate_state":
				spec.Args = append(spec.Args, "--state", job.StateDir)
			}
			if err := os.WriteFile(plist, spec.Plist(), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Uninstall(context.Background(), job.Binary, nil); err == nil {
				t.Fatal("foreign/ambiguous launchd ownership accepted")
			}
			if len(m.launchd.calls) != 0 {
				t.Fatalf("ownership failure ran launchctl: %v", m.launchd.calls)
			}
			if _, err := os.Stat(plist); err != nil {
				t.Fatalf("foreign plist removed: %v", err)
			}
		})
	}
}

func TestUninstallRetainsPlistWhenJobDisappearsButProcessRemains(t *testing.T) {
	m, job, plist := uninstallMacFixture(t)
	m.Wait = 5 * time.Millisecond
	m.ProcessAlive = func(int) (bool, error) { return true, nil }
	m.ProcessTable = func(context.Context) (string, error) {
		return "1 /sbin/launchd\n700 " + job.Binary + " run --state " + job.StateDir + "\n", nil
	}
	if dirs, err := m.Uninstall(context.Background(), job.Binary, nil); err == nil || len(dirs) != 0 {
		t.Fatalf("live former PID accepted: dirs=%v err=%v", dirs, err)
	}
	if _, err := os.Stat(plist); err != nil {
		t.Fatalf("live process lost restart definition: %v", err)
	}
	if dirs, err := m.Uninstall(context.Background(), job.Binary, nil); err == nil || len(dirs) != 0 {
		t.Fatalf("retry forgot the live former process: dirs=%v err=%v", dirs, err)
	}
}

func TestUninstallRetainsPlistOnIncompleteShutdownObservation(t *testing.T) {
	for _, behavior := range []string{"process_probe_error", "print_error", "cancel_after_bootout"} {
		t.Run(behavior, func(t *testing.T) {
			m, job, plist := uninstallMacFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stopping := false
			m.Launchctl = func(ctx context.Context, args ...string) (string, error) {
				if args[0] == "bootout" {
					stopping = true
					if behavior == "cancel_after_bootout" {
						cancel()
					}
				}
				if args[0] == "print" && stopping && behavior == "print_error" {
					return "No such process", fakeLaunchctlExit(1)
				}
				return m.launchd.run(ctx, args...)
			}
			if behavior == "process_probe_error" {
				m.ProcessAlive = func(int) (bool, error) { return false, errors.New("probe unavailable") }
			}
			if dirs, err := m.Uninstall(ctx, job.Binary, nil); err == nil || len(dirs) != 0 {
				t.Fatalf("incomplete stop accepted: dirs=%v err=%v", dirs, err)
			}
			if _, err := os.Stat(plist); err != nil {
				t.Fatalf("uncertain stop removed definition: %v", err)
			}
		})
	}
}

func TestAgentProgramPIDRejectsRunningJobWithoutPID(t *testing.T) {
	out := "system/ai.gpuworks.agent = {\n state = running\n program = /fixture/gpuworks-agent\n}"
	if _, err := AgentProgramPID(out, "/fixture/gpuworks-agent"); err == nil {
		t.Fatal("running job without PID was accepted as stopped")
	}
}
