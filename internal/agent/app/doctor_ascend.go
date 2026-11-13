package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The Ascend check only Doctor runs on demand: the same per-die runtime check
// the agent repeats every 15 minutes (internal/agent/gpu HostRuntimeCheck),
// asked now, so that a user who clicks Diagnose sees every die the automatic
// check would already have marked runtime_unusable. Doctor is the place a
// person looks for "what is wrong with this node"; a die the platform has
// stopped scheduling for that reason must not read as fine there.

// doctorHostRuntimeCheck is gpu.HostRuntimeCheckFor; a test stands in for the
// host's CANN so no die is touched.
var doctorHostRuntimeCheck = gpu.HostRuntimeCheckFor

// What a doctor run keeps for the rest of its report, and the least time worth
// starting a die's check with. A healthy die answers in about a second; a
// failing one can take its full timeout (45 seconds on Ascend), so a run that
// finds several of them cannot finish inside doctorBudget. Package vars so a
// test can shrink them.
var (
	doctorAscendReserve    = 10 * time.Second
	doctorAscendMinAttempt = 5 * time.Second
)

// doctorCardsInUseKey carries, on the context of a doctor run the agent
// serves, which dies hold a serving instance. The standalone CLI has no such
// knowledge and its context carries none.
type doctorCardsInUseKey struct{}

// withDoctorCardsInUse tells a doctor run how to ask which cards are held by
// this node's instances (NodeRuntime.cardsInUse).
func withDoctorCardsInUse(ctx context.Context, held func() (map[string]bool, bool)) context.Context {
	return context.WithValue(ctx, doctorCardsInUseKey{}, held)
}

// doctorDie is one die as the check names it.
type doctorDie struct {
	gpu     proto.GPUInfo
	visible string
	label   string
}

// doctorDieNotChecked is a die the run did not ask, and why.
type doctorDieNotChecked struct {
	die    doctorDie
	reason string
}

// doctorDieFailed is a die the runtime said it cannot use.
type doctorDieFailed struct {
	die    doctorDie
	detail string
	at     time.Time
}

// doctorCardRuntimeChecks asks the host runtime about each of this node's dies in
// turn and records the answer in the inventory table, as a finding per failing
// die and as one summary fact.
func doctorCardRuntimeChecks(ctx context.Context, r *proto.DoctorReport, gpus []proto.GPUInfo) {
	if r.Accelerators == nil {
		return
	}
	// Every vendor whose runtime can be asked on the host is asked per card
	// here, so the diagnosis covers what the periodic check does. NVIDIA's
	// whole-node checks (doctorNVIDIAChecks) do not: they see every card at
	// once, so their CUDA check answers for the first visible card only.
	vendor := accelerator.NormalizeVendor(r.Accelerators.Vendor)
	check, ok := doctorRuntimeCheck(ctx)(r.Accelerators.Vendor)
	if !ok {
		return
	}
	var dies []doctorDie
	var unnamed []doctorDieNotChecked
	for _, g := range gpus {
		if !accelerator.CardUsable(g.Ownership) {
			// Lent to a virtual machine or let go: not this node's to
			// state anything about (the table already says so).
			continue
		}
		die := doctorDie{gpu: g, label: dieLabel(g)}
		visible, named := check.Visible(g)
		if !named {
			unnamed = append(unnamed, doctorDieNotChecked{die: die, reason: "the vendor tool gave it no device number"})
			continue
		}
		die.visible = visible
		dies = append(dies, die)
	}
	total := len(dies) + len(unnamed)
	if total == 0 {
		return
	}

	run := doctorDieRun{check: check, notChecked: unnamed, vendor: vendor, unit: "card", runtime: "HIP"}
	switch vendor {
	case accelerator.VendorNVIDIA:
		run.runtime = "CUDA"
	case accelerator.VendorAscend:
		run.unit, run.runtime = "die", "ACL"
	case accelerator.VendorPPU:
		run.runtime = "HGGC"
	}
	if reason := smokeUnavailable(check); reason != "" {
		run.stopped = reason
		run.notChecked = append(run.notChecked, notChecked(dies, "the run did not start")...)
	} else {
		run.ask(ctx, dies)
	}
	r.Findings = append(r.Findings, run.findings()...)
	run.markInventory(r.Accelerators)
	r.Facts = append(r.Facts, proto.DoctorFact{Code: proto.DoctorFactCardRuntime, Value: run.summary(total)})
}

func notChecked(dies []doctorDie, reason string) []doctorDieNotChecked {
	out := make([]doctorDieNotChecked, 0, len(dies))
	for _, die := range dies {
		out = append(out, doctorDieNotChecked{die: die, reason: reason})
	}
	return out
}

func dieLabel(g proto.GPUInfo) string {
	if g.Index >= 0 {
		return strconv.Itoa(g.Index)
	}
	return g.PCIeBDF
}

// doctorDieRun is the outcome of one pass over the dies.
type doctorDieRun struct {
	check gpu.HostRuntimeCheck
	// vendor, unit and runtime word the report: Ascend checks dies through
	// ACL, Hygon cards through HIP.
	vendor, unit, runtime string
	answered              []doctorDie
	failed                []doctorDieFailed
	notChecked            []doctorDieNotChecked
	// stopped is why the run could not go on, said once as a fact about the
	// host. Nothing in it is a verdict about a die.
	stopped string
}

// ask checks the dies one after another. The remaining time is looked at
// before each: a die that does not fit is left unchecked rather than started
// and cut off, and the run never leaves the rest of the report less than
// doctorAscendReserve.
func (run *doctorDieRun) ask(ctx context.Context, dies []doctorDie) {
	deadline, bounded := ctx.Deadline()
	if !bounded {
		// The standalone CLI has no caller waiting; keep it to the same
		// budget the agent gives the server's request.
		deadline = time.Now().Add(doctorBudget)
	}
	held, holdersKnown := doctorHeldCards(ctx)
	foreign := doctorForeignCard(ctx)
	timeout := run.check.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	for i, die := range dies {
		if ctx.Err() != nil {
			run.notChecked = append(run.notChecked, notChecked(dies[i:], "the diagnosis was cancelled")...)
			return
		}
		if !holdersKnown {
			run.notChecked = append(run.notChecked, doctorDieNotChecked{die: die, reason: "which instances hold this " + run.unit + " is not known yet; a context on a serving " + run.unit + " can disturb it"})
			continue
		}
		if held[accelerator.CardIdent(die.gpu.UUID, die.gpu.PCIeBDF)] {
			run.notChecked = append(run.notChecked, doctorDieNotChecked{die: die, reason: "an instance is serving on it; a context on a serving " + run.unit + " can disturb it"})
			continue
		}
		if foreign(accelerator.CardIdent(die.gpu.UUID, die.gpu.PCIeBDF)) {
			run.notChecked = append(run.notChecked, doctorDieNotChecked{die: die, reason: "a process outside the platform is using it"})
			continue
		}
		remaining := time.Until(deadline) - doctorAscendReserve
		if remaining < doctorAscendMinAttempt {
			run.notChecked = append(run.notChecked, notChecked(dies[i:], "the diagnosis time budget ran out")...)
			return
		}
		budgetCut := remaining < timeout
		attemptTimeout := min(timeout, remaining)
		attempt, cancel := context.WithTimeout(ctx, attemptTimeout)
		end := doctorBeginAttempt(ctx, accelerator.CardIdent(die.gpu.UUID, die.gpu.PCIeBDF))
		err := run.check.Try(attempt, die.visible)
		end()
		expired := attempt.Err() != nil
		cancel()
		at := time.Now()
		switch {
		case expired && (budgetCut || ctx.Err() != nil):
			// Our own cut-off, not the die: it had less time than the
			// automatic check gives it, so it has no verdict.
			run.notChecked = append(run.notChecked, doctorDieNotChecked{die: die, reason: "the diagnosis time budget ran out while this " + run.unit + " was being checked"})
			run.notChecked = append(run.notChecked, notChecked(dies[i+1:], "the diagnosis time budget ran out")...)
			return
		case expired:
			// The automatic check grades a die that does not answer within
			// its timeout as failed, and so does this.
			detail := fmt.Sprintf("the runtime check did not finish within %s", attemptTimeout)
			run.failed = append(run.failed, doctorDieFailed{die: die, at: at, detail: detail})
			doctorRecordOutcome(ctx, die, errors.New(detail))
		default:
			if why := cardsmoke.NotHeldReason(err); why != "" {
				run.notChecked = append(run.notChecked, doctorDieNotChecked{die: die, reason: why})
				continue
			}
			if reason := gpu.HostRuntimeUnavailable(err); reason != "" {
				run.stopped = reason
				run.notChecked = append(run.notChecked, notChecked(dies[i:], "the runtime could not be used on this host")...)
				return
			}
			doctorRecordOutcome(ctx, die, err)
			if err != nil {
				run.failed = append(run.failed, doctorDieFailed{die: die, at: at, detail: err.Error()})
				continue
			}
			run.answered = append(run.answered, die)
		}
	}
}

// doctorHeldCards is the dies held by serving instances, and whether that is
// known. A run with no source for it (the standalone CLI, which runs beside
// whatever the agent is serving) asks every die.
type doctorAttemptsKey struct{}

// withDoctorAttempts tells a doctor run how to mark a check as on a card
// (cardsmoke.Runner.Begin), so the occupancy report and an instance starting
// on the card see it the way they see the periodic check.
func withDoctorAttempts(ctx context.Context, begin func(ident string) func()) context.Context {
	return context.WithValue(ctx, doctorAttemptsKey{}, begin)
}

func doctorBeginAttempt(ctx context.Context, idents ...string) func() {
	begin, ok := ctx.Value(doctorAttemptsKey{}).(func(string) func())
	if !ok || begin == nil {
		return func() {}
	}
	ends := make([]func(), 0, len(idents))
	for _, ident := range idents {
		if ident != "" {
			ends = append(ends, begin(ident))
		}
	}
	return func() {
		for _, end := range ends {
			end()
		}
	}
}

type doctorOutcomesKey struct{}

// withDoctorOutcomes tells a doctor run where a card's answer goes
// (cardsmoke.Runner.Record), so the card's state follows the diagnosis at the
// next report instead of waiting out the periodic check's backoff.
func withDoctorOutcomes(ctx context.Context, record func(ident string, err error)) context.Context {
	return context.WithValue(ctx, doctorOutcomesKey{}, record)
}

func doctorRecordOutcome(ctx context.Context, die doctorDie, err error) {
	record, ok := ctx.Value(doctorOutcomesKey{}).(func(string, error))
	if !ok || record == nil {
		return
	}
	record(accelerator.CardIdent(die.gpu.UUID, die.gpu.PCIeBDF), err)
}

type doctorHeldFindingsKey struct{}

// withDoctorHeldFindings has a doctor run add what this agent's hardware
// watches hold (PCIe, NVLink, Xid) to the cards the vendor tool describes:
// the diagnosis must see everything the reports do.
func withDoctorHeldFindings(ctx context.Context, held func([]proto.GPUInfo)) context.Context {
	return context.WithValue(ctx, doctorHeldFindingsKey{}, held)
}

type doctorRuntimeCheckKey struct{}

// withDoctorRuntimeCheck tells a doctor run how this node asks its cards, so
// it asks them the way the periodic check does (from inside a runner image
// where the host has no runtime).
func withDoctorRuntimeCheck(ctx context.Context, check func(vendor string) (gpu.HostRuntimeCheck, bool)) context.Context {
	return context.WithValue(ctx, doctorRuntimeCheckKey{}, check)
}

func doctorRuntimeCheck(ctx context.Context) func(string) (gpu.HostRuntimeCheck, bool) {
	if check, ok := ctx.Value(doctorRuntimeCheckKey{}).(func(string) (gpu.HostRuntimeCheck, bool)); ok && check != nil {
		return check
	}
	return doctorHostRuntimeCheck
}

type doctorForeignCardsKey struct{}

// withDoctorForeignCards tells a doctor run which cards the last occupancy
// round found used outside the platform; they are left alone like the
// periodic check leaves them.
func withDoctorForeignCards(ctx context.Context, foreign func(ident string) bool) context.Context {
	return context.WithValue(ctx, doctorForeignCardsKey{}, foreign)
}

func doctorForeignCard(ctx context.Context) func(string) bool {
	if foreign, ok := ctx.Value(doctorForeignCardsKey{}).(func(string) bool); ok && foreign != nil {
		return foreign
	}
	return func(string) bool { return false }
}

func doctorHeldCards(ctx context.Context) (map[string]bool, bool) {
	held, ok := ctx.Value(doctorCardsInUseKey{}).(func() (map[string]bool, bool))
	if !ok || held == nil {
		return nil, true
	}
	return held()
}

// findings is one runtime_unusable finding naming each failing die, with the
// evidence the agent records when its own check fails the die.
func (run *doctorDieRun) findings() []proto.DoctorFinding {
	if len(run.failed) == 0 {
		return nil
	}
	finding := proto.DoctorFinding{Code: accelerator.HealthRuntimeUnusable, Level: proto.DoctorLevelError}
	for _, f := range run.failed {
		target := proto.DoctorTarget{Index: f.die.gpu.Index, PCIeBDF: f.die.gpu.PCIeBDF}
		// The same guide the card carries when the periodic check failed it,
		// so Diagnose and the card's own detail name one command rather than
		// two spellings of it.
		if guide, ok := accelerator.HealthGuideFor(accelerator.HealthCard{
			Vendor: run.vendor, Code: accelerator.HealthRuntimeUnusable,
			BDF: f.die.gpu.PCIeBDF, Index: f.die.gpu.Index,
		}, nil); ok {
			target.Probe, target.Recheck, target.Actions = guide.Probe, guide.Recheck, guide.Actions
		}
		for _, field := range runtimeUnusableEvidence(f.at, f.detail) {
			target.Evidence = append(target.Evidence, proto.DoctorField{Name: field.Name, Value: field.Value})
		}
		finding.Targets = append(finding.Targets, target)
	}
	return []proto.DoctorFinding{finding}
}

// markInventory puts each failing die's verdict in the table by the rule the
// agent applies when it reports the die to the server: the most severe of
// what the vendor tool said and what trying the die said.
func (run *doctorDieRun) markInventory(a *proto.DoctorAccelerators) {
	for _, f := range run.failed {
		for i := range a.GPUs {
			row := &a.GPUs[i]
			if row.Index != f.die.gpu.Index || row.PCIeBDF != f.die.gpu.PCIeBDF {
				continue
			}
			if accelerator.MostSevereHealth(row.HealthCode, accelerator.HealthRuntimeUnusable) != accelerator.HealthRuntimeUnusable {
				break
			}
			row.HealthCode = accelerator.HealthRuntimeUnusable
			row.HealthStatus = accelerator.HealthLevel(accelerator.HealthRuntimeUnusable)
			break
		}
	}
}

// summary is the one fact: what was asked, what answered and what was not
// asked. A die is in exactly one of the three, and the lines say which.
func (run *doctorDieRun) summary(total int) string {
	checked := len(run.answered) + len(run.failed)
	if run.stopped != "" && checked == 0 {
		return fmt.Sprintf("not run: %s. None of the %d %ss was checked and no %s is blamed; the periodic per-%s runtime check does not run under this condition either", run.stopped, total, run.unit, run.unit, run.unit)
	}
	lines := []string{fmt.Sprintf("per-%s %s runtime check of the %d %ss this node owns: %d passed, %d failed, %d not checked",
		run.unit, run.runtime, total, run.unit, len(run.answered), len(run.failed), len(run.notChecked))}
	if len(run.failed) > 0 {
		labels := make([]string, len(run.failed))
		for i, f := range run.failed {
			labels[i] = f.die.label
		}
		lines = append(lines, "failed: "+strings.Join(labels, ", "))
	}
	if run.stopped != "" {
		lines = append(lines, "stopped: "+run.stopped+"; no "+run.unit+" is blamed for it")
	}
	byReason := map[string][]string{}
	var order []string
	for _, n := range run.notChecked {
		if _, seen := byReason[n.reason]; !seen {
			order = append(order, n.reason)
		}
		byReason[n.reason] = append(byReason[n.reason], n.die.label)
	}
	for _, reason := range order {
		lines = append(lines, "not checked ("+reason+"): "+strings.Join(byReason[reason], ", "))
	}
	return strings.Join(lines, "\n")
}
