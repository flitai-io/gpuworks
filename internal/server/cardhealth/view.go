package cardhealth

import (
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// staleGrace is the slack on top of two report periods before a node's checks
// are shown as stale: a report that is a little late is not news.
const staleGrace = 30 * time.Second

// Cell is one check on one card, as a person is shown it.
type Cell struct {
	Check string
	// Status is the check status; Code what a warning or failure found, or
	// why a skipped check did not run.
	Status string
	Code   string
	// CheckedAt is when the check last ran, zero when it has not.
	CheckedAt  time.Time
	DurationMS int32
	// Stale says the last report is too old to say what the check says now.
	Stale bool
}

// CardView is one card's checks in the order of the view's columns.
type CardView struct {
	Ident, UUID, PCIeBDF, Name string
	Index                      int
	Cells                      []Cell
}

// View is a node's checks as a table: the columns that apply to any of its
// cards, and each card's cells under them.
type View struct {
	ReceivedAt time.Time
	Online     bool
	Reports    bool
	Stale      bool
	Checks     []string
	Cards      []CardView
}

// BuildView lays a node's snapshot out as a table at now. A check that applies
// to none of the cards (no card reported it as anything but not applicable) is
// left out as a column.
func BuildView(n NodeSnapshot, now time.Time) View {
	v := View{ReceivedAt: n.ReceivedAt, Online: n.Online, Reports: n.Reports}
	v.Stale = !n.Online || now.Sub(n.ReceivedAt) > 2*accelerator.ReportPeriod+staleGrace
	used := map[string]bool{}
	for _, card := range n.Cards {
		for _, c := range card.Checks {
			if c.Status != proto.CheckNA {
				used[c.ID] = true
			}
		}
	}
	for _, id := range accelerator.CardCheckIDs() {
		if used[id] {
			v.Checks = append(v.Checks, id)
		}
	}
	for _, card := range n.Cards {
		byID := make(map[string]proto.CardCheck, len(card.Checks))
		for _, c := range card.Checks {
			byID[c.ID] = c
		}
		cv := CardView{Ident: card.Ident, UUID: card.UUID, PCIeBDF: card.PCIeBDF, Name: card.Name, Index: card.Index}
		for _, id := range v.Checks {
			c, ok := byID[id]
			if !ok {
				c = proto.CardCheck{ID: id, Status: proto.CheckNA}
			}
			cell := Cell{Check: id, Status: c.Status, Code: c.Code, DurationMS: c.MS, Stale: v.Stale}
			if c.Status != proto.CheckNA && c.Code != proto.CardCheckNotRunYet {
				cell.CheckedAt = n.ReceivedAt.Add(-time.Duration(c.AgeS) * time.Second).UTC()
			}
			cv.Cells = append(cv.Cells, cell)
		}
		v.Cards = append(v.Cards, cv)
	}
	return v
}

// Worst is the most serious check status among cells, for a summary
// (proto.WorseCheckStatus): a card whose checks did not all run never sums up
// as passed, and one where nothing applies is na.
func Worst(cells []Cell) string {
	worst := proto.CheckNA
	for _, c := range cells {
		worst = proto.WorseCheckStatus(worst, c.Status)
	}
	return worst
}
