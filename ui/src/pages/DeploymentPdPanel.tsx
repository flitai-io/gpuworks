import { useState } from "react";
import { Space, Typography } from "antd";
import { Badge, Tag } from "../pages/DeploymentControls";
import { Button } from "../components/ui/button";
import { useTranslation } from "react-i18next";
import { api, type Deployment, type DeploymentPd } from "../api";
import DetailSection from "../components/detail/DetailSection";
import DeploymentPdScaleModal from "./DeploymentPdScaleModal";
import { replicaCount } from "./DeploymentReplicaActions";
import { RoleSleepActions } from "./DeploymentSleepControls";
import { UnitRetryAlert } from "./OrderDialogs";

// pdStateBadge maps the derived PD state (never persisted, see ADR 0028) to
// the same Badge status vocabulary the ordinary deployment state column uses.
function pdStateBadge(state: DeploymentPd["state"]): "success" | "warning" | "default" {
  if (state === "running") return "success";
  if (state === "partial") return "warning";
  return "default";
}

export default function DeploymentPdPanel({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const [scaleOpen, setScaleOpen] = useState(false);
  const pd = dep.pd;
  if (!pd) return null;
  const stopped = dep.desired_state === "stopped";
  const ready = dep.ready_replicas ?? pd.roles.reduce((sum, role) => sum + role.ready, 0);
  return (
    <DetailSection title={t("detail.pd.title")} extra={
      <Button variant="tertiary" size="sm" onClick={() => setScaleOpen(true)}>{t("detail.pd.scale.action")}</Button>
    }>
      <div className="deployment-pd-summary">
        <Space wrap>
          <Tag color="blue">{t("detail.pd.mode", { mode: pd.mode })}</Tag>
          {!stopped && <Badge status={pdStateBadge(pd.state)} text={t(`detail.pd.state.${pd.state}`)} />}
          <Typography.Text type="secondary">{t("detail.pd.readyUnits", { ready, desired: replicaCount(dep) })}</Typography.Text>
          {!stopped && pd.short && <Tag>{t("detail.pd.short")}</Tag>}
        </Space>
        <Space wrap>
          {pd.roles.map((role) => (
            <Tag key={role.role} color={(role.asleep ?? 0) > 0 ? "orange" : undefined}>
              {t("detail.pd.roleSummary", {
                role: t(`detail.pd.roleLabel.${role.role}`),
                tp: role.tp,
                ready: role.ready,
                desired: role.desired,
              })}
              {!!role.asleep && ` · ${t("detail.pd.roleAsleep", { count: role.asleep })}`}
            </Tag>
          ))}
        </Space>
        {dep.schedule_capabilities?.sleep.ok && <RoleSleepActions dep={dep} onDone={onChanged} />}
        <PdExhaustedUnits dep={dep} onChanged={onChanged} />
      </div>
      <DeploymentPdScaleModal open={scaleOpen} dep={dep} onClose={() => setScaleOpen(false)} onChanged={onChanged} />
    </DetailSection>
  );
}

// PdExhaustedUnits lists the units whose retry budget is spent
// (retry_exhausted, derived by the server from the controller's give-up
// record) with the newest failure and a Retry, which gives that one unit its
// budget back without touching the others (ADR 0029).
function PdExhaustedUnits({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const units = (dep.pd?.roles ?? []).flatMap((role) =>
    role.units.map((unit, index) => ({ role: role.role, index, unit })).filter((item) => item.unit.retry_exhausted),
  );
  return (
    <>
      {units.map(({ role, index, unit }) => (
        <UnitRetryAlert
          key={`${role}/${index}`}
          message={t("detail.pd.retry.exhausted", {
            role: t(`detail.pd.roleLabel.${role}`),
            index: index + 1,
            node: unit.node_name || unit.node_uuid,
            attempts: unit.failed_attempts ?? 0,
            reason: unit.last_failure ?? "",
          })}
          retry={() => api.retryDeploymentUnit(dep.id, { role, index })}
          onDone={onChanged}
          editHref={`/deploy?edit=${dep.id}`}
        />
      ))}
    </>
  );
}
