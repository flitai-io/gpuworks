import { useState, type ReactElement } from "react";
import { useTranslation } from "react-i18next";
import PageHeader from "../components/PageHeader";
import CopyButton from "../components/CopyButton";
import ResourceTable, { type ResourceColumn } from "../components/resourceTable/ResourceTable";
import { useIsAdmin } from "../components/useIsAdmin";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { PageBody, PageContent, PageLayout } from "../components/ui/page-layout";
import { Popover, PopoverContent, PopoverTitle, PopoverTrigger } from "../components/ui/popover";
import { Tooltip } from "../components/ui/tooltip";
import { ConfirmPopover } from "../ui-kit/ConfirmPopover";
import { ConsoleIcon } from "../ui-kit/consoleIcons";
import { message } from "../ui-kit/feedback";
import { Notice } from "../ui-kit/Notice";
import { api, usePolling, type KubeCluster, type List } from "../api";
import { apiErrorText } from "../apiError";
import { formatTime } from "../i18n/datetime";
import { cardReturnText, kubeTroubleshootCommands, vendorsText } from "../kubeClusters";
import KubeClusterDrawer from "./kubernetes/KubeClusterDrawer";
import KubeManifestModal from "./kubernetes/KubeManifestModal";
import KubePoolsDrawer from "./kubernetes/KubePoolsDrawer";
import KubeClusterOperationModal from "./kubernetes/KubeClusterOperationModal";
import KubeUninstallModal from "./kubernetes/KubeUninstallModal";

/** Why a cluster's agents may not have joined, and the commands that tell which. */
function StalledHelp({ cluster }: { cluster: KubeCluster }) {
  const { t } = useTranslation("kubernetes");
  const commands = kubeTroubleshootCommands(cluster.namespace);
  return (
    <div className="flex max-w-[520px] flex-col gap-2">
      <p>{t("stalled.intro")}</p>
      <div className="flex items-start gap-1">
        <pre aria-label={t("stalled.copy")} className="code-block min-w-0 flex-1 overflow-x-auto text-label">{commands}</pre>
        <CopyButton size="small" label={t("stalled.copy")} text={commands} />
      </div>
      <strong className="font-semibold text-fg-default">{t("stalled.causes")}</strong>
      <ul className="m-0 list-disc ps-5">
        {(["pull", "runtimeClass", "selector", "connect"] as const).map((key) => <li key={key}>{t(`stalled.${key}`)}</li>)}
      </ul>
    </div>
  );
}

/** The Kubernetes clusters whose GPU nodes join this gpuworks, one line each. */
export default function KubernetesPage() {
  const { t } = useTranslation("kubernetes");
  const { t: tCommon } = useTranslation("common");
  const { t: tProviders } = useTranslation("providers");
  const isAdmin = useIsAdmin();
  const clusters = usePolling<List<KubeCluster>>(api.kubeClusters, 15000);
  const [adding, setAdding] = useState(false);
  const [manifestOf, setManifestOf] = useState<KubeCluster | null>(null);
  const [poolsOf, setPoolsOf] = useState<KubeCluster | null>(null);
  const [editing, setEditing] = useState<KubeCluster | null>(null);
  const [uninstalling, setUninstalling] = useState<KubeCluster | null>(null);
  const [operation, setOperation] = useState<{ cluster: KubeCluster; op: "apply" | "remove" } | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const run = async (name: string, action: () => Promise<unknown>, success: string) => {
    setBusy(name);
    setError(null);
    try {
      await action();
      message.success(success);
      void clusters.refetch();
    } catch (e) {
      setError(apiErrorText(tCommon, e));
    } finally {
      setBusy(null);
    }
  };

  const rowAction = (label: string, button: ReactElement) => (
    <Tooltip content={label}><span className="inline-flex">{button}</span></Tooltip>
  );

  const columns: ResourceColumn<KubeCluster>[] = [
    { key: "name", role: "name", title: t("column.name"), width: 160, sortValue: (c) => c.name, render: (_, c) => c.name },
    {
      key: "source",
      title: t("column.source"),
      width: 170,
      render: (_, c) => c.source
        ? (
          <div className="flex min-w-0 flex-col">
            <span className="truncate">{c.source.provider_name || tProviders(`kind.${c.source.provider_kind}`, { defaultValue: c.source.provider_kind })}</span>
            <span className="truncate text-label text-fg-muted">{c.source.cluster_id}</span>
            {c.pool_ids.length > 0 && <span className="truncate text-label text-fg-muted">{t("source.pools", { count: c.pool_ids.length })}</span>}
          </div>
        )
        : <span className="text-fg-muted">{t("source.byHand")}</span>,
    },
    { key: "namespace", title: t("column.namespace"), width: 160, defaultHidden: true, sortValue: (c) => c.namespace, render: (_, c) => <code>{c.namespace}</code> },
    { key: "nodes", title: t("column.nodes"), width: 90, align: "right", sortValue: (c) => c.node_count, render: (_, c) => c.node_count },
    { key: "vendors", title: t("column.vendors"), width: 200, sortValue: (c) => vendorsText(t, tCommon, c.vendors), render: (_, c) => vendorsText(t, tCommon, c.vendors) },
    {
      key: "cluster_uid",
      defaultHidden: true,
      title: t("column.clusterUid"),
      width: 300,
      render: (_, c) => c.cluster_uid ? <code>{c.cluster_uid}</code> : "-",
    },
    { key: "card_return", title: t("column.cardReturn"), width: 180, sortValue: (c) => c.idle_return_seconds, render: (_, c) => cardReturnText(t, c.idle_return_seconds) },
    {
      key: "status",
      title: t("column.status"),
      width: 180,
      sortValue: (c) => (c.revoked ? 2 : c.stalled ? 1 : 0),
      render: (_, c) => {
        if (c.revoked) return <Badge status="danger">{t("status.revoked")}</Badge>;
        // No node has joined for longer than the server allows: say how to
        // find out why, here where the cluster's state is read.
        if (c.stalled) {
          return (
            <div className="flex items-center gap-1">
              <Badge status="warning">{t("clusterUid.notEnrolled")}</Badge>
              <Popover>
                <PopoverTrigger render={<Button variant="link" size="sm">{t("clusterUid.troubleshoot")}</Button>} />
                <PopoverContent align="end" className="w-[560px] max-w-[calc(100vw-32px)]">
                  <PopoverTitle className="mb-2">{t("stalled.title")}</PopoverTitle>
                  <StalledHelp cluster={c} />
                </PopoverContent>
              </Popover>
            </div>
          );
        }
        if (!c.cluster_uid) return <Tooltip content={t("clusterUid.notEnrolledHint")}><Badge status="neutral">{t("clusterUid.notEnrolled")}</Badge></Tooltip>;
        return <Badge status="success">{t("status.active")}</Badge>;
      },
    },
    { key: "created_at", title: t("column.createdAt"), width: 170, defaultHidden: true, sortValue: (c) => c.created_at, render: (_, c) => formatTime(c.created_at) },
  ];
  if (isAdmin) {
    columns.push({
      key: "actions",
      role: "actions",
      title: t("column.actions"),
      width: 300,
      render: (_, c) => (
        <div className="flex items-center gap-0.5">
          {rowAction(t("actions.edit"), (
            <Button variant="ghost" size="sm" iconOnly aria-label={t("actions.edit")} onClick={() => setEditing(c)}><ConsoleIcon.edit size={16} /></Button>
          ))}
          {c.source && (
            <>
              {rowAction(t("actions.pools"), (
                <Button variant="ghost" size="sm" iconOnly aria-label={t("actions.pools")} onClick={() => setPoolsOf(c)}><ConsoleIcon.gpus size={16} /></Button>
              ))}
              {rowAction(c.can_apply || !c.operation_blocked ? t("actions.apply") : t(`blocked.${c.operation_blocked}`), (
                <Button variant="ghost" size="sm" iconOnly aria-label={t("actions.apply")} disabled={!c.can_apply} onClick={() => setOperation({ cluster: c, op: "apply" })}><ConsoleIcon.upload size={16} /></Button>
              ))}
              {rowAction(c.can_remove || !c.operation_blocked ? t("actions.remove") : t(`blocked.${c.operation_blocked}`), (
                <Button variant="ghost" size="sm" iconOnly className="text-fg-danger hover:text-fg-danger" aria-label={t("actions.remove")} disabled={!c.can_remove} onClick={() => setOperation({ cluster: c, op: "remove" })}><ConsoleIcon.unlink size={16} /></Button>
              ))}
            </>
          )}
          {!c.can_remove && (c.remove_commands?.length ?? 0) > 0 && rowAction(t("actions.uninstall"), (
            <Button variant="ghost" size="sm" iconOnly className="text-fg-danger hover:text-fg-danger" aria-label={t("actions.uninstall")} onClick={() => setUninstalling(c)}><ConsoleIcon.terminal size={16} /></Button>
          ))}
          {c.apply_command && <CopyButton size="small" label={t("actions.copyCommand")} text={c.apply_command} disabled={c.revoked} />}
          {rowAction(t("actions.manifest"), (
            <Button variant="ghost" size="sm" iconOnly aria-label={t("actions.manifest")} onClick={() => setManifestOf(c)}><ConsoleIcon.manifest size={16} /></Button>
          ))}
          {rowAction(t("actions.revoke"), (
            <ConfirmPopover
              destructive
              title={t("actions.revokeConfirm", { name: c.name })}
              confirmText={t("actions.revoke")}
              cancelText={t("actions.cancel")}
              onConfirm={() => run(`revoke-${c.uuid}`, () => api.revokeKubeCluster(c.uuid), t("actions.revoked", { name: c.name }))}
              trigger={<Button variant="ghost" size="sm" iconOnly className="text-fg-danger hover:text-fg-danger" aria-label={t("actions.revoke")} disabled={c.revoked} loading={busy === `revoke-${c.uuid}`}><ConsoleIcon.block size={16} /></Button>}
            ><p className="text-fg-muted">{t("actions.revokeHelp")}</p></ConfirmPopover>
          ))}
          {rowAction(t("actions.delete"), (
            <ConfirmPopover
              destructive
              title={t("actions.deleteConfirm", { name: c.name })}
              confirmText={t("actions.delete")}
              cancelText={t("actions.cancel")}
              onConfirm={() => run(`delete-${c.uuid}`, () => api.deleteKubeCluster(c.uuid), t("actions.deleted", { name: c.name }))}
              trigger={<Button variant="ghost" size="sm" iconOnly className="text-fg-danger hover:text-fg-danger" aria-label={t("actions.delete")} loading={busy === `delete-${c.uuid}`}><ConsoleIcon.delete size={16} /></Button>}
            >
              <p className="text-fg-muted">{t("actions.deleteHelp")}</p>
              {c.node_count > 0 && (
                <div className="mt-2 flex flex-col items-start gap-2">
                  <p className="text-fg-danger">{t("actions.deleteHasNodes", { count: c.node_count })}</p>
                  {(c.can_remove || (c.remove_commands?.length ?? 0) > 0) && (
                    <Button variant="secondary" size="sm" onClick={() => (c.can_remove ? setOperation({ cluster: c, op: "remove" }) : setUninstalling(c))}>
                      {t(c.can_remove ? "actions.remove" : "actions.uninstall")}
                    </Button>
                  )}
                </div>
              )}
            </ConfirmPopover>
          ))}
        </div>
      ),
    });
  }
  const addButton = isAdmin ? <Button variant="primary" leadingIcon={ConsoleIcon.add} onClick={() => setAdding(true)}>{t("page.add")}</Button> : undefined;

  return (
    <PageLayout gutter="none" className="h-auto gap-0">
      <PageHeader title={t("page.title")} description={t("page.description")} extra={addButton} />
      <PageContent><PageBody>
        {error && (
          <Notice
            tone="danger"
            title={t("actions.failed")}
            children={error}
            action={<Button variant="ghost" size="sm" iconOnly aria-label={tCommon("actions.close")} onClick={() => setError(null)}><ConsoleIcon.close size={16} /></Button>}
          />
        )}
        <div className="rounded-xl border border-border-subtle bg-surface-floating p-4">
          <ResourceTable<KubeCluster>
            tableId="kube-clusters"
            rowKey="uuid"
            columns={columns}
            data={clusters.data?.items ?? []}
            loading={clusters.loading && !clusters.data}
            error={clusters.error}
            onRefresh={() => void clusters.refetch()}
            updatedAt={clusters.updatedAt}
            emptyDescription={t("page.empty")}
            emptyAction={addButton}
          />
        </div>
      </PageBody></PageContent>
      <KubeClusterDrawer open={adding} onClose={() => setAdding(false)} onCreated={() => void clusters.refetch()} />
      {editing && <KubeClusterDrawer key={editing.uuid} open edit={editing} onClose={() => setEditing(null)} onCreated={() => void clusters.refetch()} onEdited={() => void clusters.refetch()} />}
      <KubePoolsDrawer cluster={poolsOf} onClose={() => setPoolsOf(null)} onSaved={() => void clusters.refetch()} />
      {uninstalling && <KubeUninstallModal cluster={uninstalling} onClose={() => setUninstalling(null)} />}
      {manifestOf && <KubeManifestModal cluster={manifestOf} onClose={() => setManifestOf(null)} />}
      {operation && <KubeClusterOperationModal cluster={operation.cluster} op={operation.op} onClose={() => setOperation(null)} onDone={() => void clusters.refetch()} />}
    </PageLayout>
  );
}
