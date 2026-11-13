package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// leaveRig is an entry "a" with a node that runs one instance, a second entry
// "b" (to take its name), and a Leave that stops that node's instance and
// cordons the node.
type leaveRig struct {
	st    *Store
	a, b  *KubeCluster
	node  *Node
	leave KubeLeave
}

func newLeaveRig(t *testing.T) *leaveRig {
	t.Helper()
	st := openKubeStore(t)
	a, err := st.CreateKubeCluster(KubeCluster{Name: "a", NodeSelector: "gpu=a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateKubeCluster(KubeCluster{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := st.EnrollKubeNode(a.Token, "uid", "n1")
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDeployment(&Deployment{Name: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	seedOpsInstance(t, st, d, node, "run", proto.InstRunning, 0, "")
	return &leaveRig{st: st, a: a, b: b, node: node, leave: KubeLeave{StopNodeIDs: []uint{node.ID}, CordonNodeUUIDs: []string{node.UUID}, Message: "node leaves"}}
}

// nothingHappened asserts the instance still runs and the node still takes replicas.
func (r *leaveRig) nothingHappened(t *testing.T) {
	t.Helper()
	if inst, err := r.st.InstanceByUUID("run"); err != nil || inst.State != proto.InstRunning || inst.Message == "node leaves" {
		t.Fatalf("instance = %+v, %v; want untouched", inst, err)
	}
	if n, err := r.st.NodeByUUID(r.node.UUID); err != nil || n.Scheduling != NodeSchedulable {
		t.Fatalf("node = %+v, %v; want schedulable", n, err)
	}
	events, err := r.st.InstanceEventsByUUID("run", 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, %v; want only the creation", events, err)
	}
}

// The stop and the cordon are written with the save, and published after it.
func TestUpdateKubeClusterLeavingStopsAndCordonsWithTheSave(t *testing.T) {
	r := newLeaveRig(t)
	edit := *r.a
	edit.NodeSelector = "gpu=b"
	got, stopped, err := r.st.UpdateKubeClusterLeaving(edit, r.leave)
	if err != nil || got.NodeSelector != "gpu=b" || len(stopped) != 1 || stopped[0].InstanceUUID != "run" || stopped[0].Deployment != "chat" || stopped[0].NodeUUID != r.node.UUID {
		t.Fatalf("= %+v, %+v, %v", got, stopped, err)
	}
	if inst, _ := r.st.InstanceByUUID("run"); inst.State != proto.InstRemoving || inst.Message != "node leaves" {
		t.Fatalf("instance = %s %q", inst.State, inst.Message)
	}
	if n, _ := r.st.NodeByUUID(r.node.UUID); n.Scheduling != NodeCordoned || n.SchedulingBy != SchedulingByOperator {
		t.Fatalf("node = %q by %q", n.Scheduling, n.SchedulingBy)
	}
	if events, _ := r.st.InstanceEventsByUUID("run", 10); len(events) != 2 {
		t.Fatalf("events = %+v; want the stop announced", events)
	}
}

// A save that fails writes neither the stop nor the cordon.
func TestUpdateKubeClusterLeavingThatFailsStopsNothing(t *testing.T) {
	r := newLeaveRig(t)
	taken := *r.a
	taken.Name = "b"
	if _, stopped, err := r.st.UpdateKubeClusterLeaving(taken, r.leave); !errors.Is(err, ErrKubeClusterNameTaken) || len(stopped) != 0 {
		t.Fatalf("name taken: %v, %+v", err, stopped)
	}
	r.nothingHappened(t)
	gone := *r.a
	gone.UUID = "nope"
	if _, _, err := r.st.UpdateKubeClusterLeaving(gone, r.leave); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown entry: %v", err)
	}
	r.nothingHappened(t)
}

// A failure of the stop itself, after the entry was written, rolls the entry back.
func TestLeavingFailureRollsTheSaveBack(t *testing.T) {
	r := newLeaveRig(t)
	boom := errors.New("boom")
	if err := r.st.db.Callback().Update().Before("gorm:update").Register("test:fail-instances", func(db *gorm.DB) {
		if db.Statement.Table == "instances" {
			_ = db.AddError(boom)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.st.db.Callback().Update().Remove("test:fail-instances") })

	edit := *r.a
	edit.NodeSelector = "gpu=b"
	if _, _, err := r.st.UpdateKubeClusterLeaving(edit, r.leave); !errors.Is(err, boom) {
		t.Fatalf("edit: %v", err)
	}
	if got, _ := r.st.KubeClusterByUUID(r.a.UUID); got.NodeSelector != "gpu=a" {
		t.Fatalf("the entry was saved although the stop failed: %q", got.NodeSelector)
	}
	r.nothingHappened(t)

	label := "alibabacloud.com/nodepool-id"
	if _, _, err := r.st.SetKubeClusterPoolsLeaving(r.a.UUID, []KubeClusterPool{{PoolID: "np-1", LabelKey: label}}, r.leave); !errors.Is(err, boom) {
		t.Fatalf("pools: %v", err)
	}
	if got, _ := r.st.KubeClusterPools(r.a.ID); len(got[r.a.ID]) != 0 {
		t.Fatalf("pools were saved although the stop failed: %v", got)
	}
	if got, _ := r.st.KubeClusterByUUID(r.a.UUID); got.NodeSelector != "gpu=a" {
		t.Fatalf("the selector was cleared although the stop failed: %q", got.NodeSelector)
	}
	r.nothingHappened(t)
}

func TestSetKubeClusterPoolsLeavingStopsWithTheSaveAndNothingOtherwise(t *testing.T) {
	r := newLeaveRig(t)
	label := "alibabacloud.com/nodepool-id"
	// Refused before anything is written: unknown entry, mixed labels.
	if _, _, err := r.st.SetKubeClusterPoolsLeaving("nope", []KubeClusterPool{{PoolID: "np-1", LabelKey: label}}, r.leave); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown entry: %v", err)
	}
	if _, _, err := r.st.SetKubeClusterPoolsLeaving(r.a.UUID, []KubeClusterPool{{PoolID: "x", LabelKey: label}, {PoolID: "y", LabelKey: "other"}}, r.leave); !errors.Is(err, ErrKubeClusterPoolLabels) {
		t.Fatalf("mixed labels: %v", err)
	}
	r.nothingHappened(t)

	got, stopped, err := r.st.SetKubeClusterPoolsLeaving(r.a.UUID, []KubeClusterPool{{PoolID: "np-1", LabelKey: label}}, r.leave)
	if err != nil || got.NodeSelector != "" || len(stopped) != 1 || stopped[0].InstanceUUID != "run" {
		t.Fatalf("= %+v, %+v, %v", got, stopped, err)
	}
	if inst, _ := r.st.InstanceByUUID("run"); inst.State != proto.InstRemoving {
		t.Fatalf("instance = %s", inst.State)
	}
	if n, _ := r.st.NodeByUUID(r.node.UUID); n.Scheduling != NodeCordoned {
		t.Fatalf("node = %q", n.Scheduling)
	}
}
