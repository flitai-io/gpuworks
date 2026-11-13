import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type CloudProvider, type KubeCluster, type ProviderInstance, type ProviderKindInfo } from "../../api";
import ProviderInstallModal from "../../components/ProviderInstallModal";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Table, TableBody, TableHead, TableHeader, TableRow } from "../../components/ui/table";
import { message } from "../../ui-kit/feedback";
import { Notice } from "../../ui-kit/Notice";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { formatTime } from "../../i18n/datetime";
import { Spinner } from "../../ui-kit/Spinner";
import KubeClusterDrawer from "../kubernetes/KubeClusterDrawer";
import ClusterGroup from "./ClusterGroup";
import { SpanRow, TableGroup } from "./GroupRows";
import InstanceRow, { WIDE_ONLY, type InstanceColumns } from "./InstanceRow";
import { PlatformResourceBadges } from "./PlatformResourceBadge";
import { useAccountClusters } from "./useAccountClusters";

// An account can hold hundreds of instances: its group shows this many, then more on request.
const INSTANCE_STEP = 10;
const INSTANCES_GROUP = "instances";

/**
 * What an account holds, under its row, in one table grouped by what the
 * machines belong to: the account's own instances, then each of its managed
 * Kubernetes clusters with the GPU node pools to connect. A kind without
 * managed Kubernetes has its instances alone, ungrouped. Instances are as the
 * last sync found them; clusters are read from the cloud when the row opens.
 */
export default function ProviderResources({ provider, kindInfo, refreshKey, entries, onEntriesChanged, admin, focusCluster, onSync, syncing = false }: {
  provider: CloudProvider;
  kindInfo?: ProviderKindInfo;
  /** Changes when the account's instances were synced: they are read again. */
  refreshKey: string;
  /** The cluster entries the page polls, by UUID. */
  entries: Record<string, KubeCluster>;
  onEntriesChanged: () => void;
  admin: boolean;
  /** The cluster entry (UUID) to bring into view once the clusters are read, as a compute node's source asks. */
  focusCluster?: string | null;
  /** Syncs the account's instances from the vendor, as the row's own sync does; absent for a kind that cannot list them. */
  onSync?: () => void;
  syncing?: boolean;
}) {
  const { t } = useTranslation("providers");
  const { t: tCommon } = useTranslation("common");
  const root = useRef<HTMLDivElement>(null);
  const [instances, setInstances] = useState<ProviderInstance[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [shown, setShown] = useState(INSTANCE_STEP);
  const [installFor, setInstallFor] = useState<ProviderInstance | null>(null);
  const [powering, setPowering] = useState<string | null>(null);
  const [folded, setFolded] = useState<ReadonlySet<string>>(new Set());

  const resources = provider.resources ?? [];
  const instanceResources = resources.filter((resource) => resource.type !== "kubernetes");
  // The account has clusters when its kind lists them. The kind schema says so, and so does the account's own row
  // when the schema could not be loaded; an account that already holds entries always shows them.
  const hasClusters = !!kindInfo?.can_list_clusters || resources.some((resource) => resource.type === "kubernetes")
    || Object.values(entries).some((entry) => entry.source?.provider_id === provider.id);
  const clusters = useAccountClusters({ providerId: provider.id, providerName: provider.name, enabled: hasClusters, onChanged: onEntriesChanged });

  const load = () => {
    setLoadError(null);
    api.providerInstances(provider.id)
      .then((list) => setInstances(list.items))
      .catch((error: Error) => { setInstances([]); setLoadError(error.message); });
  };
  useEffect(load, [provider.id, refreshKey]);
  // A sync changes the refresh key: the clusters are read from the cloud again with it, so one sync brings both up to date.
  const firstKey = useRef(refreshKey);
  useEffect(() => {
    if (refreshKey !== firstKey.current) clusters.load();
  }, [refreshKey]);

  const power = async (inst: ProviderInstance, action: "on" | "off") => {
    setPowering(`${inst.external_id}:${action}`);
    try {
      await api.powerProviderInstance(provider.id, inst.external_id, action);
      message.success(t(action === "on" ? "instances.message.powerOnSent" : "instances.message.powerOffSent"));
      load();
    } catch (e) {
      message.error(t("instances.message.actionFailed", { detail: (e as Error).message }));
    } finally {
      setPowering(null);
    }
  };

  // The cluster a compute node's source points at is brought into view once, when the cloud has answered.
  const focused = useRef(false);
  useEffect(() => {
    if (!focusCluster || focused.current || clusters.clusters === null) return;
    focused.current = true;
    root.current?.querySelector(`[data-cluster-entry="${CSS.escape(focusCluster)}"]`)?.scrollIntoView?.({ block: "center" });
  }, [focusCluster, clusters.clusters]);

  const columns: InstanceColumns = {
    price: kindInfo?.capabilities.has_pricing === true,
    region: provider.category !== "private_cloud",
    // Said per instance only where the account's instances are not all of one kind.
    type: instanceResources.length !== 1,
  };
  const titles: Array<{ title: string; className?: string }> = [
    { title: t("instances.column.instance") }, { title: t("instances.column.gpu") }, { title: t("instances.column.status") },
    ...(columns.price ? [{ title: t("instances.column.price") }] : []),
    ...(columns.region ? [{ title: t("instances.column.region"), className: WIDE_ONLY }] : []),
    ...(columns.type ? [{ title: t("instances.column.type") }] : []),
    { title: t("instances.column.sshConnection") }, { title: tCommon("resourceTable.actions") },
  ];
  const colSpan = titles.length;
  const toggle = (group: string) => setFolded((current) => {
    const next = new Set(current);
    if (!next.delete(group)) next.add(group);
    return next;
  });

  const items = instances ?? [];
  const instanceRows = (
    <>
      {instances === null && <SpanRow colSpan={colSpan}><Spinner label={t("detail.loadingInstances")} /></SpanRow>}
      {instances !== null && items.length === 0 && !loadError && <SpanRow colSpan={colSpan} className="text-center text-fg-muted">{t("instances.empty")}</SpanRow>}
      {items.slice(0, shown).map((inst, index) => (
        <InstanceRow key={inst.external_id} inst={inst} index={index} columns={columns} indent={hasClusters}
          powering={powering === `${inst.external_id}:on` ? "on" : powering === `${inst.external_id}:off` ? "off" : null}
          onPower={(action) => power(inst, action)} onInstall={() => setInstallFor(inst)} />
      ))}
      {items.length > shown && (
        <SpanRow colSpan={colSpan} className={hasClusters ? "ps-11" : undefined}>
          <Button variant="link" size="sm" onClick={() => setShown(shown + INSTANCE_STEP)}>{t("detail.showMore", { count: Math.min(INSTANCE_STEP, items.length - shown), rest: items.length - shown })}</Button>
        </SpanRow>
      )}
    </>
  );

  // The account's entries the cloud's listing does not account for: their cluster is gone from the account, or the
  // listing failed. They and their nodes are still here, so they are listed and can still be revoked, removed or deleted.
  const listed = new Set((clusters.clusters ?? []).map((c) => c.external_id));
  const unlisted = clusters.clusters === null ? [] : Object.values(entries)
    .filter((entry) => entry.source?.provider_id === provider.id && !listed.has(entry.source.cluster_id))
    .sort((a, b) => a.name.localeCompare(b.name));
  const joined = items.filter((inst) => inst.node_id !== 0).length;

  return (
    // The detail steps in under its row, behind a rule, so it reads as the row's (ui-guidelines 4.1).
    <div ref={root} className="ms-6 flex flex-col gap-2 border-s-2 border-border ps-3 [container-type:inline-size]">
      {/* Syncing lists the account's instances from the vendor again, and with them its clusters. */}
      <div className="flex min-h-7 flex-wrap items-center gap-2 text-body text-fg-muted">
        {onSync && <Button size="sm" variant="secondary" leadingIcon={ConsoleIcon.refresh} onClick={onSync} loading={syncing}>{t("table.sync")}</Button>}
        {/* A kind that cannot list its instances is never synced: it says nothing about it. */}
        {onSync && <span>{provider.synced_at ? t("detail.syncedAt", { time: formatTime(provider.synced_at) }) : t("detail.neverSynced")}</span>}
      </div>
      {/* Said where it applies: for a kind that connects container instances, or one whose resources are not known. */}
      {(!kindInfo?.resources?.length || kindInfo.resources.some((resource) => resource.type === "container")) && <Notice tone="info" title={t("instances.containerRuntimeNote")} />}
      {loadError && <Notice tone="danger" title={t("instances.loadFailed")} children={loadError} action={<Button variant="secondary" size="sm" onClick={load}>{t("instances.retry")}</Button>} />}
      {clusters.loadError && <Notice tone="danger" title={t("clusters.loadFailed")} children={clusters.loadError} action={<Button variant="secondary" size="sm" onClick={clusters.load}>{t("instances.retry")}</Button>} />}
      <Table variant="bordered" size="md" aria-busy={instances === null || (hasClusters && clusters.clusters === null)}>
        <TableHeader>
          <TableRow>{titles.map(({ title, className }) => <TableHead key={title} className={`whitespace-nowrap ${className ?? ""}`}>{title}</TableHead>)}</TableRow>
        </TableHeader>
        {!hasClusters ? <TableBody>{instanceRows}</TableBody> : (
          <>
            <TableGroup
              name={t("detail.standaloneInstances")}
              colSpan={colSpan}
              open={!folded.has(INSTANCES_GROUP)}
              onToggle={() => toggle(INSTANCES_GROUP)}
              title={
                <>
                  <strong className="font-semibold text-fg-default">{t("detail.standaloneInstances")}</strong>
                  {instanceResources.length > 0 && <PlatformResourceBadges resources={instanceResources} />}
                  {instances !== null && <Badge size="sm">{items.length}</Badge>}
                </>
              }
              extra={joined > 0 && <span className="text-body text-fg-muted">{t("clusters.field.nodes")} {joined}</span>}
            >
              {instanceRows}
            </TableGroup>
            {clusters.clusters === null && <TableBody><SpanRow colSpan={colSpan}><Spinner label={t("detail.loadingClusters")} /></SpanRow></TableBody>}
            {clusters.clusters !== null && clusters.clusters.length === 0 && unlisted.length === 0 && !clusters.loadError && (
              <TableBody><SpanRow colSpan={colSpan} className="text-center text-fg-muted">{t("clusters.empty")}</SpanRow></TableBody>
            )}
            {(clusters.clusters ?? []).map((cluster) => (
              <ClusterGroup key={cluster.external_id} cluster={cluster} entry={cluster.entry ? entries[cluster.entry.uuid] : undefined}
                colSpan={colSpan} open={!folded.has(cluster.external_id)} onToggle={() => toggle(cluster.external_id)} clusters={clusters} admin={admin} />
            ))}
            {unlisted.map((entry) => (
              <ClusterGroup key={entry.uuid} entry={entry} unread={!!clusters.loadError}
                colSpan={colSpan} open={!folded.has(entry.uuid)} onToggle={() => toggle(entry.uuid)} clusters={clusters} admin={admin} />
            ))}
          </>
        )}
      </Table>
      <ProviderInstallModal
        providerId={provider.id}
        providerKind={kindInfo?.kind}
        defaultServerDialListen={kindInfo?.default_server_dial_listen}
        serverDialSupported={kindInfo?.server_dial_supported ?? true}
        serverDialAutoPort={kindInfo?.server_dial_auto_port ?? false}
        inst={installFor}
        onClose={() => setInstallFor(null)}
        onDone={() => { setInstallFor(null); load(); }}
      />
      <KubeClusterDrawer open={clusters.connecting !== null} prefill={clusters.connecting ?? undefined} onClose={clusters.closeConnect} onCreated={clusters.reload} />
    </div>
  );
}
