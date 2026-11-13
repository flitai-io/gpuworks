package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

type localChecklistRunner struct {
	checklistRunner
	service    string
	serviceErr error
	process    string
	launchd    string
}

func (r *localChecklistRunner) Output(ctx context.Context, cmd selfinstall.Command) (string, error) {
	if cmd.Path == "systemctl" && reflect.DeepEqual(cmd.Args, []string{"show", "--no-pager", "--property=ActiveState", "--property=UnitFileState", selfinstall.ServiceName}) {
		return r.service, r.serviceErr
	}
	if cmd.Path == "ps" && len(cmd.Args) > 0 && cmd.Args[0] == "-p" {
		return r.process, nil
	}
	if cmd.Path == "launchctl" && len(cmd.Args) > 0 && cmd.Args[0] == "print" {
		return r.launchd, r.serviceErr
	}
	return r.checklistRunner.Output(ctx, cmd)
}

func TestUninstallCLIDryRunExplainsLocalServiceAndShutdownEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, output, status string
		queryErr             error
	}{
		{name: "running and enabled", output: "ActiveState=active\nUnitFileState=enabled\n", status: uninstallcheck.Warn},
		{name: "inactive and disabled", output: "ActiveState=inactive\nUnitFileState=disabled\n", status: uninstallcheck.Pass},
		{name: "manager unavailable", queryErr: errors.New("fixture cannot query manager"), status: uninstallcheck.Unknown},
		{name: "manager empty response", status: uninstallcheck.Unknown},
		{name: "partial response", output: "ActiveState=active\n", status: uninstallcheck.Unknown},
		{name: "unexpected manager value", output: "ActiveState=DO-NOT-LEAK\nUnitFileState=enabled\n", status: uninstallcheck.Unknown},
		{name: "duplicate manager field", output: "ActiveState=active\nActiveState=inactive\nUnitFileState=enabled\n", status: uninstallcheck.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout, _ := serverChecklistFixture(t)
			if err := serverconfig.Write(layout.ConfigPath(), serverconfig.File{DataDir: layout.DataDir, Log: serverconfig.Log{File: layout.LogFile()}, Listen: serverconfig.Listen{Console: []string{"127.0.0.1:8800"}, Gateway: []string{"127.0.0.1:8802"}}}, 0600); err != nil {
				t.Fatal(err)
			}
			unit := selfinstall.RenderUnit(selfinstall.UnitConfig{Binary: layout.Binary(), ConfigPath: layout.ConfigPath(), DataDir: layout.DataDir, ServiceUser: "gpuworks"})
			if err := os.WriteFile(layout.UnitPath(), []byte(unit), 0644); err != nil {
				t.Fatal(err)
			}
			before := checklistTree(t, layout.Dir)
			runner := &localChecklistRunner{service: tc.output, serviceErr: tc.queryErr}
			streams, out := uninstallStreams("", false)
			err := runUninstallCLI(t.Context(), []string{"--dir", layout.Dir, "--dry-run", "--json"}, streams, func(ctx context.Context, _ selfinstall.Layout, req selfinstall.UninstallRequest, streams *selfinstall.IO) (selfinstall.UninstallReport, error) {
				return selfinstall.New(layout, selfinstall.Deps{Run: runner}, streams).Uninstall(ctx, req, nil)
			})
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Checklist []uninstallcheck.Item `json:"checklist"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			byID := map[string]uninstallcheck.Item{}
			for _, item := range result.Checklist {
				byID[item.ID] = item
			}
			for _, id := range []string{"server.installation", "server.service", "server.listeners", "server.shutdown_checks"} {
				if _, exists := byID[id]; !exists {
					t.Errorf("missing local checklist %s: %s", id, out)
				}
			}
			if item := byID["server.service"]; item.Status != tc.status || !strings.Contains(strings.Join(item.Details, " "), layout.UnitPath()) {
				t.Errorf("service evidence=%+v", item)
			}
			if item := byID["server.installation"]; item.Status != uninstallcheck.Pass || !strings.Contains(strings.Join(item.Details, " "), layout.Binary()) || !strings.Contains(strings.Join(item.Details, " "), layout.ConfigPath()) {
				t.Errorf("installation evidence=%+v", item)
			}
			if item := byID["server.listeners"]; item.Status != uninstallcheck.Unknown || !strings.Contains(strings.Join(item.Details, " "), "127.0.0.1:8800") || !strings.Contains(strings.Join(item.Details, " "), "127.0.0.1:8802") || !strings.Contains(strings.ToLower(item.Summary), "configured") {
				t.Errorf("configuration confused with observed listeners: %+v", item)
			}
			if item := byID["server.shutdown_checks"]; !strings.Contains(strings.ToLower(item.Summary), "planned") || item.Status != uninstallcheck.Warn {
				t.Errorf("post-stop checks not identified as plan: %+v", item)
			}
			if after := checklistTree(t, layout.Dir); !reflect.DeepEqual(before, after) {
				t.Fatal("local checklist changed installation")
			}
			if strings.Contains(out.String(), "DO-NOT-LEAK") {
				t.Fatal("raw service output leaked")
			}
			if len(runner.commands) != 0 {
				t.Fatalf("preview ran mutating commands: %v", runner.commands)
			}
		})
	}
}

func TestUninstallCLIDryRunRecordedPIDOmitsFullArguments(t *testing.T) {
	layout, _ := serverChecklistFixture(t)
	if err := os.WriteFile(layout.PIDFile(), []byte("717171\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &localChecklistRunner{process: layout.Binary() + " serve --config " + layout.ConfigPath() + " --token DO-NOT-LEAK\n"}
	streams, out := uninstallStreams("", false)
	err := runUninstallCLI(t.Context(), []string{"--dir", layout.Dir, "--dry-run", "--json"}, streams, func(ctx context.Context, _ selfinstall.Layout, req selfinstall.UninstallRequest, streams *selfinstall.IO) (selfinstall.UninstallReport, error) {
		return selfinstall.New(layout, selfinstall.Deps{Run: runner}, streams).Uninstall(ctx, req, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	var result selfinstall.UninstallReport
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Checklist {
		if item.ID == "server.service" {
			found = true
			if item.Status != uninstallcheck.Warn || !strings.Contains(strings.Join(item.Details, " "), "717171") {
				t.Fatalf("managed PID observation=%+v", item)
			}
		}
	}
	if !found || strings.Contains(out.String(), "DO-NOT-LEAK") {
		t.Fatalf("missing or unsafe process evidence: %s", out)
	}
}

func TestUninstallCLIDryRunLaunchdDoesNotAssumeBootEnablement(t *testing.T) {
	layout, _ := serverChecklistFixture(t)
	if err := os.WriteFile(layout.LaunchdPath(), []byte(selfinstall.RenderPlist(selfinstall.UnitConfig{Binary: layout.Binary(), ConfigPath: layout.ConfigPath(), DataDir: layout.DataDir})), 0644); err != nil {
		t.Fatal(err)
	}
	runner := &localChecklistRunner{launchd: "state = running\npid = 717171\n"}
	streams, out := uninstallStreams("", false)
	err := runUninstallCLI(t.Context(), []string{"--dir", layout.Dir, "--dry-run", "--json"}, streams, func(ctx context.Context, _ selfinstall.Layout, req selfinstall.UninstallRequest, streams *selfinstall.IO) (selfinstall.UninstallReport, error) {
		return selfinstall.New(layout, selfinstall.Deps{Run: runner, LaunchdRunning: func() bool { return true }}, streams).Uninstall(ctx, req, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	var result selfinstall.UninstallReport
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range result.Checklist {
		if item.ID == "server.service" {
			found = true
			if item.Status != uninstallcheck.Unknown || !strings.Contains(item.Summary, "boot enablement remains unverified") || !strings.Contains(strings.Join(item.Details, " "), "717171") {
				t.Fatalf("launchd evidence=%+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("missing launchd evidence: %s", out)
	}
}
