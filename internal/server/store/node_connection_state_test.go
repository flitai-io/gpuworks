package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A node's state says whether its agent is on the other end, and nothing
// else. Accelerator trouble is the cards' business; the node stays online so
// an operator can still open a terminal on it and look.
func TestAcceleratorTroubleNeverChangesTheConnectionState(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-conn", HashToken("t"), "conn-host"); err != nil {
		t.Fatal(err)
	}
	node, err := s.NodeByUUID("uuid-conn")
	if err != nil {
		t.Fatal(err)
	}
	if node.State != NodeStateConnecting {
		t.Fatalf("a node that has never connected = %q, want %q", node.State, NodeStateConnecting)
	}

	ok, failed := true, false
	cards := []proto.GPUInfo{{Index: 0, UUID: "GPU-a", Vendor: "nvidia", PCIeBDF: "0000:1a:00.0", Healthy: true}}
	for _, st := range []proto.NodeStatus{
		{GPUProbeOK: &ok, GPUs: cards},
		{GPUProbeOK: &failed, GPUProbeError: "nvidia-smi: no answer within 10s"},
		{GPUProbeOK: &ok, GPUs: cards},
	} {
		if err := s.UpdateNodeStatus("uuid-conn", st); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeByUUID("uuid-conn")
		if err != nil {
			t.Fatal(err)
		}
		if node.State != NodeStateOnline {
			t.Fatalf("state = %q after a report (probe_ok=%v), want online throughout", node.State, st.GPUProbeOK)
		}
	}
}

func TestNodeConnectionStateMigrationRewritesPendingAndDegraded(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:node-connection-states-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &Node{}); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"gpu_probe_error", "gpu_probe_failed_at"} {
		if err := db.Exec("ALTER TABLE nodes DROP COLUMN " + column).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec(`INSERT INTO nodes (uuid, name, state) VALUES
		('n-pending','p','pending'), ('n-degraded','d','degraded'),
		('n-online','o','online'), ('n-offline','f','offline')`).Error; err != nil {
		t.Fatal(err)
	}

	m := migrationByName(t, "node_connection_states")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil || db.Migrator().HasColumn(&Node{}, "GPUProbeError") {
		t.Fatalf("failed migration left the column (err=%v)", err)
	}
	var state string
	if err := db.Raw("SELECT state FROM nodes WHERE uuid = 'n-pending'").Scan(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("failed migration kept the state rewrite: %q", state)
	}

	if err := migrateDB(db, []migration{m}); err != nil || !db.Migrator().HasColumn(&Node{}, "GPUProbeError") {
		t.Fatalf("migration err = %v", err)
	}
	want := map[string]string{
		"n-pending":  NodeStateConnecting,
		"n-degraded": NodeStateOnline, // it had a live session; the next heartbeat or disconnect settles it
		"n-online":   NodeStateOnline,
		"n-offline":  NodeStateOffline,
	}
	for uuid, expect := range want {
		var got string
		if err := db.Raw("SELECT state FROM nodes WHERE uuid = ?", uuid).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got != expect {
			t.Fatalf("%s state = %q, want %q", uuid, got, expect)
		}
	}
	if err := db.Delete(&SchemaMigration{}, m.Version).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun err = %v", err)
	}
}

// A node whose container runtime cannot give a container its accelerators
// is cordoned by the platform: its cards are fine, so nothing else would
// stop it being chosen, and every deployment placed there fails when its
// container starts.
func TestANodeWhoseRuntimeCannotUseItsCardsIsCordoned(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-blocked", HashToken("t"), "blocked-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	cards := []proto.GPUInfo{{Index: 0, UUID: "GPU-a", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}}
	const reason = accelerator.RuntimeGPUBlockedPluginMissing
	report := func(blocked string) Node {
		t.Helper()
		if err := s.UpdateNodeStatus("uuid-blocked", proto.NodeStatus{
			GPUProbeOK: &ok, GPUs: cards,
			Sys: proto.SysInfo{RuntimeDriver: "docker", RuntimeGPUBlocked: blocked},
		}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeByUUID("uuid-blocked")
		if err != nil {
			t.Fatal(err)
		}
		return *node
	}

	sub := s.Bus.Subscribe(16, "node")
	t.Cleanup(sub.Close)
	node := report(reason)
	if node.Scheduling != NodeCordoned {
		t.Fatalf("scheduling = %q, want the platform to have cordoned it", node.Scheduling)
	}
	// Every reader of scheduling changes hears that the platform did it,
	// not only the readers of routine status reports.
	announced := false
	for drained := false; !drained; {
		select {
		case ev := <-sub.C():
			announced = announced || ev.Reason == NodeChangeScheduling
		default:
			drained = true
		}
	}
	if !announced {
		t.Fatal("the platform's cordon was not announced as a scheduling change")
	}
	if node.RuntimeGPUBlocked != reason {
		t.Fatalf("reason = %q, want it kept so the operator is told why", node.RuntimeGPUBlocked)
	}
	// The cards themselves are not faulted: nothing is wrong with them.
	withCards, err := s.NodeWithGPUs("uuid-blocked")
	if err != nil {
		t.Fatal(err)
	}
	if len(withCards.GPUs) != 1 || !withCards.GPUs[0].CanTakeWork() {
		t.Fatalf("the cards were faulted for the runtime's problem: %+v", withCards.GPUs)
	}

	// The operator may know better and turn scheduling back on; the next
	// report cordons it again while the cause remains.
	if _, err := s.SetNodeScheduling("uuid-blocked", NodeSchedulable); err != nil {
		t.Fatal(err)
	}
	if node := report(reason); node.Scheduling != NodeCordoned {
		t.Fatalf("scheduling = %q, want it cordoned again while the runtime still cannot use the cards", node.Scheduling)
	}

	// Once the plugin is installed the platform stops interfering, and the
	// operator's own setting stands.
	if _, err := s.SetNodeScheduling("uuid-blocked", NodeSchedulable); err != nil {
		t.Fatal(err)
	}
	node = report("")
	if node.Scheduling != NodeSchedulable || node.RuntimeGPUBlocked != "" {
		t.Fatalf("after the plugin was installed: scheduling=%q reason=%q", node.Scheduling, node.RuntimeGPUBlocked)
	}

	// An operator's own cordon is never undone by a report either.
	if _, err := s.SetNodeScheduling("uuid-blocked", NodeCordoned); err != nil {
		t.Fatal(err)
	}
	if node := report(""); node.Scheduling != NodeCordoned {
		t.Fatalf("scheduling = %q, want the operator's cordon left alone", node.Scheduling)
	}
}

// The cordon the platform put on lasts as long as its cause: once the node
// reports a runtime that can use its cards — the plugin installed, or the
// agent moved to runveil, which needs none — scheduling comes back on its
// own and every reader of scheduling changes hears it. A report with no
// runtime at all is no such evidence. The runtime the agent passed over is
// kept for the operator, and cleared by the report that no longer has one.
func TestThePlatformsCordonIsLiftedWhenItsCauseIsGone(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-heals", HashToken("t"), "heals-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	cards := []proto.GPUInfo{{Index: 0, UUID: "GPU-a", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}}
	report := func(sys proto.SysInfo) Node {
		t.Helper()
		if err := s.UpdateNodeStatus("uuid-heals", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: sys}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeByUUID("uuid-heals")
		if err != nil {
			t.Fatal(err)
		}
		return *node
	}
	blocked := proto.SysInfo{RuntimeDriver: "docker", RuntimeGPUBlocked: accelerator.RuntimeGPUBlockedPluginMissing}
	if node := report(blocked); node.Scheduling != NodeCordoned {
		t.Fatalf("scheduling = %q, want cordoned", node.Scheduling)
	}
	// An agent that lost its runtime altogether reports nothing blocked;
	// that does not make the node able to run anything.
	if node := report(proto.SysInfo{}); node.Scheduling != NodeCordoned {
		t.Fatalf("scheduling = %q after a report with no runtime", node.Scheduling)
	}
	if node := report(blocked); node.Scheduling != NodeCordoned {
		t.Fatalf("scheduling = %q", node.Scheduling)
	}

	sub := s.Bus.Subscribe(16, "node")
	t.Cleanup(sub.Close)
	node := report(proto.SysInfo{RuntimeDriver: "udocker", RuntimeFallbackFrom: "docker",
		RuntimeFallbackReason: accelerator.RuntimeGPUBlockedPluginMissing})
	if node.Scheduling != NodeSchedulable {
		t.Fatalf("scheduling = %q, want the platform's cordon lifted on runveil", node.Scheduling)
	}
	if node.RuntimeFallbackFrom != "docker" || node.RuntimeFallbackReason != accelerator.RuntimeGPUBlockedPluginMissing {
		t.Fatalf("fallback = %q/%q, want it kept for the operator", node.RuntimeFallbackFrom, node.RuntimeFallbackReason)
	}
	announced := false
	for drained := false; !drained; {
		select {
		case ev := <-sub.C():
			announced = announced || ev.Reason == NodeChangeScheduling
		default:
			drained = true
		}
	}
	if !announced {
		t.Fatal("lifting the platform's cordon was not announced as a scheduling change")
	}

	node = report(proto.SysInfo{RuntimeDriver: "docker"})
	if node.Scheduling != NodeSchedulable || node.RuntimeFallbackFrom != "" || node.RuntimeFallbackReason != "" {
		t.Fatalf("back on docker: scheduling=%q fallback=%q/%q", node.Scheduling, node.RuntimeFallbackFrom, node.RuntimeFallbackReason)
	}
}

// Recovery is folded into the state the server reports, so a badge, a list
// filter and a dashboard count cannot disagree about a node the platform is
// restoring. The stored column keeps its own meaning: what the control
// plane knows by itself.
func TestRecoveryInFlightReadsAsConnectingWithoutRewritingTheColumn(t *testing.T) {
	cases := []struct {
		state, phase, want string
	}{
		{NodeStateOffline, RecoveryPhaseUploading, NodeStateConnecting},
		{NodeStateOffline, RecoveryPhaseReconnecting, NodeStateConnecting},
		// An attempt that ended is not underway, even one the loop retries.
		{NodeStateOffline, RecoveryPhaseUnreachable, NodeStateOffline},
		{NodeStateOffline, RecoveryPhaseProbeFailed, NodeStateOffline},
		{NodeStateOffline, RecoveryPhaseBlocked, NodeStateOffline},
		{NodeStateOffline, RecoveryPhaseStopped, NodeStateOffline},
		{NodeStateOffline, "", NodeStateOffline},
		// A node with a session is online whatever a stale phase says, and
		// the fold never invents one.
		{NodeStateOnline, RecoveryPhaseUploading, NodeStateOnline},
		{NodeStateConnecting, "", NodeStateConnecting},
	}
	for _, tc := range cases {
		n := &Node{State: tc.state, RecoveryPhase: tc.phase}
		if got := n.ConnectionState(); got != tc.want {
			t.Fatalf("state %q phase %q reads as %q, want %q", tc.state, tc.phase, got, tc.want)
		}
		if n.State != tc.state {
			t.Fatalf("the fold rewrote the stored state: %q", n.State)
		}
	}
	if got := (*Node)(nil).ConnectionState(); got != "" {
		t.Fatalf("nil node = %q", got)
	}
}
