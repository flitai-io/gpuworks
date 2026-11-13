import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { KubeCluster, ProviderCluster } from "../../api";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Tooltip } from "../../components/ui/tooltip";
import { formatTime } from "../../i18n/datetime";
import { localizedText } from "../../i18n/localizedText";
import { cardReturnText } from "../../kubeClusters";
import { kubeClusterNodesPath } from "../../nodeSources";
import KubeEntryActions from "../kubernetes/KubeEntryActions";
import KubeEntryStatus from "../kubernetes/KubeEntryStatus";
import KubeNamespaceShared from "../kubernetes/KubeNamespaceShared";
import { poolText } from "../kubernetes/KubePoolPicker";
import KubePoolsDrawer from "../kubernetes/KubePoolsDrawer";
import FactList from "./FactList";
import { SpanRow, TableGroup } from "./GroupRows";
import type { AccountClusters } from "./useAccountClusters";
import { joinedText } from "./joinedText";

/**
 * One managed Kubernetes cluster of a cloud account, as a group of the
 * account's table. The header names the cluster and says where its entry
 * stands, with what can be done to it: connecting it (its node pools are
 * chosen in the connect form), or an administrator's actions on its entry.
 * Under it, what the cluster is, as "label value" pairs: nothing to choose
 * here. The cluster's nodes are not listed: once joined they are compute
 * nodes, which the node count opens.
 *
 * `cluster` is the cloud's listing of it and `entry` the cluster entry made
 * from it. An entry the listing does not account for (its cluster is gone
 * from the account, or the listing failed) has no `cluster`: it is still
 * listed, so it can be revoked, removed or deleted.
 */
export default function ClusterGroup({ cluster, entry, unread = false, colSpan, open, onToggle, clusters, admin }: {
  cluster?: ProviderCluster;
  entry?: KubeCluster;
  /** The listing failed, so an entry without a cluster may still have one in the cloud. */
  unread?: boolean;
  colSpan: number;
  open: boolean;
  onToggle: () => void;
  clusters: AccountClusters;
  admin: boolean;
}) {
  const { t, i18n } = useTranslation("providers");
  const { t: tKube } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const language = i18n.resolvedLanguage || i18n.language;
  const [editingPools, setEditingPools] = useState(false);
  const name = cluster?.name ?? entry?.name ?? "";
  const id = cluster?.external_id ?? entry?.source?.cluster_id;

  // Where the cluster stands: its entry's state, or that it has none yet.
  let status: ReactNode;
  if (entry) status = <KubeEntryStatus cluster={entry} />;
  // The entry list may not have answered yet: the cluster is connected, and that is all that is known.
  else if (cluster?.entry) status = <Badge status="success">{t("clusters.connected")}</Badge>;
  else if (cluster && !cluster.running) status = <Badge status="neutral">{cluster.state}</Badge>;
  else status = <Badge status="neutral">{t("table.state.pending")}</Badge>;
  // A running cluster with GPU node pools and no entry yet can be connected.
  // Connecting makes an enrollment token, which only an administrator may.
  const connectable = admin && !!cluster && !cluster.entry && cluster.running && cluster.gpu_pools.length > 0;

  // The GPU node pools: those the entry holds once connected, all of the cluster's until then.
  let pools: ReactNode;
  if (!cluster) pools = <span className="text-fg-muted">{t(unread ? "clusters.entryUnread" : "clusters.entryGone")}</span>;
  else if (!cluster.entry && !cluster.running) pools = <span className="text-fg-muted">{t("clusters.notRunning")}</span>;
  else if (cluster.gpu_pools.length === 0) pools = <span className="text-fg-muted">{t("clusters.noGPUPools")}</span>;
  else {
    const shown = cluster.entry ? cluster.gpu_pools.filter((p) => p.in_entry) : cluster.gpu_pools;
    // An entry from before node pools selects by a node selector until pools are saved for it.
    const legacy = entry && entry.pool_ids.length === 0 ? entry.node_selector : "";
    pools = (
      <span className="flex flex-col gap-0.5">
        {shown.map((p) => <span key={p.id}>{poolText(t, p)}</span>)}
        {legacy && <span className="text-fg-muted">{tKube("pools.legacyHelp", { selector: legacy })}</span>}
        {/* The pools an entry holds change in a drawer, then the manifest is applied again. */}
        {admin && entry && <span><Button variant="link" size="sm" onClick={() => setEditingPools(true)}>{t("clusters.editPools")}</Button></span>}
      </span>
    );
  }

  const facts: Array<[string, ReactNode]> = [];
  if (id) facts.push([t("clusters.field.id"), <span className="break-all">{id}</span>]);
  if (cluster?.region) facts.push([t("clusters.field.region"), cluster.region]);
  if (cluster?.version) facts.push([t("clusters.field.version"), cluster.version]);
  if (cluster) {
    facts.push([t("clusters.field.api"), cluster.public_api
      ? t("clusters.publicAPI")
      : <Tooltip content={t("clusters.privateAPIHint")}><Badge size="sm" status="warning" tabIndex={0}>{t("clusters.privateAPI")}</Badge></Tooltip>]);
  }
  facts.push([t("clusters.column.gpuPools"), pools]);
  if (entry) {
    facts.push([t("clusters.field.nodes"), entry.node_count > 0
      ? <Link className="text-fg-brand hover:underline" to={kubeClusterNodesPath(entry.uuid)}>{joinedText(t, tKube, tCommon, entry)}</Link>
      : t("clusters.nodes", { count: 0 })]);
    facts.push([tKube("column.namespace"), <code>{entry.namespace}</code>]);
    facts.push([tKube("column.cardReturn"), cardReturnText(tKube, entry.idle_return_seconds)]);
    facts.push([tKube("column.createdAt"), formatTime(entry.created_at)]);
  }

  return (
    <TableGroup
      name={name}
      colSpan={colSpan}
      open={open}
      onToggle={onToggle}
      data-cluster-entry={entry?.uuid}
      title={
        <>
          <strong className="min-w-0 truncate font-semibold text-fg-default">{name}</strong>
          {/* The vendor's product the cluster is; a cluster the driver cannot name says nothing. */}
          {cluster?.type && <Badge size="sm" color="blue">{localizedText(cluster.type_label, language) || cluster.type}</Badge>}
          {entry && <KubeNamespaceShared cluster={entry} />}
        </>
      }
      extra={
        <>
          {status}
          {connectable && <Button size="sm" variant="primary" onClick={() => clusters.connect(cluster)}>{t("clusters.connect")}</Button>}
          {admin && entry && <KubeEntryActions cluster={entry} onChanged={clusters.reload} />}
        </>
      }
    >
      <SpanRow colSpan={colSpan} className="ps-11">
        {/* The facts' own box is the size container: a table cell cannot be one. */}
        <div className="[container-type:inline-size]"><FactList facts={facts} /></div>
      </SpanRow>
      {editingPools && entry && <KubePoolsDrawer cluster={entry} onClose={() => setEditingPools(false)} onSaved={clusters.reload} />}
    </TableGroup>
  );
}
