package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// prefillOneDecodeAcrossNodes: one prefill on n1 GPU 0 and one decode unit
// of TP 2 whose leader holds n1 GPU 1 and whose follower holds n2 GPU 0.
func prefillOneDecodeAcrossNodes(n1, n2 string) string {
	return fmt.Sprintf(`{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":%q,"gpus":[0]}]},"decode":{"tp":2,"units":[{"node_uuid":%q,"gpus":[1],"followers":[{"node_uuid":%q}]}]}}}}`, n1, n1, n2)
}

func pdByRole(insts []store.Instance) (prefill, leader, follower *store.Instance) {
	for i := range insts {
		inst := &insts[i]
		switch {
		case inst.Role == pdmode.RolePrefill:
			prefill = inst
		case inst.Role == pdmode.RoleDecode && inst.Headless:
			follower = inst
		case inst.Role == pdmode.RoleDecode:
			leader = inst
		}
	}
	return
}

// A generation is launched whole: the leader and its follower in one pass.
func TestPDMultiNodeUnitLaunchesLeaderAndFollowerTogether(t *testing.T) {
	f := newPDFixture(t, "pd_group_launch", prefillOneDecodeAcrossNodes)
	insts := f.reconcile(t)
	prefill, leader, follower := pdByRole(insts)
	if prefill == nil || leader == nil || follower == nil {
		t.Fatalf("instances after one pass = %+v", insts)
	}
	record := planning.ReadScheduleRecord(leader.ScheduleExpl).PDRecord()
	if record.MasterPort == 0 || record.Address == "" || planning.ReadScheduleRecord(leader.ScheduleExpl).UnitRecord().Followers == "" {
		t.Fatalf("leader record = %+v", record)
	}
	if follower.DistAttempt != 1 || leader.DistAttempt != 1 {
		t.Fatalf("attempts: leader %d follower %d", leader.DistAttempt, follower.DistAttempt)
	}
	if !strings.Contains(leader.Ports, fmt.Sprint(record.MasterPort)) {
		t.Fatalf("leader does not hold its collective port: %s", leader.Ports)
	}
	if leader.DistGroup == "" || follower.DistGroup != leader.DistGroup || follower.NodeRank != 1 || leader.NodeRank != 0 || follower.NNodes != 2 {
		t.Fatalf("group: leader %s/%d follower %s/%d/%d", leader.DistGroup, leader.NodeRank, follower.DistGroup, follower.NodeRank, follower.NNodes)
	}
	if follower.NodeID != f.nodes["n2"].ID || follower.APIPort != 0 || leader.NodeID != f.nodes["n1"].ID || leader.GPUIndexes != "[1]" {
		t.Fatalf("placement: leader node %d gpus %s, follower node %d api %d", leader.NodeID, leader.GPUIndexes, follower.NodeID, follower.APIPort)
	}
	leaderSpec, followerSpec := desiredOf(t, *leader), desiredOf(t, *follower)
	for name, spec := range map[string]proto.InstanceDesiredState{"leader": leaderSpec, "follower": followerSpec} {
		if argAfter(spec.Argv, "--nnodes") != "2" || argAfter(spec.Argv, "--master-addr") != record.Address || argAfter(spec.Argv, "--master-port") != fmt.Sprint(record.MasterPort) {
			t.Fatalf("%s argv = %v", name, spec.Argv)
		}
		if argAfter(spec.Argv, "--tensor-parallel-size") != "2" || !strings.Contains(argAfter(spec.Argv, "--kv-transfer-config"), "kv_consumer") {
			t.Fatalf("%s parallel/connector argv = %v", name, spec.Argv)
		}
		if spec.Env["VLLM_NIXL_SIDE_CHANNEL_PORT"] == "" {
			t.Fatalf("%s has no side channel: %v", name, spec.Env)
		}
	}
	if argAfter(followerSpec.Argv, "--node-rank") != "1" || !followerSpec.Headless || followerSpec.Env["VLLM_NIXL_SIDE_CHANNEL_HOST"] != "10.0.0.2" {
		t.Fatalf("follower spec: rank %s headless %v env %v", argAfter(followerSpec.Argv, "--node-rank"), followerSpec.Headless, followerSpec.Env)
	}
	// A whole unit is one instance to the status and a pass with every rank
	// present creates nothing.
	for _, uuid := range []string{prefill.UUID, leader.UUID, follower.UUID} {
		if err := f.st.SetInstanceState(uuid, proto.InstRunning, ""); err != nil {
			t.Fatal(err)
		}
	}
	insts = f.reconcile(t)
	if len(insts) != 3 {
		t.Fatalf("steady pass instances = %d", len(insts))
	}
	pd, _ := planning.PDOf(f.dep.Placement)
	status := planning.PDStatusOf(pd, insts)
	if status.State != planning.PDStateRunning || status.Roles[0].Ready != 1 || status.Short {
		t.Fatalf("status = %+v", status)
	}
}

func TestPDMultiNodeFollowerFailureRebuildsOnlyItsUnit(t *testing.T) {
	f := newPDFixture(t, "pd_group_fail", prefillOneDecodeAcrossNodes)
	f.ctrl.SetRetryPolicy(0, 3)
	f.reconcile(t)
	insts := f.reconcile(t)
	prefill, leader, follower := pdByRole(insts)
	if follower == nil {
		t.Fatalf("instances = %+v", insts)
	}
	for _, uuid := range []string{prefill.UUID, leader.UUID} {
		if err := f.st.SetInstanceState(uuid, proto.InstRunning, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.SetInstanceState(follower.UUID, proto.InstFailed, "NCCL timeout"); err != nil {
		t.Fatal(err)
	}
	insts = f.reconcile(t)
	byUUID := map[string]store.Instance{}
	for _, inst := range insts {
		byUUID[inst.UUID] = inst
	}
	if got := byUUID[leader.UUID]; got.State != proto.InstFailed || !strings.Contains(got.Message, "rank 1") || !strings.Contains(got.Message, "NCCL timeout") {
		t.Fatalf("leader = %s %q", got.State, got.Message)
	}
	if got := byUUID[follower.UUID]; got.State != proto.InstRemoving {
		t.Fatalf("follower = %s", got.State)
	}
	if got := byUUID[prefill.UUID]; got.State != proto.InstRunning {
		t.Fatalf("prefill was touched: %s", got.State)
	}
	removed := false
	for _, update := range f.hub.updates[f.nodes["n2"].UUID] {
		for _, uuid := range update.RemovedInstances {
			removed = removed || uuid == follower.UUID
		}
	}
	if !removed {
		t.Fatal("the failed follower's container was not removed")
	}
	// The agent confirms the follower gone; the next pass rebuilds the unit
	// as a new generation.
	if err := f.st.SetInstanceState(follower.UUID, proto.InstStopped, ""); err != nil {
		t.Fatal(err)
	}
	insts = f.reconcile(t)
	var rebuilt *store.Instance
	for i := range insts {
		inst := &insts[i]
		if inst.Role == pdmode.RoleDecode && !inst.Headless && inst.UUID != leader.UUID {
			rebuilt = inst
		}
		if inst.UUID == follower.UUID {
			t.Fatal("the stopped follower row was kept")
		}
	}
	if rebuilt == nil || rebuilt.DistGroup == leader.DistGroup || rebuilt.DistAttempt != 2 {
		t.Fatalf("rebuilt leader = %+v", rebuilt)
	}
}

// prePullHub is the test hub plus the hub's image pre-pull: it records the
// request and marks the node's image row pulling, as hub.PullImage does.
type prePullHub struct {
	*distributedTestHub
	st    *store.Store
	pulls []string
}

func (h *prePullHub) PullImage(_ context.Context, imageID uint, nodeUUID string, _ imagecatalog.FetchRequest) (*store.ImageNode, error) {
	node, err := h.st.NodeByUUID(nodeUUID)
	if err != nil {
		return nil, err
	}
	h.pulls = append(h.pulls, nodeUUID)
	return h.st.UpsertImageNode(&store.ImageNode{ImageVersionID: imageID, ImageID: imageID, NodeID: node.ID, State: store.ImageStatePulling})
}

func TestPDMultiNodeLeaderWaitsForTheImageOnEveryRank(t *testing.T) {
	f := newPDFixture(t, "pd_group_barrier", prefillOneDecodeAcrossNodes)
	hub := &prePullHub{distributedTestHub: f.hub, st: f.st}
	f.ctrl.hub = hub
	image, err := f.st.ResolveImageRef("vllm/vllm-openai:v0.25.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, ImageID: image.ID, NodeID: f.nodes["n1"].ID, State: store.ImageStatePresent}); err != nil {
		t.Fatal(err)
	}
	insts := f.reconcile(t)
	prefill, leader, _ := pdByRole(insts)
	if prefill == nil || leader != nil {
		t.Fatalf("the single-node prefill starts, the decode leader waits: %+v", insts)
	}
	if len(hub.pulls) != 1 || hub.pulls[0] != f.nodes["n2"].UUID {
		t.Fatalf("pre-pulls = %v, want the follower's node", hub.pulls)
	}
	// A second pass while the pull runs does not start the leader (asking
	// again is harmless: the hub returns the pull already under way).
	if _, leader, _ := pdByRole(f.reconcile(t)); leader != nil {
		t.Fatal("the leader started while the follower's image was still pulling")
	}
	if _, err := f.st.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, ImageID: image.ID, NodeID: f.nodes["n2"].ID, State: store.ImageStatePresent}); err != nil {
		t.Fatal(err)
	}
	if _, leader, _ := pdByRole(f.reconcile(t)); leader == nil {
		t.Fatal("the leader did not start once every rank had the image")
	}
}

// A PD multi-node unit is placed as a whole too: while its follower's card is
// unusable, the decode leader is not written beside the prefill, so it holds
// no card on n1; once the card is back the unit is written in one pass.
func TestPDMultiNodeUnitIsWrittenWholeOrNotAtAll(t *testing.T) {
	f := newPDFixture(t, "pd_group_gang", prefillOneDecodeAcrossNodes)
	if err := f.st.DB().Model(&store.GPU{}).Where("node_id = ?", f.nodes["n2"].ID).Update("health_status", accelerator.HealthError).Error; err != nil {
		t.Fatal(err)
	}
	insts := f.reconcile(t)
	_, leader, follower := pdByRole(insts)
	if leader != nil || follower != nil {
		t.Fatalf("a decode unit whose follower does not fit was written: leader %+v follower %+v", leader, follower)
	}
	event, found, err := f.st.LatestScheduleEvent(f.dep.ID)
	if err != nil || !found || !strings.Contains(event.Data, `"node":"`+f.nodes["n2"].UUID+`"`) || !strings.Contains(event.Data, `"rank":1`) {
		t.Fatalf("refusal does not name the follower: %+v found=%v err=%v", event, found, err)
	}
	if err := f.st.DB().Model(&store.GPU{}).Where("node_id = ?", f.nodes["n2"].ID).Update("health_status", accelerator.HealthOK).Error; err != nil {
		t.Fatal(err)
	}
	insts = f.reconcile(t)
	prefill, leader, follower := pdByRole(insts)
	if prefill == nil || leader == nil || follower == nil || leader.DistGroup == "" || follower.DistGroup != leader.DistGroup {
		t.Fatalf("instances after the card came back = %+v", insts)
	}
}
