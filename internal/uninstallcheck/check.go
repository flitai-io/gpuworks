// Package uninstallcheck describes observations made before removing a component.
package uninstallcheck

import (
	"fmt"
	"io"
	"strings"
)

// Pass, Warn, Blocked and Unknown describe the evidence available for a check.
const (
	Pass    = "pass"
	Warn    = "warn"
	Blocked = "blocked"
	Unknown = "unknown"
)

// Item separates observed facts from actions and information unavailable locally.
// These observations are transient; they are never persisted as resource state.
type Item struct {
	ID      string   `json:"id"`
	Status  string   `json:"status"`
	Summary string   `json:"summary"`
	Details []string `json:"details,omitempty"`
	Action  string   `json:"action,omitempty"`
}

// Blocking reports whether a resource observation requires operator action.
func Blocking(items []Item) bool {
	for _, item := range items {
		if item.Status == Blocked || item.Status == Unknown {
			return true
		}
	}
	return false
}

// Print writes a checklist without including authentication or connection secrets.
func Print(w io.Writer, items []Item) error {
	for _, item := range items {
		if _, err := fmt.Fprintf(w, "[%s] %s: %s\n", strings.ToUpper(item.Status), item.ID, item.Summary); err != nil {
			return err
		}
		for _, detail := range item.Details {
			if _, err := fmt.Fprintf(w, "  %s\n", detail); err != nil {
				return err
			}
		}
		if item.Action != "" {
			if _, err := fmt.Fprintf(w, "  action: %s\n", item.Action); err != nil {
				return err
			}
		}
	}
	return nil
}
