package store

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDecideCordon(t *testing.T) {
	for _, tc := range []struct {
		name   string
		was    NodeScheduling
		by     SchedulingSource
		reason string
		want   NodeScheduling
		wantBy SchedulingSource
		change bool
	}{
		{"schedulable with a reason is cordoned by the platform", NodeSchedulable, SchedulingBySelf, CordonCardNeedsHostReboot, NodeCordoned, SchedulingByPlatform, true},
		{"the platform's cordon without a reason is lifted", NodeCordoned, SchedulingByPlatform, "", NodeSchedulable, SchedulingBySelf, true},
		{"the platform's cordon with a reason stays", NodeCordoned, SchedulingByPlatform, CordonRuntimeGPUBlocked, NodeCordoned, SchedulingByPlatform, false},
		{"an operator's cordon is never lifted", NodeCordoned, SchedulingByOperator, "", NodeCordoned, SchedulingByOperator, false},
		{"a cordon of unknown author is never lifted", NodeCordoned, SchedulingBySelf, "", NodeCordoned, SchedulingBySelf, false},
		{"a drain is never touched", NodeDraining, SchedulingByOperator, CordonCardNeedsHostReboot, NodeDraining, SchedulingByOperator, false},
		{"schedulable without a reason stays", NodeSchedulable, SchedulingBySelf, "", NodeSchedulable, SchedulingBySelf, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := decideCordon(tc.was, tc.by, tc.reason)
			if d.scheduling != tc.want || d.source != tc.wantBy || d.changed != tc.change {
				t.Fatalf("decideCordon = %+v, want %q by %q changed=%v", d, tc.want, tc.wantBy, tc.change)
			}
		})
	}
}

func TestPlatformCordonReason(t *testing.T) {
	healthy := proto.GPUInfo{Index: 0, HealthCode: ""}
	reboot := proto.GPUInfo{Index: 1, HealthCode: accelerator.HealthRebootRequired}
	cycle := proto.GPUInfo{Index: 2, HealthCode: accelerator.HealthPowerCycleRequired}
	reset := proto.GPUInfo{Index: 3, HealthCode: accelerator.HealthResetRequired}
	lent := proto.GPUInfo{Index: 4, HealthCode: accelerator.HealthRebootRequired, Ownership: accelerator.CardPassedThrough}
	for _, tc := range []struct {
		blocked string
		cards   []proto.GPUInfo
		want    string
	}{
		{"", []proto.GPUInfo{healthy}, ""},
		{accelerator.RuntimeGPUBlockedPluginMissing, []proto.GPUInfo{healthy}, CordonRuntimeGPUBlocked},
		{"", []proto.GPUInfo{healthy, reboot}, CordonCardNeedsHostReboot},
		{"", []proto.GPUInfo{cycle}, CordonCardNeedsHostReboot},
		// A card reset is done on the card; the rest of the node can work.
		{"", []proto.GPUInfo{reset}, ""},
		// A card lent to a virtual machine is not this node's to judge.
		{"", []proto.GPUInfo{lent}, ""},
	} {
		if got := PlatformCordonReason(tc.blocked, true, tc.cards); got != CordonKubeUnschedulable {
			t.Errorf("PlatformCordonReason(%q, kube cordon, %+v) = %q, want %q", tc.blocked, tc.cards, got, CordonKubeUnschedulable)
		}
		if got := PlatformCordonReason(tc.blocked, false, tc.cards); got != tc.want {
			t.Errorf("PlatformCordonReason(%q, %+v) = %q, want %q", tc.blocked, tc.cards, got, tc.want)
		}
	}
}

// A card whose vendor says only a host reboot recovers it takes the whole
// node out of scheduling: the other cards sit behind the same driver. An
// operator may turn scheduling back on to try a deployment by hand; the next
// report cordons it again while the card still says so; once it does not,
// the platform lifts its own cordon.
func TestACardNeedingAHostRebootCordonsTheNode(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-reboot", HashToken("t"), "reboot-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	sys := proto.SysInfo{RuntimeDriver: "docker"}
	report := func(code string) Node {
		t.Helper()
		cards := []proto.GPUInfo{
			{Index: 0, UUID: "GPU-a", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true},
			{Index: 1, UUID: "GPU-b", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", MemoryTotalBytes: 24 << 30,
				HealthCode: code, HealthStatus: accelerator.HealthLevel(code), Healthy: code == ""},
		}
		if err := s.UpdateNodeStatus("uuid-reboot", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: sys}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeByUUID("uuid-reboot")
		if err != nil {
			t.Fatal(err)
		}
		return *node
	}
	if node := report(accelerator.HealthRebootRequired); node.Scheduling != NodeCordoned || node.SchedulingBy != SchedulingByPlatform {
		t.Fatalf("scheduling = %q by %q, want cordoned by the platform", node.Scheduling, node.SchedulingBy)
	}
	if _, err := s.SetNodeScheduling("uuid-reboot", NodeSchedulable); err != nil {
		t.Fatal(err)
	}
	if node, _ := s.NodeByUUID("uuid-reboot"); node.Scheduling != NodeSchedulable || node.SchedulingBy != SchedulingBySelf {
		t.Fatalf("after the operator turned it on: %q by %q", node.Scheduling, node.SchedulingBy)
	}
	if node := report(accelerator.HealthRebootRequired); node.Scheduling != NodeCordoned {
		t.Fatalf("scheduling = %q, want it cordoned again while the card still needs a reboot", node.Scheduling)
	}
	if node := report(""); node.Scheduling != NodeSchedulable || node.SchedulingBy != SchedulingBySelf {
		t.Fatalf("after the reboot: %q by %q, want the platform's cordon lifted", node.Scheduling, node.SchedulingBy)
	}
}

// An operator's cordon on top of the platform's survives the platform's
// reason going away: the platform lifts only what it set itself.
func TestTheOperatorsCordonOutlivesThePlatformsReason(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-both", HashToken("t"), "both-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	cards := []proto.GPUInfo{{Index: 0, UUID: "GPU-a", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}}
	report := func(blocked string) Node {
		t.Helper()
		if err := s.UpdateNodeStatus("uuid-both", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards,
			Sys: proto.SysInfo{RuntimeDriver: "docker", RuntimeGPUBlocked: blocked}}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeByUUID("uuid-both")
		if err != nil {
			t.Fatal(err)
		}
		return *node
	}
	if node := report(accelerator.RuntimeGPUBlockedPluginMissing); node.SchedulingBy != SchedulingByPlatform {
		t.Fatalf("by = %q", node.SchedulingBy)
	}
	// The operator cordons it too, for maintenance of their own.
	if _, err := s.SetNodeScheduling("uuid-both", NodeCordoned); err != nil {
		t.Fatal(err)
	}
	if node := report(""); node.Scheduling != NodeCordoned || node.SchedulingBy != SchedulingByOperator {
		t.Fatalf("after the plugin was installed: %q by %q, want the operator's cordon left", node.Scheduling, node.SchedulingBy)
	}
}

// The column arrives empty on existing rows; applying it again changes
// nothing; schema and marker roll back together.
func TestNodeSchedulingSourceMigration(t *testing.T) {
	db := nodesOnlyDB(t, "scheduling TEXT")
	m := migrationByName(t, "node_scheduling_source")
	if err := db.Exec(`INSERT INTO nodes (uuid, scheduling) VALUES ('n1', 'cordoned')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("nodes", "scheduling_by") {
		t.Fatal("scheduling_by is missing after the migration")
	}
	var by string
	if err := db.Raw(`SELECT scheduling_by FROM nodes WHERE uuid = 'n1'`).Scan(&by).Error; err != nil || by != "" {
		t.Fatalf("existing cordon by = %q, %v; want empty (author unknown)", by, err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}

	t.Run("rollback", func(t *testing.T) {
		failing := nodesOnlyDB(t, "scheduling TEXT")
		if err := failing.Exec(fmt.Sprintf(`CREATE TRIGGER fail_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
			t.Fatal(err)
		}
		if err := migrateDB(failing, []migration{m}); err == nil {
			t.Fatal("injected marker failure did not fail the migration")
		}
		if failing.Migrator().HasColumn("nodes", "scheduling_by") {
			t.Fatal("the column survived a failed migration")
		}
	})
}
