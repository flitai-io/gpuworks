package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

func TestAgentUninstallCLIUsesRuntimeEvidenceAndRetainsSecrets(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, docker, wantStatus, wantText string
	}{
		{"auto_without_backend", "", "", "unknown", "No workload runtime could be inventoried"},
		{"configured_backend_missing", "docker", "", "unknown", "Runtime inventory could not be read"},
		{"configured_local_backend_missing", "udocker", "", "unknown", "Configured local runtime state is unavailable"},
		{"shared_candidate", "docker", "candidate", "unknown", "state ownership unknown"},
		{"daemon_failure", "docker", "failure", "unknown", "Runtime inventory could not be read"},
		{"partial_running_list", "docker", "partial", "unknown", "absent from inventory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, tools := t.TempDir(), t.TempDir()
			t.Setenv("PATH", tools)
			cfg := "SERVER=https://example.invalid/private\nTOKEN=secret-node-canary\nRUNTIME_DRIVER=" + tc.runtime + "\n"
			if err := os.WriteFile(filepath.Join(state, "gpuworks-agent.conf"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.docker != "" {
				t.Setenv("DOCKER_FIXTURE_MODE", tc.docker)
				script := `#!/bin/sh
case "$*" in
  'ps -a --format {{.Names}}') kind=all ;;
  'ps --format {{.Names}}') kind=running ;;
  *) exit 92 ;;
esac
case "$DOCKER_FIXTURE_MODE" in
  failure) printf '%s\n' 'secret-command-canary https://user:secret@example.invalid/token' >&2; exit 7 ;;
  candidate) printf '%s\n' 'gpuworks-owned-elsewhere' 'unrelated-container' ;;
  partial) if [ "$kind" = running ]; then printf '%s\n' 'gpuworks-running-only'; fi ;;
  *) exit 93 ;;
esac
`
				if err := os.WriteFile(filepath.Join(tools, "docker"), []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			removed := false
			d := agentUninstallDeps{resolve: func(string) ([]string, error) { return []string{state}, nil }, observe: agentRuntimeChecklist, remove: func(context.Context, []string) error { removed = true; return nil }}
			err := runAgentUninstall(context.Background(), []string{"--yes", "--json"}, strings.NewReader(""), &out, false, d)
			if err == nil || removed || !strings.Contains(out.String(), tc.wantText) || !strings.Contains(out.String(), `"status":"`+tc.wantStatus+`"`) {
				t.Fatalf("runtime gate err=%v removed=%v output=%s", err, removed, &out)
			}
			if strings.Contains(out.String(), "secret-") || strings.Contains(out.String(), "https://") || strings.Contains(out.String(), "unrelated-container") {
				t.Fatalf("unscoped or secret evidence: %s", &out)
			}
			data, err := os.ReadFile(filepath.Join(state, "gpuworks-agent.conf"))
			if err != nil || string(data) != cfg {
				t.Fatalf("configuration was mutated: %v", err)
			}
		})
	}
}

func TestAgentUninstallCLIInspectsActualLinuxOwnershipBeforeCleanup(t *testing.T) {
	for _, otherBinary := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching_process", true: "foreign_service"}[otherBinary], func(t *testing.T) {
			u, state := uninstallFixture(t)
			addUninstallProcess(t, u, 101, u.binary, "run", "--state", state, "--token", "secret-argv-canary")
			procDir := filepath.Join(u.procDir, "101")
			for _, dir := range []string{"fd", "net"} {
				if err := os.Mkdir(filepath.Join(procDir, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("socket:[12345]", filepath.Join(procDir, "fd", "3")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(procDir, "net", "tcp"), []byte("sl local_address rem_address st tx_queue rx_queue tr tm_when retrnsmt uid timeout inode\n0: 00000000:4970 00000000:0000 0A 0:0 00:0 0 1000 0 12345\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			binary := u.binary
			if otherBinary {
				binary = "/other/service"
			}
			unit := "ExecStart=" + binary + " run --state " + state + "\n"
			if err := os.WriteFile(u.unitPath, []byte(unit), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cleanupStarted := false
			d := agentUninstallDeps{resolve: func(string) ([]string, error) { return []string{state}, nil }, observe: func(_ context.Context, s string) []uninstallcheck.Item { return linuxAgentInstallationChecklist(u, s) }, remove: func(ctx context.Context, _ []string) error { cleanupStarted = true; return u.uninstall(ctx, state) }}
			args := []string{"--dry-run", "--json"}
			if otherBinary {
				args = []string{"--yes", "--keep-resources", "--json"}
			}
			err := runAgentUninstall(context.Background(), args, strings.NewReader(""), &out, false, d)
			if otherBinary && (err == nil || cleanupStarted) {
				t.Fatal("foreign executable unit ownership did not block")
			}
			if !strings.Contains(out.String(), "PID 101") || !strings.Contains(out.String(), "TCP port 18800") || strings.Contains(out.String(), "secret-argv-canary") {
				t.Fatalf("unsafe/incomplete process evidence: %s", &out)
			}
			for _, path := range []string{u.unitPath, filepath.Join(state, "identity.json"), filepath.Join(u.procDir, "101")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("preview/blocked uninstall changed %s: %v", path, err)
				}
			}
			var report agentUninstallReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil || len(report.Checklist) == 0 {
				t.Fatalf("missing JSON checklist: %v", err)
			}
		})
	}
}

func TestAgentUninstallCLIPartialProcessInventoryCannotBeBypassed(t *testing.T) {
	u, state := uninstallFixture(t)
	addUninstallProcess(t, u, 101, u.binary, "run", "--state", state)
	cmdline := filepath.Join(u.procDir, "101", "cmdline")
	if err := os.Remove(cmdline); err != nil {
		t.Fatal(err)
	}
	// A directory gives a deterministic I/O error even when tests run as root.
	if err := os.Mkdir(cmdline, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cleanupStarted := false
	d := agentUninstallDeps{resolve: func(string) ([]string, error) { return []string{state}, nil }, observe: func(_ context.Context, state string) []uninstallcheck.Item {
		return linuxAgentInstallationChecklist(u, state)
	}, remove: func(context.Context, []string) error { cleanupStarted = true; return nil }}
	err := runAgentUninstall(context.Background(), []string{"--yes", "--keep-resources", "--json"}, strings.NewReader(""), &out, false, d)
	if err == nil || cleanupStarted || !strings.Contains(out.String(), `"status":"unknown"`) {
		t.Fatalf("partial process inventory bypassed: err=%v cleanup=%v output=%s", err, cleanupStarted, &out)
	}
}

func TestAgentUninstallCLIRetainsIncompleteLocalStateAndReportsGPUFacts(t *testing.T) {
	state, tools := t.TempDir(), t.TempDir()
	t.Setenv("PATH", tools)
	if err := os.WriteFile(filepath.Join(state, "gpuworks-agent.conf"), []byte("RUNTIME_DRIVER=udocker\nTOKEN=secret-config-canary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(state, "udocker-state"), 0o700); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(state, "udocker-state", "orphan.pid")
	if err := os.WriteFile(pidFile, []byte("invalid-pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$*" in
  '--query-compute-apps=pid,process_name,gpu_bus_id,used_gpu_memory --format=csv,noheader,nounits') printf '%s\n' '4242, python --token secret-process-canary, 00000000:01:00.0, 0' ;;
  *) exit 97 ;;
esac
`
	if err := os.WriteFile(filepath.Join(tools, "nvidia-smi"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cleanupStarted := false
	d := agentUninstallDeps{resolve: func(string) ([]string, error) { return []string{state}, nil }, observe: agentRuntimeChecklist, remove: func(context.Context, []string) error { cleanupStarted = true; return nil }}
	err := runAgentUninstall(context.Background(), []string{"--dry-run", "--json"}, strings.NewReader(""), &out, false, d)
	if err != nil || cleanupStarted || !strings.Contains(out.String(), "persisted workload entries: 1") || !strings.Contains(out.String(), "PID 4242; device 00000000:01:00.0; GPU memory unknown") || !strings.Contains(out.String(), `"id":"gpu.amd","status":"unknown"`) {
		t.Fatalf("missing honest local/GPU evidence err=%v cleanup=%v output=%s", err, cleanupStarted, &out)
	}
	if strings.Contains(out.String(), "secret-") || strings.Contains(out.String(), "python") {
		t.Fatalf("unsafe process information: %s", &out)
	}
	if data, err := os.ReadFile(pidFile); err != nil || string(data) != "invalid-pid" {
		t.Fatalf("preview changed persisted PID record: %v", err)
	}
}
