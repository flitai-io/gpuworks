import type { TFunction } from "i18next";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { TooltipPortalContainer } from "../../components/ui/tooltip";
import type { MonitorRequest, MonitorRequestSegment, SegmentKind } from "../../monitorRequestsApi";
import { HelpTip } from "../../ui-kit/HelpTip";
import { formatMS, liveEventRate } from "./requestShape";

// A request's detail: who sent it, its size, where its time went as a tree
// (the total, the wait for the first token and the decode after it, each with
// the segments the server put in it), then its speeds and result. The row's
// hover card and the detail drawer show the same. The server splits the time
// and works out the speeds; this lays them out.

interface Line {
  text: string;
  // depth is the line's indent in the time tree; 0 outside it.
  depth: number;
  // help explains how a figure is measured, behind an icon.
  help?: string;
}

const INDENT_PX = 14;

function clock(ms: number): string {
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

const sum = (segments: MonitorRequestSegment[]) => segments.reduce((total, s) => total + s.ms, 0);

// segmentLine is one segment of the tree: its name and time, and what the
// figure leaves out when it is not what its name suggests.
function segmentLine(r: MonitorRequest, s: MonitorRequestSegment, depth: number, t: TFunction): Line {
  const pd = r.pd != null;
  const name = pd && (s.kind === "queue" || s.kind === "prefill") ? t(`requests.segment.${s.kind}_pd`) : t(`requests.segment.${s.kind}`);
  const help: Partial<Record<SegmentKind, string>> = {
    preprocess: t("requests.detail.preprocessHelp"),
    waiting: t("requests.detail.waitingHelp"),
    ...(pd ? { queue: t("requests.detail.queuePDHelp"), prefill: t("requests.detail.prefillPDHelp") } : {}),
    ...(s.kind === "pd_prefill" && r.pd && (r.pd.prefill_queue_ms != null || r.pd.prefill_compute_ms != null)
      ? {
          pd_prefill: t("requests.detail.pdPrefillParts", {
            queue: r.pd.prefill_queue_ms != null ? formatMS(r.pd.prefill_queue_ms) : "?",
            compute: r.pd.prefill_compute_ms != null ? formatMS(r.pd.prefill_compute_ms) : "?",
          }),
        }
      : {}),
  };
  return { text: `${name} ${formatMS(s.ms)}`, depth, help: help[s.kind] };
}

// timeTree is the request's time: the total, and under it the wait for the
// first token and the decode, each over its segments. A reply that does not
// stream has no first token, so its segments sit under the total directly.
function timeTree(r: MonitorRequest, t: TFunction): Line[] {
  const segments = (r.segments ?? []).filter((s) => s.ms > 0);
  const total = sum(segments);
  const lines: Line[] = [
    {
      text: r.phase === "done" ? t("requests.detail.total", { time: formatMS(total) }) : t("requests.detail.elapsed", { time: formatMS(total), phase: t(`requests.phase.${r.phase}`) }),
      depth: 0,
    },
  ];
  const staged = segments.some((s) => s.stage !== "");
  if (!staged) {
    for (const s of segments) lines.push(segmentLine(r, s, 1, t));
    return lines;
  }
  const firstToken = segments.filter((s) => s.stage === "first_token");
  // A queue the engine reported as none takes no time, so it is no segment,
  // but it is a measured fact: it shows as 0 ahead of the prefill.
  if (r.engine_queue_ms === 0 && !firstToken.some((s) => s.kind === "queue")) {
    const at = firstToken.findIndex((s) => s.kind === "prefill");
    firstToken.splice(at < 0 ? firstToken.length : at, 0, { kind: "queue", ms: 0, stage: "first_token" });
  }
  const decode = segments.filter((s) => s.stage === "decode");
  if (firstToken.length > 0) {
    // Still waiting, the first-token stage is the time so far.
    lines.push({ text: r.ttft_ms != null ? t("requests.detail.ttft", { time: formatMS(r.ttft_ms) }) : t("requests.detail.ttftWaiting", { time: formatMS(sum(firstToken)) }), depth: 1 });
    for (const s of firstToken) lines.push(segmentLine(r, s, 2, t));
  }
  if (decode.length > 0) {
    lines.push({
      text: t("requests.detail.decode", { time: formatMS(sum(decode)) }),
      depth: 1,
      help: r.engine_decode_ms != null ? t("requests.detail.decodeEngineHelp", { time: formatMS(r.engine_decode_ms) }) : undefined,
    });
    for (const s of decode) lines.push(segmentLine(r, s, 2, t));
  }
  for (const s of segments.filter((s) => s.stage === "tail")) lines.push(segmentLine(r, s, 1, t));
  return lines;
}

function rate(value: number): string {
  return value >= 100 ? String(Math.round(value)) : value.toFixed(1);
}

// tpot keeps a decimal: a fast decode takes a few milliseconds a token, and
// whole milliseconds would read 3 ms for 300 tok/s and 0 ms past 2000.
function tpot(ms: number): string {
  return ms >= 100 ? formatMS(ms) : `${ms.toFixed(1)} ms`;
}

export function RequestDetailLines({ r, replica }: { r: MonitorRequest; replica: string }) {
  const { t } = useTranslation("monitoring");
  const caller = callerLabel(r, t);
  const groups: Line[][] = [];
  const head: Line[] = [{ text: `${caller} · ${clock(r.received_ms)}${replica ? ` · ${replica}` : ""}`, depth: 0 }];
  if (r.priority_class) head.push({ text: t("requests.detail.priority", { name: r.priority_class }), depth: 0 });
  groups.push(head);

  const size: Line[] = [];
  if (r.prompt_tokens != null) {
    const uncached = Math.max(0, r.prompt_tokens - (r.cached_tokens ?? 0));
    size.push({ text: t("requests.detail.input", { input: r.prompt_tokens, cached: r.cached_tokens ?? "?", uncached: r.cached_tokens != null ? uncached : "?" }), depth: 0 });
  }
  const chunks = r.stream && r.events > 0 ? t("requests.detail.chunks", { events: r.events }) : "";
  if (r.completion_tokens != null) {
    const output = r.reasoning_tokens != null
      ? t("requests.detail.outputReasoning", { output: r.completion_tokens, reasoning: r.reasoning_tokens })
      : t("requests.detail.output", { output: r.completion_tokens });
    size.push({ text: chunks ? `${output} · ${chunks}` : output, depth: 0 });
  } else if (chunks) {
    size.push({ text: chunks, depth: 0 });
  }
  if (size.length > 0) groups.push(size);

  groups.push(timeTree(r, t));

  const after: Line[] = [];
  if (r.prefill_tok_s != null) after.push({ text: t("requests.detail.prefillRate", { speed: rate(r.prefill_tok_s) }), depth: 0 });
  if (r.decode_tok_s != null) after.push({ text: t("requests.detail.decodeRate", { speed: rate(r.decode_tok_s) }), depth: 0 });
  if (r.tpot_ms != null) after.push({ text: t("requests.detail.tpot", { time: tpot(r.tpot_ms) }), depth: 0 });
  if (r.pd?.fallback) after.push({ text: t("requests.detail.pdFallback", { reason: r.pd.fallback }), depth: 0 });
  if (r.phase === "done") {
    if (r.max_gap_ms != null && r.events > 1) after.push({ text: t("requests.detail.maxGap", { time: formatMS(r.max_gap_ms) }), depth: 0 });
    after.push({ text: t("requests.detail.end", { status: r.status ?? "?", reason: r.finish_reason || "—" }), depth: 0 });
    if (r.weight > 1) after.push({ text: t("requests.detail.weight", { count: r.weight }), depth: 0 });
  } else {
    const live = liveEventRate(r);
    if (live != null) after.push({ text: t("requests.detail.eventRate", { rate: Math.round(live), events: r.events }), depth: 0 });
  }
  if (after.length > 0) groups.push(after);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 8, fontVariantNumeric: "tabular-nums" }} data-testid="request-detail">
      {groups.map((lines, g) => (
        <div key={g} style={{ display: "flex", flexDirection: "column", gap: 2 }}>
          {lines.map((line, i) => (
            <DetailLine key={i} line={line} />
          ))}
        </div>
      ))}
    </div>
  );
}

// RequestHoverCard is the detail inside a row's hover card. The card is a
// popup of its own on a dark surface, so the help popups open inside it
// (else they open beneath it) and the help icons take the card's text colour.
export function RequestHoverCard({ r, replica }: { r: MonitorRequest; replica: string }) {
  const [card, setCard] = useState<HTMLDivElement | null>(null);
  return (
    <div ref={setCard} className="monitor-request-card">
      <TooltipPortalContainer value={card}>
        <RequestDetailLines r={r} replica={replica} />
      </TooltipPortalContainer>
    </div>
  );
}

function DetailLine({ line }: { line: Line }): ReactNode {
  return (
    <span style={{ paddingLeft: line.depth * INDENT_PX, display: "inline-flex", alignItems: "center", gap: 4 }}>
      {line.text}
      {line.help && <HelpTip content={line.help} label={line.help} />}
    </span>
  );
}

// callerLabel names who sent the request: its key, or the console entry it
// came through.
export function callerLabel(r: MonitorRequest, t: TFunction): string {
  if (r.api_key_name) return r.api_key_name;
  if (r.source === "playground") return t("requests.playground");
  if (r.source === "evaluation") return t("requests.evaluation");
  if (r.source === "batch" && !r.api_key_id) return t("requests.batch");
  return r.api_key_id ? t("requests.deletedKey", { id: r.api_key_id }) : t("requests.noKey");
}

