package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Routing over several replicas: around the ones ordered to sleep, and among
// the multi-node generations that can answer.

// An order for one replica's instance takes it out of routing at once while
// the other replica keeps serving; with both ordered the model is asleep. A
// wake is routable only once the engine reports it awake.
func TestSelectInstanceRoutesAroundAnInstanceOrderedToSleep(t *testing.T) {
	st, err := store.Open("file:proxy_instance_sleep?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model-replicas", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("replica-sleep-node", store.HashToken("node"), "replica-sleep-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "replicated", Engine: "vllm", State: "active", Sleepable: true, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"replica-a", "replica-b"} {
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true, APIPort: 30011 + i})
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30011 + i, DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
	}
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"replica-sleep-node": true}}, nil, 0)
	t.Cleanup(p.Close)
	if err := st.OrderInstancesSleep(dep.ID, []string{"replica-a"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		_, tgt, status, msg, _ := p.selectDeploymentInstance("replicated")
		if status != 0 || tgt.instanceUUID != "replica-b" {
			t.Fatalf("with replica-a ordered to sleep selection = %d %q -> %q, want replica-b", status, msg, tgt.instanceUUID)
		}
		p.releaseTarget(tgt)
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{"replica-b"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	if _, _, status, msg, asleep := p.selectDeploymentInstance("replicated"); status != http.StatusServiceUnavailable || !asleep {
		t.Fatalf("every replica ordered to sleep = %d %q asleep=%v, want 503 asleep", status, msg, asleep)
	}
	// replica-a slept; waking it is routable once it reports awake.
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "replica-a", State: proto.InstRunning, SleepLevel: 1, Phase: proto.PhaseSleep}); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{"replica-a"}, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, status, _, asleep := p.selectDeploymentInstance("replicated"); status != http.StatusServiceUnavailable || !asleep {
		t.Fatalf("a wake not yet reported was routed: %d asleep=%v", status, asleep)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "replica-a", State: proto.InstRunning, SleepLevel: 0, Phase: proto.PhaseSleep}); err != nil {
		t.Fatal(err)
	}
	_, tgt, status, msg, _ := p.selectDeploymentInstance("replicated")
	if status != 0 || tgt.instanceUUID != "replica-a" {
		t.Fatalf("woken replica selection = %d %q -> %q, want replica-a", status, msg, tgt.instanceUUID)
	}
	p.releaseTarget(tgt)
}

// A route read before an order still leaves the ordered instance out: the
// pick reads the chosen instance's intent, whatever the policy.
func TestPickAwakePassesOverAnInstanceOrderedSinceTheRouteWasRead(t *testing.T) {
	st, err := store.Open("file:proxy_pick_awake?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("pick-awake-node", store.HashToken("node"), "pick-awake-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: 1, NodeID: node.ID, ServedName: "pick-awake", Engine: "vllm", State: "active", Sleepable: true, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	var candidates []candidate
	for i, uuid := range []string{"awake-a", "awake-b"} {
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true})
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30021 + i, DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, candidate{instanceUUID: uuid, nodeUUID: "pick-awake-node", port: 30021 + i, uuidHash: fnv64([]byte(uuid))})
	}
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"pick-awake-node": true}}, nil, 0)
	t.Cleanup(p.Close)
	if err := st.OrderInstancesSleep(dep.ID, []string{"awake-a"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{deploy.LeastRequest, deploy.Failover, deploy.PrefixAffinity} {
		for i := 0; i < 16; i++ {
			l := p.pickAwake(candidates, policy, fmt.Sprintf("key-%d", i), false)
			if l == nil || l.c.instanceUUID != "awake-b" {
				t.Fatalf("%s pick %d = %+v, want awake-b", policy, i, l)
			}
			l.release(outcomeCanceled)
		}
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{"awake-b"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	if l := p.pickAwake(candidates, deploy.LeastRequest, "", false); l != nil {
		t.Fatalf("every instance ordered to sleep, pick = %+v", l.c)
	}
}

// A deployment listing two multi-node replicas routes to either leader whose
// generation is complete, and only to those: a replica with a follower not
// running takes nothing while the other serves.
func TestDistributedSelectionPicksAmongCompleteReplicas(t *testing.T) {
	st, err := store.Open("file:proxy_distributed_replicas_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-72B-replicas", Revision: "main", Name: "Qwen72B"})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*store.Node
	online := map[string]bool{}
	for _, name := range []string{"r-a", "r-b", "r-c", "r-d"} {
		node, _ := st.RegisterNode(name, store.HashToken(name), name)
		nodes = append(nodes, node)
		online[name] = true
	}
	nodeIDs, _ := json.Marshal([]uint{nodes[0].ID, nodes[1].ID, nodes[2].ID, nodes[3].ID})
	dep, err := st.CreateDeployment(&store.Deployment{
		ModelID: model.ID, NodeID: nodes[0].ID, NodeIDs: string(nodeIDs), ServedName: "qwen-replicas",
		Engine: "vllm", State: "active", NNodes: 2, Replicas: 2,
		Placement: `{"units":[{"node_uuid":"r-a","followers":[{"node_uuid":"r-b"}]},{"node_uuid":"r-c","followers":[{"node_uuid":"r-d"}]}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	group := func(prefix string, leaderNode, followerNode *store.Node, followerState string) {
		t.Helper()
		if _, err := st.CreateInstance(&store.Instance{UUID: prefix + "-leader", DeploymentID: dep.ID, NodeID: leaderNode.ID, State: proto.InstRunning,
			APIPort: 30000, Ports: "[30000]", DistGroup: prefix, DistRole: "leader", NNodes: 2, DistAttempt: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInstance(&store.Instance{UUID: prefix + "-follower", DeploymentID: dep.ID, NodeID: followerNode.ID, State: followerState,
			DistGroup: prefix, DistRole: "follower", NNodes: 2, NodeRank: 1, Headless: true, DistAttempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// Both replicas launched at the same attempt: only the group tells their
	// ranks apart.
	group("g1", nodes[0], nodes[1], proto.InstStarting)
	group("g2", nodes[2], nodes[3], proto.InstRunning)
	p := New(st, storeNetworks(st), readinessTestHub{online: online}, nil, 0)
	t.Cleanup(p.Close)
	for i := 0; i < 4; i++ {
		_, tgt, status, message, _ := p.selectDeploymentInstance("qwen-replicas")
		if status != 0 || tgt.instanceUUID != "g2-leader" {
			t.Fatalf("with g1 incomplete: target=%+v status=%d message=%q", tgt, status, message)
		}
		tgt.lease.release(outcomeOK)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "g1-follower", State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var held []*lease
	for i := 0; i < 4; i++ {
		_, tgt, status, message, _ := p.selectDeploymentInstance("qwen-replicas")
		if status != 0 {
			t.Fatalf("both complete: status=%d message=%q", status, message)
		}
		seen[tgt.instanceUUID] = true
		held = append(held, tgt.lease)
	}
	for _, l := range held {
		l.release(outcomeOK)
	}
	if !seen["g1-leader"] || !seen["g2-leader"] || len(seen) != 2 {
		t.Fatalf("least-request routing over two complete replicas reached %v", seen)
	}
	// g2's follower node drops off: its row still reads running, but the
	// generation cannot answer, so everything goes to g1.
	online["r-d"] = false
	for i := 0; i < 4; i++ {
		_, tgt, status, message, _ := p.selectDeploymentInstance("qwen-replicas")
		if status != 0 || tgt.instanceUUID != "g1-leader" {
			t.Fatalf("with g2's follower node offline: target=%+v status=%d message=%q", tgt, status, message)
		}
		tgt.lease.release(outcomeOK)
	}
}

// A multi-node replica sleeps with its leader's engine, so the leader alone
// says whether it is asleep: a follower whose intent still reads asleep (say
// a row from before a failed sleep was rolled back) does not take an awake
// leader out of routing.
func TestAMultiNodeReplicaIsAsleepOnlyWhenItsLeaderIs(t *testing.T) {
	st, err := store.Open("file:proxy_leader_says_asleep?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/leader-asleep", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	nodeA, _ := st.RegisterNode("leader-asleep-a", store.HashToken("a"), "leader-asleep-a")
	nodeB, _ := st.RegisterNode("leader-asleep-b", store.HashToken("b"), "leader-asleep-b")
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: nodeA.ID, NodeIDs: fmt.Sprintf("[%d,%d]", nodeA.ID, nodeB.ID), ServedName: "leader-asleep", Engine: "vllm", State: "active", Sleepable: true, NNodes: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []store.Instance{
		{UUID: "la-leader", NodeID: nodeA.ID, APIPort: 30031, DistRole: "leader"},
		{UUID: "la-follower", NodeID: nodeB.ID, Headless: true, NodeRank: 1, DistRole: "follower"},
	} {
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: row.UUID, DesiredState: "running", Sleepable: !row.Headless, APIPort: row.APIPort})
		row.DeploymentID, row.State, row.NNodes, row.DistGroup, row.DistAttempt, row.DesiredStateJSON = dep.ID, proto.InstRunning, 2, "la", 1, string(spec)
		if _, err := st.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
	}
	// Only the follower still reads asleep.
	if err := st.OrderInstancesSleep(dep.ID, []string{"la-follower"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"leader-asleep-a": true, "leader-asleep-b": true}}, nil, 0)
	t.Cleanup(p.Close)
	_, tgt, status, msg, asleep := p.selectDeploymentInstance("leader-asleep")
	if status != 0 || tgt.instanceUUID != "la-leader" || asleep {
		t.Fatalf("selection = %d %q -> %q asleep=%v, want the awake leader", status, msg, tgt.instanceUUID, asleep)
	}
	p.releaseTarget(tgt)
}
