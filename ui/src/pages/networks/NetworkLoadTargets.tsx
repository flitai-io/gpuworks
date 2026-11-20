import { useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import DetailSection from "../../components/detail/DetailSection";
import { Button } from "../../components/ui/button";
import { ConfirmPopover } from "../../ui-kit/ConfirmPopover";
import { formatTime } from "../../i18n/datetime";
import type { NetworkAttachment, NetworkDetail } from "../../networkTypes";
import NetworkTargetTable from "./NetworkTargetTable";

type Props = {
  children: ReactNode;
  detail: NetworkDetail;
  onAttach: (attachment: NetworkAttachment) => void;
  onManageLabels: () => void;
  onDetach: (attachment: NetworkAttachment) => void;
};

export default function NetworkLoadTargets({ detail, onAttach, onDetach, onManageLabels, children }: Props) {
  const { t } = useTranslation("networks");
  const [showUnresolved, setShowUnresolved] = useState(false);
  const labels = useMemo(() => detail.attachments.filter(a => a.target === "label"), [detail.attachments]);
  const unresolved = useMemo(() => {
    const resolved = new Set(detail.nodes.map(node => node.attachment_id));
    return detail.attachments.filter(a => a.target === "node" && !resolved.has(a.id));
  }, [detail.attachments, detail.nodes]);
  const panelID = `network-${detail.id}-load-targets`;

  return <DetailSection title={t("detail.loadedNodes", { count: detail.nodes.length })}>
    <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
      <div className="inline-flex items-center gap-2">
        <span className="text-body text-fg-muted">{t("table.attachmentLabels", { count: labels.length })}</span>
        <Button variant="link" size="sm" aria-haspopup="dialog" onClick={onManageLabels}>{t("detail.manageLabels")}</Button>
      </div>
      {unresolved.length > 0 && <Button variant="secondary" size="sm" aria-expanded={showUnresolved} aria-controls={panelID}
          onClick={() => setShowUnresolved(!showUnresolved)}>{t("detail.unresolvedAttachments", { count: unresolved.length })}</Button>}
    </div>
    <div id={panelID}>
      {showUnresolved && unresolved.length > 0 && <div className="mt-3">
        <NetworkTargetTable<NetworkAttachment> networkId={detail.id} kind="attachments" data={unresolved}
          tableId="network-unresolved-attachments" rowKey="id" columnSettings={false}
          columns={[
            { key: "target", title: t("detail.attachTarget"), role: "name", width: 230, render: (_, a) => {
              const target = a.node_name || a.node_uuid || t("detail.unnamedAttachment", { id: a.id });
              const override = [a.interface, a.ip, ...a.hcas].filter(Boolean).join(" · ");
              return <div className="whitespace-normal break-all"><span>{target}</span>{override && <div className="text-fg-muted">{t("detail.overrides", { value: override })}</div>}</div>;
            } },
            { key: "time", title: t("detail.attachedTime"), width: 160, render: (_, a) => formatTime(a.created_at) },
            { key: "actions", role: "actions", width: 140, render: (_, a) => <div className="inline-flex gap-2">
              <Button size="sm" variant="link" onClick={() => onAttach(a)}>{t("actions.edit")}</Button>
              <ConfirmPopover destructive title={t("actions.confirmDetach")} confirmText={t("actions.detach")} cancelText={t("actions.cancel")}
                onConfirm={() => onDetach(a)} trigger={<Button size="sm" variant="link" className="text-fg-danger">{t("actions.detach")}</Button>} />
            </div> },
          ]} />
      </div>}
    </div>
    {children}
  </DetailSection>;
}
