package proto

import "strings"

// Check statuses (ADR 0067). Every check result the platform reports - a node
// doctor finding, a card check, a precheck, a diagnostics item - uses these
// five values and no others.
const (
	// CheckPass: the check ran and found nothing wrong.
	CheckPass = "pass"
	// CheckWarn: the check ran and found a problem that does not block.
	CheckWarn = "warn"
	// CheckFail: the check ran and found a problem that blocks.
	CheckFail = "fail"
	// CheckSkip: the check should have run but did not (blocked by an earlier
	// check, a missing capability, an old agent, a timeout, no result
	// received). A skipped check carries a code saying why. Never render it
	// as passed.
	CheckSkip = "skip"
	// CheckNA: the check does not apply to this resource. Never render it as
	// passed.
	CheckNA = "na"
)

// CheckStatuses lists the check statuses in display order.
var CheckStatuses = []string{CheckPass, CheckWarn, CheckFail, CheckSkip, CheckNA}

// ValidCheckStatus reports whether s is one of the five check statuses.
func ValidCheckStatus(s string) bool {
	for _, status := range CheckStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// NormalizeCode returns a code in the platform's single spelling, UPPER_SNAKE
// (ADR 0067). It is for the read boundary of reports from older agents, which
// sent some codes in lower_snake; code that defines or compares codes uses the
// constants directly.
func NormalizeCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}
