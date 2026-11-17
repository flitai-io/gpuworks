package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type refusalFixture struct {
	st   *store.Store
	ctrl *Controller
	hub  *distributedTestHub
	dep  *store.Deployment
	node *store.Node
}

func newRefusalFixture(t *testing.T, name string, backoff time.Duration, maxAttempts int) *refusalFixture {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode(name+"-node", store.HashToken("token"), name+"-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: name + "-gpu", Name: "NVIDIA A100", Vendor: "nvidia", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "fixture/" + name, Revision: "main", Name: name, WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/" + name}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: name, Engine: "vllm", State: "active", QuickParams: `{"max_model_len":512}`})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.SetRetryPolicy(backoff, maxAttempts)
	f := &refusalFixture{st: st, ctrl: c, hub: hub, dep: dep, node: node}
	f.reconcile(t)
	return f
}

func (f *refusalFixture) reconcile(t *testing.T) {
	t.Helper()
	if err := f.ctrl.reconcileDeployment(context.Background(), f.dep.ID); err != nil {
		t.Fatal(err)
	}
}

// active is the deployment's one instance that has not failed, or nil.
func (f *refusalFixture) active(t *testing.T) *store.Instance {
	t.Helper()
	rows, err := f.st.InstancesForDeployment(f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out *store.Instance
	for i := range rows {
		if rows[i].State != proto.InstFailed {
			if out != nil {
				t.Fatalf("two active instances: %+v", rows)
			}
			out = &rows[i]
		}
	}
	return out
}

// fail reports the active instance failed with message, as its agent does,
// and returns its ports.
func (f *refusalFixture) fail(t *testing.T, message string) []int {
	t.Helper()
	inst := f.active(t)
	if inst == nil {
		t.Fatal("no active instance to fail")
	}
	if err := f.st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstFailed, Message: message}); err != nil {
		t.Fatal(err)
	}
	return parseInts(inst.Ports)
}

func refusal(port int) string {
	return fmt.Sprintf("%s: port %d is already in use on this node; nothing was started", proto.ErrorEnginePortRefused, port)
}

// A launch the agent refused before starting anything is retried at once,
// however long the backoff, on ports none of the refused attempts had; the
// unit gets FreePortRefusals of them, and the one after that waits out the
// backoff like any failure.
func TestARefusedLaunchIsRetriedAtOnceOnOtherPorts(t *testing.T) {
	f := newRefusalFixture(t, "port_refusal_at_once", time.Hour, 3)
	refused := map[int]bool{}
	for i := 0; i < planning.FreePortRefusals; i++ {
		ports := f.fail(t, refusal(30000+i))
		for _, p := range ports {
			refused[p] = true
		}
		f.reconcile(t)
		next := f.active(t)
		if next == nil {
			t.Fatalf("refusal %d: no retry within the hour backoff", i+1)
		}
		for _, p := range parseInts(next.Ports) {
			if refused[p] {
				t.Fatalf("refusal %d: retry was given port %d, which an earlier refused attempt had", i+1, p)
			}
		}
	}
	f.fail(t, refusal(30100))
	f.reconcile(t)
	if next := f.active(t); next != nil {
		t.Fatalf("refusal %d was retried at once; past %d it must wait out the backoff", planning.FreePortRefusals+1, planning.FreePortRefusals)
	}
	f.ctrl.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	f.reconcile(t)
	if f.active(t) == nil {
		t.Fatal("no retry once the backoff passed")
	}
}

// Port refusals do not charge the retry budget until a unit has had
// FreePortRefusals of them; a failure that started something always does,
// and so does a refusal from an agent that still reports the old
// ENGINE_PORT_IN_USE code for it.
func TestPortRefusalsDoNotChargeTheRetryBudget(t *testing.T) {
	f := newRefusalFixture(t, "port_refusal_budget", 0, 3)
	f.fail(t, "image pull timed out")
	f.reconcile(t)
	for i := 0; i < planning.FreePortRefusals; i++ {
		f.fail(t, refusal(30000+i))
		f.reconcile(t)
		if f.active(t) == nil {
			t.Fatalf("refusal %d ended the unit: refusals must not charge the budget", i+1)
		}
	}
	f.fail(t, proto.ErrorEnginePortInUse+": port 30050 is already in use on this node")
	f.reconcile(t)
	if f.active(t) == nil {
		t.Fatal("two charged failures ended a unit with a budget of three")
	}
	f.fail(t, "engine exited")
	f.reconcile(t)
	if inst := f.active(t); inst != nil {
		t.Fatalf("third charged failure still retried: %+v", inst)
	}
	rows, err := f.st.InstancesForDeployment(f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := 3 + planning.FreePortRefusals; len(rows) != want {
		t.Fatalf("failed attempt rows = %d, want every one of the %d kept as evidence", len(rows), want)
	}
}
