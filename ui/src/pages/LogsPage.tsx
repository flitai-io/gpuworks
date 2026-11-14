import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { AutoComplete, Button, Card, Input, InputNumber, Select, Space, Switch, Typography } from "antd";
import { DownloadOutlined } from "@ant-design/icons";
import { useSearchParams } from "react-router-dom";
import { api, type LogsBucket, type LogEntry, type LogsFilter } from "../api";
import NodePicker from "../nodes/NodePicker";
import PageHeader from "../components/PageHeader";
import { formatTimeSec } from "../i18n/datetime";
import { useTranslation } from "react-i18next";
import LogsHistogram from "./LogsHistogram";
import LogsRangeControl from "./LogsRangeControl";
import { RANGE_MS, histogramBucketMS, initialAbsoluteWindow, type AbsoluteWindow, type RangeKey } from "./logsPageRange";

// Quick-search presets, chosen from the error signatures actually present in
// production stores (see artifacts/archive/2026-09-19-log-center/): Python tracebacks,
// the Ascend crash marker, OOM, and container lifecycle failures. Chips
// combine with OR so one click can cover several signatures at once.
const QUICK_CHIPS = ["Traceback", "ERROR", "WARNING", "ERR99999", "out of memory", "no such container"];

const PAGE_SIZE = 500;

function shortId(uuid: string): string {
  return uuid ? uuid.slice(0, 8) : "--------";
}

// A traceback head starts a Python exception dump; frame lines indent with
// whitespace and usually quote a file. Anything else (a different instance,
// a non-indented line) ends the block.
const TRACEBACK_HEAD = /Traceback \(most recent call last\):/;
const FRAME_LINE = /^\s+(File |Corruption |^\s)|^\s+\.{3}\^+$/;

export interface TracebackBlock {
  head: LogEntry;
  frames: LogEntry[];
}

export function isTracebackHead(line: string): boolean {
  return TRACEBACK_HEAD.test(line);
}

export function isTracebackFrame(line: string): boolean {
  return FRAME_LINE.test(line.trimEnd()) || /^\s{2,}/.test(line);
}

// foldTracebacks groups per-instance traceback dumps so a 60-line exception
// reads as one row instead of drowning the stream. Search results arrive
// newest-first, which puts a traceback's frames ABOVE its head row in display
// order; folding therefore runs over the chronological view (the reversed
// input) and hands the rows back in display order, with block.frames kept
// chronological so an expanded block reads like the engine printed it. A head
// whose frames fall outside the loaded page yields an empty block and the row
// renders unfolded. Rows outside blocks pass through untouched.
export function foldTracebacks(entries: LogEntry[]): Array<{ entry: LogEntry; block?: TracebackBlock }> {
  const folded: Array<{ entry: LogEntry; block?: TracebackBlock }> = [];
  let block: TracebackBlock | undefined;
  let instance = "";
  for (const e of [...entries].reverse()) {
    if (block && e.instance === instance && isTracebackFrame(e.line)) {
      block.frames.push(e);
      continue;
    }
    block = undefined;
    if (isTracebackHead(e.line)) {
      block = { head: e, frames: [] };
      instance = e.instance;
      folded.push({ entry: e, block });
      continue;
    }
    folded.push({ entry: e });
  }
  return folded.reverse();
}

// buildQuery combines the free-form query and the selected chips. Chips OR
// among themselves (one click covers several signatures) and AND with the
// free-form text; multi-word chips are quoted for FTS5.
export function buildQuery(q: string, chips: string[]): string {
  const chipPart = chips
    .map((c) => (/\s/.test(c) ? `"${c}"` : c))
    .join(" OR ");
  const text = q.trim();
  const signatures = chips.length > 1 ? `(${chipPart})` : chipPart;
  // FTS5 has no implicit AND beside parentheses; group text to preserve its ORs.
  if (!text) return signatures;
  return signatures ? `(${text}) AND ${signatures}` : text;
}

// highlightTokens extracts the literal terms a result line should emphasize:
// quotes, grouping and FTS5 operators are structural, not content.
export function highlightTokens(query: string): string[] {
  return query
    .replace(/\(|\)|"/g, " ")
    .split(/\s+/)
    .filter((t) => t && t.toUpperCase() !== "OR" && t.toUpperCase() !== "AND" && t.toUpperCase() !== "NOT");
}

export function HighlightedLine({ line, tokens }: { line: string; tokens: string[] }) {
  if (!tokens.length) return <>{line}</>;
  const escaped = tokens.map((t) => t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  const re = new RegExp(`(${escaped.join("|")})`, "gi");
  const parts = line.split(re);
  return (
    <>
      {parts.map((part, i) =>
        re.test(part) && escaped.some((e) => new RegExp(`^${e}$`, "i").test(part)) ? (
          <mark key={i} style={{ padding: "0 1px", borderRadius: 2, background: "#ffe58f" }}>
            {part}
          </mark>
        ) : (
          <span key={i}>{part}</span>
        ),
      )}
    </>
  );
}

const LEVEL_COLORS: Record<string, string> = {
  error: "#cf1322",
  fatal: "#a8071a",
  warn: "#d46b08",
  debug: "#8a94a6",
};

export function levelColor(level?: string): string | undefined {
  return level ? LEVEL_COLORS[level] : undefined;
}

// toCsv renders the current result set for offline analysis.
export function toCsv(entries: LogEntry[]): string {
  const esc = (v: string | number) => {
    const s = String(v);
    return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  };
  const head = "TimeMS,Level,Source,Instance,Node,Line";
  const rows = entries.map((e) => [e.time_ms, e.level, e.source, e.instance, e.node, e.line].map(esc).join(","));
  return [head, ...rows].join("\n");
}

/** How many instances one search offers; typing narrows the list on the server. */
const INSTANCE_PICKER_PAGE_SIZE = 50;

export default function LogsPage() {
  const { t } = useTranslation("logs");
  // Seeds the instance filter and, when both bounds are present, an absolute
  // from/to (e.g. the service monitor page's "→" per-line link,
  // /logs?instance=<uuid>&from=<ms>&to=<ms>) from a deep link - read once on
  // mount, not kept in sync afterwards: this console has no other URL-borne
  // state (node/level/q), so a two-way binding would be more machinery than
  // the one link that needs it. The seeded window is then the range
  // control's custom window, adjustable or replaced like any other choice.
  const [searchParams] = useSearchParams();
  const [absoluteWindow, setAbsoluteWindow] = useState<AbsoluteWindow | null>(() => initialAbsoluteWindow(searchParams));
  const [q, setQ] = useState("");
  const [chips, setChips] = useState<string[]>([]);
  const [instance, setInstance] = useState<string | undefined>(() => searchParams.get("instance") || undefined);
  const [node, setNode] = useState<string | undefined>();
  const [level, setLevel] = useState<string | undefined>();
  const [source, setSource] = useState<string>("instance");
  const [range, setRange] = useState<RangeKey>(() => (absoluteWindow ? "custom" : "24h"));
  const [auto, setAuto] = useState(false);

  const [entries, setEntries] = useState<LogEntry[]>([]);
  const [total, setTotal] = useState<number | undefined>();
  const [capped, setCapped] = useState(false);
  const [histogram, setHistogram] = useState<LogsBucket[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);

  const [instanceOptions, setInstanceOptions] = useState<{ uuid: string; state: string }[]>([]);

  // Context window: anchored on one hit, grep -C style, N is user-adjustable.
  const [context, setContext] = useState<{ anchor: LogEntry; rows: LogEntry[]; matchIndex: number } | null>(null);
  const [contextN, setContextN] = useState<number | undefined>(5);
  const [contextLoading, setContextLoading] = useState(false);
  const [expandedBlocks, setExpandedBlocks] = useState<Set<string>>(new Set());

  const cursorRef = useRef<{ ts: number; id: number } | null>(null);
  const fromRef = useRef<number | undefined>(undefined);

  const effectiveQuery = useMemo(() => buildQuery(q, chips), [q, chips]);
  const tokens = useMemo(() => highlightTokens(effectiveQuery), [effectiveQuery]);

  // Stable per range: a fresh Date.now() every render would change runSearch's
  // identity each render and loop the effect forever. A custom window is
  // fixed by definition (it came from a specific deep link), so it never
  // recomputes off Date.now() the way a preset does.
  const fromMS = useMemo(
    () => (range === "custom" ? absoluteWindow?.from : range === "all" ? undefined : Date.now() - RANGE_MS[range]),
    [range, absoluteWindow],
  );
  const toMS = range === "custom" ? absoluteWindow?.to : undefined;

  const runSearch = useCallback(
    async (append: boolean) => {
      setLoading(true);
      setError(null);
      try {
        const f: LogsFilter = {
          q: effectiveQuery || undefined,
          instance: instance || undefined,
          node: node || undefined,
          level: level || undefined,
          source: source,
          from: append ? fromRef.current : fromMS,
          // Arrival-order cursors do not imply a timestamp bound: keep the
          // chosen window on every page, including out-of-order arrivals.
          to: toMS,
          cursor_ts: append ? cursorRef.current?.ts : undefined,
          cursor_id: append ? cursorRef.current?.id : undefined,
          limit: PAGE_SIZE,
          with_total: !append,
        };
        fromRef.current = f.from;
        const page = await api.logsSearch(f);
        setEntries((prev) => (append ? [...prev, ...page.items] : page.items));
        cursorRef.current = page.items.length ? { ts: page.items[page.items.length - 1].time_ms, id: page.items[page.items.length - 1].id } : cursorRef.current;
        setUpdatedAt(new Date());
        if (!append) {
          setTotal(page.total);
          setCapped(!!page.total_capped);
          api
            .logsHistogram({ q: f.q, instance: f.instance, node: f.node, level: f.level, source: f.source, from: f.from, to: f.to }, histogramBucketMS(range, absoluteWindow))
            .then((list) => setHistogram(list.items))
            .catch(() => setHistogram([]));
        }
      } catch (e) {
        setError(e as Error);
        if (!append) setEntries([]);
      } finally {
        setLoading(false);
      }
    },
    [effectiveQuery, instance, node, level, source, fromMS, toMS, range, absoluteWindow],
  );

  // Instances are searched on the server, one page at a time, as typed;
  // failure keeps the plain text input usable.
  const [instanceSearch, setInstanceSearch] = useState("");
  useEffect(() => {
    let current = true;
    api.instancesPage({ page: 1, page_size: INSTANCE_PICKER_PAGE_SIZE, q: instanceSearch })
      .then((page) => { if (current) setInstanceOptions(page.items.map((i) => ({ uuid: i.uuid, state: i.state }))); })
      .catch(() => {});
    return () => { current = false; };
  }, [instanceSearch]);

  useEffect(() => {
    runSearch(false);
  }, [runSearch]);

  // Auto-refresh re-runs the head query, never appending.
  useEffect(() => {
    if (!auto) return;
    const timer = window.setInterval(() => runSearch(false), 5000);
    return () => window.clearInterval(timer);
  }, [auto, runSearch]);

  const exportCsv = () => {
    const blob = new Blob([toCsv(entries)], { type: "text/csv;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `gpuworks-logs-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, "")}.csv`;
    a.click();
    URL.revokeObjectURL(url);
  };

  // Clicking a histogram bar narrows the window to that bucket. The search's
  // `to` is inclusive, so the window ends 1 ms before the next bucket and
  // holds exactly the rows the bar counted; the narrower window then gets
  // finer buckets, so repeated clicks drill down.
  const pickBucket = (startMS: number) => {
    setAbsoluteWindow({ from: startMS, to: startMS + histogramBucketMS(range, absoluteWindow) - 1 });
    setRange("custom");
  };

  const hasMore = entries.length > 0 && entries.length % PAGE_SIZE === 0;

  const blocks = useMemo(() => foldTracebacks(entries), [entries]);

  const loadContext = async (anchor: LogEntry, n: number | undefined) => {
    if (!n) return;
    setContextLoading(true);
    try {
      // Agent/server rows carry no instance: scope the window by node instead.
      const win = await api.logsContext(anchor.instance, anchor.node, anchor.time_ms, anchor.id, n, n);
      setContext({ anchor, rows: win.items, matchIndex: win.match_index });
    } catch {
      setContext(null);
    } finally {
      setContextLoading(false);
    }
  };

  const toggleContext = (anchor: LogEntry) => {
    if (context && context.anchor.id === anchor.id && context.anchor.time_ms === anchor.time_ms) {
      setContext(null);
      return;
    }
    void loadContext(anchor, contextN ?? 5);
  };

  const toggleBlock = (key: string) => {
    setExpandedBlocks((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  const pickInstance = (uuid: string) => {
    setInstance(uuid);
  };

  return (
    <div>
      <PageHeader title={t("title")} description={t("description")} />
      <Card
        title={t("search.title")}
        extra={
          <Space size={12}>
            {total !== undefined && (
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {capped ? t("search.matchesCapped", { count: total }) : t("search.matches", { count: total })}
              </Typography.Text>
            )}
            <span>
              <Switch aria-label={t("search.autoRefresh")} size="small" checked={auto} onChange={setAuto} />{" "}
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {t("search.autoRefresh")}
              </Typography.Text>
            </span>
            <Button size="small" icon={<DownloadOutlined />} onClick={exportCsv} disabled={!entries.length}>
              {t("search.export")}
            </Button>
            {updatedAt && (
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {t("search.updated", { time: updatedAt.toLocaleTimeString() })}
              </Typography.Text>
            )}
          </Space>
        }
      >
        <Space direction="vertical" style={{ width: "100%" }} size={12}>
          <LogsHistogram buckets={histogram} onPick={pickBucket} />
          <Input.Search
            placeholder={t("search.placeholder")}
            enterButton={t("search.button")}
            allowClear
            loading={loading}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            onSearch={() => runSearch(false)}
          />
          <Space wrap>
            <AutoComplete
              allowClear
              style={{ minWidth: 220 }}
              placeholder={t("search.instancePlaceholder")}
              aria-label={t("search.instancePlaceholder")}
              value={instance}
              onChange={(v) => setInstance(v || undefined)}
              onSearch={setInstanceSearch}
              options={instanceOptions.map((i) => ({ value: i.uuid, title: i.uuid, label: `${shortId(i.uuid)} · ${i.state}` }))}
              filterOption={false}
            />
            {/* Nodes are searched on the server, not listed whole. */}
            <NodePicker
              allowClear
              style={{ minWidth: 160 }}
              placeholder={t("search.nodePlaceholder")}
              aria-label={t("search.nodePlaceholder")}
              value={node}
              onChange={(next) => setNode(next as string | undefined)}
            />
            <Select
              allowClear
              style={{ minWidth: 130 }}
              placeholder={t("search.levelPlaceholder")}
              aria-label={t("search.levelPlaceholder")}
              value={level}
              onChange={setLevel}
              options={["info", "warn", "error", "fatal"].map((l) => ({ value: l, label: `≥ ${l}` }))}
            />
            <Select
              style={{ minWidth: 130 }}
              aria-label={t("search.sourceLabel")}
              value={source ?? ""}
              onChange={setSource}
              options={[
                { value: "instance", label: t("search.sourceInstance") },
                { value: "node", label: t("search.sourceNode") },
                { value: "agent", label: t("search.sourceAgent") },
                { value: "server", label: t("search.sourceServer") },
                { value: "", label: t("search.sourceAll") },
              ]}
            />
            <LogsRangeControl
              range={range}
              window={absoluteWindow}
              onPreset={setRange}
              onWindow={(next) => {
                setAbsoluteWindow(next);
                setRange("custom");
              }}
            />
          </Space>
          <Space wrap size={6}>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {t("search.quick")}
            </Typography.Text>
            {QUICK_CHIPS.map((c) => {
              const active = chips.includes(c);
              return (
                <Button
                  key={c}
                  data-testid={`log-quick-${c}`}
                  size="small"
                  type={active ? "primary" : "default"}
                  aria-pressed={active}
                  onClick={() => setChips((prev) => (prev.includes(c) ? prev.filter((x) => x !== c) : [...prev, c]))}
                >
                  {c}
                </Button>
              );
            })}
          </Space>
          {error && (
            <Typography.Text type="danger">
              {t("search.error")}: {error.message}
            </Typography.Text>
          )}
          {context && (
            <Card size="small" title={t("search.contextTitle", { count: contextN })} extra={<Button size="small" onClick={() => setContext(null)}>{t("search.contextClose")}</Button>}>
              <Space wrap style={{ marginBottom: 8 }} align="center">
                <span style={{ fontSize: 12, color: "rgba(0,0,0,0.45)" }}>{t("search.contextLines")}</span>
                <Select
                  size="small"
                  style={{ width: 90 }}
                  value={[5, 10, 20, 50, 100].includes(contextN ?? 0) ? contextN : undefined}
                  placeholder={t("search.contextCustom")}
                  onChange={(n) => {
                    setContextN(n);
                    void loadContext(context.anchor, n);
                  }}
                  options={[5, 10, 20, 50, 100].map((n) => ({ value: n, label: `± ${n}` }))}
                />
                <InputNumber
                  size="small"
                  min={1}
                  max={1000}
                  value={contextN}
                  onChange={(v) => {
                    const n = typeof v === "number" ? v : undefined;
                    setContextN(n);
                    if (n) void loadContext(context.anchor, n);
                  }}
                />
              </Space>
              <div style={{ fontFamily: "SFMono-Regular, Menlo, Consolas, monospace", fontSize: "var(--font-size-body)" }}>
                {context.rows.map((e, i) => (
                  <div
                    key={`${e.time_ms}-${e.id}`}
                    style={{
                      display: "flex", gap: 12, padding: "2px 8px", whiteSpace: "pre-wrap", wordBreak: "break-all",
                      background: i === context.matchIndex ? "#fffbe6" : undefined,
                      borderLeft: i === context.matchIndex ? "3px solid #faad14" : "3px solid transparent",
                    }}
                  >
                    <span style={{ color: "rgba(0,0,0,0.45)", flex: "none" }}>{formatTimeSec(e.time_ms)}</span>
                    <span style={{ color: levelColor(e.level) || undefined }}>
                      <HighlightedLine line={e.line} tokens={i === context.matchIndex ? tokens : []} />
                    </span>
                  </div>
                ))}
              </div>
            </Card>
          )}
          <div data-testid="log-rows" style={{ border: "1px solid #e4e8f0", borderRadius: 8, overflow: "auto", maxHeight: 560 }}>
            {entries.length === 0 && !loading && (
              <div style={{ padding: 32, textAlign: "center", color: "rgba(0,0,0,0.45)" }}>{t("search.empty")}</div>
            )}
            {blocks.map(({ entry: e, block }) => {
              const key = `${e.instance}-${e.time_ms}-${e.id}`;
              return (
                <div key={key}>
                  <div
                    data-testid="log-row"
                    style={{ display: "flex", gap: 12, padding: "3px 12px", fontFamily: "SFMono-Regular, Menlo, Consolas, monospace", fontSize: "var(--font-size-body)", borderBottom: "1px solid #f0f2f5", whiteSpace: "nowrap", alignItems: "baseline" }}
                  >
                    <span style={{ color: "rgba(0,0,0,0.45)", flex: "none" }}>{formatTimeSec(e.time_ms)}</span>
                    <span
                      role="button"
                      title={e.instance}
                      onClick={() => pickInstance(e.instance)}
                      style={{ color: "#2456f0", cursor: "pointer", flex: "none", width: 76 }}
                    >
                      {shortId(e.instance)}
                    </span>
                    {e.level && (
                      <span style={{ color: levelColor(e.level), fontWeight: 600, flex: "none", width: 44 }} data-testid="log-level">
                        {e.level}
                      </span>
                    )}
                      {block && block.frames.length > 0 ? (
                      <span style={{ flex: 1, display: "flex", gap: 8, alignItems: "baseline" }}>
                        <span style={{ color: levelColor("error") }}>
                          <HighlightedLine line={block.head.line} tokens={tokens} />
                        </span>
                        <Button
                          size="small"
                          type="link"
                          data-testid="traceback-toggle"
                          onClick={() => toggleBlock(key)}
                        >
                          {expandedBlocks.has(key)
                            ? t("traceback.collapse", { count: block.frames.length })
                            : t("traceback.expand", { count: block.frames.length })}
                        </Button>
                      </span>
                    ) : (
                      <span style={{ display: "flex", flex: 1, gap: 8, alignItems: "baseline" }}>
                        <span style={{ color: levelColor(e.level), whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
                          <HighlightedLine line={e.line} tokens={tokens} />
                        </span>
                        <Button
                          size="small"
                          type="link"
                          style={{ marginLeft: "auto", flex: "none" }}
                          data-testid="context-button"
                          loading={contextLoading && context?.anchor.id === e.id}
                          onClick={() => toggleContext(e)}
                        >
                          {t("search.context")}
                        </Button>
                      </span>
                    )}
                  </div>
                  {block && expandedBlocks.has(key) && (
                    <div style={{ background: "#fff7f6", borderBottom: "1px solid #f0f2f5", padding: "4px 12px 6px 60px", fontFamily: "SFMono-Regular, Menlo, Consolas, monospace", fontSize: "var(--font-size-body)" }}>
                      {block.frames.map((f) => (
                        <div key={`${f.time_ms}-${f.id}`} style={{ whiteSpace: "pre-wrap", wordBreak: "break-all", color: levelColor("error") }}>
                          {f.line}
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
          <Space wrap>
            <Button size="small" onClick={() => runSearch(false)} loading={loading}>
              {t("search.refresh")}
            </Button>
            {hasMore && (
              <Button size="small" onClick={() => runSearch(true)} loading={loading}>
                {t("search.loadMore")}
              </Button>
            )}
          </Space>
        </Space>
      </Card>
    </div>
  );
}
