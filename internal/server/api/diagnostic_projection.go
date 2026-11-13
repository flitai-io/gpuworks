package api

import (
	"encoding/json"
	"net/http"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
)

const (
	maxDiagnosticChecks = 128
	maxDiagnosticBytes  = 64 << 10
)

// Diagnostic params are a public projection, not a transport for arbitrary
// agent reports, credentials, URLs or error cause text.
var diagnosticParamKeys = map[string]bool{
	"node": true, "node_id": true, "node_uuid": true, "model": true, "model_id": true,
	"storage": true, "storage_id": true, "action": true, "reason": true, "state": true,
	"observer": true, "provider": true, "repo_id": true, "revision": true, "transfer_id": true,
	"source_node": true, "source": true, "target": true, "peer": true, "phase": true,
}

func boundedDiagnosticReport(report *dto.DiagnosticReport) *dto.DiagnosticReport {
	if report == nil {
		return nil
	}
	out := &dto.DiagnosticReport{Checks: make([]dto.DiagnosticCheck, 0), Truncated: report.Truncated}
	// A large successful prefix must not hide the actual blocking failure.
	checks := append([]dto.DiagnosticCheck(nil), report.Checks...)
	sort.SliceStable(checks, func(i, j int) bool {
		return diagnosticPriority(checks[i].Status) < diagnosticPriority(checks[j].Status)
	})
	ids := make(map[string]bool)
	bytes := 64
	for _, original := range checks {
		if len(out.Checks) >= maxDiagnosticChecks {
			out.Truncated = true
			break
		}
		check := original
		check.ID = diagnostics.RedactErrorText(check.ID, 128)
		if check.ID == "" || ids[check.ID] {
			out.Truncated = true
			continue
		}
		check.Message.Code = diagnostics.RedactErrorText(check.Message.Code, 128)
		if len(check.Message.Message) > 1024 {
			out.Truncated = true
		}
		check.Message.Message = diagnostics.RedactErrorText(check.Message.Message, 1024)
		check.Message.Params = diagnosticParams(check.Message.Params)
		switch check.Status {
		case "passed", "failed", "pending", "blocked", "unknown", "not_applicable":
		default:
			check.Status = "unknown"
		}
		check.BlockedBy = append([]string(nil), check.BlockedBy...)
		if len(check.BlockedBy) > maxDiagnosticChecks {
			check.BlockedBy = check.BlockedBy[:maxDiagnosticChecks]
			out.Truncated = true
		}
		for i := range check.BlockedBy {
			check.BlockedBy[i] = diagnostics.RedactErrorText(check.BlockedBy[i], 128)
		}
		encoded, err := json.Marshal(check)
		if err != nil || bytes+len(encoded)+1 > maxDiagnosticBytes {
			out.Truncated = true
			break
		}
		bytes += len(encoded) + 1
		ids[check.ID] = true
		out.Checks = append(out.Checks, check)
	}
	for i := range out.Checks {
		check := &out.Checks[i]
		refs := check.BlockedBy[:0]
		for _, id := range check.BlockedBy {
			if id != check.ID && ids[id] {
				refs = append(refs, id)
			} else {
				out.Truncated = true
			}
		}
		check.BlockedBy = refs
	}
	return out
}

func diagnosticPriority(status string) int {
	switch status {
	case "failed":
		return 0
	case "blocked":
		return 1
	case "unknown", "pending":
		return 2
	default:
		return 3
	}
}

func diagnosticParams(params map[string]any) map[string]any {
	out := make(map[string]any)
	for key, value := range params {
		if !diagnosticParamKeys[key] {
			continue
		}
		switch v := value.(type) {
		case string:
			out[key] = diagnostics.RedactErrorText(v, 512)
		case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
			if _, err := json.Marshal(v); err == nil {
				out[key] = v
			}
		}
	}
	return out
}

// diagnosticLogSummary stays small and uses resource IDs instead of arbitrary
// display names or error details. It never serializes the full report to logs.
func diagnosticLogSummary(report *dto.DiagnosticReport) map[string]any {
	if report == nil {
		return nil
	}
	checks := make([]map[string]any, 0)
	relevant := 0
	for _, check := range report.Checks {
		if check.Status == "passed" || check.Status == "not_applicable" {
			continue
		}
		relevant++
		if len(checks) >= 16 {
			continue
		}
		item := map[string]any{"id": check.ID, "status": check.Status, "code": check.Message.Code}
		if len(check.BlockedBy) > 0 {
			item["blocked_by"] = check.BlockedBy
		}
		for _, key := range []string{"node_id", "node_uuid", "model_id", "storage_id", "source_node", "transfer_id", "reason", "action", "observer", "phase"} {
			if value, ok := check.Message.Params[key]; ok {
				item[key] = value
			}
		}
		checks = append(checks, item)
	}
	return map[string]any{"checks": checks, "check_count": len(report.Checks), "truncated": report.Truncated || relevant > len(checks)}
}

func prepareEvaluateResponse(w http.ResponseWriter, resp *dto.EvaluateResponse) {
	resp.RequestID = w.Header().Get("X-Request-ID")
	resp.Diagnostics = boundedDiagnosticReport(resp.Diagnostics)
	for writer := w; writer != nil; {
		if noter, ok := writer.(interface{ NoteEvaluationOutcome(bool, bool) }); ok {
			noter.NoteEvaluationOutcome(resp.OK, resp.Skipped)
			break
		}
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		writer = unwrapper.Unwrap()
	}
	if resp.OK {
		return
	}
	code := ""
	if len(resp.Errors) > 0 {
		code = resp.Errors[0].Code
	}
	if code == "" && resp.Diagnostics != nil {
		for _, check := range resp.Diagnostics.Checks {
			if check.Status == "failed" {
				code = check.Message.Code
				break
			}
		}
	}
	if code == "" {
		return
	}
	codes := make([]string, 0)
	for _, field := range resp.Errors {
		if len(codes) >= 16 {
			break
		}
		codes = append(codes, field.Code)
	}
	params := map[string]any{"error_count": len(resp.Errors), "codes": codes}
	if summary := diagnosticLogSummary(resp.Diagnostics); summary != nil {
		params["diagnostics"] = summary
	}
	noteProductError(w, code, params)
}
