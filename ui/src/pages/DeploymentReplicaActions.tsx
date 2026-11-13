import { useState } from "react";
import { Modal, Popconfirm, Space, Typography, message } from "antd";
import { Alert, Checkbox, InputNumber } from "../pages/DeploymentControls";
import { Button } from "../components/ui/button";
import { useTranslation } from "react-i18next";
import { api, type Deployment, type Instance, type Node, type PdScaleUnitInput, type ScheduleMessage } from "../api";
import NodePicker from "../nodes/NodePicker";
import { apiErrorText } from "../apiError";
import AutoReplicasDialog, { placeOnRefusal } from "./DeploymentAutoReplicas";
import { scheduleText } from "./deploy/deploySchedule";
import SleepModeDialog, { type SleepMode } from "../components/SleepModeDialog";
import { ConfirmOrderModal } from "./OrderDialogs";
import { useOrder } from "./useOrder";

// replicaCount is how many replicas the deployment asks for: a PD
// deployment's are its roles' units.
export function replicaCount(dep: Deployment): number {
  if (dep.pd) return dep.pd.roles.reduce((sum, role) => sum + role.desired, 0);
  return dep.replicas;
}

export function ReplicaActions({ dep, inst, label, onChanged }: { dep: Deployment; inst: Instance; label: string; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const [restartOpen, setRestartOpen] = useState(false);
  const [drain, setDrain] = useState(true);
  const removal = useOrder();
  const replica = inst.replica;
  if (dep.desired_state !== "active" || replicaCount(dep) <= 1 || !replica) return null;
  // Removing one named replica is a scale-down of an ordinary deployment: a
  // count naming the instance, or the multi-node list without the replica
  // this instance leads. A PD deployment removes units in Scale roles.
  const multiNode = (dep.nnodes ?? 1) > 1;
  const units = unitInputs(dep);
  const removable = !dep.pd && (!multiNode || replica.index < units.length);
  const remove = async () => {
    const refused = await removal.run(() =>
      api.scaleReplicas(dep.id, multiNode ? { units: units.filter((_, index) => index !== replica.index) } : { replicas: dep.replicas - 1, remove: [inst.uuid] }),
    );
    if (refused !== null) {
      message.error(t("replica.removeFailed", { detail: refused }));
      return;
    }
    message.success(t("replica.removed"));
    onChanged();
  };
  return (
    <div className="inline-flex max-w-full flex-wrap items-center gap-1 [&>button]:px-1.5">
      <Button variant="tertiary" size="sm" onClick={() => setRestartOpen(true)}>{t("replica.restart")}</Button>
      <ReplicaSleepAction inst={inst} wakeOnRequest={dep.sleep_wakes_on_request} onChanged={onChanged} />
      {removable && (
        <Popconfirm title={t("replica.removeNamed", { label })} onConfirm={() => void remove()} okText={t("replica.remove")} cancelText={t("detail.cancel")}>
          <Button variant="destructive" size="sm" loading={removal.busy}>{t("replica.remove")}</Button>
        </Popconfirm>
      )}
      {/* Draining is the default: the instance finishes the requests it
          holds while its replacement starts. */}
      <ConfirmOrderModal
        open={restartOpen}
        title={t("replica.restartNamed", { label })}
        okText={t("replica.restart")}
        failKey="replica.restartFailed"
        order={() => api.restartInstance(inst.uuid, drain)}
        onClose={() => setRestartOpen(false)}
        onDone={() => {
          message.success(t("replica.restartStarted"));
          onChanged();
        }}
      >
        <Typography.Paragraph>{t("replica.restartDescription")}</Typography.Paragraph>
        <Checkbox checked={drain} onChange={(e) => setDrain(e.target.checked)}>{t("replica.drain")}</Checkbox>
      </ConfirmOrderModal>
    </div>
  );
}

// ReplicaSleepAction sleeps (memory / L1) or wakes one replica, as the server
// says the replica may on its own (can_sleep_alone, can_wake_alone): not the
// last awake one of a PD role its mode cannot run without. A multi-node
// replica sleeps whole; its followers go with the leader.
function ReplicaSleepAction({ inst, wakeOnRequest, onChanged }: { inst: Instance; wakeOnRequest?: boolean; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const [asking, setAsking] = useState(false);
  const { busy, run } = useOrder();
  const order = async (send: () => Promise<unknown>, done: string, failKey: string) => {
    const refused = await run(send);
    if (refused !== null) {
      message.error(t(failKey, { detail: refused }));
      return;
    }
    message.success(done);
    setAsking(false);
    onChanged();
  };
  if (inst.asleep) {
    if (!inst.can_wake_alone) return null;
    return (
      <Button variant="primary" size="sm" loading={busy} onClick={() => void order(() => api.wakeInstance(inst.uuid), t("replica.wakeStarted"), "sleep.wakeFailed")}>
        {t("replica.wake")}
      </Button>
    );
  }
  if (!inst.can_sleep_alone) return null;
  return (
    <>
      <Button variant="tertiary" size="sm" loading={busy} onClick={() => setAsking(true)}>{t("replica.sleep")}</Button>
      <SleepModeDialog
        open={asking}
        level={1}
        confirming={busy}
        wakeOnRequest={wakeOnRequest}
        onConfirm={(mode: SleepMode, wakes?: boolean) => void order(() => api.sleepInstance(inst.uuid, 1, mode, wakes), t("replica.sleepStarted"), "sleep.failed")}
        onCancel={() => setAsking(false)}
      />
    </>
  );
}

// unitInputs is a multi-node deployment's replica list as the scale request
// sends it back.
function unitInputs(dep: Deployment): PdScaleUnitInput[] {
  return (dep.units ?? []).map((unit) => ({ node_uuid: unit.node_uuid, followers: (unit.followers ?? []).map((rank) => ({ node_uuid: rank.node_uuid })) }));
}

// ReplicasEditor edits an ordinary deployment's replicas: a single-node
// deployment's count in place, a multi-node deployment's list in a dialog
// (PD scales its roles). A single-node scale-down leaves the choice of which
// replicas go to the server: not yet ready first, then the newest.
export function ReplicasEditor({ dep, onChanged }: { dep: Deployment; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tDeploy } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const [draft, setDraft] = useState<number | null>();
  const value = draft === undefined ? dep.replicas : draft;
  const [firstNodes, setFirstNodes] = useState<string[]>([]);
  const { busy, run } = useOrder();
  const [listOpen, setListOpen] = useState(false);
  const ranks = dep.nnodes ?? 1;
  const startingReplicas = new Set((dep.instances ?? []).filter(inst => inst.replica && ["pulling", "starting", "optimizing"].includes(inst.state))
    .map(inst => `${inst.replica!.role ?? ""}:${inst.replica!.index}`)).size;
  const readyLabel = dep.ready_replicas != null && <Typography.Text>{t("detailLayout.ready", { ready: dep.ready_replicas, total: replicaCount(dep) })}{startingReplicas > 0 && ` · ${t("detailLayout.starting", { count: startingReplicas })}`}</Typography.Text>;
  const desiredLabel = dep.ready_replicas == null && <span>{t("detail.desiredReplicas", { desired: dep.replicas })}</span>;
  if (dep.pd || dep.desired_state === "deleting") return <Space size={8}>{readyLabel}{desiredLabel}</Space>;
  if (ranks > 1) {
    return (
      <Space size={8} wrap>
        {readyLabel}
        {desiredLabel}
        <Button variant="tertiary" size="sm" onClick={() => setListOpen(true)}>{t("replica.editUnits")}</Button>
        {listOpen && (dep.placement_mode === "auto"
          ? <AutoReplicasDialog dep={dep} ranks={ranks} onClose={() => setListOpen(false)} onChanged={onChanged} />
          : <MultiNodeReplicasDialog dep={dep} ranks={ranks} onClose={() => setListOpen(false)} onChanged={onChanged} />)}
      </Space>
    );
  }
  const added = value != null && value > dep.replicas ? value - dep.replicas : 0;
  const apply = async () => {
    if (value == null || value === dep.replicas) return;
    // Only as many first-placement nodes as replicas are added count.
    const placeOn = added > 0 ? firstNodes.slice(0, added) : [];
    const refused = await run(async () => {
      try {
        await api.scaleReplicas(dep.id, placeOn.length > 0 ? { replicas: value, place_on: placeOn } : { replicas: value });
      } catch (e) {
        throw placeOnRefusal(e, t, tDeploy, tCommon) ?? e;
      }
    });
    if (refused !== null) {
      message.error(t("replica.scaleFailed", { detail: refused }));
      return;
    }
    setDraft(undefined);
    setFirstNodes([]);
    message.success(t("replica.scaled", { count: value }));
    onChanged();
  };
  // A node that is offline or cordoned is listed but cannot take a replica.
  const unpickable = (node: Node) => node.state !== "online" || (node.scheduling ?? "schedulable") !== "schedulable";
  return (
    <Space size={8} wrap>
      {readyLabel}
      <span>{t("detail.desiredCount")}</span>
      <InputNumber disabled={busy} precision={0} id="deployment-replicas" size="small" style={{ width: 64 }} min={1} max={dep.max_replicas} value={value} onChange={setDraft} aria-label={t("detail.replicas")} />
      {added > 0 && (
        <NodePicker
          mode="multiple"
          style={{ width: 220, maxWidth: "100%" }}
          size="small"
          maxCount={added}
          placeholder={t("replica.placeOn.placeholder", { count: added })}
          aria-label={t("replica.placeOn.label")}
          value={firstNodes.slice(0, added)}
          optionDisabled={unpickable}
          onChange={(next) => setFirstNodes(Array.isArray(next) ? next : [])}
        />
      )}
      <Space size={4}>
        <Button variant="tertiary" size="sm" loading={busy} disabled={value == null || value < 1 || value > (dep.max_replicas ?? Infinity) || value === dep.replicas} onClick={() => void apply()}>{t("replica.apply")}</Button>
        {value !== dep.replicas && <Button variant="tertiary" size="sm" disabled={busy} onClick={() => { setDraft(undefined); setFirstNodes([]); }}>{t("detail.cancel")}</Button>}
      </Space>
      {added > 0 && <Typography.Text type="secondary" style={{ fontSize: "var(--font-size-label)" }}>{t("replica.placeOn.hint")}</Typography.Text>}
    </Space>
  );
}

// MultiNodeReplicasDialog edits a multi-node deployment's replica list: each
// replica a leader node and its follower nodes, one per rank. Save replaces
// the list; a replica left out drains and goes, a new one starts once its
// nodes have the model and image.
function MultiNodeReplicasDialog({ dep, ranks, onClose, onChanged }: { dep: Deployment; ranks: number; onClose: () => void; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const [units, setUnits] = useState<string[][]>(() => unitInputs(dep).map((unit) => [unit.node_uuid, ...(unit.followers ?? []).map((rank) => rank.node_uuid)]));
  const { busy, run } = useOrder();
  const [error, setError] = useState<string | null>(null);
  const { t: tDeploy } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const [suggesting, setSuggesting] = useState(false);
  const [suggested, setSuggested] = useState<{ type: "info" | "warning"; text: string } | null>(null);
  // Each rank's node is searched on the server; a node that is offline or
  // cordoned is listed but cannot be picked.
  const unpickable = (node: Node) => node.state !== "online" || (node.scheduling ?? "schedulable") !== "schedulable";
  // suggest asks the server for one more replica's nodes given the rows so
  // far, and appends it for the operator to check before saving.
  const suggest = async () => {
    setSuggesting(true);
    setSuggested(null);
    try {
      const listed = units.filter((row) => row.every(Boolean)).map(([leader, ...followers]) => ({ node_uuid: leader, followers: followers.map((node) => ({ node_uuid: node })) }));
      const result = await api.suggestUnit(dep.id, listed);
      if (result.unit) {
        setUnits((current) => [...current, [result.unit!.node_uuid, ...(result.unit!.followers ?? []).map((rank) => rank.node_uuid)]]);
        setSuggested({
          type: "info",
          text: t("replica.suggest.added", { network: result.network?.name ?? "", arch: result.arch ?? "", nodes: (result.nodes ?? []).map((node) => node.name).join(", ") }),
        });
        return;
      }
      const reason = result.reason;
      const params = reason?.params ?? {};
      setSuggested({
        type: "warning",
        text: reason?.code === "unit_no_fit"
          ? t("replica.suggest.noFit", { node: params.node, reason: params.first_reason ? scheduleText(tDeploy, params.first_reason as ScheduleMessage) : "" })
          : t("replica.suggest.none", params),
      });
    } catch (err) {
      setSuggested({ type: "warning", text: t("replica.suggest.failed", { detail: apiErrorText(tCommon, err) }) });
    } finally {
      setSuggesting(false);
    }
  };
  const setRank = (unit: number, rank: number, node: string) =>
    setUnits((current) => current.map((row, index) => (index === unit ? row.map((value, at) => (at === rank ? node : value)) : row)));
  const complete = units.length > 0 && units.every((row) => row.length === ranks && row.every(Boolean));
  const save = async () => {
    setError(null);
    const refused = await run(() =>
      api.scaleReplicas(dep.id, { units: units.map(([leader, ...followers]) => ({ node_uuid: leader, followers: followers.map((node) => ({ node_uuid: node })) })) }),
    );
    if (refused !== null) {
      setError(t("replica.scaleFailed", { detail: refused }));
      return;
    }
    message.success(t("replica.scaled", { count: units.length }));
    onClose();
    onChanged();
  };
  return (
    <Modal
      title={t("replica.unitsTitle")}
      open
      onCancel={() => !busy && onClose()}
      onOk={() => void save()}
      okText={t("replica.save")}
      cancelText={t("detail.cancel")}
      okButtonProps={{ loading: busy, disabled: !complete }}
      cancelButtonProps={{ disabled: busy }}
    >
      <Typography.Paragraph type="secondary">{t("replica.unitsHint", { nnodes: ranks })}</Typography.Paragraph>
      <Space direction="vertical" style={{ width: "100%" }}>
        {units.map((row, unit) => (
          <Space key={unit} wrap align="center">
            <Typography.Text strong>{t("replica.unitLabel", { index: unit + 1 })}</Typography.Text>
            {Array.from({ length: ranks }, (_, rank) => (
              <NodePicker
                key={rank}
                style={{ minWidth: 150 }}
                placeholder={rank === 0 ? t("replica.leaderNode") : t("replica.followerNode", { rank })}
                aria-label={rank === 0 ? t("replica.leaderNodeOf", { index: unit + 1 }) : t("replica.followerNodeOf", { index: unit + 1, rank })}
                value={row[rank] || undefined}
                optionDisabled={unpickable}
                onChange={(node) => setRank(unit, rank, node as string)}
              />
            ))}
            <Button variant="tertiary" size="sm" disabled={units.length <= 1} onClick={() => setUnits((current) => current.filter((_, index) => index !== unit))}>
              {t("replica.removeUnit", { index: unit + 1 })}
            </Button>
          </Space>
        ))}
        <Space wrap>
          <Button variant="tertiary" size="sm" disabled={dep.max_replicas !== undefined && units.length >= dep.max_replicas} onClick={() => setUnits((current) => [...current, Array.from({ length: ranks }, () => "")])}>
            {t("replica.addUnit")}
          </Button>
          <Button variant="tertiary" size="sm" loading={suggesting} disabled={dep.max_replicas !== undefined && units.length >= dep.max_replicas} onClick={() => void suggest()}>
            {t("replica.suggestUnit")}
          </Button>
        </Space>
        {suggested && <Alert type={suggested.type} showIcon message={suggested.text} />}
      </Space>
      {error && <Alert style={{ marginTop: 12 }} type="error" showIcon message={error} />}
    </Modal>
  );
}
