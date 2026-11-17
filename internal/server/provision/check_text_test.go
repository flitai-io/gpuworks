package provision

import "strings"

// text is what a person reads of a check: its fallback message and the
// evidence behind it.
func (c CheckResult) text() string {
	return strings.TrimSpace(c.Message + " " + c.Evidence)
}
