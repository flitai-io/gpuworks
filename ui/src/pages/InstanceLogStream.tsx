import { Fragment, useEffect, useMemo, useRef, useState } from "react";
import { Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { api, type InstanceEvent, type LogEntry } from "../api";
import { ansiToSegments, decodeLogEvent } from "./logStream";
import { formatTimeShort } from "../i18n/datetime";
import { instanceEventText, phaseSummaryText } from "./instanceEvents";
import { colors } from "../theme/tokens";
import { mergeLiveSystemLines, mergeSystemLines, type TimelineLogInput } from "./instanceTimeline";
import type { EventTimelineState } from "./useInstanceEvents";
import type { TimeRangeValue } from "../components/TimeRangePicker";
import { quickRange } from "../timeRange";

// The persisted viewer's window: 2000 lines matches the upstream tail the
// instance drawer reads (upstream behavior kept through the console rewrite).
const LOG_TAIL_LIMIT = 2000;

/**
 * LogStream renders the live (or persisted, once the container has exited -
 * or a history `range` is explicitly selected) log tail for one instance.
 * Instance events - state changes, schedule outcomes, errors - are merged in
 * as system lines rather than living on a separate tab, so the timeline reads
 * as one story instead of two competing ones.
 *
 * Live and non-live merge on different clocks (see instanceTimeline.ts):
 * non-live log lines and events both carry a server/agent timestamp, but a
 * live log line is stamped at receipt while an event is stamped by the
 * server, so the two are never compared directly in live mode. A `range`
 * always means "historical", even on a live instance: history replaces the
 * stream rather than merging with it, matching the deployment metrics chart's
 * own time-range control.
 */
export function LogStream({
  uuid,
  live,
  follow,
  range,
  timeline,
}: {
  uuid: string;
  live: boolean;
  follow: boolean;
  range?: TimeRangeValue;
  timeline?: EventTimelineState;
}) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [lines, setLines] = useState<TimelineLogInput[]>([]);
  const [truncated, setTruncated] = useState(false);
  // The live stream reports nothing on its own: EventSource retries a dropped
  // connection and gives up silently on a refused one. Both are surfaced here
  // so a closed stream never looks like an idle one.
  const [streamState, setStreamState] = useState<"open" | "retrying" | "failed">("open");
  const [streamAttempt, setStreamAttempt] = useState(0);
  const preRef = useRef<HTMLPreElement>(null);
  const uuidRef = useRef(uuid);
  // historical is true whenever the view is not showing the live tail: an
  // explicit range always wins over a live instance (history replaces the
  // stream), and an instance that is not live has nothing else to show.
  const historical = !!range || !live;
  // receivedAt classifies instance events by identity, not by comparing
  // clocks: whichever ids are already known the first time a stream merges
  // are history, and everything first seen after that is live, placed at the
  // moment this component observed it (see mergeLiveSystemLines). Resetting
  // it on each new EventSource re-runs that "first merge decides history"
  // rule from scratch for the new session.
  const receivedAtRef = useRef(new Map<number, number>());

  // Persisted path: a selected window (even on a live instance - history
  // replaces the stream), or the recent tail of an exited container.
  useEffect(() => {
    if (!historical) return;
    if (uuidRef.current !== uuid) {
      uuidRef.current = uuid;
      setLines([]);
    }
    let alive = true;
    let retryTimer: number | undefined;
    const toLines = (entries: LogEntry[]): TimelineLogInput[] => {
      // The search API is newest-first; the viewer reads top-to-bottom, so
      // flip to chronological order.
      return entries.slice().reverse().map((entry) => ({ timeMS: entry.time_ms, text: entry.line, level: entry.level }));
    };
    if (range) {
      // A quick range is snapshotted at query time.
      const endMS = range.kind === "custom" ? range.endMS : Date.now();
      const startMS = range.kind === "custom" ? range.startMS : endMS - quickRange(range.key).ms;
      setLines([]);
      setTruncated(false);
      api.logsSearch({ instance: uuid, from: startMS, to: endMS, limit: LOG_TAIL_LIMIT }).then((page) => {
        if (!alive) return;
        setTruncated(!!page.truncated);
        if (page.items.length > 0) setLines(toLines(page.items));
      }).catch(() => {});
      return () => { alive = false; };
    }
    // Once a container exits there is no live tunnel to open. The agent has
    // already shipped its buffered lines to the server log store, so load the
    // chronological tail and keep it visible for post-mortem inspection.
    let retries = 0;
    const loadPersisted = () => {
      api.logsSearch({ instance: uuid, limit: LOG_TAIL_LIMIT }).then((page) => {
        // Preserve lines already received over SSE until the asynchronous
        // log shipper has committed at least one persisted entry.
        if (alive && page.items.length > 0) {
          setTruncated(!!page.truncated);
          setLines(toLines(page.items));
        }
      }).catch(() => {});
      // Log shipping flushes asynchronously as the container exits. Retry
      // briefly so a state update cannot race the final persisted batch.
      if (alive && retries++ < 4) retryTimer = window.setTimeout(loadPersisted, 2000);
    };
    loadPersisted();
    return () => {
      alive = false;
      if (retryTimer !== undefined) window.clearTimeout(retryTimer);
    };
  }, [historical, uuid, range]);

  // Live path: the SSE stream never touches the log store.
  useEffect(() => {
    if (historical) return;
    if (uuidRef.current !== uuid) {
      uuidRef.current = uuid;
      setLines([]);
    }
    receivedAtRef.current = new Map();
    const es = new EventSource(`/api/v1/instances/${uuid}/logs`);
    // The live SSE frame carries no timestamp of its own; it is stamped at
    // the moment it is received, which is also the clock used for events
    // that arrive during this same session (see mergeLiveSystemLines). It
    // also carries no structured level - only the agent's later
    // classification of the persisted copy does.
    es.onmessage = (ev) => {
      const text = decodeLogEvent(ev.data);
      setLines((prev) => [...prev.slice(-LOG_TAIL_LIMIT), { timeMS: Date.now(), text }]);
    };
    es.onerror = () => {
      // A refused stream (auth failure, server error) ends CLOSED and will not
      // come back on its own; a dropped one goes back to CONNECTING and does.
      setStreamState(es.readyState === EventSource.CLOSED ? "failed" : "retrying");
    };
    es.onopen = () => setStreamState("open");
    return () => {
      es.close();
    };
  }, [historical, uuid, streamAttempt]);

  const events = timeline?.events ?? [];
  // receivedAtRef is mutated by the effect above rather than read from state,
  // so recomputing on [lines, events, historical] is sufficient.
  const merged = useMemo(() => {
    if (historical) return mergeSystemLines(lines, events);
    return mergeLiveSystemLines(lines, events, receivedAtRef.current);
  }, [lines, events, historical]);

  useEffect(() => {
    // When follow is re-enabled, this also snaps the view forward to the
    // newest line instead of waiting for the next log event.
    if (follow && preRef.current) preRef.current.scrollTop = preRef.current.scrollHeight;
  }, [merged, follow]);

  const showEventsUnavailable = !!timeline?.error;
  const showEarlierHidden = !!timeline?.hasMore && !showEventsUnavailable;
  const showHistoryHint = !!range && !truncated;
  // truncated is only ever reset inside the historical-path effect above
  // (never by the live path), so a stale true from a previously selected
  // range must not keep the banner up once the view is no longer historical
  // (range cleared back to a still-live instance's tail).
  const showTruncated = historical && truncated;

  return (
    <div>
      {showHistoryHint && (
        <Typography.Text type="secondary" style={{ display: "block", fontSize: 12, margin: "4px 0" }}>
          {t("instance.logsHistoryHint")}
        </Typography.Text>
      )}
      {showTruncated && (
        <Typography.Text type="warning" style={{ display: "block", fontSize: 12, margin: "4px 0" }}>
          {t("instance.logsTruncated", { count: LOG_TAIL_LIMIT })}
        </Typography.Text>
      )}
      <pre
        ref={preRef}
        style={{
          background: "#0d1117",
          color: "#e6edf3",
          padding: 12,
          borderRadius: 2,
          margin: 0,
          height: 320,
          overflowY: "auto",
          fontFamily: "monospace",
          fontSize: 12,
          tabSize: 4,
        }}
      >
        {showEventsUnavailable && (
          <>
            <SystemLine>
              {t("instance.log.eventsUnavailable")}{" "}
              <button
                type="button"
                onClick={timeline?.reload}
                style={{ background: "none", border: "none", color: colors.chartTeal, textDecoration: "underline", cursor: "pointer", font: "inherit", padding: 0 }}
              >
                {t("instance.log.retry")}
              </button>
            </SystemLine>
            {"\n"}
          </>
        )}
        {showEarlierHidden && (
          <>
            <SystemLine>{t("instance.log.earlierEventsHidden")}</SystemLine>
            {"\n"}
          </>
        )}
        {!historical && streamState !== "open" && (
          <>
            <SystemLine>
              {t(streamState === "failed" ? "instance.log.streamFailed" : "instance.log.streamRetrying")}
              {streamState === "failed" && (
                <>
                  {" "}
                  <button
                    type="button"
                    onClick={() => setStreamAttempt((attempt) => attempt + 1)}
                    style={{ background: "none", border: "none", color: colors.chartTeal, textDecoration: "underline", cursor: "pointer", font: "inherit", padding: 0 }}
                  >
                    {t("instance.log.retry")}
                  </button>
                </>
              )}
            </SystemLine>
            {"\n"}
          </>
        )}
        {merged.length ? merged.map((entry, index) => (
          <Fragment key={entry.kind === "system" ? `system-${entry.event.id}` : `log-${index}`}>
            {entry.kind === "log" ? (
              <span style={levelStyle(entry.level)}>
                {historical ? <span style={{ color: "#7d8590" }}>[{formatTimeShort(entry.timeMS)}] </span> : null}
                {ansiToSegments(entry.text).map((segment, segmentIndex) => (
                  <span key={segmentIndex} style={segment.style}>{segment.text}</span>
                ))}
              </span>
            ) : (
              <SystemLine>
                [{formatTimeShort(entry.event.created_at)}] [{t("instance.log.systemTag")}] {systemLineText(entry.event, t, tCommon)}
              </SystemLine>
            )}
            {index < merged.length - 1 ? "\n" : null}
          </Fragment>
        )) : !showEventsUnavailable && !showEarlierHidden ? t(range ? "instance.noLogsInRange" : live ? "instance.waitingLogs" : "instance.noLogs") : null}
      </pre>
    </div>
  );
}

// levelStyle mutes a persisted line by its classified severity - warn/error
// stand out from ordinary output without needing a legend. Undefined/unknown
// levels (including every live SSE line, which carries none) render with the
// pre's own default color.
function levelStyle(level: string | undefined): React.CSSProperties | undefined {
  switch (level) {
    case "warn": return { color: colors.warningText };
    case "error": return { color: colors.dangerText };
    default: return undefined;
  }
}

// systemLineText appends what the phase before this milestone cost, when the
// event carries one - the one thing the old events-tab timeline said that a
// bare instanceEventText does not.
function systemLineText(event: InstanceEvent, t: TFunction<"deployments">, tCommon: TFunction<"common">): string {
  const text = instanceEventText(t, tCommon, event);
  const summary = phaseSummaryText(event, t);
  return summary ? `${text} · ${summary}` : text;
}

function SystemLine({ children }: { children: React.ReactNode }) {
  return (
    <span style={{ background: "#1c2333", color: colors.chartTeal, borderRadius: 2, padding: "0 4px" }}>
      {children}
    </span>
  );
}
