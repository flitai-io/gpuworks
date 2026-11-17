import { useEffect, useMemo, useState } from "react";
import {
  api,
  usePolling,
  type CloudProvider,
  type KindResource,
  type KubeCluster,
  type List,
  type ProviderCategoryInfo,
  type ProviderKindInfo,
} from "../api";
import PageHeader from "../components/PageHeader";
import ResourceTable, { type ResourceColumn } from "../components/resourceTable/ResourceTable";
import ProviderConfigModal from "../components/ProviderConfigModal";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import { Container } from "../components/ui/container";
import { Tooltip } from "../components/ui/tooltip";
import { useIsAdmin } from "../components/useIsAdmin";
import { ConsoleIcon } from "../ui-kit/consoleIcons";
import { confirm, message } from "../ui-kit/feedback";
import { Notice } from "../ui-kit/Notice";
import { formatTime } from "../i18n/datetime";
import { useTranslation } from "react-i18next";
import { Link, useSearchParams } from "react-router-dom";
import type { TFunction } from "i18next";
import type { LocalizedText } from "../i18n/localizedText";
import { localizedText } from "../i18n/localizedText";
import { kubeClusterNodesPath, platformNodesPath } from "../nodeSources";
import KubeEntryActions from "./kubernetes/KubeEntryActions";
import KubeEntryStatus from "./kubernetes/KubeEntryStatus";
import KubeNamespaceShared from "./kubernetes/KubeNamespaceShared";
import { PlatformResourceBadges, resourceTypeLabel } from "./platforms/PlatformResourceBadge";
import ProviderKindLogo from "./platforms/ProviderKindLogo";
import ProviderResources from "./platforms/ProviderResources";
import RowActions from "./platforms/RowActions";
import StandaloneClusterDetail from "./platforms/StandaloneClusterDetail";
import {
  platformRows, providerRowKey, rowCategory, rowKind, rowName, rowNodeCount, rowResources, rowState, rowUpdatedAt,
  type PlatformRow,
} from "./platforms/platformRows";

const PROVIDER_STATES: Record<string, "success" | "danger"> = { valid: "success", invalid: "danger" };

// One list for everything that hands machines over: the accounts of cloud
// vendors, each row carrying the category and resources the server derives
// from its kind, and the Kubernetes clusters connected on their own. A
// cluster connected from an account is listed under that account.
// ?category=<type> and ?resource=<type> open the list filtered, as the
// overview's cloud counts and the old /zstack and /kubernetes addresses do;
// ?open=<row> opens one row, with &cluster=<entry> bringing that cluster of
// the account into view, as a compute node's source does.
export default function CloudProvidersPage() {
  const { t, i18n } = useTranslation("providers");
  const isAdmin = useIsAdmin();
  const [searchParams, setSearchParams] = useSearchParams();
  // A type the console has no name for is not a type: the list opens unfiltered.
  const named = (param: string, prefix: string) => {
    const value = searchParams.get(param);
    return value && i18n.exists(`providers:${prefix}.${value}`) ? value : null;
  };
  const openedCategory = named("category", "category");
  const openedResource = named("resource", "resource");
  const openedRow = searchParams.get("open");
  const openedCluster = searchParams.get("cluster");
  const language = i18n.resolvedLanguage || i18n.language;
  const { data: providers, refetch, loading, error } = usePolling<List<CloudProvider>>(api.providers, 15000);
  const clusters = usePolling<List<KubeCluster>>(api.kubeClusters, 15000);
  const data = providers?.items ?? [];
  const rows = useMemo(() => platformRows(providers?.items ?? [], clusters.data?.items ?? []), [providers, clusters.data]);
  const entries = useMemo(() => Object.fromEntries((clusters.data?.items ?? []).map((cluster) => [cluster.uuid, cluster])), [clusters.data]);
  const refetchAll = () => { void refetch(); void clusters.refetch(); };
  const [modalOpen, setModalOpen] = useState(false);
  const [defaultKind, setDefaultKind] = useState<string | null>(null);
  const [editingProvider, setEditingProvider] = useState<CloudProvider | null>(null);
  const [syncing, setSyncing] = useState<number | null>(null);
  const [syncingAll, setSyncingAll] = useState(false);
  const [instanceRefreshVersion, setInstanceRefreshVersion] = useState(0);
  const [kindInfos, setKindInfos] = useState<ProviderKindInfo[]>([]);
  const [categories, setCategories] = useState<ProviderCategoryInfo[]>([]);
  // The platform type a cluster connected on its own is added as, as the server offers it.
  const clusterCategory = categories.find((category) => category.kube_cluster);

  useEffect(() => {
    api
      .providerKinds()
      .then((infos) => { setKindInfos(infos.items); setCategories(infos.categories ?? []); })
      .catch(() => { setKindInfos([]); setCategories([]); });
  }, []);
  const kindInfoOf = (kind: string) => kindInfos.find((info) => info.kind === kind);
  // An account is synced when its kind lists instances; a kind the server does not say otherwise of can be.
  const canSync = (kind: string) => kindInfoOf(kind)?.capabilities.can_list_instances !== false;

  const openAdd = (kind?: string) => {
    setEditingProvider(null);
    setDefaultKind(kind ?? null);
    setModalOpen(true);
  };

  const openEdit = (provider: CloudProvider) => {
    setDefaultKind(null);
    setEditingProvider(provider);
    setModalOpen(true);
  };

  const closeModal = () => {
    setModalOpen(false);
    setDefaultKind(null);
    setEditingProvider(null);
  };

  const doSync = async (id: number) => {
    setSyncing(id);
    try {
      const res = await api.syncProvider(id);
      message.success(t("message.syncDone", { count: res.count }));
      await refetch();
      setInstanceRefreshVersion((version) => version + 1);
    } catch (e) {
      message.error(t("message.syncFailed", { detail: (e as Error).message }));
    } finally {
      setSyncing(null);
    }
  };

  const doSyncAll = async () => {
    const providers = data.filter((provider) => canSync(provider.kind));
    if (providers.length === 0) return;
    setSyncingAll(true);
    try {
      let ok = 0;
      let failed = 0;
      for (const provider of providers) {
        try {
          await api.syncProvider(provider.id);
          ok += 1;
        } catch {
          failed += 1;
        }
      }
      await refetch();
      setInstanceRefreshVersion((version) => version + 1);
      if (failed === 0) {
        message.success(t("message.syncAllDone", { count: ok }));
      } else {
        message.warning(t("message.syncAllPartial", { ok, failed }));
      }
    } finally {
      setSyncingAll(false);
    }
  };

  const summary = {
    total: rows.length,
    invalid: data.filter((p) => p.state === "invalid").length,
    nodes: rows.reduce((sum, row) => sum + rowNodeCount(row), 0),
  };
  // The latest of the accounts' syncs, beside the button that syncs them all.
  const lastSynced = data.reduce<string | undefined>((latest, provider) => provider.synced_at && (!latest || provider.synced_at > latest) ? provider.synced_at : latest, undefined);
  const syncableCount = data.filter((provider) => canSync(provider.kind)).length;

  const kindLabel = (kind: string) => providerKindLabel(t, kind, kindInfoOf(kind)?.label, language);
  // No rows, no choices: a filter appears with the first platform. A filter the address opened is offered even before a row has it.
  const present = (values: string[], opened: string | null) => [...new Set([...values.filter(Boolean), ...(opened ? [opened] : [])])];
  const kindsPresent = present(rows.map(rowKind), null);
  const categoriesPresent = present(rows.map(rowCategory), openedCategory);
  const resourcesPresent = present(rows.flatMap((row) => rowResources(row).map((resource) => resource.type)), openedResource);
  const statesPresent = present(rows.map(rowState), null);
  const deleteProvider = async (provider: CloudProvider) => {
    try {
      await api.deleteProvider(provider.id);
      message.success(t("message.deleted"));
      refetchAll();
    } catch (e) {
      message.error(t("message.deleteFailed", { detail: (e as Error).message }));
    }
  };
  // Deleting an account says what becomes of what came through it: nothing in the cloud is released, and the
  // clusters connected from it stay as entries made by hand (the server keeps them so, see DELETE /providers/{id}).
  const confirmDelete = (provider: CloudProvider) => {
    const fromIt = (clusters.data?.items ?? []).filter((cluster) => cluster.source?.provider_id === provider.id).length;
    confirm({
      title: t("table.deleteConfirm", { name: provider.name }),
      okText: t("table.delete"),
      cancelText: t("table.cancel"),
      okButtonProps: { danger: true },
      content: (
        <div className="flex flex-col gap-2">
          <p className="m-0 text-fg-muted">{t("table.deleteConsequence")}</p>
          {fromIt > 0 && <p className="m-0 text-fg-muted">{t("table.deleteClusterConsequence", { count: fromIt })}</p>}
        </div>
      ),
      onOk: () => deleteProvider(provider),
    });
  };
  const columns: ResourceColumn<PlatformRow>[] = [
    // The name is the row's name and nothing else: editing is the pencil in the row's actions. A local cluster that may
    // share its namespace with another entry of the same cluster says so beside its name.
    {
      key: "name", role: "name", title: t("table.column.name"), width: 110, sortValue: rowName,
      render: (_: unknown, row: PlatformRow) => row.type === "cluster" && row.cluster.namespace_shared_with?.length
        ? <span className="flex min-w-0 items-center gap-1"><span className="truncate">{row.cluster.name}</span><KubeNamespaceShared cluster={row.cluster} /></span>
        : rowName(row) || "—",
    },
    {
      key: "kind",
      title: t("table.column.type"),
      width: 132,
      filters: kindsPresent.length ? kindsPresent.map((kind) => ({ value: kind, text: kindLabel(kind) })) : undefined,
      onFilter: (value, row) => rowKind(row) === value,
      // A cluster connected on its own is no vendor's; the vendor filter is the accounts'.
      render: (_: unknown, row: PlatformRow) => rowKind(row) ? <Badge color="blue">{kindLabel(rowKind(row))}</Badge> : "—",
    },
    {
      key: "category",
      title: t("table.column.category"),
      width: 104,
      filters: categoriesPresent.length ? categoriesPresent.map((category) => ({ value: category, text: t(`category.${category}`, { defaultValue: category }) })) : undefined,
      defaultFilteredValue: openedCategory ? [openedCategory] : null,
      onFilter: (value, row) => rowCategory(row) === value,
      render: (_: unknown, row: PlatformRow) => rowCategory(row) ? t(`category.${rowCategory(row)}`, { defaultValue: rowCategory(row) }) : "—",
    },
    {
      // What the platform can hand over, by type; how much it has is under the row.
      key: "resources",
      title: <Tooltip content={t("table.help.resources")}><span className="header-help">{t("table.column.resources")}</span></Tooltip>,
      label: t("table.column.resources"),
      width: 176,
      filters: resourcesPresent.length ? resourcesPresent.map((resource) => ({ value: resource, text: resourceTypeLabel(t, resource) })) : undefined,
      defaultFilteredValue: openedResource ? [openedResource] : null,
      onFilter: (value, row) => rowResources(row).some((resource) => resource.type === value),
      render: (_: unknown, row: PlatformRow) => {
        const resources = rowResources(row);
        return resources.length ? <PlatformResourceBadges resources={resources} /> : "—";
      },
    },
    {
      // The compute nodes that joined through the platform; the count opens the node list on them.
      key: "nodes",
      title: <Tooltip content={t("table.help.nodes")}><span className="header-help">{t("table.column.nodes")}</span></Tooltip>,
      label: t("table.column.nodes"),
      width: 96,
      align: "right",
      render: (_: unknown, row: PlatformRow) => {
        const count = rowNodeCount(row);
        if (count === 0) return "0";
        // A count that can be clicked reads as a number until the pointer is on it (ui-guidelines 7.2b).
        return <Link className="text-inherit hover:text-fg-brand hover:underline" to={row.type === "provider" ? platformNodesPath(row.provider.id) : kubeClusterNodesPath(row.cluster.uuid)}>{count}</Link>;
      },
    },
    { key: "updated_at", title: t("table.column.updatedAt"), width: 156, defaultHidden: true, sortValue: rowUpdatedAt, render: (_: unknown, row: PlatformRow) => formatTime(rowUpdatedAt(row)) },
    {
      key: "state",
      title: t("table.column.status"),
      width: 108,
      filters: statesPresent.length ? statesPresent.map((state) => ({ value: state, text: t(`table.state.${state}`, { defaultValue: state }) })) : undefined,
      onFilter: (value, row) => rowState(row) === value,
      render: (_: unknown, row: PlatformRow) => {
        if (row.type === "cluster") return <KubeEntryStatus cluster={row.cluster} />;
        const state = row.provider.state;
        return state ? <Badge status={PROVIDER_STATES[state] ?? "neutral"}>{t(`table.state.${state}`, { defaultValue: state })}</Badge> : "—";
      },
    },
    {
      key: "action",
      role: "actions",
      // Three icon buttons at most: a row with more folds the rest into a menu.
      width: 120,
      render: (_: unknown, row: PlatformRow) => {
        if (row.type === "cluster") return isAdmin ? <KubeEntryActions cluster={row.cluster} onChanged={() => void clusters.refetch()} /> : null;
        const p = row.provider;
        const syncable = canSync(p.kind);
        return (
          <RowActions label={p.name} actions={[
            { key: "edit", label: t("table.edit"), icon: ConsoleIcon.edit, common: true, onSelect: () => openEdit(p) },
            { key: "sync", label: t("table.sync"), icon: ConsoleIcon.refresh, common: true, disabled: !syncable, hint: syncable ? undefined : t("table.comingSoon"), loading: syncing === p.id, onSelect: () => void doSync(p.id) },
            { key: "delete", label: t("table.delete"), icon: ConsoleIcon.delete, danger: true, onSelect: () => confirmDelete(p) },
          ]} />
        );
      },
    },
  ];

  // The quick-connect cards: every vendor the server knows, then a cluster connected on its own. The whole card
  // adds the platform; the vendor's site is the small mark beside its name.
  const kindCard = (kind: string, label: string, resources: KindResource[], onAdd: () => void, homepage?: string) => {
    const site = homepage?.replace(/^https?:\/\//, "").replace(/\/$/, "");
    return (
      <div key={kind} className="provider-kind-card">
        <div className="provider-kind-name">
          <button type="button" onClick={onAdd} className="provider-kind-add" aria-label={t("page.addKind", { kind: label })}>
            <ProviderKindLogo kind={kind} />
            <strong className="truncate text-body font-semibold text-fg-default">{label}</strong>
          </button>
          {homepage && (
            <Tooltip content={t("page.homepageOf", { site })}>
              <a href={homepage} target="_blank" rel="noopener noreferrer" className="provider-kind-homepage" aria-label={t("page.homepageOf", { site })}>
                <ConsoleIcon.external size={14} aria-hidden />
              </a>
            </Tooltip>
          )}
        </div>
        {resources.length > 0 && <PlatformResourceBadges resources={resources} />}
      </div>
    );
  };

  return (
    <>
      <PageHeader
        title={t("platforms.title")}
        description={t("platforms.description")}
        extra={
          <>
            <span className="text-body text-fg-muted">{t("page.count", { count: rows.length })}</span>
            <Button variant="primary" onClick={() => openAdd()}>{t("page.add")}</Button>
          </>
        }
      />

      <section className="platform-block">
        <h2 className="platform-block-title">{t("page.supportedTypes")}</h2>
        {error && <div className="mb-3"><Notice tone="warning" title={t("page.listFailed")} children={error.message} /></div>}
        {kindInfos.length === 0 && !error && (
          <div className="mb-3"><Notice tone="info" title={t("page.schemaMissing")} children={t("page.schemaMissingDescription")} /></div>
        )}
        {(kindInfos.length > 0 || (isAdmin && clusterCategory)) && (
          <Container className="rounded-2xl">
            <div className="provider-kind-grid">
              {kindInfos.map((kind) => kindCard(kind.kind, providerKindLabel(t, kind.kind, kind.label, language), kind.resources ?? [], () => openAdd(kind.kind), kind.homepage))}
              {/* Connecting a cluster makes an enrollment token, which only an administrator may. */}
              {isAdmin && clusterCategory && kindCard(clusterCategory.id, t("page.otherKubernetes"), clusterCategory.resources ?? [], () => openAdd(clusterCategory.id))}
            </div>
          </Container>
        )}
      </section>

      <section className="platform-block">
        <div className="platform-block-head">
          <h2 className="platform-block-title">{t("page.connected")}</h2>
          {summary.total > 0 && (
            <span className="text-body text-fg-muted">
              {t("page.summary", { count: summary.total, nodes: summary.nodes })}
              {summary.invalid > 0 && <>{" · "}<span className="text-fg-danger">{t("page.summaryInvalid", { count: summary.invalid })}</span></>}
            </span>
          )}
        </div>
        {/* The list sits in a bordered panel like the other list pages. */}
        <div className="rounded-xl border border-border-subtle bg-surface-floating p-4">
          {summary.invalid > 0 && (
            <div className="mb-3"><Notice tone="warning" title={t("page.invalidMessage")} children={t("page.invalidDescription")} /></div>
          )}
          {/* The accounts are still listed; what is missing is the clusters connected on their own and where each cluster stands. */}
          {clusters.error && (
            <div className="mb-3">
              <Notice tone="warning" title={t("page.clustersFailed")} children={clusters.error.message}
                action={<Button variant="secondary" size="sm" onClick={() => void clusters.refetch()}>{t("instances.retry")}</Button>} />
            </div>
          )}
          <ResourceTable<PlatformRow>
            // A different filter or row in the address opens the list anew with it.
            key={`${openedCategory ?? ""}|${openedResource ?? ""}|${openedRow ?? ""}|${openedCluster ?? ""}`}
            tableId="providers"
            rowKey="key"
            error={error}
            loading={loading}
            columns={columns}
            data={rows}
            // The list keeps itself current; syncing is what brings the vendors' side in. A failed read can still be retried.
            onRefresh={error ? refetchAll : undefined}
            emptyDescription={t("platforms.empty")}
            filteredEmptyDescription={t("platforms.filteredEmpty")}
            emptyKind="data"
            toolbar={
              <div className="flex flex-wrap items-center gap-2 text-body text-fg-muted">
                <Button variant="secondary" leadingIcon={ConsoleIcon.refresh} onClick={() => void doSyncAll()} loading={syncingAll} disabled={syncableCount === 0}>{t("page.syncAll")}</Button>
                {lastSynced && <span>{t("page.lastSynced", { time: formatTime(lastSynced) })}</span>}
              </div>
            }
            expandable={{
              defaultExpandedRowKeys: openedRow ? [openedRow] : undefined,
              expandedRowRender: (row) => row.type === "cluster"
                ? <StandaloneClusterDetail cluster={row.cluster} />
                : (
                  <ProviderResources
                    provider={row.provider}
                    kindInfo={kindInfoOf(row.provider.kind)}
                    refreshKey={`${row.provider.updated_at}:${instanceRefreshVersion}`}
                    entries={entries}
                    onEntriesChanged={refetchAll}
                    admin={isAdmin}
                    focusCluster={row.key === openedRow ? openedCluster : null}
                    onSync={canSync(row.provider.kind) ? () => void doSync(row.provider.id) : undefined}
                    syncing={syncing === row.provider.id}
                  />
                ),
            }}
          />
        </div>
      </section>

      <ProviderConfigModal
        open={modalOpen}
        provider={editingProvider}
        defaultKind={defaultKind}
        kindInfos={kindInfos}
        categories={categories}
        providerLabel={t("platforms.label")}
        onClose={closeModal}
        onSaved={(added) => {
          closeModal();
          refetchAll();
          if (!added) return;
          // A new account opens on what it holds: its row is expanded, and its instances are listed by a first sync
          // (its clusters are read when the row opens). Everything is then connected from there, one by one.
          setSearchParams({ open: providerRowKey(added.id) }, { replace: true });
          if (canSync(added.kind)) void doSync(added.id);
        }}
        // Kubernetes is offered among the platforms to whoever may make an enrollment token.
        canConnectCluster={isAdmin}
        onClusterCreated={() => void clusters.refetch()}
      />
    </>
  );
}

function providerKindLabel(t: TFunction, kind: string, fallback: LocalizedText | undefined, language: string): string {
  return t(`kind.${kind}`, { defaultValue: localizedText(fallback, language) || kind });
}
