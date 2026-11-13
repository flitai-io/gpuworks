package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

func uninstallStreams(input string, terminal bool) (*selfinstall.IO, *bytes.Buffer) {
	out := new(bytes.Buffer)
	return &selfinstall.IO{In: strings.NewReader(input), Out: out, Err: new(bytes.Buffer), IsTerminal: func() bool { return terminal }}, out
}

func TestUninstallCLIRequiresExplicitUnattendedConfirmation(t *testing.T) {
	for _, args := range [][]string{nil, {"--purge"}, {"--quiet"}, {"--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			streams, out := uninstallStreams("", false)
			called := false
			err := runUninstallCLI(context.Background(), args, streams, func(context.Context, selfinstall.Layout, selfinstall.UninstallRequest, *selfinstall.IO) (selfinstall.UninstallReport, error) {
				called = true
				return selfinstall.UninstallReport{}, nil
			})
			if err == nil || !strings.Contains(err.Error(), "--yes") || called {
				t.Fatalf("err=%v called=%v; unattended command must fail before execution", err, called)
			}
			if len(args) > 0 && args[0] == "--json" {
				var result map[string]any
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["success"] != false || result["error"] == "" {
					t.Fatalf("invalid error result: %s (%v)", out, err)
				}
			}
		})
	}
}

func TestUninstallCLICleanupSelection(t *testing.T) {
	cases := []struct {
		name, input string
		args        []string
		terminal    bool
		purge, yes  bool
		prompt      bool
	}{
		{name: "interactive defaults keep", input: "\n", terminal: true, prompt: true},
		{name: "interactive purge", input: "yes\n", terminal: true, purge: true, prompt: true},
		{name: "explicit purge", args: []string{"--purge"}, terminal: true, purge: true},
		{name: "explicit keep", args: []string{"--purge=false"}, terminal: true},
		{name: "script keep", args: []string{"--yes"}, yes: true},
		{name: "script purge", args: []string{"--purge", "-y"}, purge: true, yes: true},
		{name: "quiet retains choice", args: []string{"--quiet"}, input: "yes\n", terminal: true, purge: true, prompt: true},
		{name: "dry run keep", args: []string{"--dry-run"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			streams, out := uninstallStreams(tc.input, tc.terminal)
			called := false
			err := runUninstallCLI(context.Background(), tc.args, streams, func(_ context.Context, _ selfinstall.Layout, req selfinstall.UninstallRequest, _ *selfinstall.IO) (selfinstall.UninstallReport, error) {
				called = true
				if req.Purge != tc.purge || req.Yes != tc.yes {
					t.Fatalf("unexpected request: %+v", req)
				}
				return selfinstall.UninstallReport{}, nil
			})
			if err != nil || !called {
				t.Fatalf("err=%v called=%v", err, called)
			}
			if prompted := strings.Contains(out.String(), "Also delete"); prompted != tc.prompt {
				t.Fatalf("prompt=%v want=%v output=%s", prompted, tc.prompt, out)
			}
		})
	}
}

func TestUninstallCLIJSONContract(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			dir := t.TempDir()
			streams, out := uninstallStreams("", false)
			err := runUninstallCLI(context.Background(), []string{"--dir", dir, "--purge", "--yes", "--json"}, streams, func(_ context.Context, layout selfinstall.Layout, req selfinstall.UninstallRequest, streams *selfinstall.IO) (selfinstall.UninstallReport, error) {
				if layout.Dir != dir || !req.Purge || !req.Yes {
					t.Fatalf("layout=%+v req=%+v", layout, req)
				}
				streams.Say("this narration must not corrupt JSON")
				report := selfinstall.UninstallReport{Layout: layout, Mode: "systemd", ServiceUser: "gpuworks", Purge: true, Removed: []string{layout.Binary()}, Kept: []string{"service account: gpuworks"}}
				if failed {
					return report, errors.New("stop did not complete")
				}
				return report, nil
			})
			if (err != nil) != failed {
				t.Fatalf("err=%v failed=%v", err, failed)
			}
			var result map[string]any
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatalf("invalid JSON result: %s (%v)", out, err)
			}
			for _, key := range []string{"layout", "mode", "service_user", "purge", "dry_run", "already_absent", "removed", "kept", "keep_resources", "checklist", "success"} {
				if _, ok := result[key]; !ok {
					t.Errorf("missing wire field %s: %s", key, out)
				}
			}
			if result["success"] != !failed || result["mode"] != "systemd" || result["purge"] != true {
				t.Fatalf("unexpected result: %s", out)
			}
			if failed && result["error"] != "stop did not complete" {
				t.Fatalf("error not preserved: %s", out)
			}
		})
	}
}

func TestUninstallCLIArgumentErrorsRemainJSON(t *testing.T) {
	for _, args := range [][]string{{"--invalid", "--json"}, {"--json", "--yes", "unexpected"}} {
		streams, out := uninstallStreams("", false)
		err := runUninstallCLI(context.Background(), args, streams, func(context.Context, selfinstall.Layout, selfinstall.UninstallRequest, *selfinstall.IO) (selfinstall.UninstallReport, error) {
			t.Fatal("invalid arguments reached uninstall")
			return selfinstall.UninstallReport{}, nil
		})
		var result map[string]any
		if err == nil || json.Unmarshal(out.Bytes(), &result) != nil || result["success"] != false {
			t.Fatalf("err=%v output=%s", err, out)
		}
	}
}

func TestUninstallCLIRejectsEmptyAndRootDirectories(t *testing.T) {
	for _, dir := range []string{"", "  ", "/", "//", "/tmp/.."} {
		t.Run(dir, func(t *testing.T) {
			streams, out := uninstallStreams("", false)
			err := runUninstallCLI(context.Background(), []string{"--dir", dir, "--yes", "--json"}, streams, func(context.Context, selfinstall.Layout, selfinstall.UninstallRequest, *selfinstall.IO) (selfinstall.UninstallReport, error) {
				t.Fatal("invalid directory reached uninstall")
				return selfinstall.UninstallReport{}, nil
			})
			var result map[string]any
			if err == nil || !strings.Contains(err.Error(), "--dir") || json.Unmarshal(out.Bytes(), &result) != nil || result["success"] != false {
				t.Fatalf("err=%v output=%s", err, out)
			}
		})
	}
}

func TestUninstallCLIDryRunWithoutConfirmation(t *testing.T) {
	streams, out := uninstallStreams("", false)
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	err := runUninstallCLI(context.Background(), []string{"--dir", dir, "--purge", "--dry-run", "--json"}, streams, func(_ context.Context, layout selfinstall.Layout, req selfinstall.UninstallRequest, _ *selfinstall.IO) (selfinstall.UninstallReport, error) {
		if !req.DryRun || !req.Purge || req.Yes {
			t.Fatalf("unexpected request: %+v", req)
		}
		return selfinstall.UninstallReport{Layout: layout, DryRun: req.DryRun, Purge: req.Purge}, nil
	})
	if err != nil || strings.Contains(out.String(), "Also delete") {
		t.Fatalf("err=%v output=%s", err, out)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview created installation directory: %v", err)
	}
}

type uninstallRecordingRunner struct {
	commands []selfinstall.Command
	fail     string
}

func (r *uninstallRecordingRunner) Run(_ context.Context, cmd selfinstall.Command) error {
	r.commands = append(r.commands, cmd)
	if r.fail != "" && strings.Contains(cmd.String(), r.fail) {
		return errors.New("injected service failure")
	}
	return nil
}

func (r *uninstallRecordingRunner) Output(context.Context, selfinstall.Command) (string, error) {
	return "", errors.New("unexpected command output request")
}

func writeUninstallExposeUnit(t *testing.T, path string, layout selfinstall.Layout) {
	t.Helper()
	raw, err := expose.RenderServiceUnit(layout.Binary(), layout.DataDir, "gpuworks")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallExposePreservesSiblingServiceAndDoesNotCreateState(t *testing.T) {
	layout := selfinstall.NewLayout(filepath.Join(t.TempDir(), "target"), "", "")
	unitDir := t.TempDir()
	path := filepath.Join(unitDir, expose.SystemdUnitName)
	sibling := selfinstall.NewLayout(layout.Dir+"-sibling", "", "")
	writeUninstallExposeUnit(t, path, sibling)
	before, _ := os.ReadFile(path)
	runner := new(uninstallRecordingRunner)
	streams, _ := uninstallStreams("", false)
	removed, err := uninstallExpose(context.Background(), layout, runner, streams, unitDir)
	if err != nil || len(removed) != 0 || len(runner.commands) != 0 {
		t.Fatalf("removed=%v err=%v commands=%v", removed, err, runner.commands)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("sibling service changed")
	}
	if _, err := os.Stat(layout.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expose inspection created state: %v", err)
	}
}

func TestUninstallExposeStopsOnlyOwnedUnit(t *testing.T) {
	layout := selfinstall.NewLayout(t.TempDir(), "", "")
	unitDir := t.TempDir()
	path := filepath.Join(unitDir, expose.SystemdUnitName)
	writeUninstallExposeUnit(t, path, layout)
	runner := new(uninstallRecordingRunner)
	streams, _ := uninstallStreams("", false)
	removed, err := uninstallExpose(context.Background(), layout, runner, streams, unitDir)
	if err != nil || !reflect.DeepEqual(removed, []string{path}) {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	var args [][]string
	for _, cmd := range runner.commands {
		if cmd.Path != "systemctl" || !cmd.Sudo {
			t.Fatalf("unexpected command: %+v", cmd)
		}
		args = append(args, cmd.Args)
	}
	if !reflect.DeepEqual(args, [][]string{{"disable", "--now", expose.SystemdUnitName}, {"daemon-reload"}}) {
		t.Fatalf("commands=%v", args)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned unit remains: %v", err)
	}
}

func TestUninstallExposeKeepsUnitWhenStopFails(t *testing.T) {
	layout := selfinstall.NewLayout(t.TempDir(), "", "")
	unitDir := t.TempDir()
	path := filepath.Join(unitDir, expose.SystemdUnitName)
	writeUninstallExposeUnit(t, path, layout)
	runner := &uninstallRecordingRunner{fail: "disable"}
	streams, _ := uninstallStreams("", false)
	if _, err := uninstallExpose(context.Background(), layout, runner, streams, unitDir); err == nil {
		t.Fatal("service stop failure was hidden")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unit deleted after stop failure: %v", err)
	}
}

func TestUninstallExposeRefusesSharedBinaryAndOverrides(t *testing.T) {
	layout := selfinstall.NewLayout(t.TempDir(), "", "")
	path := filepath.Join(t.TempDir(), expose.SystemdUnitName)
	shared := layout
	shared.DataDir = layout.DataDir + "-sibling"
	writeUninstallExposeUnit(t, path, shared)
	if _, err := uninstallExposeUnitOwned(path, layout); err == nil {
		t.Fatal("service sharing target executable was accepted")
	}
	writeUninstallExposeUnit(t, path, layout)
	if err := os.Mkdir(path+".d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".d", "override.conf"), []byte("[Service]\nExecStart=/other/gpuworks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := uninstallExposeUnitOwned(path, layout); err == nil {
		t.Fatal("overridden service ownership was accepted")
	}
}

func TestUninstallExposeRefusesUnreadableConfigurationWithLiveSupervisor(t *testing.T) {
	layout := selfinstall.NewLayout(t.TempDir(), "", "")
	store := expose.NewStore(layout.DataDir)
	release, err := store.SupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := os.WriteFile(store.ConfigPath(), []byte("{bad-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	streams, _ := uninstallStreams("", false)
	runner := new(uninstallRecordingRunner)
	if _, err := uninstallExpose(context.Background(), layout, runner, streams, t.TempDir()); err == nil || !strings.Contains(err.Error(), "unreadable configuration") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("mutated service with unreadable config: %v", runner.commands)
	}
}

func TestUninstallExposeRequestsShutdownAndHonorsCancellation(t *testing.T) {
	layout := selfinstall.NewLayout(t.TempDir(), "", "")
	store := expose.NewStore(layout.DataDir)
	if err := store.WriteConfig(&expose.Config{Revision: "before", Provider: expose.ProviderQuick, Desired: expose.DesiredEnabled}); err != nil {
		t.Fatal(err)
	}
	release, err := store.SupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	streams, _ := uninstallStreams("", false)
	if _, err := uninstallExpose(ctx, layout, new(uninstallRecordingRunner), streams, t.TempDir()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown wait ignored cancellation: %v", err)
	}
	cfg, err := store.ReadConfig()
	if err != nil || cfg.Desired != expose.DesiredDisabled || cfg.Revision == "before" {
		t.Fatalf("shutdown request not recorded: cfg=%+v err=%v", cfg, err)
	}
}

func TestUninstallExposeRefusesUnexaminableSupervisorLock(t *testing.T) {
	layout := selfinstall.NewLayout(t.TempDir(), "", "")
	store := expose.NewStore(layout.DataDir)
	if err := store.WriteConfig(&expose.Config{Revision: "before", Provider: expose.ProviderQuick, Desired: expose.DesiredEnabled}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.Root(), ".supervisor.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	streams, _ := uninstallStreams("", false)
	runner := new(uninstallRecordingRunner)
	if _, err := uninstallExpose(context.Background(), layout, runner, streams, t.TempDir()); err == nil || !strings.Contains(err.Error(), "cannot determine") {
		t.Fatalf("unexaminable lock was accepted: %v", err)
	}
	cfg, err := store.ReadConfig()
	if err != nil || cfg.Desired != expose.DesiredEnabled || cfg.Revision != "before" || len(runner.commands) != 0 {
		t.Fatalf("changed state after lock failure: cfg=%+v err=%v commands=%v", cfg, err, runner.commands)
	}
}
