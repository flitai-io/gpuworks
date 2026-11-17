package cardhealth

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// memStore keeps episodes like the store does and counts its writes.
type memStore struct {
	mu     sync.Mutex
	rows   map[uint]Episode
	nextID uint
	writes int // rows written, inserts and updates alike
	saves  int // transactions
	fail   error
}

func newMemStore() *memStore { return &memStore{rows: map[uint]Episode{}} }

func (m *memStore) OpenEpisodes() ([]Episode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Episode
	for _, r := range m.rows {
		if r.EndedAt == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) SaveEpisodes(rows []*Episode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.saves++
	for _, r := range rows {
		if r.ID == 0 {
			m.nextID++
			r.ID = m.nextID
		}
		m.rows[r.ID] = *r
		m.writes++
	}
	return nil
}

func (m *memStore) DeleteEpisodesEndedBefore(before time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.rows {
		if n < limit && r.EndedAt != nil && r.EndedAt.Before(before) {
			delete(m.rows, id)
			n++
		}
	}
	return n, nil
}

func (m *memStore) all() []Episode {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Episode, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type harness struct {
	t     *testing.T
	st    *memStore
	tr    *Tracker
	now   time.Time
	cards int
}

func newHarness(t *testing.T, cards int) *harness {
	t.Helper()
	h := &harness{t: t, st: newMemStore(), now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), cards: cards}
	h.tr = New(slog.New(slog.DiscardHandler), h.st)
	h.tr.now = func() time.Time { return h.now }
	if err := h.tr.Load(); err != nil {
		t.Fatal(err)
	}
	return h
}

func cardUUID(i int) string  { return fmt.Sprintf("GPU-%d", i) }
func cardIdent(i int) string { return accelerator.CardIdent(cardUUID(i), "") }

// report sends one status report in which every card's runtime check has the
// given state, except the cards in override, and moves time on a minute.
func (h *harness) report(state string, override map[int]string) {
	h.t.Helper()
	gpus := make([]proto.GPUInfo, h.cards)
	for i := range gpus {
		s := state
		if o, ok := override[i]; ok {
			s = o
		}
		c := proto.CardCheck{ID: accelerator.CheckVendorHealth, Status: s}
		if accelerator.CheckProblem(s) {
			c.Code = accelerator.HealthMemoryUncorrectable
			c.Evidence = []proto.DoctorField{{Name: "ecc", Value: "3"}}
		}
		gpus[i] = proto.GPUInfo{Index: i, UUID: cardUUID(i), Vendor: "nvidia", Checks: []proto.CardCheck{c}}
	}
	h.tr.Observe("node-1", 7, h.now, gpus)
	h.now = h.now.Add(time.Minute)
}

func (h *harness) flush(all bool) {
	h.t.Helper()
	if err := h.tr.Flush(all); err != nil {
		h.t.Fatal(err)
	}
}

// A healthy fleet writes nothing, however long it reports.
func TestAHealthyFleetWritesNothing(t *testing.T) {
	h := newHarness(t, 8)
	for i := 0; i < 120; i++ {
		h.report(proto.CheckPass, nil)
		h.flush(false)
	}
	h.flush(true)
	if h.st.writes != 0 || h.st.saves != 0 {
		t.Fatalf("writes = %d in %d saves, want none", h.st.writes, h.st.saves)
	}
}

// A card that keeps failing is one row: opened within seconds, its count
// written once an hour, and closed after the quiet period.
func TestAPersistentFailureIsOneRowWrittenHourly(t *testing.T) {
	h := newHarness(t, 1)
	h.report(proto.CheckFail, nil)
	h.flush(false)
	rows := h.st.all()
	if len(rows) != 1 || rows[0].Status != proto.CheckFail || rows[0].Code != accelerator.HealthMemoryUncorrectable ||
		rows[0].FirstEvidence != "ecc=3" || rows[0].EndedAt != nil || rows[0].Count != 1 {
		t.Fatalf("after the first failure = %+v", rows)
	}
	writes := h.st.writes
	for i := 0; i < 58; i++ {
		h.report(proto.CheckFail, nil)
		h.flush(false)
	}
	if h.st.writes != writes {
		t.Fatalf("an hour of the same failure wrote %d rows before the hourly write", h.st.writes-writes)
	}
	h.report(proto.CheckFail, nil)
	h.flush(false)
	rows = h.st.all()
	if len(rows) != 1 || rows[0].Count != 60 || h.st.writes != writes+1 {
		t.Fatalf("after the hourly write = %+v (writes %d)", rows, h.st.writes-writes)
	}

	// Passing for less than the quiet period keeps it open; a failure in
	// it continues the same row.
	for i := 0; i < 10; i++ {
		h.report(proto.CheckPass, nil)
		h.flush(false)
	}
	h.report(proto.CheckFail, nil)
	h.flush(true)
	if rows = h.st.all(); len(rows) != 1 || rows[0].EndedAt != nil {
		t.Fatalf("a recurrence within the quiet period = %+v", rows)
	}

	recovered := h.now
	for i := 0; i <= int(QuietPeriod/time.Minute); i++ {
		h.report(proto.CheckPass, nil)
		h.flush(false)
	}
	rows = h.st.all()
	if len(rows) != 1 || rows[0].EndedAt == nil || !rows[0].EndedAt.Equal(recovered) {
		t.Fatalf("after the quiet period = %+v, want it closed at %s", rows, recovered)
	}
	if live := h.tr.LiveEpisodes(7); len(live) != 0 {
		t.Fatalf("a closed episode stayed in memory: %+v", live)
	}
}

// Five hundred cards flipping every report open five hundred rows once and
// mark them flapping; they do not write a row per flip.
func TestFlappingCardsDoNotWriteEveryFlip(t *testing.T) {
	h := newHarness(t, 500)
	for i := 0; i < 30; i++ {
		state := proto.CheckFail
		if i%2 == 1 {
			state = proto.CheckPass
		}
		h.report(state, nil)
		h.flush(false)
	}
	rows := h.st.all()
	if len(rows) != 500 {
		t.Fatalf("rows = %d, want one per card", len(rows))
	}
	for _, r := range rows {
		if !r.Flapping || r.EndedAt != nil {
			t.Fatalf("row = %+v, want an open flapping episode", r)
		}
	}
	// One insert each, one write each when it turned flapping.
	if h.st.writes > 1000 {
		t.Fatalf("writes = %d for 30 reports of 500 flapping cards", h.st.writes)
	}
}

// A change of reason is written within seconds; evidence that differs only in
// its values waits for the hourly write.
func TestAChangeOfReasonIsWrittenAtOnce(t *testing.T) {
	h := newHarness(t, 1)
	h.report(proto.CheckWarn, nil)
	h.flush(false)
	writes := h.st.writes
	h.report(proto.CheckFail, nil)
	h.flush(false)
	rows := h.st.all()
	if h.st.writes != writes+1 || rows[0].Status != proto.CheckFail {
		t.Fatalf("after the level changed = %+v (writes %d)", rows, h.st.writes-writes)
	}
}

// A restarted server continues the episodes still open instead of opening
// second ones for the same problems.
func TestARestartContinuesOpenEpisodes(t *testing.T) {
	h := newHarness(t, 1)
	h.report(proto.CheckFail, nil)
	h.flush(true)

	restarted := New(slog.New(slog.DiscardHandler), h.st)
	restarted.now = func() time.Time { return h.now }
	if err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	h.tr = restarted
	h.report(proto.CheckFail, nil)
	h.flush(true)
	if rows := h.st.all(); len(rows) != 1 || rows[0].Count != 2 {
		t.Fatalf("rows after restart = %+v, want the same episode continued", rows)
	}
}

// Skipped, pending and inapplicable checks neither continue nor end an
// episode.
func TestASkippedCheckSaysNothing(t *testing.T) {
	h := newHarness(t, 1)
	h.report(proto.CheckFail, nil)
	for i := 0; i < 90; i++ {
		h.report(proto.CheckSkip, nil)
	}
	h.flush(true)
	rows := h.st.all()
	if len(rows) != 1 || rows[0].EndedAt != nil || rows[0].Count != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

// An episode whose card no report names any more, while its node keeps
// reporting, is closed when it was last seen.
func TestAVanishedCardsEpisodeCloses(t *testing.T) {
	h := newHarness(t, 2)
	h.report(proto.CheckPass, map[int]string{1: proto.CheckFail})
	lastSeen := h.now.Add(-time.Minute)
	h.cards = 1
	for i := 0; i < 61; i++ {
		h.report(proto.CheckPass, nil)
	}
	h.flush(false)
	var closed *Episode
	for _, r := range h.st.all() {
		if r.CardIdent == cardIdent(1) {
			closed = &r
		}
	}
	if closed == nil || closed.EndedAt == nil || !closed.EndedAt.Equal(lastSeen) {
		t.Fatalf("vanished card's episode = %+v, want closed at %s", closed, lastSeen)
	}
}

// A failed write loses nothing: the next one carries it.
func TestAFailedWriteIsRetried(t *testing.T) {
	h := newHarness(t, 1)
	h.report(proto.CheckFail, nil)
	h.st.fail = errors.New("database is locked")
	if err := h.tr.Flush(false); err == nil {
		t.Fatal("a failed write reported success")
	}
	h.st.fail = nil
	h.flush(false)
	if rows := h.st.all(); len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

// Closed episodes older than the retention are deleted; open ones never are.
func TestPruneKeepsOpenEpisodes(t *testing.T) {
	h := newHarness(t, 2)
	h.report(proto.CheckFail, nil)
	for i := 0; i <= int(QuietPeriod/time.Minute); i++ {
		h.report(proto.CheckPass, map[int]string{1: proto.CheckFail})
	}
	h.flush(true)
	h.now = h.now.Add(Retention + time.Hour)
	if n, err := h.tr.Prune(); err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if rows := h.st.all(); len(rows) != 1 || rows[0].EndedAt != nil {
		t.Fatalf("rows = %+v, want only the open one", rows)
	}
}

// The generation moves when a state changes, not on every report.
func TestTheGenerationMovesOnlyOnChange(t *testing.T) {
	h := newHarness(t, 4)
	h.report(proto.CheckPass, nil)
	g := h.tr.Generation()
	for i := 0; i < 5; i++ {
		h.report(proto.CheckPass, nil)
	}
	if h.tr.Generation() != g {
		t.Fatal("unchanged reports moved the generation")
	}
	h.report(proto.CheckPass, map[int]string{2: proto.CheckWarn})
	if h.tr.Generation() == g {
		t.Fatal("a changed state did not move the generation")
	}
	g = h.tr.Generation()
	h.tr.SetOffline("node-1")
	if h.tr.Generation() == g {
		t.Fatal("going offline did not move the generation")
	}
	if snap, _ := h.tr.Node("node-1"); snap.Online || len(snap.Cards) != 4 {
		t.Fatalf("offline snapshot = %+v, want the last states kept", snap)
	}
}
