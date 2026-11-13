package dto

// DiagnosticReport explains the checks actually performed for one request.
// It is a transient projection, not a persisted source of resource state.
type DiagnosticReport struct {
	Checks    []DiagnosticCheck `json:"checks"`
	Truncated bool              `json:"truncated,omitempty"`
}

// DiagnosticCheck keeps a check's outcome distinct from its product message.
// A blocked check was not executed; BlockedBy references checks in this report.
// Message params identify existing resources and the scope of the observation.
type DiagnosticCheck struct {
	ID        string         `json:"id"`
	Status    string         `json:"status"`
	Message   ProductMessage `json:"message"`
	BlockedBy []string       `json:"blocked_by,omitempty"`
}
