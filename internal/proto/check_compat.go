package proto

// The server's read boundary for check results from older agents (ADR 0067).
// An agent that predates the five check statuses and UPPER_SNAKE codes sends
// lower_snake codes, a sixth card check state ("pending"), skip reasons in
// words of their own, and doctor findings with a level ("warning", "error")
// instead of a status. Mixed-version clusters are permanent, so this is not a
// migration step: the server folds every report through these functions as it
// arrives, and nothing downstream sees the old words.

// legacyCardCheckPending is the state an older agent gave a check that had not
// run yet.
const legacyCardCheckPending = "pending"

// legacyCardCheckSkips are the skip reasons an older agent sent.
var legacyCardCheckSkips = map[string]string{
	"busy":         CardCheckSkipBusy,
	"foreign":      CardCheckSkipForeign,
	"deferred":     CardCheckSkipDeferred,
	"not_held":     CardCheckSkipNotHeld,
	"unavailable":  CardCheckSkipUnavailable,
	"probe_failed": CardCheckSkipProbeFailed,
}

// legacyDoctorLevels are the finding levels an older agent sent.
var legacyDoctorLevels = map[string]string{
	"warning": CheckWarn,
	"error":   CheckFail,
}

// NormalizeCardCheck returns a card check's status and code in the current
// vocabulary, whichever agent reported them.
func NormalizeCardCheck(status, code string) (string, string) {
	switch {
	case status == legacyCardCheckPending:
		return CheckSkip, CardCheckNotRunYet
	case status == CheckSkip:
		if current, ok := legacyCardCheckSkips[code]; ok {
			return CheckSkip, current
		}
	case !ValidCheckStatus(status):
		// A status this server does not know is a check it cannot say
		// anything about; it is never read as passed.
		return CheckSkip, NormalizeCode(code)
	}
	return status, NormalizeCode(code)
}

// NormalizeGPUReport folds the health codes and card checks of a status
// report's cards into the current vocabulary, in place.
func NormalizeGPUReport(gpus []GPUInfo) {
	for i := range gpus {
		g := &gpus[i]
		g.HealthCode = NormalizeCode(g.HealthCode)
		for j := range g.Checks {
			c := &g.Checks[j]
			c.Status, c.Code = NormalizeCardCheck(c.Status, c.Code)
		}
	}
}

// NormalizeCardCheckDetail folds an agent's answer about one card check into
// the current vocabulary, in place.
func NormalizeCardCheckDetail(d *CardCheckDetail) {
	for _, run := range []*CardCheckRun{d.Last, d.LastRan, d.LastProblem} {
		if run != nil {
			run.Status, run.Code = NormalizeCardCheck(run.Status, run.Code)
		}
	}
}

// NormalizeDoctorReport folds an agent's doctor report into the current
// vocabulary, in place: every finding has a status and an UPPER_SNAKE code,
// and no Level.
func NormalizeDoctorReport(r *DoctorReport) {
	for i := range r.Findings {
		f := &r.Findings[i]
		if f.Status == "" {
			f.Status = legacyDoctorLevels[f.Level]
		}
		if !ValidCheckStatus(f.Status) {
			f.Status = CheckFail
		}
		f.Level = ""
		f.Code = NormalizeCode(f.Code)
	}
	if r.Accelerators != nil {
		for i := range r.Accelerators.GPUs {
			r.Accelerators.GPUs[i].HealthCode = NormalizeCode(r.Accelerators.GPUs[i].HealthCode)
		}
	}
}
