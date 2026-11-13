import { useEffect, useState } from "react";
import {
  Alert,
  Button,
  Popconfirm,
  Space,
  Tag,
  Tooltip,
  Typography,
  message,
} from "antd";
import { DeleteOutlined, EditOutlined, SyncOutlined } from "@ant-design/icons";
import {
  api,
  usePolling,
  type CloudProvider,
  type List,
  type ProviderKindInfo,
} from "../api";
import PageHeader from "../components/PageHeader";
import ResourceTable, { type ResourceColumn } from "../components/resourceTable/ResourceTable";
import ProviderConfigModal from "../components/ProviderConfigModal";
import ProviderInstancesTable from "../components/ProviderInstancesTable";
import ProviderClustersTable from "../components/ProviderClustersTable";
import { Tabs, TabsList, TabItem, TabPanel } from "../components/ui/tabs";
import { formatTime } from "../i18n/datetime";
import { useTranslation } from "react-i18next";
import { useSearchParams } from "react-router-dom";
import type { TFunction } from "i18next";
import type { LocalizedText } from "../i18n/localizedText";
import { localizedText } from "../i18n/localizedText";

// One list for every connected platform; each row carries the category the
// server derives from its kind, which the platform type column shows.
// ?category=<type> opens the list filtered to that type, as the overview's
// cloud counts and the old /zstack address do.
export default function CloudProvidersPage() {
  const { t, i18n } = useTranslation("providers");
  const [searchParams] = useSearchParams();
  const requested = searchParams.get("category");
  // A type the console has no name for is not a type: the list opens unfiltered.
  const openedCategory = requested && i18n.exists(`providers:category.${requested}`) ? requested : null;
  const language = i18n.resolvedLanguage || i18n.language;
  const { data: providers, refetch, loading, error, updatedAt } = usePolling<List<CloudProvider>>(api.providers, 15000);
  const data = providers?.items ?? [];
  const [modalOpen, setModalOpen] = useState(false);
  const [defaultKind, setDefaultKind] = useState<string | null>(null);
  const [editingProvider, setEditingProvider] = useState<CloudProvider | null>(null);
  const [syncing, setSyncing] = useState<number | null>(null);
  const [syncingAll, setSyncingAll] = useState(false);
  const [instanceRefreshVersion, setInstanceRefreshVersion] = useState(0);
  const [kindInfos, setKindInfos] = useState<ProviderKindInfo[]>([]);

  useEffect(() => {
    api
      .providerKinds()
      .then((infos) => setKindInfos(infos.items))
      .catch(() => setKindInfos([]));
  }, []);

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
    const providers = data.filter((provider) => {
      const kindInfo = kindInfos.find((kind) => kind.kind === provider.kind);
      return kindInfo?.capabilities.can_list_instances !== false;
    });
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
    total: data.length,
    invalid: data.filter((p) => p.state === "invalid").length,
    instances: data.reduce((sum, p) => sum + (p.instance_count ?? 0), 0),
  };
  const syncableCount = data.filter((provider) => {
    const kindInfo = kindInfos.find((kind) => kind.kind === provider.kind);
    return kindInfo?.capabilities.can_list_instances !== false;
  }).length;

  const kindsPresent = [...new Set(data.map((provider) => provider.kind))];
  const categoriesPresent = [...new Set(data.map((provider) => provider.category).filter(Boolean))];
  const columns: ResourceColumn<CloudProvider>[] = [
    {
      key: "name",
      role: "name",
      title: t("table.column.name"),
      width: 132,
      sortValue: (p) => p.name,
      render: (_: unknown, p: CloudProvider) => <Button type="link" onClick={() => openEdit(p)}>{p.name || "—"}</Button>,
    },
    {
      key: "kind",
      title: t("table.column.type"),
      width: 130,
      sortValue: (p) => p.kind,
      // No rows, no choices: the filter appears with the first platform.
      filters: kindsPresent.length ? kindsPresent.map((kind) => ({ value: kind, text: providerKindLabel(t, kind, kindInfos.find((info) => info.kind === kind)?.label, language) })) : undefined,
      onFilter: (value, p) => p.kind === value,
      render: (_: unknown, p: CloudProvider) => <Tag color="blue">{providerKindLabel(t, p.kind, undefined, language)}</Tag>,
    },
    {
      key: "category",
      title: t("table.column.category"),
      width: 96,
      filters: categoriesPresent.length || openedCategory ? [...new Set([...categoriesPresent, ...(openedCategory ? [openedCategory] : [])])].map((category) => ({ value: category, text: t(`category.${category}`) })) : undefined,
      defaultFilteredValue: openedCategory ? [openedCategory] : null,
      onFilter: (value, p) => p.category === value,
      render: (_: unknown, p: CloudProvider) => p.category ? t(`category.${p.category}`) : "—",
    },
    { key: "instance_count", title: t("table.column.instances"), width: 72, align: "right", sortValue: (p) => p.instance_count ?? 0, render: (_: unknown, p: CloudProvider) => `${p.instance_count ?? 0}` },
    { key: "updated_at", title: t("table.column.updatedAt"), width: 156, sortValue: (p) => p.updated_at, render: (_: unknown, p: CloudProvider) => formatTime(p.updated_at) },
    {
      key: "state",
      title: t("table.column.status"),
      width: 90,
      sortValue: (p) => p.state,
      filters: [{ value: "valid", text: t("table.state.valid") }, { value: "invalid", text: t("table.state.invalid") }],
      onFilter: (value, p) => p.state === value,
      render: (_: unknown, p: CloudProvider) => {
        if (p.state === "valid") return <Tag color="green">{t("table.state.valid")}</Tag>;
        if (p.state === "invalid") return <Tag color="red">{t("table.state.invalid")}</Tag>;
        return <Tag>{p.state || "—"}</Tag>;
      },
    },
    {
      key: "action",
      role: "actions",
      width: 110,
      render: (_: unknown, p: CloudProvider) => {
        const kindInfo = kindInfos.find((k) => k.kind === p.kind);
        const syncable = kindInfo?.capabilities.can_list_instances !== false;
        return (
          <Space size={2}>
            <Tooltip title={t("table.edit")}>
              <Button type="text" size="small" icon={<EditOutlined />} aria-label={t("table.edit")} onClick={() => openEdit(p)} />
            </Tooltip>
            <Tooltip title={syncable ? t("table.sync") : t("table.comingSoon")}>
              <Button type="text" size="small" icon={<SyncOutlined />} aria-label={t("table.sync")} disabled={!syncable} loading={syncing === p.id} onClick={() => doSync(p.id)} />
            </Tooltip>
            <Popconfirm
              title={t("table.deleteConfirm")}
              okText={t("table.delete")}
              cancelText={t("table.cancel")}
              okButtonProps={{ danger: true }}
              onConfirm={async () => {
                try {
                  await api.deleteProvider(p.id);
                  message.success(t("message.deleted"));
                  refetch();
                } catch (e) {
                  message.error(t("message.deleteFailed", { detail: (e as Error).message }));
                }
              }}
            >
              <Tooltip title={t("table.delete")}>
                <Button type="text" size="small" danger icon={<DeleteOutlined />} aria-label={t("table.delete")} />
              </Tooltip>
            </Popconfirm>
          </Space>
        );
      },
    },
  ];

  return (
    <>
      <PageHeader
        title={t("platforms.title")}
        description={t("platforms.description")}
        extra={
          <>
            <Typography.Text type="secondary">{t("page.count", { count: data.length })}</Typography.Text>
            <Button type="primary" onClick={() => openAdd()}>
              {t("page.add")}
            </Button>
          </>
        }
      />

      <section className="operational-panel">
        <Typography.Title level={5} style={{ margin: "0 0 14px" }}>
          {t("page.supportedTypes")}
        </Typography.Title>
        {error && (
          <Alert
            type="warning"
            showIcon
            style={{ marginBottom: 12 }}
            message={t("page.listFailed")}
            description={error.message}
          />
        )}
        <div className="provider-kind-grid">
          {kindInfos.map((kind) => {
            const label = providerKindLabel(t, kind.kind, kind.label, language);
            return (
              <div key={kind.kind} className="provider-kind-card">
                <button
                  type="button"
                  onClick={() => openAdd(kind.kind)}
                  className="provider-kind-add"
                  aria-label={t("page.addKind", { kind: label })}
                >
                  <ProviderKindLogo kind={kind.kind} />
                  <Typography.Text strong>{label}</Typography.Text>
                </button>
                {kind.homepage && (
                  <Typography.Link href={kind.homepage} target="_blank" rel="noopener noreferrer" className="provider-kind-homepage">
                    {kind.homepage.replace(/^https?:\/\//, "")}
                  </Typography.Link>
                )}
              </div>
            );
          })}
          {kindInfos.length === 0 && !error && (
            <Alert
              type="info"
              showIcon
              message={t("page.schemaMissing")}
              description={t("page.schemaMissingDescription")}
            />
          )}
        </div>
      </section>

      <section className="operational-panel">
        <div className="provider-connected-heading">
          <Typography.Title level={5} style={{ margin: 0 }}>
            {t("page.connected")}
          </Typography.Title>
          {summary.total > 0 && (
            <Typography.Text type="secondary">
              {t("page.summary", { count: summary.total, instances: summary.instances })}
              {summary.invalid > 0 && (
                <>
                  {" · "}
                  <Typography.Text type="danger">{t("page.summaryInvalid", { count: summary.invalid })}</Typography.Text>
                </>
              )}
            </Typography.Text>
          )}
        </div>
        {summary.invalid > 0 && (
          <Alert
            type="warning"
            showIcon
            style={{ marginBottom: 12 }}
            message={t("page.invalidMessage")}
            description={t("page.invalidDescription")}
          />
        )}
        <ResourceTable<CloudProvider>
          // A different ?category= opens the list anew with that filter.
          key={openedCategory ?? "all"}
          tableId="providers"
          rowKey="id"
          error={error}
          loading={loading}
          columns={columns}
          data={data}
          onRefresh={refetch}
          updatedAt={updatedAt}
          emptyDescription={t("platforms.empty")}
          emptyKind="data"
          toolbarExtra={
            <Button onClick={doSyncAll} loading={syncingAll} disabled={syncableCount === 0}>
              {t("page.syncAll")}
            </Button>
          }
          expandable={{
            expandedRowRender: (p) => {
              const kindInfo = kindInfos.find((k) => k.kind === p.kind);
              const instances = (
                <ProviderInstancesTable
                  providerId={p.id}
                  kindInfo={kindInfo}
                  refreshKey={`${p.updated_at}:${instanceRefreshVersion}`}
                  showRegion={p.category !== "private_cloud"}
                />
              );
              if (!kindInfo?.can_list_clusters) return instances;
              // Clusters are read from the cloud when their tab is opened.
              return (
                <Tabs defaultValue="instances" variant="underline">
                  <TabsList>
                    <TabItem value="instances" label={t("tabs.instances")} />
                    <TabItem value="clusters" label={t("tabs.clusters")} />
                  </TabsList>
                  <TabPanel value="instances" className="pt-4">{instances}</TabPanel>
                  <TabPanel value="clusters" className="pt-4"><ProviderClustersTable providerId={p.id} providerName={p.name} /></TabPanel>
                </Tabs>
              );
            },
          }}
        />
      </section>

      <ProviderConfigModal
        open={modalOpen}
        provider={editingProvider}
        defaultKind={defaultKind}
        kindInfos={kindInfos}
        providerLabel={t("platforms.label")}
        onClose={closeModal}
        onSaved={() => {
          closeModal();
          refetch();
        }}
      />
    </>
  );
}

const PROVIDER_LOGOS: Record<string, string> = {
  aliyun: "/provider-logos/aliyun.webp",
  autodl: "/provider-logos/autodl.webp",
  compshare: "/provider-logos/compshare.webp",
  digitalocean: "/provider-logos/digitalocean.svg",
  runpod: "/provider-logos/runpod.webp",
  vast: "/provider-logos/vast.webp",
  zstack: "/provider-logos/zstack.svg",
};

// Vendor marks are UI assets under public/provider-logos; a kind without one
// shows its initial instead.
function ProviderKindLogo({ kind }: { kind: string }) {
  const logo = PROVIDER_LOGOS[kind];
  if (logo) {
    return <img className="provider-kind-logo" src={logo} alt="" width={32} height={32} />;
  }
  return <span className="provider-kind-logo provider-kind-logo-fallback" aria-hidden="true">{kind.slice(0, 1).toUpperCase()}</span>;
}

function providerKindLabel(t: TFunction, kind: string, fallback: LocalizedText | undefined, language: string): string {
  return t(`kind.${kind}`, { defaultValue: localizedText(fallback, language) || kind });
}
