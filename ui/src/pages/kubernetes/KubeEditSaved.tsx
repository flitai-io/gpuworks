import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { KubeClusterEdit } from "../../api";
import { Button } from "../../components/ui/button";
import { Notice } from "../../ui-kit/Notice";
import KubeClusterOperationModal from "./KubeClusterOperationModal";
import KubeCommandList from "./KubeCommandList";

/**
 * What follows saving an edit that changed the manifest: the record changed,
 * the cluster did not. It offers applying again when the server can reach the
 * cluster, and the commands to run when it cannot.
 */
export default function KubeEditSaved({ edit, onApplied }: { edit: KubeClusterEdit; onApplied?: () => void }) {
  const { t } = useTranslation("kubernetes");
  const [applying, setApplying] = useState(false);
  const { cluster, commands } = edit;
  return (
    <div className="flex flex-col gap-4">
      <Notice tone="info" title={t("edit.savedTitle", { name: cluster.name })}
        action={cluster.can_apply ? <Button variant="primary" size="sm" onClick={() => setApplying(true)}>{t("edit.applyNow")}</Button> : undefined}>
        {t("edit.savedHelp")}
        {!cluster.can_apply && cluster.operation_blocked && commands.length === 0 ? ` ${t(`blocked.${cluster.operation_blocked}`)}` : ""}
      </Notice>
      {!cluster.can_apply && commands.length > 0 && (
        <div className="flex flex-col gap-2">
          <strong className="font-semibold text-fg-default">{t("edit.commandsTitle")}</strong>
          <span className="text-fg-muted">{t("edit.commandsHelp")}</span>
          <KubeCommandList commands={commands} />
        </div>
      )}
      {applying && <KubeClusterOperationModal cluster={cluster} op="apply" onClose={() => setApplying(false)} onDone={onApplied} />}
    </div>
  );
}
