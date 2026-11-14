package store

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

var wholeNodeReadSQL = regexp.MustCompile("^SELECT \\* FROM [`\"]?nodes[`\"]?( ORDER BY [^ ]+( desc| asc)?)?$")

// fleetFixture is a store with the node snapshot on, three nodes with a card
// each, a clock the test moves, and a count of the whole-fleet reads it serves.
type fleetFixture struct {
	st    *Store
	now   time.Time
	reads atomic.Int64
	uuids []string
}

func newFleetFixture(t *testing.T) *fleetFixture {
	t.Helper()
	f := &fleetFixture{st: newTestStore(t), now: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)}
	t.Cleanup(func() { _ = f.st.Close() })
	for i := 1; i <= 3; i++ {
		uuid := fmt.Sprintf("fleet-%d", i)
		if _, err := f.st.RegisterNode(uuid, HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		if err := f.st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: uuid, GPUVendor: "nvidia"},
			GPUs: []proto.GPUInfo{{Index: 0, UUID: "GPU-" + uuid, Name: "NVIDIA A100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true}},
		}); err != nil {
			t.Fatal(err)
		}
		f.uuids = append(f.uuids, uuid)
	}
	f.st.EnableFleetNodeSnapshot(func() time.Time { return f.now })
	if err := f.st.DB().Callback().Query().After("gorm:query").Register("test:fleet_reads", func(tx *gorm.DB) {
		if wholeNodeReadSQL.MatchString(strings.TrimSpace(tx.Statement.SQL.String())) {
			f.reads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.DB().Callback().Query().Remove("test:fleet_reads") })
	return f
}

// fleet reads the snapshot and reports whether that read rebuilt it.
func (f *fleetFixture) fleet(t *testing.T) ([]Node, bool) {
	t.Helper()
	before := f.reads.Load()
	nodes, err := f.st.FleetNodes()
	if err != nil {
		t.Fatal(err)
	}
	return nodes, f.reads.Load() > before
}

func nodeByUUID(nodes []Node, uuid string) *Node {
	for i := range nodes {
		if nodes[i].UUID == uuid {
			return &nodes[i]
		}
	}
	return nil
}

// With nothing changed, every read after the first is served without reading
// the fleet, and is the same as ListNodes: rows, cards and credential flags,
// newest first.
func TestFleetNodesServesUnchangedReadsWithoutReadingTheFleet(t *testing.T) {
	f := newFleetFixture(t)
	first, rebuilt := f.fleet(t)
	if !rebuilt {
		t.Fatal("the first read did not read the fleet")
	}
	for range 5 {
		if _, rebuilt := f.fleet(t); rebuilt {
			t.Fatal("an unchanged fleet was read again")
		}
	}
	want, err := f.st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(want) {
		t.Fatalf("snapshot has %d nodes, ListNodes %d", len(first), len(want))
	}
	for i := range want {
		if first[i].UUID != want[i].UUID || len(first[i].GPUs) != len(want[i].GPUs) || first[i].HasSSHCredential != want[i].HasSSHCredential {
			t.Fatalf("snapshot node %d = %s with %d cards, ListNodes %s with %d", i, first[i].UUID, len(first[i].GPUs), want[i].UUID, len(want[i].GPUs))
		}
	}
}

// Every change a node event announces is seen by the very next read: an
// operator who renames, labels, cordons or deletes a node and reloads the
// page sees it.
func TestFleetNodesSeesEveryAnnouncedChangeAtOnce(t *testing.T) {
	f := newFleetFixture(t)
	box, err := auth.NewSecretbox("fleet-nodes-test")
	if err != nil {
		t.Fatal(err)
	}
	f.st.SetSecretbox(box)
	for _, tc := range []struct {
		name   string
		change func() error
		seen   func([]Node) bool
	}{
		{"rename", func() error { _, err := f.st.RenameNode("fleet-1", "renamed"); return err },
			func(n []Node) bool { return nodeByUUID(n, "fleet-1").Name == "renamed" }},
		{"labels", func() error { _, err := f.st.SetNodeLabels("fleet-1", map[string]string{"zone": "a"}); return err },
			func(n []Node) bool { return strings.Contains(nodeByUUID(n, "fleet-1").Labels, `"zone":"a"`) }},
		{"cordon", func() error { _, err := f.st.SetNodeScheduling("fleet-2", NodeCordoned); return err },
			func(n []Node) bool { return nodeByUUID(n, "fleet-2").Scheduling == NodeCordoned }},
		{"state", func() error { return f.st.SetNodeState("fleet-2", NodeStateOffline) },
			func(n []Node) bool { return nodeByUUID(n, "fleet-2").State == NodeStateOffline }},
		{"recovery", func() error { return f.st.SetNodeRecoveryPhase("fleet-2", RecoveryPhaseProbing, 1, nil, "") },
			func(n []Node) bool { return nodeByUUID(n, "fleet-2").ConnectionState() == NodeStateConnecting }},
		{"ssh credential", func() error {
			_, err := f.st.SetNodeSSHCredential("fleet-3", "gpu.example", 22, "root", NodeSSHSecrets{Password: "high entropy password"})
			return err
		}, func(n []Node) bool { return nodeByUUID(n, "fleet-3").HasSSHCredential }},
		// The gateway's direct-path outcome publishes nothing (a node event
		// would make the controller re-plan); the node list still shows it.
		{"serving forward", func() error {
			return f.st.SetNodeServingForward("fleet-3", ServingForwardStatus{Mode: "direct", At: f.now})
		}, func(n []Node) bool { return strings.Contains(nodeByUUID(n, "fleet-3").ServingForward, `"direct"`) }},
		{"register", func() error { _, err := f.st.RegisterNode("fleet-4", HashToken("fleet-4"), "fleet-4"); return err },
			func(n []Node) bool { return nodeByUUID(n, "fleet-4") != nil }},
		{"delete", func() error { return f.st.DeleteNode("fleet-4") },
			func(n []Node) bool { return nodeByUUID(n, "fleet-4") == nil }},
	} {
		f.fleet(t) // the snapshot is current before the change
		if err := tc.change(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got, _ := f.fleet(t); !tc.seen(got) {
			t.Errorf("%s: the next read does not show it", tc.name)
		}
	}
}

// A node's routine status report (every agent, once a minute) rebuilds the
// snapshot at most once per report window, and what it reported is seen
// after the window: a thousand nodes reporting is ~17 reports a second, and
// rebuilding on each would be no snapshot at all.
func TestFleetNodesFoldsRoutineReportsIntoOneRebuildPerWindow(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(t)
	report := func(uuid, hostname string) {
		t.Helper()
		if err := f.st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: hostname, GPUVendor: "nvidia"},
			GPUs: []proto.GPUInfo{{Index: 0, UUID: "GPU-" + uuid, Name: "NVIDIA A100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 20 {
		report(f.uuids[i%3], fmt.Sprintf("host-%d", i))
		f.now = f.now.Add(50 * time.Millisecond)
		if _, rebuilt := f.fleet(t); rebuilt {
			t.Fatalf("report %d inside the window rebuilt the snapshot", i)
		}
	}
	f.now = f.now.Add(fleetNodesReportWindow)
	got, rebuilt := f.fleet(t)
	if !rebuilt || nodeByUUID(got, f.uuids[19%3]).Hostname != "host-19" {
		t.Fatalf("after the window: rebuilt %v, hostname %q; want the last report seen", rebuilt, nodeByUUID(got, f.uuids[19%3]).Hostname)
	}
	// A heartbeat is routine too.
	if err := f.st.TouchNodesAt(map[string]time.Time{"fleet-1": f.now}); err != nil {
		t.Fatal(err)
	}
	if _, rebuilt := f.fleet(t); rebuilt {
		t.Fatal("a heartbeat inside the window rebuilt the snapshot")
	}
}

// A write no event announces - another process, or a store path that
// publishes nothing - is seen once the snapshot is fleetNodesMaxAge old.
func TestFleetNodesSeesUnannouncedWritesWithinItsMaximumAge(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(t)
	if err := f.st.DB().Model(&Node{}).Where("uuid = ?", "fleet-1").Update("name", "behind-its-back").Error; err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(fleetNodesMaxAge - time.Millisecond)
	if got, _ := f.fleet(t); nodeByUUID(got, "fleet-1").Name == "behind-its-back" {
		t.Fatal("an unannounced write was seen before the maximum age: the snapshot was not used")
	}
	f.now = f.now.Add(time.Millisecond)
	if got, _ := f.fleet(t); nodeByUUID(got, "fleet-1").Name != "behind-its-back" {
		t.Fatal("an unannounced write is not seen at the maximum age")
	}
}

// Each read gets its own copy: a caller that sorts the nodes or changes a
// card does not change what the next caller reads.
func TestFleetNodesHandsEachCallerItsOwnCopy(t *testing.T) {
	f := newFleetFixture(t)
	first, _ := f.fleet(t)
	slices.Reverse(first)
	first[0].Name = "mutated"
	first[0].GPUs[0].Name = "mutated card"
	again, rebuilt := f.fleet(t)
	if rebuilt {
		t.Fatal("unexpected rebuild")
	}
	if again[0].UUID != "fleet-3" || again[2].Name == "mutated" || again[2].GPUs[0].Name == "mutated card" {
		t.Fatalf("a caller's changes leaked into the snapshot: %s %q %q", again[0].UUID, again[2].Name, again[2].GPUs[0].Name)
	}
}

// Many readers arriving together while the fleet changes share rebuilds and
// never race (run with -race).
func TestFleetNodesConcurrentReadersShareTheSnapshot(t *testing.T) {
	f := newFleetFixture(t)
	f.fleet(t)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 20 {
				nodes, err := f.st.FleetNodes()
				if err != nil {
					t.Error(err)
					return
				}
				slices.SortFunc(nodes, func(a, b Node) int { return strings.Compare(a.UUID, b.UUID) })
				nodes[0].GPUs[0].Name = "mine"
				if i == 0 && j%5 == 0 {
					if _, err := f.st.SetNodeLabels("fleet-1", map[string]string{"round": fmt.Sprint(j)}); err != nil {
						t.Error(err)
					}
				}
			}
		}()
	}
	wg.Wait()
	if n := f.reads.Load(); n > 6 {
		t.Fatalf("16 readers x 20 reads with 4 changes read the fleet %d times, want at most 6", n)
	}
}

// Without the snapshot switched on (tests that write the database directly,
// one-shot commands) every read is a ListNodes.
func TestFleetNodesWithoutTheSnapshotReadsEachTime(t *testing.T) {
	st := newTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("plain", HashToken("plain"), "plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FleetNodes(); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&Node{}).Where("uuid = ?", "plain").Update("name", "direct").Error; err != nil {
		t.Fatal(err)
	}
	nodes, err := st.FleetNodes()
	if err != nil || len(nodes) != 1 || nodes[0].Name != "direct" {
		t.Fatalf("nodes = %+v err %v, want the direct write seen", nodes, err)
	}
}
