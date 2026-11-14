package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var wholeFleetRead = regexp.MustCompile("^SELECT \\* FROM [`\"]?nodes[`\"]?( ORDER BY [^ ]+( desc| asc)?)?$")

func countFleetReads(t *testing.T, st *store.Store) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	name := "test:controller_fleet_reads:" + t.Name()
	if err := st.DB().Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if wholeFleetRead.MatchString(strings.TrimSpace(tx.Statement.SQL.String())) {
			n.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DB().Callback().Query().Remove(name) })
	return &n
}

// With the server's node snapshot on, a round of reconciles - pool replicas
// placed, a replica's node lost and replaced - reads the fleet once, not once
// per deployment or per choice; the placements are the ones it makes reading
// the fleet each time.
func TestReconcilesShareTheFleetSnapshot(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "ctrl_fleet_reads", 4, 2)
	st.EnableFleetNodeSnapshot(nil)
	c := New(st, hub, nil)
	c.SetRetryPolicy(0, 3)
	var deps []*store.Deployment
	for i := range 6 {
		dep, err := createTestDeploymentWithRequirements(t, st, &store.Deployment{
			Name: fmt.Sprintf("%s-%d", model.Name, i), ModelID: model.ID, NodeID: nodes[0].ID, ServedName: fmt.Sprintf("%s-%d", model.Name, i), Engine: "vllm",
			Placement: `{"tp":1}`, QuickParams: `{"max_model_len":512}`, Replicas: 1, PlacementStrategy: store.PlacementSpread,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		deps = append(deps, dep)
	}
	if _, err := st.FleetNodes(); err != nil { // the snapshot is current
		t.Fatal(err)
	}
	reads := countFleetReads(t, st)
	for _, dep := range deps {
		if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
			t.Fatal(err)
		}
		if got := replicaNodes(t, st, dep.ID); len(got) != 1 {
			t.Fatalf("deployment %d placed %v, want one replica", dep.ID, got)
		}
	}
	if n := reads.Load(); n != 0 {
		t.Fatalf("placing 6 pool deployments read the whole fleet %d times, want 0 (the snapshot was current)", n)
	}
	// A node is lost: the replicas on it are replaced on the others.
	insts, err := st.InstancesForDeployment(deps[0].ID)
	if err != nil || len(insts) != 1 {
		t.Fatalf("instances = %+v %v", insts, err)
	}
	var lost string
	for _, n := range nodes {
		if n.ID == insts[0].NodeID {
			lost = n.UUID
		}
	}
	hub.online[lost] = false
	if err := st.SetNodeState(lost, store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", insts[0].UUID).Update("state", proto.InstFailed).Error; err != nil {
		t.Fatal(err)
	}
	reads.Store(0)
	for _, dep := range deps {
		if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := replicaNodes(t, st, deps[0].ID); got[insts[0].NodeID] != 0 {
		t.Fatalf("after %s was lost the replica still runs there: %v", lost, got)
	}
	// The node going offline is a change: one rebuild, then shared.
	if n := reads.Load(); n != 1 {
		t.Fatalf("reconciling 6 deployments after a node was lost read the whole fleet %d times, want 1", n)
	}
}

// A status batch refreshes the snapshot before it enqueues: the deployments it
// enqueues read the reports that enqueued them, as they did reading the fleet
// each time.
func TestStatusBatchReadsItsOwnReports(t *testing.T) {
	st, hub, nodes, _ := poolFixture(t, "ctrl_status_refresh", 1, 1)
	now := time.Date(2026, 11, 14, 11, 0, 0, 0, time.UTC)
	st.EnableFleetNodeSnapshot(func() time.Time { return now })
	c := New(st, hub, nil)
	if _, err := st.FleetNodes(); err != nil {
		t.Fatal(err)
	}
	// The card stops being healthy: a routine report, inside the window.
	if err := st.UpdateNodeStatus(nodes[0].UUID, proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: nodes[0].UUID, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{{Index: 0, UUID: nodes[0].UUID + "-gpu-0", Name: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 80 << 30, Healthy: false}},
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	c.nodeStatusReported()
	c.settleNodeStatus()
	got, err := st.FleetNodes()
	if err != nil {
		t.Fatal(err)
	}
	if got[0].GPUs[0].Healthy() {
		t.Fatal("after the status batch the snapshot still shows the card healthy: the batch's deployments would plan on it")
	}
}

// An operator's choice - choosing nodes for a scale, a suggestion, a create
// - answers on the report the agent just sent, inside the snapshot's report
// window too: what the operator sees is what the node said last.
func TestOperatorChoicesReadTheNodesAsTheyAreNow(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "ctrl_choice_now", 1, 1)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	st.EnableFleetNodeSnapshot(func() time.Time { return now })
	if _, err := st.FleetNodes(); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(nodes[0].UUID, proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: nodes[0].UUID, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{{Index: 0, UUID: nodes[0].UUID + "-gpu-0", Name: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 80 << 30, Healthy: false}},
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	dep := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil)
	facts, err := planning.LoadUnitSuggestFacts(st, func([]store.Node) (netplan.Resolution, error) { return netplan.Resolution{}, nil }, dep, nil, nil, hub.Online, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Nodes) != 1 || facts.Nodes[0].GPUs[0].Healthy() {
		t.Fatalf("an operator's choice planned on the card as it was before the report: %+v", facts.Nodes)
	}
}
