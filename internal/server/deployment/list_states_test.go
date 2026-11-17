package deployment

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A snapshot works every deployment's state out once, for every reader: the
// dashboard counts, the list's state filter and order, the monitor and the
// batches. The states are those each deployment's own derivation gives.
func TestListRowsStatesAreWorkedOutOncePerSnapshot(t *testing.T) {
	f := newListsFixture(t)
	if _, err := f.st.CreateDeployment(&store.Deployment{Name: "stopped", ModelID: f.modelID, NodeID: f.node.ID, Replicas: 1, State: store.DeployStopped}); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.rows(t)
	states := rows.States()
	if again := rows.States(); reflect.ValueOf(again).Pointer() != reflect.ValueOf(states).Pointer() {
		t.Fatal("the states were worked out again for the same snapshot")
	}
	for _, d := range rows.Deployments() {
		insts := rows.Instances(d.ID)
		want := EffectiveState(&d, RuntimeState(rows, &d, insts, RecordedModelProgress(rows, &d)), FactsFor(rows, &d, insts))
		if states[d.ID] != want {
			t.Errorf("%s: snapshot state %q, its own derivation %q", d.Name, states[d.ID], want)
		}
	}
	if states[f.dep.ID] != "pulling" {
		t.Fatalf("pulling deployment reads %q", states[f.dep.ID])
	}
	// A later snapshot, after a change, works them out afresh.
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: f.instance, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	next, rebuilt := f.rows(t)
	if !rebuilt || next.States()[f.dep.ID] != "running" {
		t.Fatalf("after the instance ran: rebuilt %v, state %q", rebuilt, next.States()[f.dep.ID])
	}
}

// The one state that changes with no change announced is a deployment the
// scheduler has refused past the pending timeout: pending becomes failed when
// the timeout passes. The snapshot's states are worked out again then, the
// rows are not read again.
func TestListRowsStatesFollowThePendingTimeout(t *testing.T) {
	f := newListsFixture(t)
	if err := f.st.SetSetting(store.SettingDeploymentPolicy, `{"pending_timeout_sec":5}`); err != nil {
		t.Fatal(err)
	}
	// A model with no file anywhere: nothing to download, so a refusal past
	// the timeout is a stall.
	model, err := f.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/refused", Revision: "main", Name: "org/refused"})
	if err != nil {
		t.Fatal(err)
	}
	refused, err := f.st.CreateDeployment(&store.Deployment{Name: "refused", ModelID: model.ID, NodeID: f.node.ID, Replicas: 1, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	f.st.AddDeploymentEventCode(refused.ID, store.EventKindSchedule, "DEPLOYMENT_UNSCHEDULABLE", map[string]any{"reason": "no_gpu"})
	// The refusal was 300 ms short of the timeout, on the lists' clock.
	if err := f.st.DB().Model(&store.InstanceEvent{}).Where("deployment_id = ?", refused.ID).
		Update("created_at", f.now.Add(-5*time.Second+300*time.Millisecond)).Error; err != nil {
		t.Fatal(err)
	}
	rows, _ := f.rows(t)
	if got := rows.States()[refused.ID]; got != "pending" {
		t.Fatalf("before the timeout: %q, want pending", got)
	}
	// One instant short of the deadline the states still hold.
	f.now = f.now.Add(300*time.Millisecond - time.Nanosecond)
	if got := rows.States()[refused.ID]; got != "pending" {
		t.Fatalf("just before the timeout: %q, want pending", got)
	}
	f.now = f.now.Add(time.Nanosecond)
	again, rebuilt := f.rows(t)
	if rebuilt {
		t.Fatal("the timeout passing rebuilt the rows")
	}
	if got := again.States()[refused.ID]; got != "failed" {
		t.Fatalf("after the timeout: %q, want failed", got)
	}
}

// placementPreparation is the controller's model preparation as the lists
// see it: the nodes a pass chose for a deployment's instances to come, held
// in memory and changed by no write a list could see, with the generation
// the controller counts their changes by.
type placementPreparation struct {
	mu    sync.Mutex
	nodes map[uint][]uint
	gen   uint64
}

func (p *placementPreparation) place(depID uint, nodes ...uint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodes[depID] = nodes
	p.gen++
}

func (p *placementPreparation) ModelCopyObservation(*store.ModelFile) (proto.ModelFileStatus, bool) {
	return proto.ModelFileStatus{}, false
}

func (p *placementPreparation) CachePhase(store.FileRef) string { return "" }

func (p *placementPreparation) PreparationNodes(dep *store.Deployment) []uint {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.nodes[dep.ID])
}

func (p *placementPreparation) PreparationGeneration() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen
}

func (p *placementPreparation) RetryCacheCopy(context.Context, store.FileRef) error { return nil }

// A deployment with no instance reads its state from the model files on the
// nodes the controller chose for it, a choice held in memory that no write
// announces. When the controller places it on a node whose model needs a
// download it cannot start, every reader of the snapshot's states - the
// state filter, the dashboard, the monitor, the batches - reads failed at
// once, as the row the page shows does, not pending until the snapshot ages
// out.
func TestListRowsStatesFollowTheControllersPlacement(t *testing.T) {
	f := newListsFixture(t)
	model, err := f.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/placed", Revision: "main", Name: "org/placed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: f.node.ID, State: proto.MFDownloadRequired, Path: "/placed"}); err != nil {
		t.Fatal(err)
	}
	// Not pinned: only the controller's choice puts it on a node.
	dep, err := f.st.CreateDeployment(&store.Deployment{Name: "placed", ModelID: model.ID, Replicas: 1, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	prep := &placementPreparation{nodes: map[uint][]uint{}}
	f.lists = NewLists(f.st, prep, func() time.Time { return f.now })
	live := func(rows *ListRows) string {
		t.Helper()
		page, err := rows.Fresh([]uint{dep.ID})
		if err != nil {
			t.Fatal(err)
		}
		d := page.Deployment(dep.ID)
		insts := page.Instances(dep.ID)
		return EffectiveState(d, RuntimeState(page, d, insts, RecordedModelProgress(page, d)), FactsFor(page, d, insts))
	}
	rows, _ := f.rows(t)
	if got, want := rows.States()[dep.ID], live(rows); got != "pending" || want != "pending" {
		t.Fatalf("before a placement: snapshot %q, page %q, want pending", got, want)
	}
	// The controller chooses the node; nothing is written.
	prep.place(dep.ID, f.node.ID)
	rows, _ = f.rows(t)
	if want := live(rows); want != "failed" {
		t.Fatalf("the page reads %q once the controller chose a node whose model needs a download, want failed", want)
	}
	if got := rows.States()[dep.ID]; got != "failed" {
		t.Fatalf("the snapshot's state reads %q after the controller's placement, the page failed", got)
	}
	// With the choice unchanged the snapshot is kept.
	if _, rebuilt := f.rows(t); rebuilt {
		t.Fatal("an unchanged placement rebuilt the snapshot")
	}
}
