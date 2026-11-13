import type { TFunction } from "i18next";
import { detailBadgeTone, stateLabel } from "./state";
import { distRank, instanceSummaryState } from "./instanceGroups";
import type { Deployment, Instance } from "../api";

type Slot = NonNullable<Instance["replica"]>;

export interface ReplicaGroup {
  key: string;
  slot?: Slot;
  instances: Instance[];
}

export function deploymentReplicaGroups(dep: Deployment): ReplicaGroup[] {
  const groups = new Map<string, ReplicaGroup>();
  function getGroup(slot?: Slot) {
    const key = slot ? `${slot.role ?? ""}:${slot.index}` : "unknown";
    let group = groups.get(key);
    if (!group) {
      group = { key, slot, instances: [] };
      groups.set(key, group);
    }
    return group;
  }
  const roles = dep.pd?.roles ?? [{ role: "", desired: dep.replicas }];
  for (const { role, desired } of roles) {
    for (let index = 0; index < desired; index++) getGroup({ role, index });
  }
  const instances = dep.instances ?? [];
  for (const inst of instances) {
    const generation = inst.dist_role === "follower" ? distRank(inst, instances)?.leaderUUID : undefined;
    const owner = generation ? instances.find(record => record.uuid === generation) ?? inst : inst;
    getGroup(inst.replica ?? owner.replica).instances.push(inst);
  }
  return [...groups.values()].sort((a, b) => Number(!a.slot) - Number(!b.slot));
}

export function replicaLabel(group: ReplicaGroup, t: TFunction) {
  if (!group.slot) return t("detailLayout.unknownReplica");
  const { role, index } = group.slot;
  return `${role ? role + " " : ""}${t("replica.unitLabel", { index: index + 1 })}`;
}

export function replicaLeader(group: ReplicaGroup) {
  return group.instances.find(inst => inst.replica && inst.dist_role !== "follower");
}

export function replicaStatus(group: ReplicaGroup, dep: Deployment, t: TFunction, tCommon: TFunction) {
  const ranks = group.instances.filter(inst => inst.replica);
  const current = ranks.find(inst => ["pulling", "starting", "optimizing"].includes(inst.state)) ?? replicaLeader(group);
  if (current) {
    const badge = stateLabel(instanceSummaryState(current), tCommon);
    return { label: badge.text, tone: detailBadgeTone[badge.status] };
  }
  const stopped = dep.retry_exhausted?.index === group.slot?.index && !!group.slot && !group.slot.role;
  const label = group.slot && dep.desired_state === "active"
    ? t(stopped ? "detailLayout.rebuildStopped" : "detailLayout.waitingInstance") : undefined;
  return { label, tone: "neutral" as const };
}

export function replicaContextLabel(group: ReplicaGroup, dep: Deployment, t: TFunction, tCommon: TFunction) {
  const leader = replicaLeader(group);
  const node = leader && dep.runtime_nodes?.find(item => item.node_uuid === leader.node_uuid);
  return [node?.node_name, replicaStatus(group, dep, t, tCommon).label].filter(Boolean).join(" · ");
}
