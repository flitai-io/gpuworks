import { Tooltip, Typography } from "antd";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import type { MonitorRequest } from "../../monitorRequestsApi";
import { colors } from "../../theme/tokens";
import { formatMS, formatTokens, requestPhases, type SegmentKind } from "./requestShape";

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

import { callerLabel, RequestDetailLines, RequestHoverCard } from "./RequestDetail";
export { callerLabel as callerName, RequestDetailLines };

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
  // The time to the first token leads the total once there is one.
  const ttft = r.ttft_ms != null ? formatMS(r.ttft_ms) : null;
  const replica = place ?? replicaText(r, replicaLabel, t);
  return (
    <Tooltip title={<RequestHoverCard r={r} replica={replica} />} trigger={["hover", "focus"]} placement="topLeft" mouseEnterDelay={0.15} styles={{ root: { maxWidth: 380 } }}>
      <div
        className="monitor-request-row"
        tabIndex={0}
        role="listitem"
        aria-label={`${callerLabel(r, t)} ${sizeTop} ${sizeBottom} ${ttft ? `TTFT ${ttft} · ` : ""}${formatMS(phases.totalMS)}`}
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
            {callerLabel(r, t)}
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
          <span>
            {ttft && <span style={{ color: colors.muted }} title={t("requests.ttftTitle")}>{ttft} · </span>}
            {formatMS(phases.totalMS)}
          </span>
          {note && <span style={{ color: colors.dangerText, fontSize: 11 }}>{note}</span>}
        </span>
      </div>
    </Tooltip>
  );
}
