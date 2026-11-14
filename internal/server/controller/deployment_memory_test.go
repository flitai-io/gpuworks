package controller

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/placewait"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// waitedImage is the image row the tests' deployments wait for.
const waitedImage uint = 11

// What the controller keeps by deployment is dropped in two steps: what
// concerns running units once the deployment stops, and what still holds when
// it starts again (node replacement's moves, a placement hint) once it is
// gone. Each is written the way the controller writes it.
func TestForgetDeploymentLeavesNoMemory(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("memory-node", store.HashToken("token"), "memory-node")
	if err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	var deps []*store.Deployment
	for _, name := range []string{"forgotten", "kept"} {
		dep, err := st.CreateDeployment(&store.Deployment{Name: name, NodeID: node.ID, State: store.DeployActive})
		if err != nil {
			t.Fatal(err)
		}
		deps = append(deps, dep)
		t.Cleanup(func() { planning.HintPlacement(dep.ID, nil) })
		id := dep.ID
		c.rememberPoolRefusal(id, node.ID)
		c.rememberPoolWait(id)
		c.waits.Wait(id, placewait.Cards{NodeID: node.ID, GPUIndexes: []int{0}}, []string{"holder"}, now)
		c.placements.set(&store.Deployment{ID: id, UpdatedAt: now}, []uint{node.ID})
		c.recordSleepPushed(id, proto.InstanceDesiredState{UUID: "inst"})
		c.drainedUnits(id, []store.Instance{{UUID: "inst"}})
		c.markDGPULimitPush(id)
		c.imageWaits.set(id, waitedImage)
		c.restoreFailovers(dep, &unitDeployment{}, nil)
		c.failover.set(id, []failoverMove{{Unit: 1, From: "a", To: "b", MovedAt: now}})
		c.failover.setSurges(id, []drainSurge{{}})
		c.recordNoCandidate(dep, map[string]bool{"lost-node": true}, &unitDeployment{})
		c.autoRepickFailed(id)
		planning.HintPlacement(id, []string{"node-a"})
	}
	dep, other := deps[0].ID, deps[1].ID
	keptWhileStopped := map[string]bool{"node replacement moves": true, "restored moves marker": true, "placement hint": true}

	c.forgetDeployment(dep, false)
	for what, held := range memoryOf(c, dep) {
		if held != keptWhileStopped[what] {
			t.Errorf("stopped deployment %d: holds %s = %v, want %v", dep, what, held, keptWhileStopped[what])
		}
	}

	c.forgetDeployment(dep, true)
	for what, held := range memoryOf(c, dep) {
		if held {
			t.Errorf("deployment %d still has %s after it was gone", dep, what)
		}
	}
	// Forgetting one deployment leaves the others alone.
	for what, held := range memoryOf(c, other) {
		if !held {
			t.Errorf("forgetting deployment %d also dropped deployment %d's %s", dep, other, what)
		}
	}
}

// A deployment deleted while it waited for an image copy is not queued
// again by the copy's later events.
func TestAForgottenDeploymentIsNotQueuedByItsImage(t *testing.T) {
	const dep, image uint = 7, waitedImage
	c := New(nil, nil, nil)
	c.imageWaits.set(dep, image)

	c.forgetDeployment(dep, true)
	c.onEvent(bus.Event{Topic: "image", Key: strconv.FormatUint(uint64(image), 10), Reason: "node"})

	c.q.mu.Lock()
	_, queued := c.q.pending[depKey(dep)]
	c.q.mu.Unlock()
	if queued {
		t.Fatalf("deployment %d was queued by image %d after it was forgotten", dep, image)
	}
}

// memoryOf reports, for each piece of per-deployment memory, whether the
// controller holds any for the deployment.
func memoryOf(c *Controller, dep uint) map[string]bool {
	c.poolMu.Lock()
	_, refused := c.poolRefused[dep]
	_, poolWaiting := c.poolWaiting[dep]
	c.poolMu.Unlock()
	_, cardWait := c.waits.Get(dep, c.now())
	c.sleepMu.Lock()
	_, sleepPushed := c.sleepPushed[dep]
	c.sleepMu.Unlock()
	c.drainMu.Lock()
	_, draining := c.draining[dep]
	c.drainMu.Unlock()
	c.dgpuMu.Lock()
	dgpu := c.dgpuPending[dep]
	c.dgpuMu.Unlock()
	imageWait := false
	for _, d := range c.imageWaits.waiting(waitedImage) {
		imageWait = imageWait || d == dep
	}
	c.failover.mu.Lock()
	_, moves := c.failover.moves[dep]
	_, surges := c.failover.drains[dep]
	_, noneSince := c.failover.noneSince[dep]
	_, repicked := c.failover.repicked[dep]
	_, restored := c.failover.restored[dep]
	c.failover.mu.Unlock()
	_, hint := planning.NextPlacementHint(dep)
	c.placements.mu.Lock()
	_, placement := c.placements.nodes[dep]
	c.placements.mu.Unlock()
	return map[string]bool{
		"pool refusals":            refused,
		"pool wait":                poolWaiting,
		"card wait":                cardWait,
		"preparation placement":    placement,
		"sleep pushes":             sleepPushed,
		"drain clocks":             draining,
		"pending dGPU limit push":  dgpu,
		"image wait":               imageWait,
		"node replacement moves":   moves,
		"drain surges":             surges,
		"no-candidate report time": noneSince,
		"automatic re-pick time":   repicked,
		"restored moves marker":    restored,
		"placement hint":           hint,
	}
}

// A placement hint given while a deployment is stopped (the replicas API
// takes place_on then) survives the passes over the stopped deployment and
// goes with the deployment.
func TestAStoppedDeploymentKeepsItsPlacementHint(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("hint-node", store.HashToken("token"), "hint-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "hinted", NodeID: node.ID, State: store.DeployStopped})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { planning.HintPlacement(dep.ID, nil) })
	c := New(st, nil, nil)
	planning.HintPlacement(dep.ID, []string{node.UUID})

	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if got, ok := planning.NextPlacementHint(dep.ID); !ok || got != node.UUID {
		t.Fatalf("after a pass over the stopped deployment the hint is %q, %v; want %q", got, ok, node.UUID)
	}

	if err := st.DeleteDeployment(dep.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if got, ok := planning.NextPlacementHint(dep.ID); ok {
		t.Fatalf("the deleted deployment kept its hint %q", got)
	}
}

// A deployment being deleted keeps its hint until its row is removed, and
// the pass that removes the row drops the hint.
func TestADeletedDeploymentDropsItsPlacementHintWithItsRow(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("deleting-hint-node", store.HashToken("token"), "deleting-hint-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "deleting", NodeID: node.ID, State: store.DeployDeleting})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { planning.HintPlacement(dep.ID, nil) })
	c := New(st, nil, nil)
	planning.HintPlacement(dep.ID, []string{node.UUID})

	// No instances are left to tear down: this pass removes the row.
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDeployment(dep.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the deployment row is still there: %v", err)
	}
	if got, ok := planning.NextPlacementHint(dep.ID); ok {
		t.Fatalf("the deleted deployment kept its hint %q", got)
	}
}
