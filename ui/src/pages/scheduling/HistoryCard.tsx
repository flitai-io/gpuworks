import { Fragment, useEffect, useRef, useState } from "react";
import { Alert, Button, Card, Segmented, Select, Space, Spin, Tag, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, usePolling } from "../../api";
import { schedulingApi, type SchedulingCategory, type SchedulingHistoryEntry } from "../../schedulingApi";
import EmptyState from "../../components/EmptyState";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import TimeRangePicker from "../../components/TimeRangePicker";
import { formatTime } from "../../i18n/datetime";
import { SCHEDULING_QUICK_RANGES, resolveRange, type QuickRangeKey, type TimeRangeSelection } from "../../timeRange";
import { instanceEventText } from "../instanceEvents";
import { NodeLink } from "./NodeLink";

const HISTORY_POLL_MS = 15_000;
// A page shows PAGE_SIZE entries; one more is asked for to know whether there
// is a next page without counting the history.
const PAGE_SIZE = 20;
const CATEGORY_COLORS: Record<SchedulingCategory, string> = { auto: "blue", failure: "red", manual: "default" };

interface Filters {
  category: SchedulingCategory | "all";
  deployment: number | null;
  // A quick range slides with now, a custom one is fixed.
  range: TimeRangeSelection;
}

// The history opens on the last day.
const DEFAULT_RANGE_KEY: QuickRangeKey = "1d";
const DEFAULT_RANGE: TimeRangeSelection = { kind: "quick", key: DEFAULT_RANGE_KEY };
const NO_FILTERS: Filters = { category: "all", deployment: null, range: DEFAULT_RANGE };

/**
 * The since/until of a history query: since now minus the span for a quick
 * range (it slides as the page polls, so it has no end), and both ends of a
 * custom range.
 */
export function historyWindow(range: TimeRangeSelection, nowMS: number): { since?: string; until?: string } {
  const window = resolveRange(range, nowMS);
  return {
    since: new Date(window.startMS).toISOString(),
    until: range.kind === "custom" ? new Date(window.endMS).toISOString() : undefined,
  };
}

// HistoryCard is the scheduling history: when, why and what the platform or an
// operator did to where, whether and how many replicas run, newest first. It
// pages with the entry id as cursor, like a deployment's timeline.
export default function HistoryCard() {
  const { t } = useTranslation("scheduling");
  const [filters, setFilters] = useState<Filters>(NO_FILTERS);
  // cursors[n] is the `before` of page n+1; the first page has none.
  const [cursors, setCursors] = useState<number[]>([0]);
  const page = cursors.length;
  const before = cursors[page - 1] || undefined;
  const history = usePolling(
    () => {
      return schedulingApi.history({
        limit: PAGE_SIZE + 1,
        before,
        category: filters.category === "all" ? undefined : [filters.category],
        deployment_id: filters.deployment ?? undefined,
        ...historyWindow(filters.range, Date.now()),
      });
    },
    HISTORY_POLL_MS,
    [before, filters.category, filters.deployment, filters.range],
  );
  const refine = (next: Partial<Filters>) => {
    setFilters((current) => ({ ...current, ...next }));
    setCursors([0]);
  };
  const entries = history.data?.items ?? [];
  const shown = entries.slice(0, PAGE_SIZE);
  const hasNext = entries.length > PAGE_SIZE;
  const filtered = filters.category !== "all" || filters.deployment !== null || !(filters.range.kind === "quick" && filters.range.key === DEFAULT_RANGE_KEY);

  let body;
  if (!history.data && history.error) {
    body = (
      <Alert
        type="error"
        showIcon
        message={t("history.loadFailed")}
        description={history.error.message}
        action={<Button onClick={() => void history.refetch()}>{t("page.retry")}</Button>}
      />
    );
  } else if (!history.data) {
    body = <div role="status" className="scheduling-loading"><Spin /> <span>{t("history.loading")}</span></div>;
  } else {
    body = (
      <>
        {history.error && (
          <Alert
            style={{ marginBottom: 12 }}
            type="warning"
            showIcon
            message={t("history.refreshFailed", { detail: history.error.message })}
            action={<Button size="small" onClick={() => void history.refetch()}>{t("page.retry")}</Button>}
          />
        )}
        <Spin spinning={history.loading}>
          {shown.length === 0 ? (
            <EmptyState kind="data" compact description={t(filtered ? "history.emptyFiltered" : "history.empty")} />
          ) : (
            <HistoryTable rows={shown} />
          )}
        </Spin>
        {(page > 1 || hasNext) && (
          <nav className="scheduling-pagination" aria-label={t("history.pagination")}>
            <Space>
              <Typography.Text type="secondary">{t("history.page", { page })}</Typography.Text>
              <Button disabled={page === 1} onClick={() => setCursors((current) => current.slice(0, -1))}>{t("history.newer")}</Button>
              <Button disabled={!hasNext} onClick={() => setCursors((current) => [...current, shown[shown.length - 1].id])}>{t("history.older")}</Button>
            </Space>
          </nav>
        )}
      </>
    );
  }

  return (
    <Card className="scheduling-card">
      <Space wrap className="scheduling-filters">
        <Segmented
          aria-label={t("history.filters.category")}
          value={filters.category}
          options={(["all", "auto", "failure", "manual"] as const).map((value) => ({ value, label: t(`history.category.${value}`) }))}
          onChange={(value) => refine({ category: value as Filters["category"] })}
        />
        <DeploymentFilter value={filters.deployment} onChange={(deployment) => refine({ deployment })} />
        <TimeRangePicker quickRanges={SCHEDULING_QUICK_RANGES} value={filters.range} onChange={(range) => { if (range) refine({ range }); }} />
      </Space>
      {body}
    </Card>
  );
}

// DeploymentFilter picks one deployment by searching names on the server, so
// a fleet's worth of deployments is never loaded into the page.
function DeploymentFilter({ value, onChange }: { value: number | null; onChange: (id: number | null) => void }) {
  const { t } = useTranslation("scheduling");
  const [options, setOptions] = useState<{ value: number; label: string }[]>([]);
  const [search, setSearch] = useState("");
  const chosen = useRef<{ value: number; label: string } | null>(null);
  useEffect(() => {
    let live = true;
    const timer = setTimeout(() => {
      api.deploymentsPage({ page: 1, page_size: 20, q: search.trim() || undefined })
        .then((page) => { if (live) setOptions(page.items.map((item) => ({ value: item.id, label: item.name }))); })
        .catch(() => { if (live) setOptions([]); });
    }, search ? 250 : 0);
    return () => { live = false; clearTimeout(timer); };
  }, [search]);
  // The chosen deployment stays an option while the search moves on.
  const shown = chosen.current && value === chosen.current.value && !options.some((o) => o.value === value) ? [chosen.current, ...options] : options;
  return (
    <Select<number>
      showSearch
      allowClear
      style={{ width: 240 }}
      aria-label={t("history.filters.deployment")}
      placeholder={t("history.filters.deploymentPlaceholder")}
      value={value ?? undefined}
      options={shown}
      filterOption={false}
      onSearch={setSearch}
      onChange={(id) => {
        chosen.current = id == null ? null : (shown.find((o) => o.value === id) ?? null);
        onChange(id ?? null);
      }}
      onClear={() => { chosen.current = null; onChange(null); }}
      notFoundContent={t("history.filters.deploymentNone")}
    />
  );
}

// DeploymentRef names a deployment an entry is about: a link to it, or - when
// it was deleted, or the server could not name it - its name or "#id" with no
// page to open.
function DeploymentRef({ id, name, deleted }: { id: number; name?: string; deleted?: boolean }) {
  const { t } = useTranslation("scheduling");
  const label = name || `#${id}`;
  if (deleted) return <span>{t("history.deleted", { name: label })}</span>;
  if (!name) return <span>{label}</span>;
  return <Link to={`/deployments?deployment=${id}`}>{label}</Link>;
}

// TARGET marks where the details name the deployment the entry acted for:
// the wording is plain text (i18next), so the name is put in as a marker and
// each marker is then rendered as a DeploymentRef.
const TARGET = "\u0000target\u0000";

// DetailsCell words an entry as the deployment's own timeline words it, so
// the two read the same, with the deployment it acted for (a yield's) named
// and linked rather than "#id".
function DetailsCell({ row }: { row: SchedulingHistoryEntry }) {
  const { t: tDeployments } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const event = { id: row.id, kind: row.kind, code: row.code, params: row.params, created_at: row.created_at };
  const target = row.target;
  const text = instanceEventText(tDeployments, tCommon, event, { withActor: false, deploymentLabel: target ? () => TARGET : undefined });
  const details = !target || !text.includes(TARGET)
    ? <Typography.Text>{text}</Typography.Text>
    : (
      <Typography.Text>
        {text.split(TARGET).map((part, index) => (
          <Fragment key={index}>
            {index > 0 && <DeploymentRef id={target.id} name={target.name} deleted={target.deleted} />}
            {part}
          </Fragment>
        ))}
      </Typography.Text>
    );
  if (!row.logs) return details;
  return <Space size={8} wrap>{details}<FailedAttemptLogs logs={row.logs} /></Space>;
}

// FailedAttemptLogs opens the logs page on the failed attempt's output in the
// window the server named: the entry gives the cause in a line, its output
// the rest.
function FailedAttemptLogs({ logs }: { logs: NonNullable<SchedulingHistoryEntry["logs"]> }) {
  const { t } = useTranslation("scheduling");
  return <Link to={`/logs?instance=${encodeURIComponent(logs.instance)}&from=${logs.from}&to=${logs.to}`}>{t("history.viewLogs")}</Link>;
}

// NodesCell names the nodes an entry concerns, each opening its drawer; none
// is a dash.
function NodesCell({ nodes }: { nodes?: SchedulingHistoryEntry["nodes"] }) {
  if (!nodes?.length) return <span>—</span>;
  return (
    <span>
      {nodes.map((node, index) => (
        <Fragment key={`${node.uuid ?? ""}:${node.name}`}>
          {index > 0 && ", "}
          <NodeLink uuid={node.uuid} name={node.name} />
        </Fragment>
      ))}
    </span>
  );
}

// HistoryTable lists the entries; the actor has a column of its own.
function HistoryTable({ rows }: { rows: SchedulingHistoryEntry[] }) {
  const { t } = useTranslation("scheduling");
  const columns: ResourceColumn<SchedulingHistoryEntry>[] = [
    { key: "at", title: t("history.columns.at"), width: 160, render: (_: unknown, row) => formatTime(row.created_at) },
    { key: "deployment", title: t("history.columns.deployment"), width: 180, render: (_: unknown, row) => <DeploymentRef id={row.deployment_id} name={row.deployment} deleted={row.deployment_deleted} /> },
    { key: "node", title: t("history.columns.node"), width: 140, render: (_: unknown, row) => <NodesCell nodes={row.nodes} /> },
    { key: "action", title: t("history.columns.action"), width: 160, render: (_: unknown, row) => t(`history.action.${row.code}`, { defaultValue: row.code }) },
    {
      key: "details",
      title: t("history.columns.details"),
      width: 440,
      ellipsis: false,
      render: (_: unknown, row) => <DetailsCell row={row} />,
    },
    { key: "category", title: t("history.columns.category"), width: 90, render: (_: unknown, row) => <Tag color={CATEGORY_COLORS[row.category]}>{t(`history.category.${row.category}`)}</Tag> },
    { key: "actor", title: t("history.columns.actor"), width: 110, render: (_: unknown, row) => row.actor || "—" },
  ];
  return <ResourceTable<SchedulingHistoryEntry> tableId="scheduling-history" columns={columns} data={rows} rowKey={(row) => String(row.id)} pagination={false} columnSettings={false} emptyDescription={t("history.empty")} />;
}
