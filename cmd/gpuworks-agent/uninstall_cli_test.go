package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/uninstallcheck"
)

func TestAgentUninstallPreviewAndResourceGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		mutate bool
	}{
		{"preview", []string{"--dry-run", "--json"}, false},
		{"yes_does_not_override", []string{"--yes", "--json"}, false},
		{"explicit_retention", []string{"-y", "--keep-resources", "--json"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			state := t.TempDir()
			var out bytes.Buffer
			d := agentUninstallDeps{
				resolve: func(string) ([]string, error) { return []string{state}, nil },
				observe: func(context.Context, string) []uninstallcheck.Item {
					return []uninstallcheck.Item{{ID: "runtime.docker", Status: uninstallcheck.Unknown, Summary: "shared-daemon candidate", Details: []string{"gpuworks-0123456789ab"}, Action: "Stop or migrate workloads; ownership cannot be inferred from a name."}}
				},
				remove: func(context.Context, []string) error { calls++; return nil },
			}
			err := runAgentUninstall(context.Background(), tc.args, strings.NewReader(""), &out, false, d)
			if (calls == 1) != tc.mutate {
				t.Fatalf("cleanup calls=%d err=%v output=%s", calls, err, &out)
			}
			if !tc.mutate && tc.name != "preview" && err == nil {
				t.Fatal("blocked resources reported success")
			}
			var wire map[string]json.RawMessage
			if json.Unmarshal(out.Bytes(), &wire) != nil {
				t.Fatalf("invalid JSON: %s", &out)
			}

			if !bytes.Contains(wire["checklist"], []byte(`"status":"unknown"`)) || !bytes.Contains(wire["checklist"], []byte(`"action"`)) {
				t.Fatalf("missing evidence/actions: %s", &out)
			}
		})
	}
}

func TestAgentUninstallRechecksAfterConfirmation(t *testing.T) {
	observations, removed := 0, 0
	var out bytes.Buffer
	d := agentUninstallDeps{
		resolve: func(string) ([]string, error) { return []string{"/fixture/agent"}, nil },
		observe: func(context.Context, string) []uninstallcheck.Item {
			observations++
			status := uninstallcheck.Pass
			if observations > 1 {
				status = uninstallcheck.Blocked
			}
			return []uninstallcheck.Item{{ID: "workloads", Status: status, Summary: "live runtime observation"}}
		},
		remove: func(context.Context, []string) error { removed++; return nil },
	}
	err := runAgentUninstall(context.Background(), nil, strings.NewReader("yes\n"), &out, true, d)
	if err == nil || removed != 0 || observations != 2 {
		t.Fatalf("err=%v removed=%d observations=%d", err, removed, observations)
	}
	if !strings.Contains(out.String(), "BLOCKED") {
		t.Fatalf("updated checklist missing: %s", &out)
	}
}

func TestAgentUninstallHelpDoesNotInspectTheHost(t *testing.T) {
	var out bytes.Buffer
	d := agentUninstallDeps{resolve: func(string) ([]string, error) { t.Fatal("help inspected host"); return nil, nil }}
	err := runAgentUninstall(context.Background(), []string{"--help"}, strings.NewReader(""), &out, false, d)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(out.String(), "keep-resources") || !strings.Contains(out.String(), "yes") {
		t.Fatalf("help err=%v output=%s", err, &out)
	}
}

func TestAgentUninstallReportsOptionalGPUUnknownWithoutClaimingIdle(t *testing.T) {
	var out bytes.Buffer
	removed := false
	d := agentUninstallDeps{resolve: func(string) ([]string, error) { return []string{"/fixture/agent"}, nil }, observe: func(context.Context, string) []uninstallcheck.Item {
		return []uninstallcheck.Item{{ID: "runtime.local", Status: uninstallcheck.Pass, Summary: "No workload records"}, {ID: "gpu.observation", Status: uninstallcheck.Unknown, Summary: "No vendor observation; GPU occupancy unknown"}}
	}, remove: func(context.Context, []string) error { removed = true; return nil }}
	if err := runAgentUninstall(context.Background(), []string{"--yes", "--json"}, strings.NewReader(""), &out, false, d); err != nil || !removed {
		t.Fatalf("err=%v removed=%v output=%s", err, removed, &out)
	}
	if !strings.Contains(out.String(), `"status":"unknown"`) {
		t.Fatal("missing optional GPU unknown evidence")
	}
}

func TestAgentUninstallRetentionCannotBypassInstallationSafety(t *testing.T) {
	var out bytes.Buffer
	removed := false
	d := agentUninstallDeps{resolve: func(string) ([]string, error) { return []string{"/fixture/state"}, nil }, observe: func(context.Context, string) []uninstallcheck.Item {
		return []uninstallcheck.Item{{ID: "agent.safety.processes", Status: uninstallcheck.Unknown, Summary: "Process identity could not be inspected"}}
	}, remove: func(context.Context, []string) error { removed = true; return nil }}
	err := runAgentUninstall(context.Background(), []string{"--yes", "--keep-resources", "--json"}, strings.NewReader(""), &out, false, d)
	if err == nil || removed {
		t.Fatalf("unsafe component removal: err=%v removed=%v output=%s", err, removed, &out)
	}
}

type failedChecklistWriter struct{}

func (failedChecklistWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestAgentUninstallRefusesUnconfirmedOrInvisibleChanges(t *testing.T) {
	for _, tc := range []struct {
		name, input                     string
		args                            []string
		terminal, failOutput, cancelled bool
	}{
		{name: "unattended_requires_yes"},
		{name: "operator_declines", input: "no\n", terminal: true},
		{name: "checklist_write_failure", args: []string{"--yes"}, failOutput: true},
		{name: "context_cancelled", args: []string{"--yes"}, cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			var out bytes.Buffer
			var writer interface{ Write([]byte) (int, error) } = &out
			if tc.failOutput {
				writer = failedChecklistWriter{}
			}
			removed := false
			d := agentUninstallDeps{resolve: func(string) ([]string, error) { return []string{"/fixture/state"}, nil }, observe: func(context.Context, string) []uninstallcheck.Item {
				return []uninstallcheck.Item{{ID: "runtime.fixture", Status: uninstallcheck.Pass, Summary: "Known empty fixture inventory"}}
			}, remove: func(context.Context, []string) error { removed = true; return nil }}
			if err := runAgentUninstall(ctx, tc.args, strings.NewReader(tc.input), writer, tc.terminal, d); err == nil || removed {
				t.Fatalf("unconfirmed/invisible removal: err=%v removed=%v", err, removed)
			}
		})
	}
}
