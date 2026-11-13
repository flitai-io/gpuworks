import { formatGPUCapacity } from "../gpuMemory";
import { useEffect, useMemo, useState } from "react";
import {
  Alert, Badge, Button, Card, Input, Progress,
  Space, Tabs, Tag, Tooltip, Typography,
} from "antd";
import {
  ClusterOutlined, DashboardOutlined, DatabaseOutlined, DeploymentUnitOutlined,
  ProfileOutlined, EditOutlined,
} from "@ant-design/icons";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import type { TablePaginationConfig } from "antd/es/table/interface";
import {
  api, useEvents, usePolling, type GPUListResponse, type GPUPage as GPUListPage,
  type PhysicalGPU, type DGPU,
} from "../api";
import { usePagedList } from "../pagedList";
import PageHeader from "../components/PageHeader";
import { useTranslation } from "react-i18next";
import PartitionCapability from "../components/PartitionCapability";
import { stateLabel } from "./state";
import { cardHasDetail, physicalCardKey } from "../nodes/cardIdentity";
import GPUHealthTag, { gpuHealthStatus, gpuOwned } from "../components/GPUHealth";
import ResourceTable, { storedPageSize, type ResourceColumn, type ServerSort } from "../components/resourceTable/ResourceTable";
import DGPUQuotaModal from "./DGPUQuotaModal";
import { DGPUDetail, PhysicalGPUDetail } from "./gpus/GPUDetailDrawers";
import { allocationLabels, dash, fmtGiB, fmtRate, gpuHeading, optionalPercent, shortID, vendorLabel } from "./gpus/gpuFormat";



const GPU_TABLE_ID = "gpus";
const GPU_FILTER_KEYS = ["node", "model", "vendor", "allocation_state"];

export default function GPUPage() {
  const { t } = useTranslation("gpus");
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState("");
  const [filters, setFilters] = useState<Record<string, string[]>>({});
  const [sort, setSort] = useState<ServerSort | null>(null);
  // The search follows typing, settled for a moment before the server is asked.
  useEffect(() => {
    const timer = setTimeout(() => setSearch(query.trim()), 300);
    return () => clearTimeout(timer);
  }, [query]);
  // The server pages, filters and sorts the fleet's GPUs from its 20-second
  // inventory snapshot; polling faster would only reread the same snapshot.
  const physical = usePagedList(api.gpusPage, {
    filters: { q: search, extra: filters, ...(sort ? { sort: `${sort.order === "desc" ? "-" : ""}${sort.key}` } : {}) },
    pageSize: storedPageSize(GPU_TABLE_ID, 20),
    intervalMs: 30000,
  });
  const dgpu = usePolling(api.dgpus, 30000);
  const [searchParams, setSearchParams] = useSearchParams();
  const [activeTab, setActiveTab] = useState("physical");
  const [detail, setDetail] = useState<PhysicalGPU | null>(null);
  const [dgpuDetail, setDGPUDetail] = useState<DGPU | null>(null);
  const [quotaDeployment, setQuotaDeployment] = useState<number | null>(null);
  useEvents("node", () => { void physical.refresh(); void dgpu.refresh(); });
  useEvents("instance", () => { void physical.refresh(); void dgpu.refresh(); });
  // A deployment's dGPU limit is what a slice under an older one gets at restart.
  useEvents("deployment", () => { void dgpu.refresh(); });

  // A deep link names a GPU that need not be on the current page.
  const target = searchParams.get("gpu");
  useEffect(() => {
    if (!target) return;
    let alive = true;
    // Either a card's UUID, or its node and its own key for a card that
    // has no UUID: "<node uuid>/<cardKey>".
    const sep = target.indexOf("/");
    api.gpusPage({ page: 1, page_size: 200, q: sep >= 0 ? target.slice(0, sep) : target })
      .then((page) => {
        const found = page.items.find((gpu) => cardHasDetail(gpu) && (gpu.uuid === target || physicalCardKey(gpu) === target));
        if (alive && found) {
          setActiveTab("physical");
          setDetail(found);
        }
      })
      .catch(() => {});
    return () => { alive = false; };
  }, [target]);

  const closePhysical = () => {
    setDetail(null);
    if (searchParams.has("gpu")) setSearchParams({}, { replace: true });
  };

  return (
    <div className="gpu-page">
      <PageHeader
        title={t("page.title")}
        description={t("page.description")}
        extra={<Typography.Text type="secondary">{t("page.physicalCount", { count: physical.data?.overview.total ?? 0 })}</Typography.Text>}
      />
      <Tabs
        activeKey={activeTab}
        onChange={setActiveTab}
        items={[
          {
            key: "physical",
            label: t("tab.physical"),
            children: (
              <PhysicalGPUView
                page={physical.data}
                loading={physical.loading}
                error={physical.error}
                pagination={physical.pagination}
                query={query}
                onQuery={setQuery}
                filters={filters}
                onFilters={(next) => setFilters(Object.fromEntries(GPU_FILTER_KEYS.filter((key) => next[key]?.length).map((key) => [key, next[key]])))}
                sort={sort}
                onSort={setSort}
                onOpen={setDetail}
                onRefresh={physical.refetch}
              />
            ),
          },
          {
            key: "dgpu",
            label: t("tab.dgpu"),
            children: <DGPUView rows={dgpu.data?.items ?? []} loading={dgpu.loading} error={dgpu.error} onOpen={setDGPUDetail} onEditQuota={(row) => setQuotaDeployment(row.deployment_id)} onRefresh={dgpu.refetch} />,
          },
        ]}
      />
      {/* An open detail follows the polled list, so its readings stay current;
          a card off the shown page keeps the copy it was opened with. */}
      <PhysicalGPUDetail gpu={detail && (physical.data?.items.find((row) => physicalCardKey(row) === physicalCardKey(detail)) ?? detail)} onClose={closePhysical} />
      <DGPUDetail gpu={dgpuDetail && (dgpu.data?.items.find((row) => row.instance_uuid === dgpuDetail.instance_uuid) ?? dgpuDetail)} onClose={() => setDGPUDetail(null)} />
      {quotaDeployment != null && (
        <DGPUQuotaModal deploymentID={quotaDeployment} onClose={() => setQuotaDeployment(null)} onChanged={() => dgpu.refetch()} />
      )}
    </div>
  );
}

function PhysicalGPUView({ page, loading, error, pagination, query, onQuery, filters, onFilters, sort, onSort, onOpen, onRefresh }: {
  page: GPUListPage | null;
  loading: boolean;
  error: Error | null;
  pagination: TablePaginationConfig;
  query: string;
  onQuery: (query: string) => void;
  filters: Record<string, string[]>;
  onFilters: (filters: Record<string, string[]>) => void;
  sort: ServerSort | null;
  onSort: (sort: ServerSort | null) => void;
  onOpen: (gpu: PhysicalGPU) => void;
  onRefresh: () => void;
}) {
  const { t } = useTranslation("gpus");
  const navigate = useNavigate();
  // The filter choices cover the whole fleet, as the server lists them.
  const facets = page?.facets;
  const options = useMemo(() => ({
    nodes: (facets?.nodes ?? []).map((node) => ({ value: node.uuid, text: node.name || node.uuid })),
    models: (facets?.models ?? []).map((value) => ({ value, text: value.replace(/^NVIDIA\s+/i, "") })),
    vendors: (facets?.vendors ?? []).map((value) => ({ value, text: vendorLabel(value, t) })),
  }), [facets, t]);
  const filtered = (key: string) => filters[key] ?? null;

  const columns: ResourceColumn<PhysicalGPU>[] = [
    {
      key: "node", role: "name", title: t("column.nodeGPU"), width: 200,
      serverSort: true, filters: options.nodes, filterSearch: true, filteredValue: filtered("node"),
      render: (_, gpu) => <>
        {cardHasDetail(gpu)
          ? <Button type="link" onClick={(event) => { event.stopPropagation(); onOpen(gpu); }}>{gpu.node_name}</Button>
          : <Typography.Text>{gpu.node_name}</Typography.Text>}
        <span className="resource-table-sub">{gpuHeading(gpu)}</span>
      </>,
    },
    {
      key: "model", title: t("column.model"), width: 190, serverSort: true,
      filters: options.models, filterSearch: true, filteredValue: filtered("model"),
      render: (_, gpu) => gpu.name.replace(/^NVIDIA\s+/i, ""),
    },
    { key: "vendor", title: t("column.vendor"), width: 100, serverSort: true, filters: options.vendors, filteredValue: filtered("vendor"), render: (_, gpu) => vendorLabel(gpu.vendor, t) },
    {
      // One column for what is being asked of the card, with the reason
      // it takes no work — health, ownership, somebody else's process —
      // beside it. There used to be a second, "work state", saying
      // normal/abnormal/unknown from the same verdict: it could only ever
      // repeat this one.
      // Wide enough for the verdict, its reason and the reason's detail
      // button; at 190 the button was cut to a stray "(" beside the tag.
      key: "allocation_state", title: t("column.allocationState"), width: 230, serverSort: true,
      filters: Object.entries(allocationLabels).map(([value, item]) => ({ value, text: t(item.labelKey) })), filteredValue: filtered("allocation_state"),
      render: (_, gpu) => (
        <Space size={4} wrap>
          <Tag color={allocationLabels[gpu.allocation_state].color} style={{ marginInlineEnd: 0 }}>{t(allocationLabels[gpu.allocation_state].labelKey)}</Tag>
          {/* Why it takes no work, beside the verdict: a finding, or a card
              that is not this node's, or one held outside the platform. A
              card without a device number has no detail page, so the row
              is the only place this is said. */}
          {(gpuHealthStatus(gpu) !== "OK" || !!gpu.health_code || !gpuOwned(gpu) || !!gpu.foreign_processes?.length) && <GPUHealthTag gpu={gpu} />}
        </Space>
      ),
    },
    {
      // What is carved out of the card, once anything is; before that, whether
      // it can be. A card that takes no work says nothing here - its
      // backend may be ready on the node, but nothing can be placed on it,
      // and the reason is in the allocation column.
      key: "partition", title: t("column.partitionCapability"), width: 200, ellipsis: false,
      render: (_, gpu) => <SliceSummary gpu={gpu} />,
    },
    { key: "occupants", title: t("column.occupants"), width: 100, align: "right", serverSort: true, render: (_, gpu) => gpu.occupants.length || "-" },
    { key: "utilization", title: t("metric.utilization"), width: 100, align: "right", serverSort: true, render: (_, gpu) => optionalPercent(gpu.load.utilization_pct) },
    {
      key: "memory", title: t("metric.vram"), width: 170, serverSort: true,
      render: (_, gpu) => gpu.load.memory_used_bytes == null ? "-" : `${fmtGiB(gpu.load.memory_used_bytes)} / ${formatGPUCapacity(gpu, fmtGiB)}`,
    },
    { key: "temperature", title: t("metric.temperature"), width: 90, align: "right", serverSort: true, render: (_, gpu) => gpu.load.temperature_c == null ? "-" : `${gpu.load.temperature_c.toFixed(0)} °C` },
    {
      key: "power", title: t("column.powerLimit"), width: 125, align: "right", serverSort: true,
      render: (_, gpu) => gpu.load.power_watts == null ? "-" : `${gpu.load.power_watts.toFixed(0)} / ${gpu.power_limit_watts?.toFixed(0) ?? "-"} W`,
    },
    { key: "fan", title: t("metric.fan"), width: 90, align: "right", defaultHidden: true, serverSort: true, render: (_, gpu) => optionalPercent(gpu.load.fan_speed_pct) },
    {
      key: "pcie", title: "PCIe RX / TX", width: 160, align: "right", defaultHidden: true,
      render: (_, gpu) => gpu.load.pcie_rx_bytes_per_sec == null && gpu.load.pcie_tx_bytes_per_sec == null ? "-" : `${fmtRate(gpu.load.pcie_rx_bytes_per_sec)} / ${fmtRate(gpu.load.pcie_tx_bytes_per_sec)}`,
    },
    { key: "bdf", title: "BDF", width: 135, defaultHidden: true, serverSort: true, render: (_, gpu) => dash(gpu.pcie_bdf) },
    { key: "numa", title: "NUMA", width: 80, align: "right", defaultHidden: true, serverSort: true, render: (_, gpu) => gpu.numa_node < 0 ? "-" : gpu.numa_node },
    {
      key: "actions", role: "actions", width: 100,
      render: (_, gpu) => <Space size={2} onClick={(event) => event.stopPropagation()}>
        <Tooltip title={t("action.details")}><Button type="text" size="small" disabled={!cardHasDetail(gpu)} icon={<ProfileOutlined />} aria-label={t("action.details")} onClick={() => onOpen(gpu)} /></Tooltip>
        <Tooltip title={t("action.openNode")}><Button type="text" size="small" icon={<ClusterOutlined />} aria-label={t("action.openNode")} onClick={() => navigate("/nodes", { state: { nodeUUID: gpu.node_uuid } })} /></Tooltip>
      </Space>,
    },
  ];

  return (
    <>
      <GPUOverviewCards overview={page?.overview} />
      <Card>
        <ResourceTable<PhysicalGPU>
          tableId={GPU_TABLE_ID}
          rowKey={physicalCardKey}
          columns={columns}
          data={page?.items ?? []}
          loading={loading}
          error={error}
          onRefresh={onRefresh}
          pagination={pagination}
          sort={sort}
          onSortChange={onSort}
          onFiltersChange={onFilters}
          toolbar={
            <Input.Search
              id="gpu-search"
              name="gpu-search"
              aria-label={t("filter.searchAria")}
              allowClear
              placeholder={t("filter.searchPlaceholder")}
              value={query}
              onChange={(event) => onQuery(event.target.value)}
              style={{ width: 300 }}
            />
          }
          emptyDescription={t("empty.physical")}
          onRow={(gpu) => cardHasDetail(gpu) ? { onClick: () => onOpen(gpu), className: "gpu-clickable-row" } : {}}
        />
      </Card>
    </>
  );
}

function GPUOverviewCards({ overview }: { overview?: GPUListResponse["overview"] }) {
  const { t } = useTranslation("gpus");
  const health = overview?.health ?? { ok: 0, warning: 0, error: 0 };
  const allocation = overview?.allocation ?? { allocated: 0, allocatable: 0, unusable: 0, unknown: 0 };
  const total = overview?.total ?? 0;
  return (
    <div className="gpu-overview-grid">
      <Card title={<Space><DashboardOutlined />{t("overview.health")}</Space>} className="gpu-overview-card">
        <OverviewDonut total={total} value={health.ok} color="#3ab333" label={t("health.ok")} />
        <OverviewLegend items={[{ label: t("health.ok"), value: health.ok, color: "#3ab333" }, { label: t("health.warning"), value: health.warning, color: "#f5a623" }, { label: t("health.error"), value: health.error, color: "#db2e43" }]} />
      </Card>
      <Card title={<Space><DatabaseOutlined />{t("overview.allocationState")}</Space>} className="gpu-overview-card">
        <OverviewDonut total={total} value={allocation.allocated} color="#0076f7" label={t("allocation.allocated")} />
        <OverviewLegend items={[{ label: t("allocation.allocated"), value: allocation.allocated, color: "#0076f7" }, { label: t("allocation.available"), value: allocation.allocatable, color: "#3ab333" }, { label: t("allocation.unusable"), value: allocation.unusable, color: "#db2e43" }, { label: t("allocation.unknown"), value: allocation.unknown, color: "#8c8c8c" }]} />
        <Typography.Text type="secondary" style={{ gridColumn: "1 / -1" }}>{t("overview.allocationHint")}</Typography.Text>
      </Card>
    </div>
  );
}

function OverviewDonut({ total, value, color, label }: { total: number; value: number; color: string; label: string }) {
  return <div className="gpu-overview-donut"><Progress type="circle" size={104} percent={total ? Math.round(value / total * 100) : 0} strokeColor={color} trailColor="#e6e8eb" format={() => <><strong>{value}</strong><small>{label}</small></>} /></div>;
}

function OverviewLegend({ items }: { items: { label: string; value: number; color: string }[] }) {
  return <div className="gpu-overview-legend">{items.map((item) => <div key={item.label}><span style={{ background: item.color }} /><Typography.Text type="secondary">{item.label}</Typography.Text><strong>{item.value}</strong></div>)}</div>;
}

function DGPUView({ rows, loading, error, onOpen, onEditQuota, onRefresh }: { rows: DGPU[]; loading: boolean; error: Error | null; onOpen: (gpu: DGPU) => void; onEditQuota: (gpu: DGPU) => void; onRefresh: () => void }) {
  const { t } = useTranslation("gpus");
  const columns: ResourceColumn<DGPU>[] = [
    {
      key: "deployment", role: "name", title: t("column.deployment"), width: 180, sortValue: (row) => row.deployment_name,
      render: (_, row) => <Button type="link" onClick={(event) => { event.stopPropagation(); onOpen(row); }}>{row.deployment_name || t("unnamedDeployment")}</Button>,
    },
    { key: "instance", title: t("column.instance"), width: 130, render: (_, row) => <Typography.Text code>{shortID(row.instance_uuid)}</Typography.Text> },
    { key: "state", title: t("column.state"), width: 110, sortValue: (row) => row.state, render: (_, row) => <SliceState row={row} /> },
    { key: "node", title: t("column.node"), width: 140, sortValue: (row) => row.node_name, render: (_, row) => <Link to="/nodes" state={{ nodeUUID: row.node_uuid }} onClick={(event) => event.stopPropagation()}>{row.node_name}</Link> },
    { key: "gpu", title: t("column.physicalGPU"), width: 170, sortValue: (row) => `${row.node_name}\u0000${String(row.gpu_index).padStart(4, "0")}`, render: (_, row) => `${row.gpu_name?.replace(/^NVIDIA\s+/i, "") || "GPU"} #${row.gpu_index}` },
    {
      key: "memory_limit", title: t("column.vramLimit"), width: 130, align: "right", sortValue: (row) => row.memory_limit_bytes,
      render: (_, row) => row.pending_limit_bytes
        ? <Space direction="vertical" size={0} align="end">{fmtGiB(row.memory_limit_bytes)}<Tooltip title={t("quota.pendingTooltip")}><Tag color="orange">{t("quota.pendingTag", { limit: fmtGiB(row.pending_limit_bytes) })}</Tag></Tooltip></Space>
        : fmtGiB(row.memory_limit_bytes),
    },
    { key: "memory_used", title: <Tooltip title={t("tooltip.actualVRAM")}><span className="gpu-column-help">{t("column.actualVRAM")}</span></Tooltip>, width: 120, align: "right", sortValue: (row) => row.memory_used_bytes, render: (_, row) => row.memory_used_bytes == null ? t("noData") : fmtGiB(row.memory_used_bytes) },
    { key: "sm_limit", title: t("column.smLimit"), width: 110, align: "right", sortValue: (row) => row.sm_limit_pct, render: (_, row) => row.sm_limit_pct ? `${row.sm_limit_pct}%` : "-" },
    { key: "sm_util", title: <Tooltip title={t("tooltip.smReference")}><span className="gpu-column-help">{t("column.smReference")}</span></Tooltip>, width: 110, align: "right", sortValue: (row) => row.sm_util_pct, render: (_, row) => row.sm_util_pct == null ? t("noData") : `${row.sm_util_pct.toFixed(0)}%` },
    {
      key: "actions", role: "actions", width: 120,
      render: (_, row) => <Space size={2} onClick={(event) => event.stopPropagation()}>
        <Tooltip title={t("action.details")}><Button type="text" size="small" icon={<ProfileOutlined />} aria-label={t("action.details")} onClick={() => onOpen(row)} /></Tooltip>
        <Tooltip title={t("action.editQuota")}><Button type="text" size="small" icon={<EditOutlined />} aria-label={t("action.editQuota")} onClick={() => onEditQuota(row)} /></Tooltip>
        <Tooltip title={t("action.openDeployment")}><Link to={`/deployments?deployment=${row.deployment_id}`} aria-label={t("action.openDeployment")}><Button type="text" size="small" icon={<DeploymentUnitOutlined />} /></Link></Tooltip>
      </Space>,
    },
  ];
  return <>
    <Alert className="dgpu-note" type="info" showIcon message={t("dgpu.note")} />
    <Card>
      <ResourceTable<DGPU>
        tableId="dgpus"
        rowKey={(row) => `${row.instance_uuid}:${row.gpu_index}`}
        columns={columns}
        data={rows}
        loading={loading}
        error={error}
        onRefresh={onRefresh}
        emptyDescription={t("empty.dgpu")}
        onRow={(row) => ({ onClick: () => onOpen(row), className: "gpu-clickable-row" })}
      />
    </Card>
  </>;
}

// SliceSummary is the dGPU column of the card list.
function SliceSummary({ gpu }: { gpu: PhysicalGPU }) {
  const { t } = useTranslation("gpus");
  const slices = gpu.occupants.filter((item) => (item.limit_bytes ?? 0) > 0);
  if (slices.length) {
    const limit = slices.reduce((sum, item) => sum + (item.limit_bytes ?? 0), 0);
    return (
      <Tooltip title={t("slices.summaryTitle", { limit: fmtGiB(limit), total: formatGPUCapacity(gpu, fmtGiB) })}>
        <Tag color="blue" style={{ marginInlineEnd: 0 }}>{t("slices.summary", { count: slices.length, limit: fmtGiB(limit), total: formatGPUCapacity(gpu, fmtGiB) })}</Tag>
      </Tooltip>
    );
  }
  if (!gpu.schedulable || !gpu.partition_capabilities?.length) return <>-</>;
  return <Space direction="vertical" size={2}>{gpu.partition_capabilities.map((capability) => <PartitionCapability key={capability.backend} capability={capability} compact />)}</Space>;
}

// SliceState is a slice's lifecycle, with a sleeper named as one: the ledger
// keeps only its residue, and "running" alone would read as holding its quota.
function SliceState({ row }: { row: DGPU }) {
  const { t } = useTranslation("gpus");
  const { t: tc } = useTranslation("common");
  if (row.sleep_level) return <Badge status="default" text={t("state.asleep", { level: row.sleep_level })} />;
  const label = stateLabel(row.state, tc);
  return <Badge status={label.status} text={label.text} />;
}
