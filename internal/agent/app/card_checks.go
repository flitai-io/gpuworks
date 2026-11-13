package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Each card's checks, one state per check in every status report
// (proto.GPUInfo.Checks), and the last run of each kept in memory for a person
// who asks (RPCNodeCardCheckDetail).
//
// Nothing here is kept across a restart, and only the last run is kept: a
// check that keeps passing has nothing more to tell. The server records the
// runs that found a problem.
//
// A card's vendor verdict recovers slowly: a problem the vendor tool reported
// holds until vendorReleaseRounds readings in a row are clean. A card whose
// counters flip every minute would otherwise go in and out of scheduling with
// every report, and every flip would be a row the server writes.

// vendorReleaseRounds is how many clean vendor readings in a row release a
// problem the vendor tool reported: about three minutes.
const vendorReleaseRounds = 3

// checkResult is one check's answer this round on one card.
type checkResult struct {
	state, reason string
	at            time.Time
	took          time.Duration
	evidence      []accelerator.HealthField
	// ownSchedule: the check runs on a schedule of its own, so its age is
	// reported; the others ran for this very report.
	ownSchedule bool
}

type checkRuns struct {
	last, lastRan, lastProblem *proto.CardCheckRun
}

// vendorHold is a vendor problem being held while the card reads clean.
type vendorHold struct {
	code     string
	evidence []accelerator.HealthField
	clean    int
}

type cardCheckMemory struct {
	mu        sync.Mutex
	startedAt time.Time
	now       func() time.Time
	runs      map[string]map[string]*checkRuns
	cards     map[string]accelerator.HealthCard
	held      map[string]vendorHold
}

func (m *cardCheckMemory) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *cardCheckMemory) init() {
	if m.runs != nil {
		return
	}
	if m.startedAt.IsZero() {
		m.startedAt = m.clock()
	}
	m.runs = map[string]map[string]*checkRuns{}
	m.cards = map[string]accelerator.HealthCard{}
	m.held = map[string]vendorHold{}
}

// checkRound collects one report's answers, by card identity and check.
type checkRound map[string]map[string]checkResult

func (r checkRound) put(ident, check string, result checkResult) {
	if ident == "" {
		return
	}
	if r[ident] == nil {
		r[ident] = map[string]checkResult{}
	}
	r[ident][check] = result
}

// vendorChecks records what the vendor tool said about each card and holds a
// problem it reported until the card has read clean vendorReleaseRounds times
// in a row. It runs before this agent adds its own findings, while each card
// still carries only the vendor tool's verdict.
func (n *NodeRuntime) vendorChecks(gpus []proto.GPUInfo, cliAnswered bool, now time.Time, round checkRound) {
	m := &n.checks
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	for i := range gpus {
		card := &gpus[i]
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if ident == "" || !accelerator.CardUsable(card.Ownership) {
			continue
		}
		if card.HealthCode == accelerator.HealthCLIUnanswered || !cliAnswered {
			round.put(ident, accelerator.CheckVendorCLI, checkResult{state: accelerator.CheckFail, reason: accelerator.HealthCLIUnanswered, at: now})
			round.put(ident, accelerator.CheckVendorHealth, checkResult{state: accelerator.CheckSkip, reason: accelerator.CheckSkipProbeFailed, at: now})
			continue
		}
		round.put(ident, accelerator.CheckVendorCLI, checkResult{state: accelerator.CheckPass, at: now})
		code := card.HealthCode
		evidence := accelerator.ParseHealthEvidence(card.HealthMessage)
		state := accelerator.CheckStateFor(code)
		if accelerator.CheckProblem(state) {
			m.held[ident] = vendorHold{code: code, evidence: evidence}
		} else if hold, ok := m.held[ident]; ok {
			hold.clean++
			if hold.clean >= vendorReleaseRounds {
				delete(m.held, ident)
			} else {
				m.held[ident] = hold
				code, state = hold.code, accelerator.CheckStateFor(hold.code)
				evidence = append(append([]accelerator.HealthField(nil), hold.evidence...),
					accelerator.HealthField{Name: "Recovery", Value: fmt.Sprintf("%d of %d clean readings", hold.clean, vendorReleaseRounds)})
				if accelerator.MostSevereHealth(card.HealthCode, code) == code {
					gpu.SetCardFinding(card, code, evidence)
				}
			}
		}
		reason := ""
		if accelerator.CheckProblem(state) {
			reason = code
		}
		round.put(ident, accelerator.CheckVendorHealth, checkResult{state: state, reason: reason, at: now, evidence: evidence})
	}
}

// occupancyChecks records whether somebody outside the platform uses each card.
func (n *NodeRuntime) occupancyChecks(gpus []proto.GPUInfo, now time.Time, round checkRound) {
	if _, ok := n.prober.(gpu.ComputeProcessLister); !ok {
		return
	}
	for _, card := range gpus {
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if !accelerator.CardUsable(card.Ownership) {
			continue
		}
		if len(card.ForeignProcesses) == 0 {
			round.put(ident, accelerator.CheckOccupancy, checkResult{state: accelerator.CheckPass, at: now})
			continue
		}
		evidence := make([]accelerator.HealthField, 0, len(card.ForeignProcesses))
		for _, p := range card.ForeignProcesses {
			evidence = append(evidence, accelerator.HealthField{
				Name:  fmt.Sprintf("PID %d", p.PID),
				Value: fmt.Sprintf("%s, %d MiB", p.Name, p.MemoryBytes>>20),
			})
		}
		round.put(ident, accelerator.CheckOccupancy, checkResult{state: accelerator.CheckWarn, reason: accelerator.CheckReasonForeignProcesses, at: now, evidence: evidence})
	}
}

// hardwareChecks records this round's PCIe, Xid and NVLink findings.
func hardwareChecks(pcie map[string]pcieCardFindings, xid map[string]finding, nvlink nvlinkFindings, now time.Time, round checkRound) {
	for ident, f := range pcie {
		putFinding(round, ident, accelerator.CheckPCIeLink, f.link, now, now, false)
		putFinding(round, ident, accelerator.CheckPCIeErrors, f.aer, now, now, false)
	}
	for ident, f := range xid {
		putFinding(round, ident, accelerator.CheckXid, f, now, now, false)
	}
	for ident, f := range nvlink.cards {
		putFinding(round, ident, accelerator.CheckNVLink, f, nvlink.at, now, true)
	}
}

func putFinding(round checkRound, ident, check string, f finding, at, now time.Time, ownSchedule bool) {
	if !f.known {
		return
	}
	state, reason := accelerator.CheckStateFor(f.code), ""
	if accelerator.CheckProblem(state) {
		reason = f.code
	}
	if at.IsZero() || at.After(now) {
		at = now
	}
	round.put(ident, check, checkResult{state: state, reason: reason, at: at, evidence: f.evidence, ownSchedule: ownSchedule})
}

// runtimeChecks records each card's runtime check: its last answer, or why it
// was left unasked since.
func (n *NodeRuntime) runtimeChecks(gpus []proto.GPUInfo, now time.Time, round checkRound) {
	if n.cardSmoke == nil {
		if gpu.IsSynthetic(n.prober) {
			return
		}
		for _, card := range gpus {
			if accelerator.CardUsable(card.Ownership) {
				round.put(accelerator.CardIdent(card.UUID, card.PCIeBDF), accelerator.CheckRuntime,
					checkResult{state: accelerator.CheckSkip, reason: accelerator.CheckSkipUnavailable, at: now, ownSchedule: true})
			}
		}
		return
	}
	outcomes, skips := n.cardSmoke.Outcomes(), n.cardSmoke.Skips()
	for _, card := range gpus {
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if ident == "" || !accelerator.CardUsable(card.Ownership) {
			continue
		}
		outcome, answered := outcomes[ident]
		skip, skipped := skips[ident]
		switch {
		case answered && !outcome.OK:
			// A failing card's backoff is not a skip: its failure stands.
			round.put(ident, accelerator.CheckRuntime, checkResult{state: accelerator.CheckFail, reason: accelerator.HealthRuntimeUnusable,
				at: outcome.At, took: outcome.Duration, ownSchedule: true,
				evidence: []accelerator.HealthField{{Name: "Detail", Value: outcome.Detail}}})
		case skipped && (!answered || skip.At.After(outcome.At)):
			round.put(ident, accelerator.CheckRuntime, checkResult{state: accelerator.CheckSkip, reason: runtimeSkipReason(skip.Reason), at: skip.At, ownSchedule: true})
		case answered:
			round.put(ident, accelerator.CheckRuntime, checkResult{state: accelerator.CheckPass, at: outcome.At, took: outcome.Duration, ownSchedule: true})
		}
	}
}

// finishChecks puts each card's check states on it and keeps their runs. A
// check this round said nothing about is pending until it has run once, and
// not applicable on a card it has nothing to look at.
func (n *NodeRuntime) finishChecks(gpus []proto.GPUInfo, now time.Time, round checkRound, cliAnswered bool) {
	m := &n.checks
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	seen := map[string]bool{}
	synthetic := gpu.IsSynthetic(n.prober)
	for i := range gpus {
		card := &gpus[i]
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if ident == "" || !accelerator.CardUsable(card.Ownership) {
			continue
		}
		seen[ident] = true
		vendor := accelerator.NormalizeVendor(card.Vendor, n.vendor())
		m.cards[ident] = accelerator.HealthCard{Vendor: vendor, BDF: card.PCIeBDF, Index: card.Index, UUID: card.UUID}
		runs := m.runs[ident]
		if runs == nil {
			runs = map[string]*checkRuns{}
			m.runs[ident] = runs
		}
		for _, spec := range accelerator.CardChecks(vendor) {
			result, ok := round[ident][spec.ID]
			if !ok {
				result = checkResult{state: missingState(spec.ID, synthetic)}
			}
			check := proto.CardCheck{ID: spec.ID, State: result.state, Reason: result.reason}
			if result.ownSchedule && !result.at.IsZero() {
				check.AgeS = int32(now.Sub(result.at) / time.Second)
			}
			if result.took > 0 {
				check.MS = int32(result.took / time.Millisecond)
			}
			if accelerator.CheckProblem(result.state) {
				check.Evidence = boundedEvidence(result.evidence)
			}
			card.Checks = append(card.Checks, check)
			if ok {
				keepRun(runs, spec.ID, m.cards[ident], result)
			}
		}
	}
	if cliAnswered {
		// A card the tool no longer lists is gone; one it could not list
		// this round may well still be there.
		for ident := range m.runs {
			if !seen[ident] {
				delete(m.runs, ident)
				delete(m.cards, ident)
				delete(m.held, ident)
			}
		}
	}
}

// missingState is what a check is when this round said nothing about it.
// Every check that runs with each report speaks every round it can; the ones
// that cannot have nothing to read (no AER, no NVLink, no kernel log) and are
// not applicable. The runtime check is pending until it first answers, except
// on cards that exist only in a fixture, which it never asks.
func missingState(check string, synthetic bool) string {
	if check == accelerator.CheckRuntime && !synthetic {
		return accelerator.CheckPending
	}
	return accelerator.CheckNotApplicable
}

func keepRun(runs map[string]*checkRuns, check string, card accelerator.HealthCard, result checkResult) {
	if result.state == accelerator.CheckPending || result.state == accelerator.CheckNotApplicable {
		return
	}
	run := &proto.CardCheckRun{
		State: result.state, Reason: result.reason,
		At:         result.at.UTC().Format(time.RFC3339),
		DurationMS: result.took.Milliseconds(),
		Probe:      accelerator.CardCheckProbe(check, card),
	}
	for _, f := range result.evidence {
		run.Evidence = append(run.Evidence, proto.DoctorField{Name: f.Name, Value: f.Value})
	}
	r := runs[check]
	if r == nil {
		r = &checkRuns{}
		runs[check] = r
	}
	r.last = run
	if run.State != accelerator.CheckSkip {
		r.lastRan = run
	}
	if accelerator.CheckProblem(run.State) {
		r.lastProblem = run
	}
}

// checkDetail is what this agent holds about one check on one card.
func (n *NodeRuntime) checkDetail(ident, check string) proto.CardCheckDetail {
	m := &n.checks
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	detail := proto.CardCheckDetail{Ident: ident, Check: check, AgentStartedAt: m.startedAt.UTC().Format(time.RFC3339)}
	r := m.runs[ident][check]
	if r == nil {
		return detail
	}
	detail.Last = r.last
	if r.lastRan != r.last {
		detail.LastRan = r.lastRan
	}
	if r.lastProblem != r.last && r.lastProblem != r.lastRan {
		detail.LastProblem = r.lastProblem
	}
	return detail
}

// recheckCard runs the runtime check on one card now and keeps its answer as
// the check's last run; the periodic runner records it like its own.
func (n *NodeRuntime) recheckCard(ctx context.Context, ident string) (proto.CardCheckDetail, error) {
	n.checks.mu.Lock()
	n.checks.init()
	card, known := n.checks.cards[ident]
	n.checks.mu.Unlock()
	if !known {
		return proto.CardCheckDetail{}, fmt.Errorf("no card %q on this node", ident)
	}
	var result checkResult
	outcome, skip := n.cardSmoke.CheckCard(ctx, ident)
	switch {
	case skip != "":
		result = checkResult{state: accelerator.CheckSkip, reason: runtimeSkipReason(skip), at: n.checks.clock()}
	case outcome.OK:
		result = checkResult{state: accelerator.CheckPass, at: outcome.At, took: outcome.Duration}
	default:
		result = checkResult{state: accelerator.CheckFail, reason: accelerator.HealthRuntimeUnusable, at: outcome.At, took: outcome.Duration,
			evidence: []accelerator.HealthField{{Name: "Detail", Value: outcome.Detail}}}
	}
	n.checks.mu.Lock()
	runs := n.checks.runs[ident]
	if runs == nil {
		runs = map[string]*checkRuns{}
		n.checks.runs[ident] = runs
	}
	keepRun(runs, accelerator.CheckRuntime, card, result)
	n.checks.mu.Unlock()
	return n.checkDetail(ident, accelerator.CheckRuntime), nil
}

// vendor is the vendor of this node's cards, for a card that does not say.
func (n *NodeRuntime) vendor() string {
	if n.prober == nil {
		return ""
	}
	return n.prober.Vendor()
}

// runtimeSkipReason names the runner's reason for leaving a card unasked in
// the check catalog's words.
func runtimeSkipReason(reason string) string {
	switch reason {
	case cardsmoke.SkipBusy:
		return accelerator.CheckSkipBusy
	case cardsmoke.SkipForeign:
		return accelerator.CheckSkipForeign
	case cardsmoke.SkipDeferred:
		return accelerator.CheckSkipDeferred
	case cardsmoke.SkipNotHeld:
		return accelerator.CheckSkipNotHeld
	}
	return accelerator.CheckSkipUnavailable
}

// boundedEvidence is as much of the evidence as fits a report's bound, whole
// fields only.
func boundedEvidence(fields []accelerator.HealthField) []proto.DoctorField {
	var out []proto.DoctorField
	size := 0
	for _, f := range fields {
		size += len(f.Name) + len(f.Value) + 2
		if size > proto.CardCheckEvidenceBytes {
			break
		}
		out = append(out, proto.DoctorField{Name: f.Name, Value: f.Value})
	}
	return out
}
