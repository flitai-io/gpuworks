import { useState } from "react";
import { useTranslation } from "react-i18next";
import type { KubeCluster } from "../../api";
import { Button } from "../../components/ui/button";
import { Notice } from "../../ui-kit/Notice";
import KubeClusterOperationModal from "./KubeClusterOperationModal";

/**
 * What follows saving an entry's node pools: the record changed, the cluster
 * did not. It offers applying the manifest again when the server can, and says
 * why it cannot (or to run the command) when it cannot.
 */
export default function KubePoolsSaved({ cluster, onApplied }: { cluster: KubeCluster; onApplied?: () => void }) {
  const { t } = useTranslation("kubernetes");
  const [applying, setApplying] = useState(false);
  return (
    <>
      <Notice tone="info" title={t("pools.saved", { name: cluster.name })}
        action={cluster.can_apply ? <Button variant="primary" size="sm" onClick={() => setApplying(true)}>{t("pools.applyNow")}</Button> : undefined}>
        {t("pools.savedHelp")}
        {!cluster.can_apply && cluster.operation_blocked ? ` ${t(`blocked.${cluster.operation_blocked}`)}` : ""}
      </Notice>
      {applying && <KubeClusterOperationModal cluster={cluster} op="apply" onClose={() => setApplying(false)} onDone={onApplied} />}
    </>
  );
}
