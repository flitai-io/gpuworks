package engine

import (
	"encoding/json"
	"net/http"
	"strings"
)

// The catalog-owned wrapper keeps HTTP alive after its one-shot loader fails.
// Only its documented terminal response justifies ending startup early; an
// arbitrary engine's 503 (or a proxy error page) may still mean loading.
func callableStartupFailure(resp *http.Response, state engineReadiness) string {
	server := strings.Fields(resp.Header.Get("Server"))
	if resp.StatusCode != http.StatusServiceUnavailable || len(server) == 0 || server[0] != "GPUWorksCallable/1" {
		return ""
	}
	var status, reason string
	if json.Unmarshal(state.Status, &status) != nil || status != "failed" {
		return ""
	}
	message := "callable initialization failed"
	if json.Unmarshal(state.Error, &reason) == nil {
		// Exception text is untrusted and can include a multi-line repr. Keep
		// the status message bounded; the original traceback remains in logs.
		reason = strings.Join(strings.Fields(reason), " ")
		text := []rune(reason)
		if len(text) > 1024 {
			reason = string(text[:1024]) + "..."
		}
		if reason != "" {
			message += ": " + reason
		}
	}
	return message
}
