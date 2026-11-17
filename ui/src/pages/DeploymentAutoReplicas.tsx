import { useState } from "react";
import { Alert, InputNumber, Modal, Space, Tag, Typography, message } from "antd";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import { api, type Deployment, type Node } from "../api";
import { APIError, apiErrorText } from "../apiError";
import NodePicker from "../nodes/NodePicker";
import { HelpTip } from "../ui-kit/HelpTip";
import { scheduleText } from "./deploy/deploySchedule";
import { useOrder } from "./useOrder";

// placeOnRefusal words a refused first placement (REPLICAS_PLACE_ON_REFUSED):
// the node, why it cannot take the new replica (the scheduler's own reason
// when it has no room), and the detail the server gave.
export function placeOnRefusal(e: unknown, t: TFunction<"deployments">, tDeploy: TFunction<"deploy">, tCommon: TFunction<"common">): Error | null {
  if (!(e instanceof APIError) || e.code !== "REPLICAS_PLACE_ON_REFUSED") return null;
  const params = e.params ?? {};
  const reason = String(params.reason ?? "");
  const detail = reason === "no_room" && params.code
    ? scheduleText(tDeploy, { code: String(params.code), params: params.params as Record<string, unknown> | undefined })
    : String(params.detail ?? "");
  const text = t(`replica.placeOn.refused.${reason}`, { node: params.node, detail, defaultValue: "" });
  return new Error(text || apiErrorText(tCommon, e));
}

// AutoReplicasDialog scales a multi-node deployment the platform places: the
// replica count, and for each new replica the nodes of its first ranks the
// deployer wants it on. The platform picks every rank left unnamed in the same
// network, so naming is optional and partial: a rank is named only after the
// ranks before it (place_on lists the new replicas' ranks in order), and each
// slot says whether it is specified or scheduled automatically.
export default function AutoReplicasDialog({ dep, ranks, onClose, onChanged }: { dep: Deployment; ranks: number; onClose: () => void; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tDeploy } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const { busy, run } = useOrder();
  const [count, setCount] = useState<number | null>(dep.replicas);
  // One slot per rank of every new replica, in order; "" is not named.
  const [slots, setSlots] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const added = count != null && count > dep.replicas ? count - dep.replicas : 0;
  const visible = Array.from({ length: added * ranks }, (_, i) => slots[i] ?? "");
  // A rank can be named only when every rank before it is.
  const nameable = (index: number) => visible.slice(0, index).every(Boolean);
  const setSlot = (index: number, node: string | undefined) =>
    setSlots(visible.map((value, at) => (at === index ? node ?? "" : node || at < index ? value : "")));
  const unpickable = (node: Node) => node.state !== "online" || (node.scheduling ?? "schedulable") !== "schedulable";
  const named = visible.filter(Boolean);
  const save = async () => {
    if (count == null || count === dep.replicas) return;
    setError(null);
    const refused = await run(async () => {
      try {
        await api.scaleReplicas(dep.id, named.length > 0 ? { replicas: count, place_on: named } : { replicas: count });
      } catch (e) {
        throw placeOnRefusal(e, t, tDeploy, tCommon) ?? e;
      }
    });
    if (refused !== null) {
      setError(t("replica.scaleFailed", { detail: refused }));
      return;
    }
    message.success(t("replica.scaled", { count }));
    onClose();
    onChanged();
  };
  return (
    <Modal
      title={t("replica.autoTitle")}
      open
      onCancel={() => !busy && onClose()}
      onOk={() => void save()}
      okText={t("replica.apply")}
      cancelText={t("detail.cancel")}
      okButtonProps={{ loading: busy, disabled: count == null || count === dep.replicas }}
      cancelButtonProps={{ disabled: busy }}
    >
      <Typography.Paragraph type="secondary">{t("replica.autoHint", { nnodes: ranks })}</Typography.Paragraph>
      <Space direction="vertical" style={{ width: "100%" }}>
        <InputNumber id="deployment-auto-replicas" min={1} max={dep.max_replicas} value={count} onChange={(next) => setCount(next)} aria-label={t("detail.replicas")} />
        {added > 0 && (
          <>
            <Typography.Text type="secondary" className="inline-flex items-center gap-1">{t("unitsPlacement.scale.autoNote")}<HelpTip content={t("unitsPlacement.scale.prefixHelp")} /></Typography.Text>
            {Array.from({ length: added }, (_, replica) => (
              <Space key={replica} wrap align="center">
                <Typography.Text strong>{t("unitsPlacement.scale.newReplica", { index: replica + 1 })}</Typography.Text>
                {Array.from({ length: ranks }, (_, rank) => {
                  const index = replica * ranks + rank;
                  const value = visible[index];
                  return (
                    <Space key={rank} size={4}>
                      <NodePicker
                        style={{ minWidth: 170 }}
                        allowClear
                        disabled={!nameable(index)}
                        placeholder={t("unitsPlacement.scale.preferPlaceholder")}
                        aria-label={t("unitsPlacement.scale.rankSlotAria", { index: replica + 1, rank: rank + 1 })}
                        value={value || undefined}
                        optionDisabled={unpickable}
                        onChange={(node) => setSlot(index, typeof node === "string" ? node : undefined)}
                      />
                      <Tag color={value ? "blue" : "default"}>
                        {t(value ? "unitsPlacement.scale.specified" : "unitsPlacement.scale.automatic")}
                      </Tag>
                    </Space>
                  );
                })}
              </Space>
            ))}
          </>
        )}
      </Space>
      {error && <Alert style={{ marginTop: 12 }} type="error" showIcon message={error} />}
    </Modal>
  );
}
