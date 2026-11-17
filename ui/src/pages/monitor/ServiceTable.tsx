import { useEffect, useState, type ReactNode } from "react";
import { Badge } from "../../components/ui/badge";
import StateSummary from "./StateSummary";
import { STATE_BADGE, offeredStates } from "./serviceStates";
import { useTranslation } from "react-i18next";
import { Link, useNavigate } from "react-router-dom";
import type { HealthStatus, MonitorService, MonitorStateCount } from "../../api";
import { HealthCheckHelp, HealthTag } from "../../components/DeploymentHealth";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { colors } from "../../theme/tokens";
import Dash, { renderNumber } from "./Dash";
import { formatLatencyText } from "./signalFormat";

const NARROW_BREAKPOINT = 768;

// The shared list lays columns out fixed at their declared widths; the table
// scrolls sideways when they exceed the viewport.
const SERVICE_COL_WIDTH = 220;
const HEALTH_COL_WIDTH = 150;

// HEALTH_ORDER sorts the health column the way the server orders the rows:
// failed and timed-out checks first, unchecked last.
const HEALTH_ORDER: HealthStatus[] = ["failed", "timeout", "no_replica", "pending", "ok", "off"];

// A small, dependency-free media query hook. antd's own column `responsive`
// prop exists, but it is driven by a global matchMedia listener that is hard
// to control per test; owning the query here lets tests flip narrow/wide
// deterministically by stubbing window.matchMedia.
function useNarrow(breakpointPx = NARROW_BREAKPOINT): boolean {
  const query = () => (typeof window !== "undefined" && typeof window.matchMedia === "function" ? `(min-width: ${breakpointPx}px)` : null);
  const [narrow, setNarrow] = useState(() => {
    const q = query();
    return q ? !window.matchMedia(q).matches : false;
  });
  useEffect(() => {
    const q = query();
    if (!q) return;
    const mql = window.matchMedia(q);
    const handler = () => setNarrow(!mql.matches);
    handler();
    if (mql.addEventListener) mql.addEventListener("change", handler);
    else mql.addListener(handler);
    return () => {
      if (mql.removeEventListener) mql.removeEventListener("change", handler);
      else mql.removeListener(handler);
    };
  }, [breakpointPx]);
  return narrow;
}

function num(value: number | null, digits = 0): ReactNode {
  return value == null ? <Dash /> : value.toFixed(digits);
}

// KvBar renders KV cache usage as a small inline bar rather than a bare
// number, per plan §3.1 ("KV 使用率" column). It shows the fact only: how
// full is too full depends on the workload.
function KvBar({ pct }: { pct: number | null }) {
  if (pct == null) return <Dash />;
  const color = colors.chartPrimary;
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 6, minWidth: 84 }}>
      <div style={{ flex: 1, height: 6, borderRadius: 3, background: colors.borderSoft, overflow: "hidden" }}>
        <div style={{ width: `${Math.min(100, Math.max(0, pct))}%`, height: "100%", background: color }} />
      </div>
      <span style={{ fontVariantNumeric: "tabular-nums" }}>{pct.toFixed(0)}%</span>
    </div>
  );
}

export default function ServiceTable({
  services,
  loading,
  error,
  onRefresh,
  stateCounts,
  stateFilter: stateFilterProp,
  onStateFilterChange,
  signalFilter,
  onClearSignalFilter,
}: {
  services: MonitorService[];
  /** The overview's state_counts: the state summary and the state column's choices, in the server's order. */
  stateCounts: MonitorStateCount[];
  loading?: boolean;
  error?: Error | null;
  onRefresh?: () => void;
  /** The state filter, shared by the state summary and the state column; uncontrolled when absent. */
  stateFilter?: string[];
  onStateFilterChange?: (states: string[]) => void;
  /** A signal tile's narrowing to some services, listed as a toolbar filter. */
  signalFilter?: { label: string; deploymentIds: number[] } | null;
  onClearSignalFilter?: () => void;
}) {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const navigate = useNavigate();
  const [ownStateFilter, setOwnStateFilter] = useState<string[]>([]);
  const stateFilter = stateFilterProp ?? ownStateFilter;
  const setStateFilter = (next: string[]) => {
    if (!stateFilterProp) setOwnStateFilter(next);
    onStateFilterChange?.(next);
  };
  const narrow = useNarrow();

  const rows = signalFilter ? services.filter((s) => signalFilter.deploymentIds.includes(s.deployment_id)) : services;

  const allColumns: ResourceColumn<MonitorService>[] = [
    {
      key: "service",
      role: "name",
      title: t("table.serviceName"),
      width: SERVICE_COL_WIDTH,
      sortValue: (row) => row.name,
      render: (_: unknown, row: MonitorService) => (
        <>
          <Link to={`/monitor/d/${row.deployment_id}`} onClick={(event) => event.stopPropagation()}>
            {row.name}
          </Link>
          <span className="resource-table-sub">{t("table.placement", { engine: row.engine, placement: row.placement })}</span>
        </>
      ),
    },
    {
      key: "state",
      filteredValue: stateFilter,
      title: t("table.state"),
      width: 110,
      sortValue: (row) => row.state,
      // The summary shows the single picked state as its pressed chip; a
      // multi-state pick from the funnel keeps the toolbar chip.
      filterChip: stateFilter.length !== 1,
      filters: offeredStates(stateCounts, stateFilter).map(({ state }) => ({ value: state, text: t(`states.${state}`, { defaultValue: state }) })),
      onFilter: (value, row) => row.state === value,
      render: (_: unknown, row: MonitorService) => (
        <Badge variant="dot" status={STATE_BADGE[row.state] ?? "neutral"}>{t(`states.${row.state}`, { defaultValue: row.state })}</Badge>
      ),
    },
    {
      key: "health",
      title: (
        <span>
          {t("table.health")}
          <HealthCheckHelp />
        </span>
      ),
      width: HEALTH_COL_WIDTH,
      sortValue: (row) => HEALTH_ORDER.indexOf(row.health.status),
      filters: HEALTH_ORDER.filter((status) => services.some((row) => row.health.status === status)).map((status) => ({ value: status, text: tCommon(`healthCheck.status.${status}`) })),
      onFilter: (value, row) => row.health.status === value,
      render: (_: unknown, row: MonitorService) => <HealthTag health={row.health} />,
    },
    { key: "replicas", title: t("table.replicas"), width: 90, sortValue: (row) => row.replicas.ready, render: (_: unknown, row: MonitorService) => `${row.replicas.ready}/${row.replicas.desired}` },
    { key: "request_rate", title: t("table.requestRate"), width: 100, sortValue: (row) => row.request_rate, render: (_: unknown, row: MonitorService) => num(row.request_rate, 1) },
    { key: "ttft_p95_ms", title: t("table.ttftP95"), width: 100, sortValue: (row) => row.ttft_p95_ms, render: (_: unknown, row: MonitorService) => renderNumber(row.ttft_p95_ms, formatLatencyText) },
    {
      key: "concurrency",
      title: t("table.concurrency"),
      width: 110,
      sortValue: (row) => row.running,
      render: (_: unknown, row: MonitorService) => (
        <>
          {renderNumber(row.running)} / {renderNumber(row.waiting)}
        </>
      ),
    },
    { key: "kv_usage_pct", title: t("table.kvUsage"), width: 130, sortValue: (row) => row.kv_usage_pct, render: (_: unknown, row: MonitorService) => <KvBar pct={row.kv_usage_pct} /> },
    { key: "kv_hit_rate_pct", title: t("table.kvHitRate"), width: 100, sortValue: (row) => row.kv_hit_rate_pct, render: (_: unknown, row: MonitorService) => (row.kv_hit_rate_pct == null ? <Dash /> : `${row.kv_hit_rate_pct.toFixed(0)}%`) },
    { key: "prompt_tok_s", title: t("table.prefillTokS"), width: 110, sortValue: (row) => row.prompt_tok_s, render: (_: unknown, row: MonitorService) => num(row.prompt_tok_s, 0) },
    { key: "generation_tok_s", title: t("table.decodeTokS"), width: 110, sortValue: (row) => row.generation_tok_s, render: (_: unknown, row: MonitorService) => num(row.generation_tok_s, 0) },
  ];

  // Narrow order is service/state/health/TTFT, which is not the wide
  // table's column order - reorder explicitly rather than filter. The narrow
  // table keeps its own saved layout, so neither overwrites the other.
  const narrowOrder = ["service", "state", "health", "ttft_p95_ms"];
  const byKey = new Map(allColumns.map((c) => [c.key, c]));
  const columns = narrow ? narrowOrder.map((key) => byKey.get(key)!) : allColumns;

  return (
    <section className="min-w-0">
    <ResourceTable<MonitorService>
      tableId={narrow ? "monitor-services-narrow" : "monitor-services"}
      rowKey="deployment_id"
      columns={columns}
      toolbar={<div className="flex flex-wrap items-center gap-4"><h2 className="m-0 text-body font-semibold">{t("table.service")}</h2><StateSummary counts={stateCounts} selected={stateFilter} narrowed={!!signalFilter} onSelect={state => setStateFilter(state === null || (stateFilter.length === 1 && stateFilter[0] === state) ? [] : [state])} /></div>}
      onFilterStateChange={filters => { const next = filters.state ?? []; if (next.join("|") !== stateFilter.join("|")) setStateFilter(next); }}
      extraFilters={signalFilter ? [{ key: "signal", name: t("table.signalFilter"), value: signalFilter.label, onClear: () => onClearSignalFilter?.() }] : undefined}
      data={rows}
      loading={loading}
      error={error}
      onRefresh={onRefresh}
      emptyDescription={tCommon("dataTable.empty")}
      // Mouse-only row affordance (same pattern as GPUPage/PointResultsTable):
      // keyboard/AT users reach the destination through the real <Link> in
      // the service cell instead, so the <tr> keeps its plain row semantics
      // and stays in the accessibility tree as a row, not a link.
      onRow={(row) => ({ onClick: () => navigate(`/monitor/d/${row.deployment_id}`), className: "gpu-clickable-row" })}
    />
    </section>
  );
}
