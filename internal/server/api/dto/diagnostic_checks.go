package dto

// DiagnosticReport explains the checks actually performed for one request.
// It is a transient projection, not a persisted source of resource state.
type DiagnosticReport struct {
	Checks    []DiagnosticCheck `json:"checks"`
	Truncated bool              `json:"truncated,omitempty"`
}

// DiagnosticCheck keeps a check's outcome distinct from its product message.
// Status is one of proto.CheckPass/Warn/Fail/Skip/NA (ADR 0067). A skipped
// check was not executed or not observed; when another check in this report
// stopped it, BlockedBy names that check. params.reason says why.
// Message params identify existing resources and the scope of the observation.
type DiagnosticCheck struct {
	ID        string         `json:"id"`
	Status    string         `json:"status"`
	Message   ProductMessage `json:"message"`
	BlockedBy []string       `json:"blocked_by,omitempty"`
}
