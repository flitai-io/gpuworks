import { Tooltip, Typography } from "antd";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import type { MonitorRequest } from "../../monitorRequestsApi";
import { colors } from "../../theme/tokens";
import { formatMS, formatTokens, liveEventRate, requestPhases, requestSpeeds, type SegmentKind } from "./requestShape";

// Phase colours distinguish stages; successful generation uses the success token. The unsplit wait is a neutral hatch - it means
// "queue and prefill, not told apart".
export const SEGMENT_STYLE: Record<SegmentKind, string> = {
  gateway: colors.border,
  pd_prefill: colors.chartPrimary,
  preprocess: colors.chartViolet,
  queue: colors.muted,
  prefill: colors.chartSecondary,
  waiting: `repeating-linear-gradient(135deg, ${colors.weak} 0 3px, ${colors.controlBorder} 3px 6px)`,
  thinking: colors.chartTeal,
  answering: colors.success,
  tail: colors.borderSoft,
};

export const LEGEND_ORDER: SegmentKind[] = ["pd_prefill", "queue", "waiting", "prefill", "thinking", "answering", "tail", "preprocess", "gateway"];

// segmentLabel names a segment. In a PD request the decode instance's queue
// holds the wait for the KV transfer and its prefill only recomputes the
// last token, so those two read differently there.
export function segmentLabel(kind: SegmentKind, pd: boolean, t: TFunction): string {
  if (pd && (kind === "queue" || kind === "prefill")) return t(`requests.segment.${kind}_pd`);
  return t(`requests.segment.${kind}`);
}

export function callerName(r: MonitorRequest, t: TFunction): string {
  if (r.api_key_name) return r.api_key_name;
  if (r.source === "playground") return t("requests.playground");
  if (r.source === "batch" && !r.api_key_id) return t("requests.batch");
  return r.api_key_id ? t("requests.deletedKey", { id: r.api_key_id }) : t("requests.noKey");
}

function clock(ms: number): string {
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

// sizeLines is the row's size column: input (and what the cache covered)
// over output (and how much of it was reasoning); before usage arrives, the
// request body and the stream so far.
function sizeLines(r: MonitorRequest, t: TFunction): [string, string] {
  if (r.prompt_tokens == null) {
    const kb = r.body_bytes >= 1024 ? `${formatTokens(r.body_bytes)}B` : `${r.body_bytes} B`;
    const body = r.images > 0 ? t("requests.bodyImages", { size: kb, count: r.images }) : t("requests.body", { size: kb });
    return [body, r.events > 0 ? t("requests.chunks", { count: r.events }) : ""];
  }
  const input =
    r.cached_tokens != null && r.cached_tokens > 0
      ? t("requests.inputCached", { input: formatTokens(r.prompt_tokens), cached: formatTokens(r.cached_tokens) })
      : t("requests.input", { input: formatTokens(r.prompt_tokens) });
  // A reply that does not stream has no first token to see: its whole wait
  // is one segment, and the row says why.
  const nonStream = r.stream ? "" : t("requests.nonStream");
  if (r.completion_tokens == null) return [input, nonStream];
  const output =
    r.reasoning_tokens != null && r.reasoning_tokens > 0
      ? t("requests.outputReasoning", { output: formatTokens(r.completion_tokens), reasoning: formatTokens(r.reasoning_tokens) })
      : t("requests.output", { output: formatTokens(r.completion_tokens) });
  return [input, nonStream ? `${output} · ${nonStream}` : output];
}

// outcomeNote is a finished request's result, unless it simply completed;
// an error shows its status code when it had one.
function outcomeNote(r: MonitorRequest, t: TFunction): string | null {
  if (r.phase !== "done" || !r.result || r.result === "completed") return null;
  if (r.result === "error" && r.status != null && r.status > 0 && r.outcome !== "upstream_error") return String(r.status);
  return t(`requests.result.${r.result}`);
}

// replicaText names the replica that served a request: its position and
// node when the replica is still listed, its uuid's start otherwise.
export function replicaText(r: MonitorRequest, replica: { n: number; node: string } | undefined, t: TFunction): string {
  if (replica) return t("requests.replica", { n: replica.n, node: replica.node });
  return r.instance_uuid ? r.instance_uuid.slice(0, 8) : "";
}

// RequestDetailLines are a request's times, speeds, tokens and result, one
// line each: the row's hover card and the detail drawer show the same.
export function RequestDetailLines({ r, replica }: { r: MonitorRequest; replica: string }) {
  const { t } = useTranslation("monitoring");
  const phases = requestPhases(r);
  const speeds = requestSpeeds(r);
  const lines: string[] = [];
  const at = (kind: SegmentKind) => phases.segments.filter((s) => s.kind === kind).reduce((sum, s) => sum + s.ms, 0);
  lines.push(`${callerName(r, t)} · ${clock(r.received_ms)}${replica ? ` · ${replica}` : ""}`);
  if (r.prompt_tokens != null) {
    lines.push(
      t("requests.detail.input", {
        input: r.prompt_tokens,
        cached: r.cached_tokens ?? "?",
        uncached: speeds.uncached ?? "?",
      }),
    );
  }
  if (r.completion_tokens != null) {
    lines.push(
      r.reasoning_tokens != null
        ? t("requests.detail.outputReasoning", { output: r.completion_tokens, reasoning: r.reasoning_tokens })
        : t("requests.detail.output", { output: r.completion_tokens }),
    );
  }
  if (at("gateway") > 0) lines.push(t("requests.detail.gateway", { time: formatMS(at("gateway")) }));
  if (at("pd_prefill") > 0) lines.push(t("requests.detail.pdPrefill", { time: formatMS(at("pd_prefill")) }));
  if (r.pd && (r.pd.prefill_queue_ms != null || r.pd.prefill_compute_ms != null)) {
    const parts = {
      queue: r.pd.prefill_queue_ms != null ? formatMS(r.pd.prefill_queue_ms) : "?",
      compute: r.pd.prefill_compute_ms != null ? formatMS(r.pd.prefill_compute_ms) : "?",
    };
    lines.push(
      speeds.prefillTokS != null
        ? t("requests.detail.pdPrefillPartsSpeed", { ...parts, speed: Math.round(speeds.prefillTokS) })
        : t("requests.detail.pdPrefillParts", parts),
    );
  }
  if (r.pd?.fallback) lines.push(t("requests.detail.pdFallback", { reason: r.pd.fallback }));
  if (at("preprocess") > 0) lines.push(t("requests.detail.preprocess", { time: formatMS(at("preprocess")) }));
  if (r.engine_queue_ms != null || r.engine_prefill_ms != null) {
    if (r.engine_queue_ms != null) {
      lines.push(t(r.pd ? "requests.detail.queuePD" : "requests.detail.queue", { time: formatMS(r.engine_queue_ms) }));
    }
    if (r.engine_prefill_ms != null) {
      if (r.pd) {
        lines.push(t("requests.detail.prefillPD", { time: formatMS(r.engine_prefill_ms) }));
      } else {
        lines.push(
          speeds.prefillTokS != null
            ? t("requests.detail.prefillSpeed", { time: formatMS(r.engine_prefill_ms), speed: Math.round(speeds.prefillTokS) })
            : t("requests.detail.prefill", { time: formatMS(r.engine_prefill_ms) }),
        );
      }
    }
  } else if (phases.firstTokenMS != null || r.phase === "waiting") {
    lines.push(t(r.phase === "waiting" ? "requests.detail.waitingNow" : "requests.detail.firstToken", { time: formatMS(r.phase === "waiting" ? phases.totalMS : (phases.firstTokenMS ?? 0)) }));
  }
  if (r.phase === "done" && r.engine_queue_ms == null) lines.push(t("requests.detail.queueUnavailable"));
  if (at("thinking") > 0) lines.push(t("requests.detail.thinking", { time: formatMS(at("thinking")) }));
  if (at("answering") > 0) lines.push(t("requests.detail.answering", { time: formatMS(at("answering")) }));
  if (r.phase === "done") {
    if (speeds.decodeTokS != null) lines.push(t("requests.detail.decodeSpeed", { speed: Math.round(speeds.decodeTokS) }));
    if (r.engine_decode_ms != null) lines.push(t("requests.detail.decodeEngine", { time: formatMS(r.engine_decode_ms) }));
    lines.push(t("requests.detail.end", { status: r.status ?? "?", reason: r.finish_reason || "—", total: formatMS(phases.totalMS) }));
    if (r.max_gap_ms != null && r.events > 1) lines.push(t("requests.detail.maxGap", { time: formatMS(r.max_gap_ms), events: r.events }));
    if (r.weight > 1) lines.push(t("requests.detail.weight", { count: r.weight }));
  } else {
    const rate = liveEventRate(r);
    if (rate != null) lines.push(t("requests.detail.eventRate", { rate: Math.round(rate), events: r.events }));
  }
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 2, fontVariantNumeric: "tabular-nums" }}>
      {lines.map((line, i) => (
        <span key={i}>{line}</span>
      ))}
    </div>
  );
}

// RequestRow is one request: who sent it, its size, and its phases as a bar
// of its own duration. Hover or focus shows the times and speeds behind it.
// onOpen, when given, makes the row open the request's details on a click or
// Enter.
export default function RequestRow({
  request: r,
  replica: replicaLabel,
  place,
  onOpen,
}: {
  request: MonitorRequest;
  replica?: { n: number; node: string };
  // place, when given, says where the request went in place of its replica
  // (a list across deployments names the deployment).
  place?: string;
  onOpen?: (r: MonitorRequest) => void;
}) {
  const { t } = useTranslation("monitoring");
  const phases = requestPhases(r);
  const total = phases.segments.reduce((sum, s) => sum + s.ms, 0);
  const note = outcomeNote(r, t);
  const [sizeTop, sizeBottom] = sizeLines(r, t);
  // A finished request says when it arrived; one in flight, its phase (its
  // age is the duration at the end of the row).
  const when = r.phase === "done" ? clock(r.received_ms) : t(`requests.phase.${r.phase}`);
  const replica = place ?? replicaText(r, replicaLabel, t);
  return (
    <Tooltip title={<RequestDetailLines r={r} replica={replica} />} trigger={["hover", "focus"]} placement="topLeft" mouseEnterDelay={0.15} styles={{ root: { maxWidth: 380 } }}>
      <div
        className="monitor-request-row"
        tabIndex={0}
        role="listitem"
        aria-label={`${callerName(r, t)} ${sizeTop} ${sizeBottom} ${formatMS(phases.totalMS)}`}
        style={onOpen ? { cursor: "pointer" } : undefined}
        onClick={onOpen ? () => onOpen(r) : undefined}
        onKeyDown={
          onOpen
            ? (e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  onOpen(r);
                }
              }
            : undefined
        }
      >
        <div className="monitor-request-who">
          <Typography.Text ellipsis style={{ fontSize: 12, maxWidth: "100%" }}>
            {callerName(r, t)}
          </Typography.Text>
          <span style={{ fontSize: 11, color: colors.muted, fontVariantNumeric: "tabular-nums", whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>
            {when}
            {replica && ` · ${replica}`}
          </span>
        </div>
        <div className="monitor-request-size">
          <span>{sizeTop}</span>
          {sizeBottom && <span style={{ color: colors.muted }}>{sizeBottom}</span>}
        </div>
        <div className="monitor-request-bar" aria-hidden>
          {phases.segments.map((segment, i) => (
            <span
              key={i}
              data-kind={segment.kind}
              style={{ width: `${total > 0 ? (segment.ms / total) * 100 : 0}%`, background: SEGMENT_STYLE[segment.kind] }}
            />
          ))}
        </div>
        <span className="monitor-request-total">
          <span>{formatMS(phases.totalMS)}</span>
          {note && <span style={{ color: colors.dangerText, fontSize: 11 }}>{note}</span>}
        </span>
      </div>
    </Tooltip>
  );
}
