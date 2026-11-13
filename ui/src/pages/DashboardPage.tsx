import { useState, type ReactNode } from "react";
import { Button } from "../components/ui/button";
import { Tooltip } from "../components/ui/tooltip";
import { Notice } from "../ui-kit/Notice";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, useEvents, usePolling, type CloudProvider, type GPUListResponse, type Health, type List, type ModelListItem, type ModelPage, type MonitorOverview, type NodePage, type Storage, type StorageNodeUsage } from "../api";
import { analyticsApi, type UsageAnalytics } from "../analyticsApi";
import { fmtCelsius, fmtCompact, fmtInt, fmtPct, fmtWatts } from "./analytics/format";
import { storageTypeInfo } from "../storageTypes";
import { formatLatencyText } from "./monitor/signalFormat";
import PageHeader from "../components/PageHeader";
import { Badge } from "../components/ui/badge";
import { storageDetailPath } from "./storage/storageLinks";
import { AttentionBanner, Block, CardOccupancy, EmptyState, Figure, Metric, ModelComposition, ModuleArrow, Panel, StatusCount, StorageCapacityRow, fmt, vendorGroups, type StorageRow } from "./dashboard/OverviewParts";

const DAY = 24 * 60 * 60 * 1000;
const ATTENTION_DETAIL_LIMIT = 5;
const STORAGE_PAGE_SIZE = 2;

type ModelOverview = { total: number; ready: number; preparing: number; failed: number; items: ModelListItem[] };
type StorageOverview = { rows: StorageRow[]; problems: { id: number; name: string; count: number }[]; failedDetails: number };
type Issue = { kind: string; name: string; reason: string; to: string };
type AttentionGroup = { label: string; count: number; to: string };

async function readModels(): Promise<ModelOverview> {
  const first = await api.modelsPage({ page: 1, page_size: 100 });
  const items = [...first.items];
  for (let page = 2; items.length < first.total && page <= 100; page++) {
    const next: ModelPage = await api.modelsPage({ page, page_size: 100 });
    if (!next.items.length) break;
    items.push(...next.items);
  }
  const complete = items.length >= first.total;
  return {
    total: first.total,
    ready: complete ? items.filter((model) => (model.presence?.ready_nodes ?? 0) + (model.presence?.ready_storages ?? 0) > 0).length : -1,
    preparing: complete ? items.filter((model) => (model.presence?.in_flight ?? 0) > 0).length : -1,
    failed: complete ? items.filter((model) => (model.presence?.failed ?? 0) > 0).length : -1,
    items,
  };
}

async function readStorageOverview(storages: Storage[], usage: StorageNodeUsage[]): Promise<StorageOverview> {
  const sharedStorages = storages.filter((storage) => storageTypeInfo(storage.type).shared);
  const results = await Promise.allSettled(sharedStorages.map((storage) => api.storage(storage.id, true)));
  const details = results.filter((result): result is PromiseFulfilledResult<Storage> => result.status === "fulfilled").map((result) => result.value);
  const usageByNode = new Map(usage.map((node) => [node.node_uuid, node]));
  const rows: StorageRow[] = [];
  const problems = storages.flatMap((storage) => {
    const count = (storage.nodes?.problems ?? []).filter((node) => node.state !== "offline").length;
    return count ? [{ id: storage.id, name: storage.name, count }] : [];
  });
  for (const storage of details) {
    const nodes = storage.nodes.all ?? [];
    const measured = nodes.flatMap((node) => {
      const fresh = usageByNode.get(node.node_uuid)?.path_usage?.find((path) => path.kind === "storage" && path.storage_id === storage.id);
      const report = fresh && fresh.total_bytes > 0 ? fresh : node;
      return report.total_bytes != null && report.total_bytes > 0 && report.available_bytes != null
        ? [{ total: report.total_bytes, available: report.available_bytes }]
        : [];
    });
    const shared = measured[0];
    rows.push({
      id: storage.id,
      name: storage.name,
      total: storage.total_bytes || shared?.total,
      available: storage.total_bytes ? storage.available_bytes : shared?.available,
      problems: problems.find((problem) => problem.id === storage.id)?.count ?? 0,
    });
  }
  return { rows, problems, failedDetails: results.length - details.length };
}

const ratio = (part?: number | null, whole?: number | null) => part != null && whole != null && whole > 0 ? part / whole * 100 : undefined;
const pct = (value?: number) => value == null ? "—" : <>{Math.round(value)}<small className="dashboard-metric-unit">%</small></>;

export default function DashboardPage() {
  const { t, i18n } = useTranslation("dashboard");
  const language = i18n.resolvedLanguage ?? "en";
  const [storagePage, setStoragePage] = useState(1);
  const health = usePolling<Health>(api.health, 10000);
  const summary = usePolling(() => api.dashboardSummary(), 10000);
  const providers = usePolling<List<CloudProvider>>(api.providers, 30000);
  const gpus = usePolling<GPUListResponse>(api.gpus, 60000);
  const models = usePolling<ModelOverview>(readModels, 60000);
  const offlineNodes = usePolling<NodePage>(() => api.nodesPage({ page: 1, page_size: 20, state: ["offline"] }), 30000);
  const monitor = usePolling<MonitorOverview>(() => api.monitorOverview("6h"), 30000);
  const storages = usePolling<List<Storage>>(api.storages, 30000);
  const storageUsage = usePolling<List<StorageNodeUsage>>(api.storageUsage, 30000);
  const storageIds = storages.data?.items.map((storage) => storage.id).join(",") ?? "";
  const storage = usePolling<StorageOverview>(() => readStorageOverview(storages.data?.items ?? [], storageUsage.data?.items ?? []), 30000, [storageIds, storageUsage.updatedAt]);
  const usage = usePolling<UsageAnalytics>(() => {
    const to = Date.now();
    return analyticsApi.usage({ from: to - DAY, to, granularity: "hour" });
  }, 30000);
  useEvents("node", () => { void health.refresh(); void summary.refresh(); void gpus.refresh(); void offlineNodes.refresh(); void storageUsage.refresh(); });
  useEvents("deployment", () => { summary.refetch(); monitor.refetch(); });
  useEvents("provider", () => providers.refetch());

  const fleet = summary.data;
  const cloudList = providers.data?.items ?? [];
  const publicClouds = cloudList.filter((cloud) => cloud.category === "public_cloud");
  const privateClouds = cloudList.filter((cloud) => cloud.category === "private_cloud");
  const invalidClouds = cloudList.filter((cloud) => cloud.state === "invalid");
  const deployments = fleet?.deployments;
  const replicaSignal = monitor.data?.signals?.find((signal) => signal.key === "ready_replicas");
  const readyReplicas = replicaSignal?.collected ? replicaSignal.value : undefined;
  const desiredReplicas = replicaSignal?.collected ? replicaSignal.denominator : undefined;
  const requestRate = monitor.data?.signals?.find((signal) => signal.key === "request_rate");
  const errorRate = monitor.data?.signals?.find((signal) => signal.key === "error_rate");
  const ttft = monitor.data?.signals?.find((signal) => signal.key === "ttft");
  const waiting = monitor.data?.signals?.find((signal) => signal.key === "waiting");
  const monitorStatus = monitor.error ? t("overview.monitorUnavailable") : !monitor.data ? t("overview.loading") : monitor.data.stale ? t("overview.monitorStale") : requestRate?.detail?.no_recent_requests === true ? t("overview.monitorIdle") : t("overview.monitorWindow");
  const failedServices = (monitor.data?.services ?? []).filter((service) => service.state === "running" && ["failed", "timeout", "no_replica"].includes(service.health?.status));
  const unusableGpus = (gpus.data?.items ?? []).filter((gpu) => gpu.allocation_state === "unusable");
  const failedModels = (models.data?.items ?? []).filter((model) => (model.presence?.failed ?? 0) > 0);
  const modelIssuesIncomplete = models.data?.failed === -1;
  const failedModelCount = modelIssuesIncomplete ? failedModels.length : models.data?.failed ?? 0;
  const controlStatus = health.error ? undefined : health.data?.status;
  const currentGpu = summary.error ? undefined : fleet?.current_metrics;
  const gpuPower = currentGpu?.power_watts;
  const gpuPowerValue = gpuPower == null ? "—" : gpuPower >= 1000 ? `${(gpuPower / 1000).toFixed(2)} kW` : fmtWatts(gpuPower);
  const cards = summary.error ? undefined : fleet?.cards ?? undefined;
  const tasks = fleet?.tasks;
  const totals = usage.data?.totals;
  const totalTokens = totals ? totals.prompt_tokens + totals.completion_tokens : undefined;
  // Open usage analytics on the same window the token panel shows.
  const usageLink = usage.data ? `/analytics/usage?range=custom&from=${usage.data.range.from_ms}&to=${usage.data.range.to_ms}&gran=${usage.data.range.granularity}` : "/analytics/usage";
  const storageIncomplete = !!(storages.error || storage.error || storage.data?.failedDetails);
  const storageRows = storage.data?.rows ?? [];
  const currentStoragePage = Math.min(storagePage, Math.max(1, Math.ceil(storageRows.length / STORAGE_PAGE_SIZE)));
  const attentionGroups: AttentionGroup[] = [
    { label: t("overview.offlineNodesGroup", { count: fmt(fleet?.nodes.offline ?? offlineNodes.data?.total ?? 0) }), count: fleet?.nodes.offline ?? offlineNodes.data?.total ?? 0, to: "/nodes" },
    { label: t("overview.unusableGpusGroup", { count: fmt(gpus.data?.overview.allocation?.unusable ?? unusableGpus.length) }), count: gpus.data?.overview.allocation?.unusable ?? unusableGpus.length, to: "/gpus" },
    { label: t("overview.failedServicesGroup", { count: fmt(failedServices.length) }), count: failedServices.length, to: "/monitor" },
    { label: t(modelIssuesIncomplete ? "overview.failedModelsGroupPartial" : "overview.failedModelsGroup", { count: fmt(failedModelCount) }), count: failedModelCount, to: "/models" },
    { label: t("overview.problemStoragesGroup", { count: fmt(storage.data?.problems.length ?? 0) }), count: storage.data?.problems.length ?? 0, to: "/storage" },
    { label: t("overview.invalidPublicCloudsGroup", { count: fmt(invalidClouds.filter((cloud) => cloud.category === "public_cloud").length) }), count: invalidClouds.filter((cloud) => cloud.category === "public_cloud").length, to: "/providers?category=public_cloud" },
    { label: t("overview.invalidPrivateCloudsGroup", { count: fmt(invalidClouds.filter((cloud) => cloud.category === "private_cloud").length) }), count: invalidClouds.filter((cloud) => cloud.category === "private_cloud").length, to: "/providers?category=private_cloud" },
  ].filter((group) => group.count > 0);
  const attentionCount = attentionGroups.reduce((sum, group) => sum + group.count, 0);
  const issues: Issue[] = attentionCount <= ATTENTION_DETAIL_LIMIT ? [
    ...(offlineNodes.data?.items ?? []).map((node) => ({ kind: t("overview.issueNode"), name: node.name || node.hostname, reason: t("overview.offline"), to: "/nodes" })),
    ...unusableGpus.map((gpu) => ({ kind: "GPU", name: gpu.name, reason: t("overview.unavailableGpu"), to: `/gpus?gpu=${encodeURIComponent(gpu.uuid)}` })),
    ...failedServices.map((service) => ({ kind: t("overview.issueService"), name: service.name, reason: t("overview.probeFailed"), to: `/monitor/d/${service.deployment_id}` })),
    ...failedModels.map((model) => ({ kind: t("overview.models"), name: model.name, reason: t("overview.fileFailed"), to: `/models?detail=${model.id}` })),
    ...(storage.data?.problems ?? []).map((problem) => ({ kind: t("overview.storageTitle"), name: problem.name, reason: t("overview.storageProblem", { count: problem.count }), to: storageDetailPath(problem.id, "nodes") })),
    ...invalidClouds.map((cloud) => ({ kind: t("overview.clouds"), name: cloud.name, reason: t("overview.cloudProblem"), to: `/providers?category=${cloud.category}` })),
  ] : [];
  const groupedAttention = attentionCount > ATTENTION_DETAIL_LIMIT || issues.length !== attentionCount || modelIssuesIncomplete;
  const attentionEntries = groupedAttention
    ? attentionGroups.map((group) => ({ to: group.to, title: group.label, content: <strong>{group.label}</strong> }))
    : issues.map((issue) => ({ to: issue.to, title: `${issue.kind} · ${issue.name} · ${issue.reason}`, content: <><span>{issue.kind}</span><strong>{issue.name}</strong><span>{issue.reason}</span></> }));
  const partial = [summary, providers, gpus, models, offlineNodes, monitor, storages, storageUsage, storage, usage].some((query) => !!query.error) || !!storage.data?.failedDetails || modelIssuesIncomplete;
  const groups = gpus.data ? vendorGroups(gpus.data, t("overview.unknownGpu")) : [];
  const modelCount = groups.reduce((sum, group) => sum + group.models.length, 0);

  const nodes = fleet?.nodes;
  const unit = (value: ReactNode, suffix: string) => <>{value}<small className="dashboard-metric-unit">{suffix}</small></>;

  // Compute is two panels of equal height: what the hardware is (static) and
  // how it is used right now (live).
  const computeBlock = <Block key="compute" title={t("overview.computeBlock")} className="dashboard-compute">
    <Panel title={t("overview.hardwarePanel")} to="/nodes" label={t("overview.nodes")} className="dashboard-hardware">
      <div className="dashboard-kpis">
        <Metric className="is-kpi" label={t("overview.nodesKpi")} to={(nodes?.total ?? 0) > 0 ? "/nodes" : undefined} value={nodes ? unit(fmt(nodes.total), t("overview.nodeUnit")) : "—"} />
        <Metric className="is-kpi" label={t("overview.gpusKpi")} to={(nodes?.gpus ?? 0) > 0 ? "/gpus" : undefined} value={nodes ? unit(fmt(nodes.gpus), t("overview.gpuUnit")) : "—"} />
        <Metric className="is-kpi" label={t("overview.vramKpi")} to={(nodes?.vram_gib ?? 0) > 0 ? "/gpus" : undefined} value={nodes ? unit(fmt(Math.round(nodes.vram_gib)), "GiB") : "—"} />
      </div>
      <div className="dashboard-states-line">
        <StatusCount count={nodes?.online} label={t("overview.online", { count: fmt(nodes?.online) })} tone="success" />
        <StatusCount count={nodes?.connecting} label={t("overview.connecting", { count: fmt(nodes?.connecting) })} tone="warning" />
        <StatusCount count={nodes?.offline} label={t("overview.offlineCount", { count: fmt(nodes?.offline) })} tone="danger" />
      </div>
      <div className="dashboard-subsection">
        <div className="dashboard-subsection-head"><span>{t("overview.modelComposition")}</span>{groups.length > 0 && <span className="dashboard-subsection-tag">{groups.length > 1 ? t("overview.vendorsAndModels", { vendors: groups.length, models: modelCount }) : t("overview.modelCount", { count: modelCount })}</span>}<ModuleArrow to="/gpus" label={t("overview.viewGpu")} /></div>
        {groups.length ? <ModelComposition groups={groups} /> : <EmptyState title={gpus.error ? t("overview.unavailable") : t("overview.noGpu")} detail="" />}
      </div>
    </Panel>
    <Panel title={t("overview.usagePanel")} tag={t("overview.usageTag")} to="/scheduling" label={t("overview.viewScheduling")} className="dashboard-usage">
      <div className="dashboard-kpis">
        <Metric className="is-kpi" label={t("overview.allocationRate")} tip={t("overview.allocationTip")} value={pct(ratio(cards?.allocated, cards?.cards))} hint={cards ? t("overview.cardsOf", { used: fmt(cards.allocated), total: fmt(cards.cards) }) : undefined} />
        <Metric className="is-kpi" label={t("overview.gpuUtilization")} tip={t("overview.gpuUtilizationTip")} value={pct(currentGpu?.utilization_pct ?? undefined)} hint={currentGpu?.utilization_pct != null ? t("overview.gpuUtilizationSub") : undefined} />
        <Metric className="is-kpi" label={t("overview.vramUtilization")} tip={t("overview.vramUtilizationTip")} value={pct(ratio(currentGpu?.vram_used_bytes, currentGpu?.vram_total_bytes))} hint={currentGpu?.vram_used_bytes != null && currentGpu?.vram_total_bytes ? `${fmt(Math.round(currentGpu.vram_used_bytes / 1024 ** 3))} / ${fmt(Math.round(currentGpu.vram_total_bytes / 1024 ** 3))} GiB` : undefined} />
      </div>
      <div className="dashboard-subsection">
        {cards ? cards.cards > 0 ? <CardOccupancy cards={cards} /> : <EmptyState title={t("overview.noGpu")} detail="" /> : <EmptyState title={summary.data || summary.error ? t("overview.unavailable") : t("overview.loading")} detail="" />}
      </div>
      <div className="dashboard-usage-foot">
        <Figure label={t("overview.maxTemperature")} value={fmtCelsius(currentGpu?.temperature_c)} to={(currentGpu?.temperature_c ?? 0) > 0 ? "/analytics/gpus?metric=temp" : undefined} />
        <Figure label={t("overview.totalPower")} value={gpuPowerValue} to={(gpuPower ?? 0) > 0 ? "/analytics/gpus?metric=power" : undefined} />
        <Figure label={t("overview.yieldsLastHour")} value={cards?.contention.yields_last_hour} to="/scheduling" />
      </div>
    </Panel>
  </Block>;

  const servicesBlock = <Block key="services" title={t("overview.servicesBlock")} className="dashboard-services">
    
      <Panel title={t("overview.deployments")} to="/deployments" label={t("overview.deployments")}>
        <div className="dashboard-big">{(deployments?.total ?? 0) > 0 ? <Link to="/deployments">{fmt(deployments?.total)}</Link> : fmt(deployments?.total)}<small>{t("overview.deploymentUnit")}</small></div>
        <div className="dashboard-facts">
          <StatusCount count={deployments?.running} label={t("overview.running", { count: fmt(deployments?.running) })} tone="success" />
          <StatusCount count={deployments?.warming} label={t("overview.warming", { count: fmt(deployments?.warming) })} tone="warning" />
          <StatusCount count={deployments?.sleeping} label={t("overview.sleepingCount", { count: fmt(deployments?.sleeping) })} tone="info" />
          <StatusCount count={deployments?.other} label={t("overview.otherStates", { count: fmt(deployments?.other) })} tone="neutral" />
        </div>
        <div className="dashboard-panel-foot">
          <Tooltip content={t("overview.replicaCountTip")}><span className="dashboard-figure"><span>{t("overview.readyReplicasLabel")}</span><b>{fmt(readyReplicas)}/{fmt(desiredReplicas)}</b></span></Tooltip>
          <Figure label={t("overview.pendingLabel")} value={deployments?.pending} to="/scheduling" hot={(deployments?.pending ?? 0) > 0} />
          <Figure label={t("overview.waitingWakesLabel")} value={cards?.contention.waiting_wakes} to="/scheduling" />
        </div>
      </Panel>
      <Panel title={t("overview.usageTitle")} tag={t("overview.usageRange")} to={usageLink} label={t("overview.viewUsage")}>
        <div className="dashboard-big" title={fmtInt(totalTokens)}>{(totalTokens ?? 0) > 0 ? <Link to={usageLink}>{fmtCompact(totalTokens, language)}</Link> : fmtCompact(totalTokens, language)}<small>tokens</small></div>
        <div className="dashboard-facts"><span>{t("overview.inputTokens")} {fmtCompact(totals?.prompt_tokens, language)}</span><span>{t("overview.outputTokens")} {fmtCompact(totals?.completion_tokens, language)}</span></div>
        <Figure label={t("overview.requests")} value={totals?.requests} to={usageLink} />
        {totals?.no_usage ? <div className="dashboard-panel-foot"><span className="dashboard-figure is-zero">{t("overview.unreportedUsageHint", { count: fmt(totals.no_usage) })}</span></div> : null}
      </Panel>
    <Panel title={t("overview.tasksGroup")} tag={t("overview.tasksGroupHint")} className="dashboard-tasks">
      <TaskRow count={tasks?.batches?.active} name={t("overview.batches")} tip={t("overview.batchesActiveTip")} to="/batches" label={t("overview.viewBatches")} detail={tasks?.batches ? <Figure label={t("overview.batchesWaitingLabel")} value={tasks.batches.waiting} /> : undefined} />
      <TaskRow count={tasks?.benchmarks} name={t("overview.evaluations")} tip={t("overview.evaluationsActiveTip")} to="/evaluations" label={t("overview.viewEvaluations")} />
    </Panel>
      <Panel title={t("overview.monitorSummary")} tag={monitorStatus} to="/monitor" label={t("overview.viewMonitor")} className="dashboard-quality">
        <div className="dashboard-metrics">
          <Metric label={t("overview.requestRate")} value={requestRate?.collected && requestRate.value != null ? `${requestRate.value.toFixed(1)} req/s` : "—"} tip={t("overview.requestRateTip")} />
          <Metric label={t("overview.concurrency")} value={requestRate?.collected && requestRate.denominator != null ? String(Number(requestRate.denominator.toFixed(1))) : "—"} tip={t("overview.concurrencyTip")} />
          <Metric label={t("overview.errorRate")} value={errorRate?.collected ? fmtPct(errorRate.value) : "—"} tip={t("overview.errorRateTip")} />
          <Metric label={t("overview.ttftP95")} value={ttft?.collected && ttft.value != null ? formatLatencyText(ttft.value) : "—"} tip={t("overview.ttftP95Tip")} />
          <Metric label={t("overview.waitingP95")} value={waiting?.collected && waiting.value != null ? waiting.value.toFixed(1) : "—"} tip={t("overview.waitingP95Tip")} />
        </div>
      </Panel>

  </Block>;

  const storageBlock = <Block key="storage" title={t("overview.storageBlock")} className="dashboard-storage">
    <Panel title={t("overview.storageCapacity")} tag={t("overview.storageCount", { count: storages.data?.items.filter((item) => storageTypeInfo(item.type).shared).length ?? 0 })} to="/storage" label={t("overview.viewStorage")}>
      {storageRows.length ? <><div className={storageRows.length > STORAGE_PAGE_SIZE ? "dashboard-overview-storage-page" : undefined}>{storageRows.slice((currentStoragePage - 1) * STORAGE_PAGE_SIZE, currentStoragePage * STORAGE_PAGE_SIZE).map((row) => <StorageCapacityRow key={row.id} row={row} />)}</div>{storageRows.length > STORAGE_PAGE_SIZE && <nav className="dashboard-overview-storage-pagination" aria-label={t("overview.storageCapacity")}><Button variant="ghost" size="sm" disabled={currentStoragePage === 1} onClick={() => setStoragePage(currentStoragePage - 1)}>{t("common:resourceTable.previousPage")}</Button><span>{currentStoragePage} / {Math.ceil(storageRows.length / STORAGE_PAGE_SIZE)}</span><Button variant="ghost" size="sm" disabled={currentStoragePage * STORAGE_PAGE_SIZE >= storageRows.length} onClick={() => setStoragePage(currentStoragePage + 1)}>{t("common:resourceTable.nextPage")}</Button></nav>}</> : <EmptyState title={storageIncomplete ? t("overview.unavailable") : t("overview.noStorage")} detail={storageIncomplete ? "" : t("overview.noStorageHint")} />}
    </Panel>
    <Panel title={t("overview.resourceAccess")}>
      <div className="dashboard-sources">
        <Source count={providers.data ? publicClouds.length : undefined} label={t("overview.publicCloud")} to="/providers?category=public_cloud" />
        <Source count={providers.data ? privateClouds.length : undefined} label={t("overview.privateCloud")} to="/providers?category=private_cloud" />
      </div>
    </Panel>
  </Block>;

  // The blocks in the order the page reads: the hardware first, then what it
  // carries, then storage and external resources.
  const blocks: ReactNode[] = [computeBlock, servicesBlock, storageBlock];

  return <div className="dashboard-overview">
    <PageHeader title={t("title")} description={t("overview.description")} extra={<div className="dashboard-header-meta">
      <span className="dashboard-header-service">{t("overview.managementService")} {controlStatus === "ok"
        // A light green pill with a bright dot: the dark success text alone read as black.
        ? <Badge status="success" size="sm" leadingIcon={<span className="size-1.5 rounded-full bg-success-strong" />}>{t("overview.normal")}</Badge>
        : <b>{controlStatus ?? "—"}</b>}</span>
    </div>} />
    {attentionEntries.length > 0 && <AttentionBanner entries={attentionEntries} grouped={groupedAttention} count={groupedAttention ? attentionGroups.length : attentionCount} />}
    {partial && <Notice tone="warning" title={t("overview.unavailable")} />}
    {blocks}
  </div>;
}

function TaskRow({ count, name, tip, to, label, detail }: { count?: number; name: string; tip: string; to: string; label: string; detail?: ReactNode }) {
  return <div className="dashboard-task">
    <Tooltip content={tip}><span className={`dashboard-task-count${count === 0 ? " is-zero" : ""}`}>{(count ?? 0) > 0 ? <Link to={to}>{fmt(count)}</Link> : fmt(count)}</span></Tooltip>
    <span className="dashboard-task-body"><span className="dashboard-task-name">{name}</span>{detail && <span className="dashboard-task-facts">{detail}</span>}</span>
    <ModuleArrow to={to} label={label} />
  </div>;
}

function Source({ count, label, to }: { count?: number; label: string; to: string }) {
  const content = <><span className={`dashboard-source-value${count === 0 ? " is-zero" : ""}`}>{fmt(count)}</span><span className="dashboard-source-label">{label}</span></>;
  return count != null && count > 0 ? <Link to={to} className="dashboard-source">{content}</Link> : <div className="dashboard-source">{content}</div>;
}
