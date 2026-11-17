import { useCallback, useEffect, useRef, useState } from "react";
import { Alert, Button, DatePicker, InputNumber, Select, Space, Typography } from "antd";
import type { Dayjs } from "dayjs";
import dayjs from "dayjs";
import { useTranslation } from "react-i18next";
import { api, type MonitorRange } from "../../api";
import { MONITOR_REQUEST_RESULTS, type MonitorRequest, type MonitorRequestResult } from "../../monitorRequestsApi";
import { colors } from "../../theme/tokens";
import { Placeholder, RequestList, type ReplicaLabels } from "./RequestView";

const PAGE_SIZE = 50;

// The filters that pick which finished requests are listed. A change starts
// over at the first page.
interface Filters {
  // An explicit time range; null follows the page's range, ending now.
  window: [Dayjs, Dayjs] | null;
  results: MonitorRequestResult[];
  // End-to-end bounds in seconds; null is open.
  minSec: number | null;
  maxSec: number | null;
  // Time-to-first-token bounds in milliseconds; null is open. Either keeps
  // only streamed replies that had a first token (the server's filter).
  minTtftMs: number | null;
  maxTtftMs: number | null;
}

interface PageState {
  requests: MonitorRequest[];
  // cursors[i] opened page i ("" is the first); the next page's cursor is
  // next. Going back re-reads a page from its cursor.
  cursors: string[];
  next: string;
  // The range the first page applied, pinned so later pages and the total
  // describe the same requests even as "now" moves on.
  fromMS: number;
  toMS: number;
  total: number | null;
  loading: boolean;
  error: Error | null;
}

const EMPTY: PageState = { requests: [], cursors: [""], next: "", fromMS: 0, toMS: 0, total: null, loading: true, error: null };

// RequestHistory lists a deployment's finished requests a page at a time,
// newest first: within the page's range or a picked one, by result, by
// end-to-end time, and by the caller and replica the view narrows to. Pages
// move by cursor (previous / next), so a page deep in a busy range is as
// quick to read as the first.
export default function RequestHistory({
  deploymentId,
  range,
  apiKey,
  instance,
  replicas,
  onRows,
}: {
  deploymentId: number;
  range: MonitorRange;
  apiKey: number;
  instance: string;
  replicas: ReplicaLabels;
  onRows: (r: MonitorRequest[]) => void;
}) {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const [filters, setFilters] = useState<Filters>({ window: null, results: [], minSec: null, maxSec: null, minTtftMs: null, maxTtftMs: null });
  const [state, setState] = useState<PageState>(EMPTY);
  // A generation counter drops answers to a query the user has since changed.
  const generation = useRef(0);

  // load reads page index from cursor; the first page (index 0) applies the
  // range afresh, later ones keep the range the first page pinned.
  const load = useCallback(
    async (index: number, cursor: string, pinned: { fromMS: number; toMS: number } | null) => {
      const mine = ++generation.current;
      setState((prev) => ({ ...prev, loading: true, error: null }));
      const window = pinned ?? (filters.window ? { fromMS: filters.window[0].valueOf(), toMS: filters.window[1].valueOf() } : null);
      try {
        const page = await api.monitorRequests(deploymentId, {
          view: "history",
          range,
          ...(window ? { from: window.fromMS, to: window.toMS } : {}),
          limit: PAGE_SIZE,
          key: apiKey,
          instance,
          results: filters.results,
          minMs: filters.minSec != null ? filters.minSec * 1000 : undefined,
          maxMs: filters.maxSec != null ? filters.maxSec * 1000 : undefined,
          minTtftMs: filters.minTtftMs ?? undefined,
          maxTtftMs: filters.maxTtftMs ?? undefined,
          cursor,
        });
        if (mine !== generation.current) return;
        onRows(page.requests);
        setState((prev) => ({
          requests: page.requests,
          cursors: [...(index === 0 ? [""] : prev.cursors.slice(0, index)), cursor].slice(0, index + 1),
          next: page.next_cursor,
          fromMS: page.from_ms ?? 0,
          toMS: page.to_ms ?? 0,
          total: index === 0 ? (page.range_total ?? null) : prev.total,
          loading: false,
          error: null,
        }));
      } catch (e) {
        if (mine !== generation.current) return;
        setState((prev) => ({ ...prev, loading: false, error: e as Error }));
      }
    },
    [deploymentId, range, apiKey, instance, filters, onRows],
  );

  const reload = useCallback(() => void load(0, "", null), [load]);
  useEffect(() => {
    reload();
  }, [reload]);

  const page = state.cursors.length - 1;
  const pinned = { fromMS: state.fromMS, toMS: state.toMS };
  const filtered = filters.results.length > 0 || filters.minSec != null || filters.maxSec != null || filters.minTtftMs != null || filters.maxTtftMs != null || apiKey !== 0 || instance !== "";
  const sampled = state.requests.some((r) => r.weight > 1);

  return (
    <div>
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center", marginBottom: 8 }}>
        <DatePicker.RangePicker
          size="small"
          showTime={{ format: "HH:mm" }}
          format="MM-DD HH:mm"
          allowClear
          value={filters.window}
          placeholder={[t("requests.from"), t("requests.to")]}
          aria-label={t("requests.timeRange")}
          disabledDate={(d) => d.isAfter(dayjs())}
          onChange={(value) =>
            setFilters((f) => ({ ...f, window: value && value[0] && value[1] ? [value[0], value[1]] : null }))
          }
        />
        <Select
          size="small"
          mode="multiple"
          allowClear
          aria-label={t("requests.resultFilter")}
          placeholder={t("requests.allResults")}
          value={filters.results}
          onChange={(results) => setFilters((f) => ({ ...f, results }))}
          options={MONITOR_REQUEST_RESULTS.map((r) => ({ value: r, label: t(`requests.result.${r}`) }))}
          style={{ minWidth: 160 }}
          maxTagCount="responsive"
          popupMatchSelectWidth={false}
        />
        <Space.Compact size="small">
          <InputNumber
            size="small"
            min={0}
            step={1}
            aria-label={t("requests.latencyMin")}
            placeholder={t("requests.latencyMin")}
            value={filters.minSec}
            onChange={(v) => setFilters((f) => ({ ...f, minSec: v != null && v > 0 ? v : null }))}
            style={{ width: 104 }}
          />
          <InputNumber
            size="small"
            min={0}
            step={1}
            aria-label={t("requests.latencyMax")}
            placeholder={t("requests.latencyMax")}
            value={filters.maxSec}
            onChange={(v) => setFilters((f) => ({ ...f, maxSec: v != null && v > 0 ? v : null }))}
            style={{ width: 104 }}
            suffix="s"
          />
        </Space.Compact>
        <Space.Compact size="small">
          <InputNumber
            size="small"
            min={0}
            step={100}
            precision={0}
            aria-label={t("requests.ttftMin")}
            placeholder={t("requests.ttftMin")}
            value={filters.minTtftMs}
            onChange={(v) => setFilters((f) => ({ ...f, minTtftMs: v != null && v > 0 ? v : null }))}
            style={{ width: 104 }}
          />
          <InputNumber
            size="small"
            min={0}
            step={100}
            precision={0}
            aria-label={t("requests.ttftMax")}
            placeholder={t("requests.ttftMax")}
            value={filters.maxTtftMs}
            onChange={(v) => setFilters((f) => ({ ...f, maxTtftMs: v != null && v > 0 ? v : null }))}
            style={{ width: 112 }}
            suffix="ms"
          />
        </Space.Compact>
        <Button size="small" onClick={reload} loading={state.loading && page === 0}>
          {t("requests.reload")}
        </Button>
      </div>
      {state.error && (
        <Alert
          type="error"
          showIcon
          message={t("requests.loadFailed")}
          description={state.error.message}
          action={<Button size="small" onClick={() => void load(page, state.cursors[page], page === 0 ? null : pinned)}>{tCommon("actions.retry")}</Button>}
          style={{ marginBottom: 8 }}
        />
      )}
      {state.loading && state.requests.length === 0 && !state.error && <Placeholder text={t("requests.loading")} />}
      {!state.loading && !state.error && state.requests.length === 0 && (
        <Placeholder text={filtered ? t("requests.historyEmptyFiltered") : t("requests.historyEmpty")} />
      )}
      {state.requests.length > 0 && <RequestList requests={state.requests} label={t("requests.history")} replicas={replicas} />}
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 8, flexWrap: "wrap", marginTop: 8 }}>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {state.toMS > 0 &&
            t("requests.rangeSummary", {
              from: dayjs(state.fromMS).format("MM-DD HH:mm"),
              to: dayjs(state.toMS).format("MM-DD HH:mm"),
            })}
          {state.total != null && ` · ${t("requests.rangeTotal", { count: state.total })}`}
          {sampled && <span style={{ color: colors.muted }}> · {t("requests.sampledNote")}</span>}
        </Typography.Text>
        <Space size={6}>
          <Button
            size="small"
            disabled={page === 0 || state.loading}
            onClick={() => void load(page - 1, state.cursors[page - 1], page - 1 === 0 ? null : pinned)}
          >
            {t("requests.prevPage")}
          </Button>
          <Typography.Text style={{ fontSize: 12, fontVariantNumeric: "tabular-nums" }}>{t("requests.page", { n: page + 1 })}</Typography.Text>
          <Button size="small" disabled={!state.next || state.loading} onClick={() => void load(page + 1, state.next, pinned)}>
            {t("requests.nextPage")}
          </Button>
        </Space>
      </div>
    </div>
  );
}
