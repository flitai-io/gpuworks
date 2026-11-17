import { useState, type ReactNode } from "react";
import { DropdownContent, DropdownMenu, DropdownTrigger } from "../components/ui/dropdown";
import { MenuItem } from "../components/ui/menu-item";
import { useIcon } from "../lib/icon-context";
import { Modal, Radio, Space, Tag, Typography, message } from "antd";
import { Button } from "../components/ui/button";
import { useTranslation } from "react-i18next";
import { api, type Deployment } from "../api";
import { ConfirmOrderModal, UnitRetryAlert } from "./OrderDialogs";
import { useOrder } from "./useOrder";

interface DeploymentAction {
  key: string;
  label: string;
  icon?: ReactNode;
  hint?: string;
  disabled?: boolean;
  danger?: boolean;
  onClick: () => void;
}

export interface DeploymentActions {
  primary: DeploymentAction[];
  terminal: DeploymentAction;
  menu: DeploymentAction[];
  remove: DeploymentAction;
}

export default function DeploymentDetailActions({ dep, actions, onChanged }: {
  dep: Deployment; actions: DeploymentActions; onChanged: () => void;
}) {
  const { t } = useTranslation("deployments");
  const chevron = useIcon("chevron-down");
  const { busy, run } = useOrder();
  const [confirm, setConfirm] = useState<"restart" | "stop" | null>(null);
  const deleting = dep.desired_state === "deleting";

  const order = async (action: "start" | "stop") => {
    const refused = await run(() => (action === "start" ? api.startDeployment(dep.id) : api.stopDeployment(dep.id)));
    if (refused !== null) {
      message.error(t(action === "start" ? "detail.startFailed" : "detail.stopFailed", { detail: refused }));
      return;
    }
    message.success(t(action === "start" ? "detail.started" : "detail.stopped"));
    onChanged();
  };

  return (
    <div className="deployment-detail-actions">
      {dep.restart_requested_at && <Tag color="processing">{t("detail.restart.rolling")}</Tag>}
      {actions.primary.map(action => <span key={action.key} title={action.hint}>
        <Button variant="tertiary" size="sm" disabled={action.disabled} onClick={action.onClick}>{action.label}</Button>
      </span>)}
      {dep.desired_state === "stopped" ? (
        <Button variant="primary" size="sm" loading={busy} onClick={() => void order("start")}>{t("detail.start")}</Button>
      ) : <>
        <Button variant="tertiary" size="sm" disabled={deleting} onClick={() => setConfirm("restart")}>{t("detail.restart.action")}</Button>
        <Button variant="tertiary" size="sm" className="text-fg-danger" disabled={deleting || busy} onClick={() => setConfirm("stop")}>{t("detail.stop")}</Button>
      </>}
      <Button variant="tertiary" size="sm" className="text-fg-danger" onClick={actions.remove.onClick}>{actions.remove.label}</Button>
      <DropdownMenu>
        <DropdownTrigger render={<Button className="ms-auto" variant="tertiary" size="sm" trailingIcon={chevron}>{t("detailLayout.more")}</Button>} />
        <DropdownContent align="end">
          {actions.menu.map((action, index) => <MenuItem key={action.key} index={index} label={action.label}
            leading={action.icon} disabled={action.disabled} onSelect={action.onClick} />)}
        </DropdownContent>
      </DropdownMenu>
      <RestartConfirm open={confirm === "restart"} deploymentID={dep.id} onClose={() => setConfirm(null)} onChanged={onChanged} />
      <Modal open={confirm === "stop"} title={t("detail.stopConfirm")} okText={t("detail.stop")} cancelText={t("detail.cancel")}
        okButtonProps={{ danger: true, loading: busy }} cancelButtonProps={{ disabled: busy }}
        onCancel={() => { if (!busy) setConfirm(null); }}
        onOk={async () => { await order("stop"); setConfirm(null); }} />
    </div>
  );
}

// RestartConfirm asks how to restart: rolling (the default, one replica at a
// time while the rest serve) or all at once (unavailable until every
// instance is ready again).
function RestartConfirm({ open, deploymentID, onClose, onChanged }: { open: boolean; deploymentID: number; onClose: () => void; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const [strategy, setStrategy] = useState<"rolling" | "all">("rolling");
  return (
    <ConfirmOrderModal
      open={open}
      title={t("detail.restart.confirmTitle")}
      okText={t("detail.restart.confirmOk")}
      danger={strategy === "all"}
      failKey="detail.restart.failed"
      order={() => api.restartDeployment(deploymentID, strategy)}
      onClose={onClose}
      onDone={() => {
        message.success(t("detail.restart.started"));
        onChanged();
      }}
    >
      <Radio.Group value={strategy} onChange={(e) => setStrategy(e.target.value)}>
        <Space direction="vertical">
          <Radio value="rolling">{t("detail.restart.strategyRolling")}</Radio>
          <Radio value="all">{t("detail.restart.strategyAll")}</Radio>
        </Space>
      </Radio.Group>
      <Typography.Paragraph type="secondary" style={{ marginTop: 12 }}>
        {t(strategy === "rolling" ? "detail.restart.rollingDescription" : "detail.restart.confirmDescription")}
      </Typography.Paragraph>
    </ConfirmOrderModal>
  );
}

// ReplicaRetryAlert is an ordinary deployment's give-up notice: the replica
// the controller stopped rebuilding, its newest failure, and its Retry (a PD
// deployment shows this per unit instead).
export function ReplicaRetryAlert({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const exhausted = dep.retry_exhausted;
  if (!exhausted || dep.pd) return null;
  return (
    <UnitRetryAlert
      message={t("detail.retry.exhausted", { attempts: exhausted.failed_attempts, reason: exhausted.last_failure ?? "" })}
      retry={() => api.retryDeploymentUnit(dep.id, { role: "", index: exhausted.index })}
      onDone={onChanged}
      editHref={`/deploy?edit=${dep.id}`}
    />
  );
}
