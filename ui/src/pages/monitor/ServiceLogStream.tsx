import { logColors, logSurfaceStyle, logEventStyle, logReplicaColors } from "../../theme/terminalTheme";
import { Fragment, useEffect, useMemo, useState } from "react";
import { Alert, Button, Skeleton, Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { Link } from "react-router-dom";
import { api, type InstanceEvent, type MonitorRange } from "../../api";
import { rangeBounds } from "../../monitorRange";
import { ansiToSegments } from "../logStream";
import { formatTimeShort } from "../../i18n/datetime";
import { instanceEventText, phaseSummaryText } from "../instanceEvents";
import { colors } from "../../theme/tokens";
import { mergeSystemLines, type TimelineLine, type TimelineLogInput } from "../instanceTimeline";
import { INSTANCE_EVENT_PAGE } from "../useInstanceEvents";

// Same multi-series palette TimeSeriesChart uses, so a replica's tint here
// reads consistently with the charts above this stream.
const PALETTE = [colors.chartPrimary, colors.chartSecondary, colors.chartTertiary, colors.chartQuaternary, colors.chartTeal, colors.chartViolet];

// The persisted window this view fetches in one shot - matches the instance
// panel's own LogStream tail length (InstanceLogStream.tsx's LOG_TAIL_LIMIT).
// The server now honors this as a real cap and reports Truncated itself
// (logstore's own limit is far higher), so this is a genuine request
// parameter, not a client-side guess.
const LOG_TAIL_LIMIT = 2000;

interface FetchWindow {
  from: number;
  to: number;
}

interface ReplicaInfo {
  uuid: string;
  color: string;
  logColor: string;
}

function shortId(uuid: string): string {
  return uuid.slice(0, 8);
}

// A replica-tagged instance event: InstanceEvent.id is only unique within one
// instance's own stream, so once mergeSystemLines interleaves several
// replicas' histories, id alone can no longer say which replica a system
// line came from. mergeSystemLines keeps the event object by reference (see
// instanceTimeline.ts), so this extra field survives the merge untouched.
type ReplicaEvent = InstanceEvent & { replicaUuid: string };

// mergeSystemLines rebuilds each log line from only {timeMS, text, level}
// (instanceTimeline.ts), dropping any extra field - so which replica a line
// came from has to survive as literal text; this prefix bakes in and parses
// back out.
const REPLICA_PREFIX = /^\[([0-9a-f]{8})] ([\s\S]*)$/;

interface FetchedLine {
  uuid: string;
  timeMS: number;
  text: string;
  level?: string;
}

function deepLinkHref(uuid: string, w: FetchWindow): string {
  return `/logs?instance=${encodeURIComponent(uuid)}&from=${w.from}&to=${w.to}`;
}

/**
 * ServiceLogStream is the service page's merged log view (plan §3.2,
 * "日志流"): every current replica's persisted log tail, interleaved with
 * each replica's own instance events as system lines, newest first (plan
 * §3.2's own ordering for this stream - unlike the instance panel's
 * chronological, follow-to-bottom tail). There is no live SSE tail here
 * (the instance panel keeps that): this view always reads the persisted log
 * store, following the service page's shared time range control, and
 * refetches only when the replica set or the range changes.
 */
export default function ServiceLogStream({ logInstances, range }: { logInstances: string[]; range: MonitorRange }) {
  const { t } = useTranslation("monitoring");
  const { t: tDeploy } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");

  const [entries, setEntries] = useState<FetchedLine[]>([]);
  const [eventsByReplica, setEventsByReplica] = useState<Record<string, InstanceEvent[]>>({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  const [truncated, setTruncated] = useState(false);
  const [excluded, setExcluded] = useState<Set<string>>(new Set());
  const [reloadKey, setReloadKey] = useState(0);
  // The exact from/to this fetch used, so every per-line deep link opens the
  // same window that produced what's on screen, not a freshly recomputed
  // "now" that would drift from it.
  const [fetchWindow, setFetchWindow] = useState<FetchWindow | null>(null);

  const key = logInstances.join(",");

  // A different replica set (a new deployment, or replicas coming and
  // going) drops any per-replica filter the viewer had set - stale
  // exclusions for replicas that no longer exist are just confusing.
  useEffect(() => {
    setExcluded(new Set());
  }, [key]);

  useEffect(() => {
    if (!key) {
      setEntries([]);
      setTruncated(false);
      setError(null);
      setLoading(false);
      setFetchWindow(null);
      return;
    }
    let alive = true;
    setLoading(true);
    setError(null);
    const { from, to } = rangeBounds(range);
    setFetchWindow({ from, to });
    api.logsSearch({ instances: logInstances, from, to, limit: LOG_TAIL_LIMIT })
      .then((page) => {
        if (!alive) return;
        // Newest first (plan §3.2), same order the search API itself
        // returns - no reordering needed.
        setEntries(page.items.map((entry) => ({
          uuid: entry.instance,
          timeMS: entry.time_ms,
          text: entry.line,
          level: entry.level,
        })));
        setTruncated(!!page.truncated);
      })
      .catch((e) => { if (alive) setError(e as Error); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [key, range, reloadKey]);

  useEffect(() => {
    if (!key) {
      setEventsByReplica({});
      return;
    }
    let alive = true;
    Promise.allSettled(
      logInstances.map((uuid) => api.instanceEvents(uuid, { limit: INSTANCE_EVENT_PAGE }).then((page) => [uuid, page.items] as const)),
    ).then((results) => {
      if (!alive) return;
      const next: Record<string, InstanceEvent[]> = {};
      for (const result of results) {
        if (result.status === "fulfilled") next[result.value[0]] = result.value[1];
      }
      setEventsByReplica(next);
    });
    return () => { alive = false; };
  }, [key, reloadKey]);

  const replicaByShort = useMemo(() => {
    const map = new Map<string, ReplicaInfo>();
    logInstances.forEach((uuid, index) => map.set(shortId(uuid), { uuid, color: PALETTE[index % PALETTE.length], logColor: logReplicaColors[index % logReplicaColors.length] }));
    return map;
  }, [key]);

  const lines = useMemo<TimelineLogInput[]>(
    () => entries
      .filter((entry) => !excluded.has(entry.uuid))
      .map((entry) => ({ timeMS: entry.timeMS, text: `[${shortId(entry.uuid)}] ${entry.text}`, level: entry.level })),
    [entries, excluded],
  );

  const events = useMemo<ReplicaEvent[]>(() => {
    const all: ReplicaEvent[] = [];
    for (const uuid of logInstances) {
      if (excluded.has(uuid)) continue;
      for (const event of eventsByReplica[uuid] ?? []) all.push({ ...event, replicaUuid: uuid });
    }
    return all;
  }, [key, eventsByReplica, excluded]);

  // mergeSystemLines only ever produces chronological (oldest-first) order;
  // reversing its output - rather than re-implementing the merge - is what
  // turns that into this stream's newest-first order (plan §3.2) without
  // duplicating the interleaving/tie-break rules.
  const merged = useMemo(() => mergeSystemLines(lines, events).reverse(), [lines, events]);

  const lineLinkLabel = (replica: string) => t("deployment.logs.openLineConsole", { replica });

  const firstReplica = logInstances[0];
  // The console (LogsPage) seeds a single `instance` plus an absolute
  // from/to on mount; a multi-replica header link still degrades to the
  // first replica only (LogsPage has no multi-instance filter), but now
  // carries the same window this stream fetched.
  const consoleHref = firstReplica && fetchWindow ? deepLinkHref(firstReplica, fetchWindow) : firstReplica ? `/logs?instance=${encodeURIComponent(firstReplica)}` : "/logs";

  if (logInstances.length === 0) {
    return <Typography.Text type="secondary">{t("deployment.logs.empty")}</Typography.Text>;
  }

  return (
    <div>
      <Space wrap style={{ marginBottom: 8, width: "100%", justifyContent: "space-between" }} align="center">
        <Space wrap size={6}>
          {logInstances.map((uuid) => {
            const id = shortId(uuid);
            const color = replicaByShort.get(id)?.color ?? colors.chartPrimary;
            const checked = !excluded.has(uuid);
            return (
              <Tag.CheckableTag
                key={uuid}
                checked={checked}
                onChange={() => setExcluded((prev) => {
                  const next = new Set(prev);
                  if (checked) next.add(uuid);
                  else next.delete(uuid);
                  return next;
                })}
                style={{ border: `1px solid ${color}`, color: checked ? color : colors.weak }}
              >
                {id}
              </Tag.CheckableTag>
            );
          })}
        </Space>
        <Link to={consoleHref}>{t("deployment.logs.openConsole")}</Link>
      </Space>

      {truncated && (
        <Typography.Text type="warning" style={{ display: "block", fontSize: 12, margin: "4px 0" }}>
          {t("deployment.logs.truncated", { count: LOG_TAIL_LIMIT })}
        </Typography.Text>
      )}

      {error && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 8 }}
          message={t("deployment.logs.loadFailed")}
          description={error.message}
          action={<Button size="small" onClick={() => setReloadKey((value) => value + 1)}>{tCommon("actions.retry")}</Button>}
        />
      )}

      {error ? null : loading && entries.length === 0 ? (
        <Skeleton active paragraph={{ rows: 4 }} />
      ) : (
        <pre
          style={{
            ...logSurfaceStyle,
            padding: 12,
            borderRadius: 2,
            margin: 0,
            height: 320,
            overflowY: "auto",
            tabSize: 4,
          }}
        >
          {merged.length ? merged.map((entry, index) => (
            <Fragment key={lineKey(entry, index)}>
              {entry.kind === "log" ? (
                <LogLine text={entry.text} level={entry.level} replicaByShort={replicaByShort} fetchWindow={fetchWindow} linkLabel={lineLinkLabel} />
              ) : (
                <SystemLine
                  event={entry.event as ReplicaEvent}
                  replicaByShort={replicaByShort}
                  timeMS={entry.timeMS}
                  t={tDeploy}
                  tCommon={tCommon}
                  fetchWindow={fetchWindow}
                  linkLabel={lineLinkLabel}
                />
              )}
              {index < merged.length - 1 ? "\n" : null}
            </Fragment>
          )) : t("deployment.logs.noLogsInRange")}
        </pre>
      )}
    </div>
  );
}

function lineKey(entry: TimelineLine, index: number): string {
  if (entry.kind !== "system") return `log-${index}`;
  const replica = (entry.event as ReplicaEvent).replicaUuid;
  return `system-${replica}-${entry.event.id}`;
}

function levelStyle(level: string | undefined): React.CSSProperties | undefined {
  switch (level) {
    case "warn": return { color: logColors.warning };
    case "error": return { color: logColors.error };
    default: return undefined;
  }
}

// LineLink is the per-line "→" deep link into the log console: the same
// replica this line belongs to, scoped to the stream's own fetch window
// (not this line's own timestamp) - plan §9's "跳转 /logs 控制台带 instance
// 与时间窗" applied per line instead of only once at the header. It renders
// inside a .log-line-link sibling (see LineRow) that CSS (src/styles.css)
// hides until the line is hovered or focused, and marks user-select:none so
// copying a line - or the whole log - never picks up the arrow glyph. It
// stays keyboard-reachable regardless (opacity, not display, hides it).
function LineLink({ uuid, fetchWindow, linkLabel }: { uuid: string; fetchWindow: FetchWindow | null; linkLabel: (replica: string) => string }) {
  if (!fetchWindow) return null;
  return (
    <Link to={deepLinkHref(uuid, fetchWindow)} aria-label={linkLabel(shortId(uuid))}>
      →
    </Link>
  );
}

// LineRow is the shared per-line shell: the actual text in one span (what a
// selection/copy picks up), the reveal-on-hover/focus arrow link in a
// sibling span outside it (what a selection/copy must not pick up). tabIndex
// makes the whole line a keyboard stop, so tabbing through the stream reveals
// (and can then tab into) each line's own link without needing to hover it.
function LineRow({
  textStyle, children, uuid, fetchWindow, linkLabel,
}: {
  textStyle?: React.CSSProperties;
  children: React.ReactNode;
  uuid: string | undefined;
  fetchWindow: FetchWindow | null;
  linkLabel: (replica: string) => string;
}) {
  return (
    <span className="log-line" tabIndex={0}>
      <span style={textStyle}>{children}</span>
      {uuid && (
        <span className="log-line-link">
          <LineLink uuid={uuid} fetchWindow={fetchWindow} linkLabel={linkLabel} />
        </span>
      )}
    </span>
  );
}

// LogLine parses the replica prefix baked into `text` back out so it can be
// colored on its own, then renders the remainder through the same ANSI
// decoder InstanceLogStream uses.
function LogLine({
  text, level, replicaByShort, fetchWindow, linkLabel,
}: {
  text: string;
  level: string | undefined;
  replicaByShort: Map<string, ReplicaInfo>;
  fetchWindow: FetchWindow | null;
  linkLabel: (replica: string) => string;
}) {
  const match = REPLICA_PREFIX.exec(text);
  const prefix = match?.[1];
  const rest = match ? match[2] : text;
  const replica = prefix ? replicaByShort.get(prefix) : undefined;
  return (
    <LineRow textStyle={levelStyle(level)} uuid={replica?.uuid} fetchWindow={fetchWindow} linkLabel={linkLabel}>
      {prefix && <span style={{ color: replica?.logColor, marginRight: 6 }}>[{prefix}]</span>}
      {ansiToSegments(rest).map((segment, index) => (
        <span key={index} style={segment.style}>{segment.text}</span>
      ))}
    </LineRow>
  );
}

function SystemLine({
  event, replicaByShort, timeMS, t, tCommon, fetchWindow, linkLabel,
}: {
  event: ReplicaEvent;
  replicaByShort: Map<string, ReplicaInfo>;
  timeMS: number;
  t: TFunction<"deployments">;
  tCommon: TFunction<"common">;
  fetchWindow: FetchWindow | null;
  linkLabel: (replica: string) => string;
}) {
  const id = shortId(event.replicaUuid);
  const color = replicaByShort.get(id)?.logColor;
  const text = instanceEventText(t, tCommon, event);
  const summary = phaseSummaryText(event, t);
  return (
    <LineRow
      textStyle={logEventStyle}
      uuid={event.replicaUuid}
      fetchWindow={fetchWindow}
      linkLabel={linkLabel}
    >
      [{formatTimeShort(timeMS)}] <span style={{ color }}>[{id}]</span> {summary ? `${text} · ${summary}` : text}
    </LineRow>
  );
}
