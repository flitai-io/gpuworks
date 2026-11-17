package proto

// checkStatusRank orders the check statuses for a summary: a failure outranks
// a warning, a warning a check that did not run, and that a pass, so a summary
// over checks that did not all run never reads as passed. A check that does
// not apply says nothing.
var checkStatusRank = map[string]int{CheckNA: 0, CheckPass: 1, CheckSkip: 2, CheckWarn: 3, CheckFail: 4}

// WorseCheckStatus is the more serious of two check statuses; "" is neither.
func WorseCheckStatus(a, b string) string {
	if a == "" || checkStatusRank[b] > checkStatusRank[a] {
		return b
	}
	return a
}
