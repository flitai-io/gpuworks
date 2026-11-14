package store

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// fleetNodesReportWindow is how often routine reports alone (an agent's
	// status report, a heartbeat) may rebuild the node snapshot. Every agent
	// reports once a minute, so a thousand nodes make ~17 reports a second:
	// rebuilding on each would be no snapshot at all. 2 s is the controller's
	// own status window (controller/node_status.go).
	fleetNodesReportWindow = 2 * time.Second
	// fleetNodesMaxAge is how long the snapshot is kept with no change
	// announced: a write that announces nothing (another process, a heartbeat
	// that fell inside every window) is seen this late at most.
	fleetNodesMaxAge = 10 * time.Second
)

// fleetNodeCache is FleetNodes' snapshot and what it was built from.
type fleetNodeCache struct {
	// enabled is set by EnableFleetNodeSnapshot; until then FleetNodes reads
	// the fleet every time.
	enabled atomic.Bool
	// changes counts node changes that must be seen by the next read (every
	// node event but a routine report, and the writes that announce nothing
	// but are shown); reports counts routine reports and heartbeats.
	changes, reports atomic.Uint64

	mu  sync.Mutex
	now func() time.Time
	// rows were read at started, when changes and reports read changesAt and
	// reportsAt.
	rows                 []Node
	ok                   bool
	started              time.Time
	changesAt, reportsAt uint64
}

// EnableFleetNodeSnapshot turns FleetNodes' snapshot on; now is its clock
// (nil is time.Now). The server switches it on; a store opened for a test or
// a one-shot command reads the fleet on every FleetNodes call, so a test that
// writes the database directly reads what it wrote.
func (s *Store) EnableFleetNodeSnapshot(now func() time.Time) {
	c := &s.fleetNodes
	c.mu.Lock()
	defer c.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	c.now = now
	c.enabled.Store(true)
}

// FleetNodes is ListNodes - every node, its cards and credential flag,
// newest first - read from a snapshot shared by every caller once the
// snapshot is on. Each caller gets its own copy, which it may sort or change.
//
// The snapshot is rebuilt on the next read after any node change a node event
// announces, so a page reloaded after a rename, a cordon or a delete shows it;
// after routine reports only once per fleetNodesReportWindow; and at least
// every fleetNodesMaxAge for writes that announce nothing. Reads are
// serialized, so readers that miss together share one rebuild: those that
// waited behind it take its rows unless a change landed meanwhile.
func (s *Store) FleetNodes() ([]Node, error) {
	c := &s.fleetNodes
	if !c.enabled.Load() {
		return s.ListNodes()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	arrived := c.now()
	if c.fresh(arrived) {
		return cloneNodes(c.rows), nil
	}
	if err := s.rebuildFleetNodes(arrived); err != nil {
		return nil, err
	}
	return cloneNodes(c.rows), nil
}

// rebuildFleetNodes reads the fleet into the snapshot; the caller holds the
// lock. The counts are read before the rows: a change that lands while the
// rows are read makes the next read rebuild again.
func (s *Store) rebuildFleetNodes(now time.Time) error {
	c := &s.fleetNodes
	changes, reports := c.changes.Load(), c.reports.Load()
	rows, err := s.ListNodes()
	if err != nil {
		return err
	}
	c.rows, c.ok, c.started, c.changesAt, c.reportsAt = rows, true, now, changes, reports
	return nil
}

// fresh reports whether the snapshot may answer a read arriving at now.
func (c *fleetNodeCache) fresh(now time.Time) bool {
	if !c.ok {
		return false
	}
	age := now.Sub(c.started)
	if age >= fleetNodesMaxAge || c.changes.Load() != c.changesAt {
		return false
	}
	return c.reports.Load() == c.reportsAt || age < fleetNodesReportWindow
}

// RefreshFleetNodes rebuilds the snapshot now if a node report or change has
// landed since it was built, inside the report window too: the controller
// calls it once per batch of status reports, so the deployments the batch
// enqueues plan on those reports. Off, it does nothing.
func (s *Store) RefreshFleetNodes() error {
	c := &s.fleetNodes
	if !c.enabled.Load() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && c.changes.Load() == c.changesAt && c.reports.Load() == c.reportsAt {
		return nil
	}
	return s.rebuildFleetNodes(c.now())
}

// noteNodeChange counts a node change for FleetNodes: a routine status report
// waits for the report window, anything else is seen by the next read.
func (s *Store) noteNodeChange(reason string) {
	if reason == NodeChangeStatus {
		s.fleetNodes.reports.Add(1)
		return
	}
	s.fleetNodes.changes.Add(1)
}

// cloneNodes copies rows and each node's cards, so a caller's changes stay
// its own. Pointer fields (timestamps) are shared and never written through.
func cloneNodes(rows []Node) []Node {
	out := slices.Clone(rows)
	for i := range out {
		out[i].GPUs = slices.Clone(out[i].GPUs)
	}
	return out
}
