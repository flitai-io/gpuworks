package deployment

import (
	"reflect"
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
	// The refusal was 300 ms short of the timeout.
	if err := f.st.DB().Model(&store.InstanceEvent{}).Where("deployment_id = ?", refused.ID).
		Update("created_at", time.Now().Add(-5*time.Second+300*time.Millisecond)).Error; err != nil {
		t.Fatal(err)
	}
	rows, _ := f.rows(t)
	if got := rows.States()[refused.ID]; got != "pending" {
		t.Fatalf("before the timeout: %q, want pending", got)
	}
	time.Sleep(400 * time.Millisecond)
	again, rebuilt := f.rows(t)
	if rebuilt {
		t.Fatal("the timeout passing rebuilt the rows")
	}
	if got := again.States()[refused.ID]; got != "failed" {
		t.Fatalf("after the timeout: %q, want failed", got)
	}
}
