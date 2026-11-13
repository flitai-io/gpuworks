import type { TFunction } from "i18next";

import type { InstanceEvent } from "../api";
import { isNodeReplacementEvent, nodeReplacementEventText } from "./nodeReplacementEvents.ts";
import { automationEventText, isAutomationEvent, type DeploymentLabel } from "./automationEvents.ts";
import { isOperatorEvent, operatorEventText } from "./operatorEvents.ts";
import { stateLabel } from "./state.ts";
import { formatBytes as formatSize } from "../format";
import { storageStateLabel } from "../storageDisplay";

export const instanceEventCodes = [
  "INSTANCE_CREATED",
  "INSTANCE_STATE_CHANGED",
  "INSTANCE_STATUS_UPDATED",
  "INSTANCE_SLEEP_RESULT",
  "INSTANCE_REMOVED",
] as const;

const knownInstanceEventCodes = new Set<string>(instanceEventCodes);

/**
 * Deployment lifecycle timeline entries: the server's own PD handoff check on
 * a prefill instance (ADR 0028), an operator's restart of the deployment or
 * of one replica, an edit of its configuration, a change of its dGPU memory
 * limit, a template component the create downloads into a shared storage
 * (ADR 0062), a replica giving up
 * after its retry budget or being retried (ADR 0029) or rebuilt within it, and a rolling restart waiting on a replica that gave
 * up, and the shared storage the deployment reads its weights from becoming
 * unavailable on an instance's node or recovering. PD_GROUP_RESTARTED is
 * what restarts wrote before they covered every deployment shape.
 */
export const lifecycleEventCodes = [
  "PD_HANDOFF_VERIFIED",
  "PD_HANDOFF_NOT_RDMA",
  "PD_HANDOFF_FAILED",
  "PD_GROUP_RESTARTED",
  "DEPLOYMENT_RESTARTED",
  "DEPLOYMENT_ROLLING_RESTARTED",
  "DEPLOYMENT_ROLLING_STALLED",
  "DEPLOYMENT_DGPU_QUOTA_CHANGED",
  "DEPLOYMENT_EDITED",
  "DEPLOYMENT_COMPONENT_DOWNLOAD",
  "REPLICA_RESTARTED",
  "UNIT_RETRY_EXHAUSTED",
  "UNIT_RETRIED",
  "REPLICA_REBUILT",
  "STORAGE_UNAVAILABLE",
  "STORAGE_RECOVERED",
] as const;

const knownLifecycleEventCodes = new Set<string>(lifecycleEventCodes);

function lifecycleEventText(t: TFunction<"deployments">, event: InstanceEvent, code: string): string {
  const bytes = numberParam(event, "bytes");
  const rdmaBytes = numberParam(event, "rdma_bytes");
  const fromBytes = numberParam(event, "from_bytes");
  const toBytes = numberParam(event, "to_bytes");
  const transport = paramText(event, "transport");
  const text = t(`instance.event.${code}`, {
    transport: transport ? t(`instance.pdTransport.${transport}`, { defaultValue: transport }) : "",
    bytes: bytes != null ? formatSize(bytes, { compact: true }) : "",
    rdmaBytes: rdmaBytes != null ? formatSize(rdmaBytes, { compact: true }) : "",
    from: fromBytes != null ? formatSize(fromBytes, { compact: true }) : "",
    to: toBytes != null ? formatSize(toBytes, { compact: true }) : "",
    reason: paramText(event, "reason"),
    what: paramText(event, "what"),
    instances: numberParam(event, "instances") ?? "",
    live: numberParam(event, "live") ?? "",
    pending: numberParam(event, "pending") ?? "",
    replicas: numberParam(event, "replicas") ?? "",
    replica: replicaLabel(t, event),
    instance: paramText(event, "instance").slice(0, 8),
    attempts: numberParam(event, "attempts") ?? "",
    attempt: numberParam(event, "attempt") ?? "",
    reasonNote: paramText(event, "reason") ? t("instance.reasonNote", { reason: paramText(event, "reason") }) : "",
    storage: paramText(event, "storage"),
    node: paramText(event, "node"),
    component: paramText(event, "component"),
    model: paramText(event, "model"),
    fields: paramText(event, "fields"),
    count: numberParam(event, "count") ?? "",
    apply: paramText(event, "apply") ? t(`instance.editApply.${paramText(event, "apply")}`, { defaultValue: paramText(event, "apply") }) : "",
    state: storageStateLabel(t as unknown as TFunction, paramText(event, "state")),
    defaultValue: "",
  });
  return text || event.message?.trim() || code;
}

// replicaLabel names the replica an event is about: its number, and on a PD
// deployment its role too, since prefill and decode each count from 1.
function replicaLabel(t: TFunction<"deployments">, event: InstanceEvent): string | number {
  const replica = numberParam(event, "replica");
  if (replica == null) return "";
  const role = paramText(event, "role");
  return role ? `${t(`detail.pd.role.${role}`, { defaultValue: role })} ${replica}` : replica;
}

function paramText(event: InstanceEvent, name: string): string {
  const value = event.params?.[name];
  return typeof value === "string" ? value.trim() : "";
}

function numberParam(event: InstanceEvent, name: string): number | undefined {
  const value = event.params?.[name];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function formatDuration(ms: number, t: TFunction): string {
  if (ms < 1000) return t("instance.duration.ms", { value: Math.max(0, Math.round(ms)) });
  const seconds = ms / 1000;
  if (seconds < 90) return t("instance.duration.seconds", { value: seconds < 10 ? seconds.toFixed(1) : Math.round(seconds) });
  const minutes = Math.floor(seconds / 60);
  return t("instance.duration.minutes", { minutes, seconds: Math.round(seconds - minutes * 60) });
}

/**
 * phaseSummaryText renders what the phase this entry ended actually achieved.
 *
 * A milestone row carries the counters and the elapsed time of the phase before
 * it, because once the instance moves on to the next phase nothing else
 * remembers them. Moved here (formerly InstanceEventTimeline.tsx, deleted
 * alongside the events tab) so the merged log stream's system lines can still
 * say what a folded phase cost, not just that it changed.
 */
export function phaseSummaryText(event: InstanceEvent, t: TFunction): string {
  const phase = paramText(event, "prev_phase");
  const duration = numberParam(event, "prev_duration_ms");
  if (!phase && duration == null) return "";
  const label = phase ? t(`instance.phase.${phase}`, { defaultValue: phase }) : "";
  const parts: string[] = [];
  if (label && duration != null) parts.push(t("instance.phaseSummary.done", { phase: label, duration: formatDuration(duration, t) }));
  else if (label) parts.push(label);
  else if (duration != null) parts.push(formatDuration(duration, t));
  const bytes = numberParam(event, "prev_bytes_done");
  if (bytes && bytes > 0) {
    const rate = numberParam(event, "prev_rate_bps");
    parts.push(rate && rate > 0
      ? t("instance.phaseSummary.transfer", { bytes: formatSize(bytes, { compact: true }), rate: formatSize(rate, { compact: true }) })
      : formatSize(bytes, { compact: true }));
  }
  return parts.join(" · ");
}

function translatedState(tCommon: TFunction<"common">, state: string): string {
  return stateLabel(state, (key) => tCommon(key, { defaultValue: state })).text;
}

function translatedPhase(t: TFunction<"deployments">, phase: string): string {
  if (!phase) return "";
  return t(`instance.phase.${phase}`, { defaultValue: phase });
}

/**
 * Render new code/params events while preserving every historical message.
 * An operator's order names the user who gave it, unless the caller shows the
 * actor in a column of its own (withActor false).
 */
export function instanceEventText(
  t: TFunction<"deployments">,
  tCommon: TFunction<"common">,
  event: InstanceEvent,
  options: { withActor?: boolean; deploymentLabel?: DeploymentLabel } = {},
): string {
  const text = eventBodyText(t, tCommon, event, options.deploymentLabel);
  const actor = paramText(event, "actor");
  return actor && options.withActor !== false ? text + t("instance.actorNote", { actor }) : text;
}

function eventBodyText(
  t: TFunction<"deployments">,
  tCommon: TFunction<"common">,
  event: InstanceEvent,
  deploymentLabel?: DeploymentLabel,
): string {
  const code = event.code?.trim() ?? "";
  if (isOperatorEvent(code)) return operatorEventText(t, event, code);
  if (knownLifecycleEventCodes.has(code)) return lifecycleEventText(t, event, code);
  if (isNodeReplacementEvent(code)) return nodeReplacementEventText(t, event, code);
  if (isAutomationEvent(code)) return automationEventText(t, event, code, deploymentLabel);
  if (!knownInstanceEventCodes.has(code)) return event.message?.trim() || code;

  const rawState = paramText(event, "state");
  const state = translatedState(tCommon, rawState);
  const key = `instance.event.${code}`;
  const level = numberParam(event, "level") ?? numberParam(event, "actual") ?? numberParam(event, "target");
  const result = paramText(event, "result");
  let text = t(key, { state, result, level: level ?? "", defaultValue: "" });
  if (!text || text === key) text = event.message?.trim() || code;

  if (code === "INSTANCE_STATUS_UPDATED") {
    const phase = translatedPhase(t, paramText(event, "phase"));
    if (phase) text += ` [${phase}]`;
  }
  const detail = paramText(event, "detail");
  if (detail) text += `: ${detail}`;
  return text;
}

export function instanceEventStage(event: InstanceEvent): { phase: string; message: string } | undefined {
  const phase = paramText(event, "phase");
  if (phase) return { phase, message: paramText(event, "detail") };
  if (!event.data) return undefined;
  try {
    const data = JSON.parse(event.data) as { phase?: unknown };
    if (typeof data.phase === "string" && data.phase) {
      return { phase: data.phase, message: event.message?.trim() ?? "" };
    }
  } catch {
    // Historical Data was not required to contain JSON.
  }
  return undefined;
}

/** Return the newest sleep failure after an event-id watermark.
 * A successful result closes older failures, so a stale timeline entry cannot
 * remain visible forever after a later successful order.
 */
export function latestSleepFailure(events: InstanceEvent[], afterID = 0): string | undefined {
  for (const event of events) {
    if (event.id <= afterID) continue;
    if (event.code === "INSTANCE_SLEEP_RESULT") {
      const result = paramText(event, "result");
      if (result === "applied") return undefined;
      if (result === "failed_rollback") return paramText(event, "detail") || event.message?.trim() || undefined;
    }
    const phase = paramText(event, "phase");
    const detail = paramText(event, "detail") || event.message?.trim() || "";
    if (phase === "sleep" && detail) return detail;
  }
  return undefined;
}
