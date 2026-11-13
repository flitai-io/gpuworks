import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { KubeCluster } from "../../api";
import CopyButton from "../../components/CopyButton";
import { formatTime } from "../../i18n/datetime";
import { cardReturnText } from "../../kubeClusters";
import { kubeClusterNodesPath } from "../../nodeSources";
import FactList from "./FactList";
import { joinedText } from "./joinedText";

/**
 * A local Kubernetes cluster (connected on its own, not from a cloud
 * account), under its row: how it was connected and what it selects, laid out
 * flat as "label value" pairs, then, until a node has joined, the command that
 * connects it.
 */
export default function StandaloneClusterDetail({ cluster }: { cluster: KubeCluster }) {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const { t: tProviders } = useTranslation("providers");
  const facts: Array<[string, ReactNode]> = [
    [tProviders("standalone.connectedBy"), tProviders("standalone.byKubectl")],
    [t("drawer.nodeSelector"), cluster.node_selector ? <code>{cluster.node_selector}</code> : tProviders("standalone.allNodes")],
    [t("column.namespace"), <code>{cluster.namespace}</code>],
    [t("column.cardReturn"), cardReturnText(t, cluster.idle_return_seconds)],
    [t("column.createdAt"), formatTime(cluster.created_at)],
    [tProviders("clusters.field.nodes"), cluster.node_count > 0
      ? <Link className="text-fg-brand hover:underline" to={kubeClusterNodesPath(cluster.uuid)}>{joinedText(tProviders, t, tCommon, cluster)}</Link>
      : tProviders("clusters.nodes", { count: 0 })],
  ];
  return (
    // The detail steps in under its row, behind a rule, so it reads as the row's (ui-guidelines 4.1).
    <div className="ms-6 flex flex-col gap-3 border-s-2 border-border py-1 ps-3 [container-type:inline-size]">
      <FactList facts={facts} />
      {cluster.node_count === 0 && (
        <div className="flex max-w-[880px] flex-col gap-1.5">
          <p className="m-0 text-label text-fg-muted">{tProviders(cluster.revoked ? "standalone.revoked" : "standalone.waiting")}</p>
          {cluster.apply_command && !cluster.revoked && (
            <div className="flex items-start gap-1">
              <pre aria-label={t("actions.copyCommand")} className="code-block m-0 min-w-0 flex-1 overflow-x-auto text-label">{cluster.apply_command}</pre>
              <CopyButton size="small" label={t("actions.copyCommand")} text={cluster.apply_command} />
            </div>
          )}
        </div>
      )}
    </div>
  );
}
