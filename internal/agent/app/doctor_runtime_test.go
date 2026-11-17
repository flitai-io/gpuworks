package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	nerdctldrv "gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
	"gpuworks.ai/gpuworks/internal/proto"
)

// go test ./internal/agent/app -run '^TestDoctorNerdctlCommand$' -count=1
func TestDoctorNerdctlCommand(t *testing.T) {
	prev := doctorOnMac
	doctorOnMac = false
	t.Cleanup(func() { doctorOnMac = prev })
	for _, tt := range []struct {
		name, script, status, version, evidence string
		selected                                bool
	}{
		{name: "selected missing", selected: true, status: proto.DoctorRuntimeNotFound, evidence: "not found"},
		{name: "selected command fails", selected: true, script: "echo 'nerdctl cannot execute' >&2; exit 1", status: proto.DoctorRuntimeQueryFailed, evidence: "nerdctl cannot execute"},
		{name: "selected version", selected: true, script: "echo 'nerdctl version 2.1.3'", status: proto.DoctorRuntimeOK, version: "2.1.3"},
		{name: "unselected missing", status: proto.DoctorRuntimeNotFound},
		{name: "unselected command fails", script: "echo 'nerdctl cannot execute' >&2; exit 1", status: proto.DoctorRuntimeQueryFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			if tt.script != "" {
				script := "#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = '--version' ] || exit 64\n" + tt.script + "\n"
				if err := os.WriteFile(filepath.Join(dir, "nerdctl"), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			var driver runtime.Driver = nerdctldrv.New()
			if !tt.selected {
				driver = dockerdrv.New()
				script := "#!/bin/sh\n[ \"$*\" = 'version --format {{.Server.Version}}' ] || exit 64\necho '26.1.3'\n"
				if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			var report proto.DoctorReport
			doctorRuntimes(context.Background(), &report, driver)
			count := 0
			for _, rt := range report.Runtimes {
				if rt.Name != "nerdctl" {
					continue
				}
				count++
				if rt.Status != tt.status || rt.Version != tt.version || rt.Selected != tt.selected || rt.Command != "nerdctl --version" {
					t.Errorf("nerdctl = %+v, want status=%s version=%q selected=%v and version command", rt, tt.status, tt.version, tt.selected)
				}
			}
			if count != 1 {
				t.Errorf("nerdctl entries = %d, want 1", count)
			}
			if tt.evidence == "" {
				if len(report.Findings) != 0 {
					t.Fatalf("unexpected findings: %+v", report.Findings)
				}
				return
			}
			if len(report.Findings) != 1 {
				t.Fatalf("findings = %+v, want one runtime-unavailable finding", report.Findings)
			}
			f := report.Findings[0]
			if f.Code != proto.DoctorRuntimeUnavailable || f.Status != proto.CheckFail || f.Probe != "nerdctl --version" || f.Recheck != "nerdctl --version" || len(f.Evidence) != 1 || !strings.Contains(f.Evidence[0].Value, tt.evidence) {
				t.Fatalf("finding = %+v, want nerdctl failure with evidence %q", f, tt.evidence)
			}
		})
	}
}
