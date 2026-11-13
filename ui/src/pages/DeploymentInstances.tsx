import { useEffect } from "react";
import { Alert, Empty, Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { Deployment, Instance } from "../api";
import InstancePanel from "./InstancePanel";
import { distRank, followerCount, orderInstances } from "./instanceGroups";
import { imageMismatch } from "./instanceImage";
import { ReplicaActions } from "./DeploymentReplicaActions";

// DeploymentInstances is the instance list of the deployment drawer. A PD
// deployment groups its instances by role (dep.pd.roles' order); within a
// group, and for every other deployment, a multi-node replica's ranks sit
// together, leader first, each tagged with its rank. An instance no role
// claims (mid-rebuild) still renders, in a trailing group, so nothing
// silently disappears. A failed attempt whose unit is already filled by
// another row is a previous attempt: it stays, with its own message, after
// the row that fills the unit. Ordinary replicas share one unit key, so a
// sibling being up is not that. focus names an instance to scroll to (the
// monitor's replica links carry it). onChanged refreshes the drawer after a
// replica action.
export default function DeploymentInstances({ dep, focus, onChanged }: { dep: Deployment; focus?: string | null; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const instances = dep.instances ?? [];
  const followers = followerCount(instances);
  const differing = imageMismatch(instances).mismatched;
  const listed = instances.some((inst) => inst.uuid === focus);
  useEffect(() => {
    if (!focus || !listed) return;
    document.getElementById(`instance-${focus}`)?.scrollIntoView?.({ block: "start" });
  }, [focus, listed]);
  const panels = (group: Instance[]) => {
    const { instances: ordered, previous } = orderInstances(group, dep.replicas);
    return ordered.map((inst) => {
      const rank = distRank(inst, instances);
      return (
        <InstancePanel
          key={inst.uuid}
          inst={inst}
          params={dep.quick_params}
          rank={rank}
          previousAttempt={previous.has(inst.uuid)}
          imageDiffers={differing.has(inst.uuid)}
          actions={<ReplicaActions dep={dep} inst={inst} onChanged={onChanged} />}
        />
      );
    });
  };
  return (
    <>
      <Typography.Title level={5} style={{ marginTop: 20 }}>
        {followers > 0
          ? t("detail.instancesWithFollowers", { count: instances.length, followers })
          : t("detail.instances", { count: instances.length })}
      </Typography.Title>
      {differing.size > 0 && (
        <Alert
          type="warning"
          showIcon
          role="status"
          style={{ marginBottom: 12 }}
          message={t("detail.imageMismatch", { count: differing.size })}
        />
      )}
      {!instances.length ? (
        <Empty description={t("detail.noInstances")} />
      ) : dep.pd ? (
        <>
          {dep.pd.roles.map((role) => {
            const roleInstances = instances.filter((inst) => inst.role === role.role);
            if (!roleInstances.length) return null;
            return (
              <div key={role.role} style={{ marginTop: 12 }}>
                <Space size={8} style={{ marginBottom: 4 }}>
                  <Tag color="geekblue">{t(`detail.pd.role.${role.role}`)}</Tag>
                  <Typography.Text type="secondary">
                    TP {role.tp} · {role.ready}/{role.desired}
                  </Typography.Text>
                </Space>
                {panels(roleInstances)}
              </div>
            );
          })}
          {panels(instances.filter((inst) => !dep.pd!.roles.some((role) => role.role === inst.role)))}
        </>
      ) : (
        panels(instances)
      )}
    </>
  );
}
