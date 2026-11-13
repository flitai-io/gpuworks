package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func TestDiagnosticProjectionBoundsAndKeepsBlockingFailure(t *testing.T) {
	report := &dto.DiagnosticReport{}
	for i := 0; i < maxDiagnosticChecks+3; i++ {
		report.Checks = append(report.Checks, dto.DiagnosticCheck{ID: fmt.Sprintf("ready:%d", i), Status: "passed", Message: dto.ProductMessage{Code: "MODEL_SOURCE_UNAVAILABLE", Message: "ready"}})
	}
	report.Checks = append(report.Checks, dto.DiagnosticCheck{ID: "storage:failed", Status: "failed", Message: dto.ProductMessage{Code: "STORAGE_UNAVAILABLE_ON_NODE", Params: map[string]any{"node_id": 7, "storage_id": 3, "reason": "not_mounted", "authorization": "Bearer secret-value", "env": map[string]any{"TOKEN": "secret-env"}}, Message: "mount failed"}}, dto.DiagnosticCheck{ID: "read:blocked", Status: "blocked", BlockedBy: []string{"storage:failed"}, Message: dto.ProductMessage{Code: "MODEL_SOURCE_UNAVAILABLE", Message: "not executed"}})
	out := boundedDiagnosticReport(report)
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Truncated || len(out.Checks) > maxDiagnosticChecks || len(encoded) > maxDiagnosticBytes {
		t.Fatalf("unbounded report: checks=%d bytes=%d truncated=%v", len(out.Checks), len(encoded), out.Truncated)
	}
	if out.Checks[0].ID != "storage:failed" || out.Checks[1].ID != "read:blocked" || len(out.Checks[1].BlockedBy) != 1 {
		t.Fatalf("blocking evidence lost: %s", encoded)
	}
	if strings.Contains(string(encoded), "secret-") || strings.Contains(string(encoded), "authorization") {
		t.Fatalf("unsafe projection: %s", encoded)
	}
	if len(report.Checks[len(report.Checks)-2].Message.Params) != 5 {
		t.Fatal("projection mutated source params")
	}
}

func TestDiagnosticProjectionByteBoundAndMissingDependencies(t *testing.T) {
	report := &dto.DiagnosticReport{}
	for i := 0; i < maxDiagnosticChecks; i++ {
		report.Checks = append(report.Checks, dto.DiagnosticCheck{ID: fmt.Sprintf("blocked:%d", i), Status: "blocked", BlockedBy: []string{"not-returned"}, Message: dto.ProductMessage{Code: "MODEL_SOURCE_UNAVAILABLE", Message: strings.Repeat("detail ", 1000)}})
	}
	out := boundedDiagnosticReport(report)
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Truncated || len(encoded) > maxDiagnosticBytes {
		t.Fatalf("byte cap absent: %d", len(encoded))
	}
	for _, check := range out.Checks {
		if len(check.BlockedBy) > 0 || len(check.Message.Message) > 1024 {
			t.Fatalf("invalid check projection: %#v", check)
		}
	}
}

func TestDiagnosticLogSummaryReportsActualTruncation(t *testing.T) {
	for _, n := range []int{16, 17} {
		report := &dto.DiagnosticReport{}
		for i := 0; i < n; i++ {
			report.Checks = append(report.Checks, dto.DiagnosticCheck{ID: fmt.Sprintf("failure:%d", i), Status: "failed", Message: dto.ProductMessage{Code: "MODEL_SOURCE_UNAVAILABLE", Params: map[string]any{"node_id": i, "node": "private-display-name", "detail": "must not be logged"}}})
		}
		summary := diagnosticLogSummary(report)
		encoded, _ := json.Marshal(summary)
		if summary["truncated"] != (n > 16) || strings.Contains(string(encoded), "private-display-name") || strings.Contains(string(encoded), "must not be logged") {
			t.Fatalf("summary: %s", encoded)
		}
	}
}

func TestDiagnosticLogSummaryPreservesCausalReferences(t *testing.T) {
	report := boundedDiagnosticReport(&dto.DiagnosticReport{Checks: []dto.DiagnosticCheck{
		{ID: "peer:7", Status: "failed", Message: dto.ProductMessage{Code: "NO_STORAGE_PATH", Params: map[string]any{"node_id": 7, "source_node": "peer-uuid", "reason": "no_storage_path"}, Message: "No permitted path"}},
		{ID: "source:7", Status: "blocked", BlockedBy: []string{"peer:7"}, Message: dto.ProductMessage{Code: "MODEL_SOURCE_UNAVAILABLE", Message: "Blocked"}},
	}})
	encoded, err := json.Marshal(diagnosticLogSummary(report))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"blocked_by":["peer:7"]`) || !strings.Contains(string(encoded), `"source_node":"peer-uuid"`) {
		t.Fatalf("causal identity missing: %s", encoded)
	}
}
