import assert from "node:assert/strict";
import test from "node:test";
import type { Deployment, Instance } from "../src/api";
import { deploymentReplicaGroups, replicaLeader } from "../src/pages/replicaGroups";

function instance(uuid: string, fields: Partial<Instance>): Instance {
  return { uuid, ...fields } as Instance;
}

test("an interchangeable unit key and a matching node cannot identify the former replica of a failed attempt", () => {
  const live = instance("live", { replica: { index: 0 }, state: "running", node_uuid: "node-1", schedule: { unit: { key: "|[]" } } });
  const history = instance("old", { state: "failed", schedule: { unit: { key: "|[]" } } });
  const unknown = instance("unknown", { state: "failed", node_uuid: live.node_uuid });
  const groups = deploymentReplicaGroups({ replicas: 2, instances: [live, history, unknown] } as Deployment);
  assert.deepEqual(groups.map(group => group.instances.map(inst => inst.uuid)), [["live"], [], ["old", "unknown"]]);
  assert.equal(replicaLeader(groups[0]), live);
  assert.equal(replicaLeader(groups[2]), undefined);
});

test("a follower inherits only its own generation’s assigned replica and PD role", () => {
  const rank = { dist_group: "placement-a", nnodes: 2, role: "decode", state: "failed" };
  const leader = (attempt: number, index?: number) => instance("leader-" + attempt, {
    ...rank, state: index == null ? "failed" : "running", dist_attempt: attempt, dist_role: "leader", replica: index == null ? undefined : { role: rank.role, index },
  });
  const follower = instance("follower", { ...rank, dist_attempt: 2, node_rank: 1, dist_role: "follower" });
  const groups = deploymentReplicaGroups({
    pd: { roles: [{ role: "decode", desired: 2 }, { role: "prefill", desired: 1 }] },
    instances: [leader(1), follower, leader(2, 0)],
  } as Deployment);
  assert.deepEqual(groups.map(group => [group.key, group.instances.map(inst => inst.uuid)]), [
    ["decode:0", ["follower", "leader-2"]], ["decode:1", []], ["prefill:0", []], ["unknown", ["leader-1"]],
  ]);
});
