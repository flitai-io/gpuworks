import { Button, Popconfirm, Space, Tag, Typography, message } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type CardScheduling, type GPU, type Node } from "../../api";
import { apiErrorText, productMessageText } from "../../apiError";

// CardSchedulingTag marks a card an operator took out of scheduling, one an
// extension holds out of it, or one they let work onto although the platform
// judged it unusable; a card left to the platform shows nothing here (its
// health tag already speaks).
export function CardSchedulingTag({ gpu }: { gpu: GPU }) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  if (gpu.scheduling_stopped || gpu.scheduling_hold) {
    const hold = !gpu.scheduling_stopped && gpu.scheduling_hold ? gpu.scheduling_hold : undefined;
    return <Tag color="orange" title={hold && productMessageText(tCommon, hold)}>{t(hold ? "cardScheduling.heldTag" : "cardScheduling.stoppedTag")}</Tag>;
  }
  if (gpu.scheduling_allowed) return <Tag color="blue">{t("cardScheduling.allowedTag")}</Tag>;
  return null;
}

// CardSchedulingControl states where the card stands and offers what the
// server says may be done to it. Letting work onto a card the platform judged
// unusable is confirmed first, and a card processes outside the platform use
// says so: enabling it is how an operator consents to sharing it. The
// decision persists until disabled.
export function CardSchedulingControl({ node, gpu, onUpdated }: { node: Node; gpu: GPU; onUpdated: (node: Node) => void }) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const [busy, setBusy] = useState<CardScheduling | null>(null);
  const actions = gpu.scheduling_actions ?? [];
  const set = async (to: CardScheduling) => {
    setBusy(to);
    try {
      onUpdated(await api.setCardScheduling(node.uuid, { pcie_bdf: gpu.pcie_bdf, uuid: gpu.uuid }, to));
      message.success(t(`cardScheduling.done.${to}`));
    } catch (err) {
      message.error(apiErrorText(tCommon, err));
    } finally {
      setBusy(null);
    }
  };
  const heldOutside = !!gpu.foreign_processes?.length;
  const state = gpu.scheduling_stopped ? "stopped" : gpu.scheduling_allowed && !gpu.scheduling_hold ? "allowed" : gpu.schedulable ? "schedulable" : heldOutside ? "heldOutside" : "blocked";
  // A card an extension holds says why, in the extension's own words.
  const text = !gpu.scheduling_stopped && gpu.scheduling_hold ? productMessageText(tCommon, gpu.scheduling_hold) : t(`cardScheduling.state.${state}`);
  return (
    <Space size={8} wrap>
      <Typography.Text>{text}</Typography.Text>
      {actions.includes("stopped") && (
        <Button size="small" loading={busy === "stopped"} onClick={() => set("stopped")}>
          {t("cardScheduling.stop")}
        </Button>
      )}
      {actions.includes("schedulable") &&
        (gpu.scheduling_stopped ? (
          <Button size="small" loading={busy === "schedulable"} onClick={() => set("schedulable")}>
            {t("cardScheduling.resume")}
          </Button>
        ) : (
          <Popconfirm title={t(heldOutside ? "cardScheduling.allowConfirmHeldOutside" : "cardScheduling.allowConfirm")} okText={t("cardScheduling.resume")} onConfirm={() => set("schedulable")}>
            <Button size="small" loading={busy === "schedulable"}>{t("cardScheduling.resume")}</Button>
          </Popconfirm>
        ))}
    </Space>
  );
}
