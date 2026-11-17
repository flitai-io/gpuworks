// Package cardhealth keeps what each card's checks last said and records the
// stretches in which one of them warned or failed.
//
// The results themselves are never stored: every status report brings them
// again, so they live in memory and a restarted server shows "waiting for a
// report" for at most one report period. Only problems are written, one row per
// stretch (an episode), so a healthy fleet writes nothing and the writes grow
// with the cards in trouble, not with the fleet:
//
//   - an episode opens when a check first warns or fails, and is written within
//     seconds;
//   - while it lasts, the count, the time last seen and the latest evidence
//     change only in memory and are written once an hour, so a card that keeps
//     failing for a month is one row and about 720 updates;
//   - a change of reason or level is written within seconds;
//   - it closes after the check has passed for a quiet period, and a failure
//     within that period continues it; an episode that recurs often in a day
//     is marked flapping.
//
// Behaviour: docs/how-to/troubleshooting.md, 卡的健康检查.
package cardhealth

import (
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	// QuietPeriod is how long a check must pass before its episode closes.
	QuietPeriod = 30 * time.Minute
	// FlappingRecurrences within FlappingWindow mark an episode flapping.
	FlappingRecurrences = 10
	FlappingWindow      = 24 * time.Hour
	// Retention is how long a closed episode is kept.
	Retention = 30 * 24 * time.Hour
	// MaxEvidenceBytes bounds the evidence an episode keeps.
	MaxEvidenceBytes = 1024

	urgentFlushInterval = 5 * time.Second
	countFlushInterval  = time.Hour
	// vanishedAfter closes an episode whose card no report has named for
	// this long while its node kept reporting: the card is gone.
	vanishedAfter = time.Hour
)

// Episode is one stretch in which a card check warned or failed.
type Episode struct {
	// ID is the stored row's, 0 until it is first written.
	ID        uint
	NodeID    uint
	CardIdent string
	CheckID   string
	// Status is the check status, warn or fail; Code what the check found.
	Status     string
	Code       string
	StartedAt  time.Time
	EndedAt    *time.Time
	LastSeenAt time.Time
	Count      int
	Flapping   bool
	// FirstEvidence and LatestEvidence are in the text form of a card's
	// health message, at most MaxEvidenceBytes each.
	FirstEvidence, LatestEvidence string
}

// EpisodeStore is where episodes are kept. Save assigns the id of a row
// written for the first time.
type EpisodeStore interface {
	OpenEpisodes() ([]Episode, error)
	SaveEpisodes(rows []*Episode) error
	DeleteEpisodesEndedBefore(before time.Time, limit int) (int, error)
}

// CardSnapshot is what the last report said about one card.
type CardSnapshot struct {
	Ident, UUID, PCIeBDF, Name, Vendor string
	Index                              int
	Checks                             []proto.CardCheck
}

// NodeSnapshot is what the last report said about a node's cards.
type NodeSnapshot struct {
	NodeID     uint
	ReceivedAt time.Time
	Online     bool
	// Reports says the agent reports card checks at all; an older agent's
	// cards carry none.
	Reports bool
	Cards   []CardSnapshot
}

type episodeKey struct {
	node         uint
	ident, check string
}

type episode struct {
	row     Episode
	lastRun time.Time
	// seenAt is when a report last named the card with this check, in any
	// state: a card that is still there is not gone.
	seenAt       time.Time
	healthySince time.Time
	recurrences  []time.Time
	// dirty: changed since written; urgent: to be written within seconds.
	dirty, urgent, closed bool
	// version moves with every change, so a write that raced one leaves
	// the episode dirty.
	version uint64
}

// Tracker is the card checks of every node and their open episodes.
type Tracker struct {
	log   *slog.Logger
	store EpisodeStore
	now   func() time.Time

	flushMu  sync.Mutex
	mu       sync.Mutex
	nodes    map[string]*NodeSnapshot
	episodes map[episodeKey]*episode
	lastFull time.Time
	// generation moves when a check's state changes anywhere, so a reader
	// can cache what it built from the snapshots.
	generation atomic.Uint64
}

// New returns a tracker. Load must be called before reports are observed.
func New(log *slog.Logger, st EpisodeStore) *Tracker {
	return &Tracker{log: log, store: st, now: time.Now, nodes: map[string]*NodeSnapshot{}, episodes: map[episodeKey]*episode{}}
}

// Load reads the episodes still open, so a restart continues them rather than
// opening a second one for the same problem.
func (t *Tracker) Load() error {
	rows, err := t.store.OpenEpisodes()
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, row := range rows {
		t.episodes[episodeKey{row.NodeID, row.CardIdent, row.CheckID}] = &episode{row: row, lastRun: row.LastSeenAt, seenAt: t.now()}
	}
	t.lastFull = t.now()
	return nil
}

// Generation moves whenever a check's state changes.
func (t *Tracker) Generation() uint64 { return t.generation.Load() }

// Observe takes one status report's cards.
func (t *Tracker) Observe(nodeUUID string, nodeID uint, at time.Time, gpus []proto.GPUInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.nodes[nodeUUID]
	next := &NodeSnapshot{NodeID: nodeID, ReceivedAt: at, Online: true}
	for _, g := range gpus {
		ident := accelerator.CardIdent(g.UUID, g.PCIeBDF)
		if ident == "" || !accelerator.CardUsable(g.Ownership) {
			continue
		}
		if len(g.Checks) > 0 {
			next.Reports = true
		}
		next.Cards = append(next.Cards, CardSnapshot{
			Ident: ident, UUID: g.UUID, PCIeBDF: g.PCIeBDF, Name: g.Name, Vendor: g.Vendor, Index: g.Index,
			Checks: append([]proto.CardCheck(nil), g.Checks...),
		})
	}
	t.nodes[nodeUUID] = next
	if statesChanged(prev, next) {
		t.generation.Add(1)
	}
	for _, card := range next.Cards {
		for _, c := range card.Checks {
			t.observeCheck(episodeKey{nodeID, card.Ident, c.ID}, c, at)
		}
	}
}

func (t *Tracker) observeCheck(key episodeKey, c proto.CardCheck, at time.Time) {
	ep := t.episodes[key]
	if ep != nil {
		ep.seenAt = at
	}
	ranAt := at.Add(-time.Duration(c.AgeS) * time.Second)
	switch {
	case accelerator.CheckProblem(c.Status):
		evidence := evidenceText(c.Evidence)
		if ep == nil || ep.closed {
			t.episodes[key] = &episode{
				row: Episode{
					NodeID: key.node, CardIdent: key.ident, CheckID: key.check,
					Status: c.Status, Code: c.Code, StartedAt: ranAt.UTC(), LastSeenAt: at.UTC(), Count: 1,
					FirstEvidence: evidence, LatestEvidence: evidence,
				},
				lastRun: ranAt, seenAt: at, dirty: true, urgent: true,
			}
			return
		}
		if !ep.healthySince.IsZero() {
			ep.healthySince = time.Time{}
			ep.recurrences = append(trimBefore(ep.recurrences, at.Add(-FlappingWindow)), at)
			if !ep.row.Flapping && len(ep.recurrences) >= FlappingRecurrences {
				ep.row.Flapping, ep.urgent = true, true
				ep.version++
			}
		}
		if ranAt.After(ep.lastRun.Add(time.Second)) {
			ep.row.Count++
			ep.lastRun = ranAt
		}
		if ep.row.Status != c.Status || ep.row.Code != c.Code {
			ep.row.Status, ep.row.Code, ep.urgent = c.Status, c.Code, true
		}
		ep.row.LatestEvidence = evidence
		ep.row.LastSeenAt = at.UTC()
		ep.dirty = true
		ep.version++
	case c.Status == proto.CheckPass:
		if ep == nil || ep.closed {
			return
		}
		if ep.healthySince.IsZero() {
			ep.healthySince = ranAt
		}
		if at.Sub(ep.healthySince) >= QuietPeriod {
			t.closeLocked(ep, ep.healthySince)
		}
	}
	// A skipped, pending or inapplicable check says nothing either way.
}

func (t *Tracker) closeLocked(ep *episode, at time.Time) {
	end := at.UTC()
	ep.row.EndedAt = &end
	ep.closed, ep.dirty, ep.urgent = true, true, true
	ep.version++
}

// SetOffline marks a node's snapshot as no longer current; it is kept, so its
// last states are still shown.
func (t *Tracker) SetOffline(nodeUUID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := t.nodes[nodeUUID]; n != nil && n.Online {
		n.Online = false
		t.generation.Add(1)
	}
}

// ForgetNode drops a deleted node. Its episodes go with its rows.
func (t *Tracker) ForgetNode(nodeUUID string, nodeID uint) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.nodes, nodeUUID)
	for key := range t.episodes {
		if key.node == nodeID {
			delete(t.episodes, key)
		}
	}
	t.generation.Add(1)
}

// Node is a copy of a node's last snapshot.
func (t *Tracker) Node(nodeUUID string) (NodeSnapshot, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.nodes[nodeUUID]
	if n == nil {
		return NodeSnapshot{}, false
	}
	out := *n
	out.Cards = append([]CardSnapshot(nil), n.Cards...)
	return out, true
}

// LiveEpisodes are the open episodes of a node as they stand in memory, newer
// than what was last written, keyed by row id (0 for one not yet written).
func (t *Tracker) LiveEpisodes(nodeID uint) []Episode {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Episode
	for key, ep := range t.episodes {
		if key.node == nodeID {
			out = append(out, ep.row)
		}
	}
	return out
}

// Flush writes what is due: urgent changes always, every change when all is
// true or an hour has passed since the last full write. Closed episodes leave
// memory once written.
func (t *Tracker) Flush(all bool) error {
	t.flushMu.Lock()
	defer t.flushMu.Unlock()
	now := t.now()
	t.mu.Lock()
	full := all || now.Sub(t.lastFull) >= countFlushInterval
	t.closeVanishedLocked(now)
	var due []*episode
	for _, ep := range t.episodes {
		if ep.urgent || (full && ep.dirty) {
			due = append(due, ep)
		}
	}
	rows := make([]*Episode, len(due))
	versions := make([]uint64, len(due))
	for i, ep := range due {
		row := ep.row
		rows[i], versions[i] = &row, ep.version
	}
	t.mu.Unlock()
	if len(rows) > 0 {
		if err := t.store.SaveEpisodes(rows); err != nil {
			return err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, ep := range due {
		ep.row.ID = rows[i].ID
		if ep.version != versions[i] {
			// Changed while it was being written: written next time.
			continue
		}
		ep.urgent, ep.dirty = false, false
		if ep.closed {
			for key, cur := range t.episodes {
				if cur == ep {
					delete(t.episodes, key)
				}
			}
		}
	}
	if full {
		t.lastFull = now
	}
	return nil
}

// closeVanishedLocked closes the episodes of cards that no report has named
// for vanishedAfter while their node kept reporting.
func (t *Tracker) closeVanishedLocked(now time.Time) {
	reporting := map[uint]bool{}
	for _, n := range t.nodes {
		if n.Online && now.Sub(n.ReceivedAt) < 2*accelerator.ReportPeriod {
			reporting[n.NodeID] = true
		}
	}
	for key, ep := range t.episodes {
		if !ep.closed && reporting[key.node] && now.Sub(ep.seenAt) >= vanishedAfter {
			t.closeLocked(ep, ep.seenAt)
		}
	}
}

// Prune deletes closed episodes older than the retention, a batch at a time.
func (t *Tracker) Prune() (int, error) {
	total := 0
	before := t.now().Add(-Retention)
	for {
		n, err := t.store.DeleteEpisodesEndedBefore(before, 1000)
		total += n
		if err != nil || n < 1000 {
			return total, err
		}
	}
}

func statesChanged(prev, next *NodeSnapshot) bool {
	if prev == nil || !prev.Online || len(prev.Cards) != len(next.Cards) {
		return true
	}
	for i, card := range next.Cards {
		old := prev.Cards[i]
		if old.Ident != card.Ident || len(old.Checks) != len(card.Checks) {
			return true
		}
		for j, c := range card.Checks {
			if o := old.Checks[j]; o.ID != c.ID || o.Status != c.Status || o.Code != c.Code {
				return true
			}
		}
	}
	return false
}

func trimBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	return times[i:]
}

// evidenceText is the evidence in the text form a card's health message uses,
// cut to MaxEvidenceBytes.
func evidenceText(fields []proto.DoctorField) string {
	hf := make([]accelerator.HealthField, 0, len(fields))
	for _, f := range fields {
		hf = append(hf, accelerator.HealthField{Name: f.Name, Value: f.Value})
	}
	text := accelerator.FormatHealthEvidence(hf)
	if len(text) <= MaxEvidenceBytes {
		return text
	}
	cut := MaxEvidenceBytes
	for cut > 0 && !utf8Start(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut]) + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
