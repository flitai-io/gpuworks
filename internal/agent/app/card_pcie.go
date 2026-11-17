package app

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/pcilink"
	"gpuworks.ai/gpuworks/internal/proto"
)

// How each card's PCIe path is doing, read from sysfs every report.
//
// A link narrower than both its ends support is a fact at any time. A slower
// one is not: GPUs and the switches above them lower link speed while idle
// (a K100's switch uplink idles at 2.5 of 8 GT/s), so speed is judged only on
// a card one of this node's instances is using and that is not asleep.
// Either has to hold for pcieRounds reports in a row to raise the finding,
// and its absence as long to clear it, so one odd sample changes nothing.
//
// AER counters count since boot. The first reading is the baseline; what
// counts is an increase. New non-fatal (recovered) errors warn for
// pcieErrorsWarnFor. New fatal ones stop the card until a runtime check has
// passed on it since — a link that needed resetting is not trusted on its
// own word. An agent restart takes a new baseline.

const (
	pcieRounds        = 3
	pcieErrorsWarnFor = 15 * time.Minute
)

type pcieWatch struct {
	mu     sync.Mutex
	reader pcilink.Reader
	now    func() time.Time

	virtual, virtualKnown bool
	bad, good             map[string]int
	degraded              map[string][]accelerator.HealthField
	aerBase               map[string]pcilink.AER
	nonFatalAt, fatalAt   map[string]time.Time
	aerEvidence           map[string][]accelerator.HealthField
}

func (w *pcieWatch) init() {
	if w.bad != nil {
		return
	}
	if w.now == nil {
		w.now = time.Now
	}
	w.bad, w.good = map[string]int{}, map[string]int{}
	w.degraded = map[string][]accelerator.HealthField{}
	w.aerBase = map[string]pcilink.AER{}
	w.nonFatalAt, w.fatalAt = map[string]time.Time{}, map[string]time.Time{}
	w.aerEvidence = map[string][]accelerator.HealthField{}
}

// pcieCardFindings is what this round's PCIe reading found on one card, link
// and error counters apart: each is a check of its own, and the card carries
// only the more severe of the two. known is false when nothing could be read.
type pcieCardFindings struct {
	link, aer finding
}

// finding is one check's code and evidence; known is false when the check had
// nothing to read.
type finding struct {
	code     string
	evidence []accelerator.HealthField
	known    bool
}

// applyPCIeFindings adds this round's PCIe findings to the cards and returns
// them by card identity. busy are the cards whose link speed may be judged;
// passedSince reports whether a runtime check has passed on a card after the
// given time.
func (n *NodeRuntime) applyPCIeFindings(gpus []proto.GPUInfo, busy map[string]bool, passedSince func(ident string, t time.Time) bool) map[string]pcieCardFindings {
	found := map[string]pcieCardFindings{}
	w := &n.pcie
	w.mu.Lock()
	defer w.mu.Unlock()
	w.init()
	if !w.virtualKnown {
		w.virtual, w.virtualKnown = w.reader.Virtual(), true
	}
	now := w.now()
	for i := range gpus {
		card := &gpus[i]
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if ident == "" || card.PCIeBDF == "" || !accelerator.CardUsable(card.Ownership) {
			continue
		}
		bdf := accelerator.NormalizeBDF(card.PCIeBDF)
		link := w.linkFinding(ident, bdf, busy[ident])
		aer := w.aerFinding(ident, bdf, now, passedSince)
		found[ident] = pcieCardFindings{link: link, aer: aer}
		code, evidence := link.code, link.evidence
		if aer.code != "" && accelerator.MostSevereHealth(code, aer.code) == aer.code {
			code, evidence = aer.code, aer.evidence
		}
		if code == "" || accelerator.MostSevereHealth(card.HealthCode, code) != code {
			continue
		}
		gpu.SetCardFinding(card, code, evidence)
	}
	return found
}

// linkFinding debounces this round's link reading into a finding.
func (w *pcieWatch) linkFinding(ident, bdf string, judgeSpeed bool) finding {
	links, err := w.reader.Links(bdf, w.virtual)
	if err != nil || len(links) == 0 {
		// Unknown is not degraded, and does not move the count either way.
		if ev, ok := w.degraded[ident]; ok {
			return finding{code: accelerator.HealthPCIeLinkDegraded, evidence: ev, known: true}
		}
		return finding{}
	}
	var evidence []accelerator.HealthField
	for _, l := range links {
		if l.Narrow() || (judgeSpeed && l.Slow()) {
			evidence = append(evidence, accelerator.HealthField{
				Name:  accelerator.PCIeLinkField + l.BDF,
				Value: fmt.Sprintf("x%d of x%d, %.1f of %.1f GT/s", l.Width, l.ExpectedWidth, l.Speed, l.ExpectedSpeed),
			})
		}
	}
	if len(evidence) > 0 {
		w.bad[ident]++
		w.good[ident] = 0
		if _, already := w.degraded[ident]; already || w.bad[ident] >= pcieRounds {
			if !already {
				slog.Warn("PCIe link degraded", "card", bdf, "links", accelerator.FormatHealthEvidence(evidence))
			}
			w.degraded[ident] = evidence
		}
	} else {
		w.good[ident]++
		w.bad[ident] = 0
		if _, was := w.degraded[ident]; was && w.good[ident] >= pcieRounds {
			slog.Info("PCIe link back to full width and speed", "card", bdf)
			delete(w.degraded, ident)
		}
	}
	if ev, ok := w.degraded[ident]; ok {
		return finding{code: accelerator.HealthPCIeLinkDegraded, evidence: ev, known: true}
	}
	return finding{evidence: linkFacts(links), known: true}
}

// linkFacts are the links as read, for a link that is fine: what the check
// looked at, shown when a person asks.
func linkFacts(links []pcilink.Link) []accelerator.HealthField {
	out := make([]accelerator.HealthField, 0, len(links))
	for _, l := range links {
		out = append(out, accelerator.HealthField{
			Name:  accelerator.PCIeLinkField + l.BDF,
			Value: fmt.Sprintf("x%d of x%d, %.1f of %.1f GT/s", l.Width, l.ExpectedWidth, l.Speed, l.ExpectedSpeed),
		})
	}
	return out
}

// aerFinding compares the AER counters on the card's path with the last
// reading and returns what the increases mean now.
func (w *pcieWatch) aerFinding(ident, bdf string, now time.Time, passedSince func(string, time.Time) bool) finding {
	chain, err := w.reader.Chain(bdf)
	known := false
	if err == nil {
		for _, dev := range chain {
			counts, ok := w.reader.AER(dev)
			if !ok {
				continue
			}
			known = true
			base, seen := w.aerBase[dev]
			w.aerBase[dev] = counts
			if !seen {
				continue
			}
			field := accelerator.HealthField{Name: accelerator.PCIeAERField + dev,
				Value: fmt.Sprintf("fatal +%d, non-fatal +%d, correctable +%d", counts.Fatal-base.Fatal, counts.NonFatal-base.NonFatal, counts.Correctable-base.Correctable)}
			switch {
			case counts.Fatal > base.Fatal:
				w.fatalAt[ident] = now
				w.aerEvidence[ident] = []accelerator.HealthField{field}
				slog.Warn("PCIe fatal errors on the card's path", "card", bdf, "device", dev, "fatal", counts.Fatal-base.Fatal)
			case counts.NonFatal > base.NonFatal:
				w.nonFatalAt[ident] = now
				if _, fatal := w.fatalAt[ident]; !fatal {
					w.aerEvidence[ident] = []accelerator.HealthField{field}
				}
			}
		}
	}
	if at, ok := w.fatalAt[ident]; ok {
		if passedSince == nil || !passedSince(ident, at) {
			return finding{code: accelerator.HealthPCIeFatal, evidence: w.aerEvidence[ident], known: true}
		}
		slog.Info("PCIe fatal errors cleared: a runtime check passed on the card since", "card", bdf)
		delete(w.fatalAt, ident)
	}
	if at, ok := w.nonFatalAt[ident]; ok {
		if now.Sub(at) < pcieErrorsWarnFor {
			return finding{code: accelerator.HealthPCIeErrors, evidence: w.aerEvidence[ident], known: true}
		}
		delete(w.nonFatalAt, ident)
	}
	return finding{known: known}
}

// cardsBusy are the cards one of this node's instances uses and that are not
// asleep: the ones whose link speed means something.
func (n *NodeRuntime) cardsBusy() map[string]bool {
	ours, _, known := n.cardsInUse()
	if !known {
		return nil
	}
	sleeping := n.sleepingCardPIDs()
	busy := make(map[string]bool, len(ours))
	for ident := range ours {
		if _, asleep := sleeping[ident]; !asleep {
			busy[ident] = true
		}
	}
	return busy
}

// runtimeCheckPassedSince reports whether the runtime check passed on the
// card after t.
func (n *NodeRuntime) runtimeCheckPassedSince(ident string, t time.Time) bool {
	if n.cardSmoke == nil {
		return false
	}
	outcome, ok := n.cardSmoke.Outcomes()[ident]
	return ok && outcome.OK && outcome.At.After(t)
}

// held is what the watch holds for a card now, without reading anything:
// Doctor reports it, and reading would move the counts the reports keep.
func (w *pcieWatch) held(ident string, now time.Time, passedSince func(string, time.Time) bool) (string, []accelerator.HealthField) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if at, ok := w.fatalAt[ident]; ok && (passedSince == nil || !passedSince(ident, at)) {
		return accelerator.HealthPCIeFatal, w.aerEvidence[ident]
	}
	if at, ok := w.nonFatalAt[ident]; ok && now.Sub(at) < pcieErrorsWarnFor {
		return accelerator.HealthPCIeErrors, w.aerEvidence[ident]
	}
	if ev, ok := w.degraded[ident]; ok {
		return accelerator.HealthPCIeLinkDegraded, ev
	}
	return "", nil
}

// applyHeldHardwareFindings adds to the cards what this agent's own hardware
// watches (PCIe, NVLink, Xid) hold now, without a new reading. Doctor uses
// it, so a card the reports call degraded is never shown healthy there.
func (n *NodeRuntime) applyHeldHardwareFindings(gpus []proto.GPUInfo) {
	now := time.Now()
	if n.pcie.now != nil {
		now = n.pcie.now()
	}
	for i := range gpus {
		card := &gpus[i]
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if ident == "" || !accelerator.CardUsable(card.Ownership) {
			continue
		}
		code, evidence := n.pcie.held(ident, now, n.runtimeCheckPassedSince)
		n.nvlink.mu.Lock()
		if ev, down := n.nvlink.down[ident]; down && accelerator.MostSevereHealth(code, accelerator.HealthNVLinkDown) == accelerator.HealthNVLinkDown {
			code, evidence = accelerator.HealthNVLinkDown, ev
		}
		n.nvlink.mu.Unlock()
		if code != "" && accelerator.MostSevereHealth(card.HealthCode, code) == code {
			gpu.SetCardFinding(card, code, evidence)
		}
	}
	n.applyXidFindings(gpus, n.runtimeCheckPassedSince)
}
