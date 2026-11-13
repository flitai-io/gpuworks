import { acceleratorMessageParams } from "../../acceleratorMessages";
import type { TFunction } from "i18next";

import type { GPUPredicate, ScheduleMessageLike } from "../../api";
import { durationText } from "../durationText";
import { formatBytes } from "../../format";

/** Render a structured scheduler reason while preserving historical strings. */
export function scheduleText(
  t: TFunction<"deploy">,
  value: ScheduleMessageLike | null | undefined,
): string {
  if (typeof value === "string") return value;
  if (!value) return "";

  const code = value.code?.trim() ?? "";
  if (code === "accelerator_feature_unsupported") {
    return t("schedule.accelerator_feature_unsupported", acceleratorMessageParams(t, value.params));
  }
  if (code) {
    // A storage reason says which storage and why; the more specific text
    // wins when there is one for that why.
    const why = typeof value.params?.reason === "string" ? value.params.reason : "";
    if (code === "storage_unavailable" && why && value.params?.storage) {
      const specific = t(`storageRefusal.${why}`, { ...value.params, defaultValue: "" });
      if (specific) return specific;
    }
    // schedule.* holds the scheduler's own codes (the i18n check ties it to
    // the scheduler source), worded for replicas that exist.
    const key = `schedule.${code}`;
    const params = code === "waiting_for_yield"
      ? { ...value.params, holders: holdersText(t, value.params?.holders) }
      : code === "reserved_for_sleepers"
        ? { ...value.params, sleepers: sleepersText(t, value.params?.sleepers), gpus: listText(value.params?.gpus) }
        : code === "does_not_wait_for_yield" || code === "held_by_non_yielding"
          ? heldParams(t, value.params)
          : value.params;
    const translated = t(key, { ...params, defaultValue: "" });
    if (translated && translated !== key) return translated + scheduleNote(t, value);
  }
  return value.message?.trim() || code;
}

/**
 * Why the deploy form's evaluation turned a node down for a draft. The
 * evaluate API adds codes of its own for refusals before or around planning
 * (placementReason.*), worded for a deployment that does not exist yet; they
 * win over the scheduler's replica wording of the same code (a cordoned node
 * has no replica to wait). Any other code reads as the scheduler says it.
 */
export function placementRefusalText(
  t: TFunction<"deploy">,
  value: ScheduleMessageLike | null | undefined,
): string {
  const code = value && typeof value !== "string" ? value.code?.trim() ?? "" : "";
  if (code && value && typeof value !== "string") {
    if ((code === "chip_mismatch" || code === "vendor_mismatch") && Array.isArray(value.params?.actual_gpus)) {
      const facts = value.params.actual_gpus as GPUPredicate[];
      const actual = [...new Set(facts.map((gpu) => [gpu.name || t("placementReason.gpuUnknown"), gpu.vendor, gpu.arch_id,
        (gpu.vendor === "nvidia" || gpu.cc) ? t("placementReason.gpuCC", { value: gpu.cc || t("placementReason.gpuUnknown") }) : ""].filter(Boolean).join(" · ")))].join("; ") || t("placementReason.noReportedGPU");
      const required = [value.params.required_vendor || value.params.vendor, value.params.required_arch_id,
        value.params.required_cc ? t("placementReason.gpuCC", { value: value.params.required_cc }) : "",
        value.params.required_name ? t("placementReason.gpuNamePattern", { value: value.params.required_name }) : ""].filter(Boolean).join(" · ");
      return t("placementReason.hardwareDifference", { required, actual });
    }
    const key = `placementReason.${code}`;
    const translated = t(key, { ...value.params, defaultValue: "" });
    if (translated && translated !== key) return translated + scheduleNote(t, value);
  }
  return scheduleText(t, value);
}

// Detail a reason carries only sometimes goes in a note rather than the message
// itself, so the message never renders a placeholder for a value nobody wrote.
function scheduleNote(t: TFunction<"deploy">, value: Exclude<ScheduleMessageLike, string>): string {
  switch (value.code) {
    case "insufficient_disk":
    case "insufficient_runtime_disk":
      return diskNote(t, value);
    // A card that hosts sleeping instances offers less than its utilization
    // share; the reason names the residue so the figure can be reconciled with
    // the card.
    case "no_fit":
      return note(t, "scheduleNote.sleeping_residue", "sleeping_gib", value.params?.sleeping_gib);
    // The image ledger's own failure text: what the pull, load or peer transfer
    // reported, which is the part that says whether to retry or pick another
    // source.
    case "image_delivery_failed":
      return note(t, "scheduleNote.image_delivery_detail", "detail", value.params?.detail);
    // What the failed acquisition reported, when it reported anything: the
    // part that says whether a retry can work.
    case "model_unavailable":
      return note(t, "scheduleNote.model_unavailable_detail", "detail", value.params?.detail);
    // The planner's own error for a node it could not plan.
    case "plan_failed":
      return note(t, "scheduleNote.plan_failed_detail", "detail", value.message?.trim());
    // Beside the holders a deployment that never yields would wait for once
    // it shares cards, the ones that never yield keep theirs regardless.
    case "does_not_wait_for_yield":
      return note(t, "scheduleNote.blockers_keep_cards", "blockers", namesText(t, value.params?.blockers));
    // Beside the holders that never yield, the ones that would give way, on
    // their terms.
    case "held_by_non_yielding":
      return policyEntries(value.params?.holders).length > 0
        ? note(t, "scheduleNote.holders_give_way", "holders", holdersText(t, value.params?.holders))
        : "";
    default:
      return "";
  }
}

/** Registration must target the model the server actually evaluated. */
export function diskRegistrationModelID(value: ScheduleMessageLike | null | undefined): number | undefined {
  if (!value || typeof value === "string" || value.code !== "insufficient_disk" || value.params?.storage_type !== "local") return undefined;
  const id = value.params.model_id;
  return typeof id === "number" && Number.isSafeInteger(id) && id > 0 ? id : undefined;
}

function diskNote(t: TFunction<"deploy">, value: Exclude<ScheduleMessageLike, string>): string {
  const params = value.params ?? {};
  const parts: string[] = [];
  for (const key of ["node", "storage", "path"] as const) {
    if (typeof params[key] === "string" && params[key].trim()) {
      parts.push(t(`scheduleNote.disk_${key}`, { [key]: params[key] }));
    }
  }
  const bytes = (value: unknown) => typeof value === "number" && Number.isSafeInteger(value) && value >= 0 ? formatBytes(value) : undefined;
  const required = bytes(params.required_bytes);
  const available = bytes(params.available_bytes);
  if (required !== undefined && available !== undefined) parts.push(t("scheduleNote.disk_capacity", { required, available }));
  else if (required !== undefined) parts.push(t("scheduleNote.disk_required", { required }));
  else if (available !== undefined) parts.push(t("scheduleNote.disk_available", { available }));
  if (diskRegistrationModelID(value) !== undefined) parts.push(t("scheduleNote.disk_register"));
  return parts.length ? ` ${parts.join(" ")}` : "";
}

function note(t: TFunction<"deploy">, key: string, param: string, value: unknown): string {
  if (value === undefined || value === null || value === "") return "";
  return " " + t(key, { [param]: value });
}

// A holder a replica waits for, as the server states it: the deployment and
// the terms it gives way on (at once, or once idle, or at the end of its
// turn).
interface YieldHolderTerms {
  deployment?: string;
  at_once?: boolean;
  idle_sec?: number;
  max_serve_sec?: number;
}

function spanText(t: TFunction<"deploy">, seconds: number): string {
  return durationText(t as never, seconds, "exact");
}

// holdersText words who a replica waits for and when each gives way.
function holdersText(t: TFunction<"deploy">, holders: unknown): string {
  const list = Array.isArray(holders) ? (holders as YieldHolderTerms[]) : [];
  if (list.length === 0) return t("scheduleHolder.sleeping");
  return list
    .map((holder) => {
      const deployment = holder.deployment ?? "";
      if (holder.at_once) return t("scheduleHolder.atOnce", { deployment });
      const idle = spanText(t, holder.idle_sec ?? 0);
      if (holder.max_serve_sec) return t("scheduleHolder.idleOrTurn", { deployment, idle, turn: spanText(t, holder.max_serve_sec) });
      return t("scheduleHolder.idle", { deployment, idle });
    })
    .join(t("scheduleHolder.separator"));
}

// sleepersText names the deployments asleep on cards they keep, each with
// its yield policy's name.
function sleepersText(t: TFunction<"deploy">, sleepers: unknown): string {
  return policyEntries(sleepers).map((sleeper) => policyEntryText(t, sleeper)).join(t("scheduleHolder.separator"));
}

// A deployment a reason names with its yield policy, as the server states it.
interface PolicyEntry {
  deployment?: string;
  policy?: string;
}

function policyEntries(values: unknown): PolicyEntry[] {
  return Array.isArray(values) ? (values as PolicyEntry[]) : [];
}

// policyEntryText is a deployment with its yield policy's name.
function policyEntryText(t: TFunction<"deploy">, entry: PolicyEntry): string {
  return t("scheduleSleeper", {
    deployment: entry.deployment ?? "",
    policy: t(`yieldPolicy.policy.name.${entry.policy ?? ""}`, { ns: "deployments", defaultValue: entry.policy ?? "" }),
  });
}

function namesText(t: TFunction<"deploy">, values: unknown): string {
  return policyEntries(values).map((entry) => entry.deployment ?? "").join(t("scheduleHeld.separator"));
}

// heldParams words a reason that names the awake deployments yield policy
// keeps a replica from: every one on the cards with its policy (occupants),
// the ones that would give way (yielders) and the ones that never do
// (blockers), as the server sorted them.
function heldParams(t: TFunction<"deploy">, params: Record<string, unknown> | undefined): Record<string, unknown> {
  const holders = policyEntries(params?.holders);
  const blockers = policyEntries(params?.blockers);
  return {
    ...params,
    gpus: listText(params?.gpus),
    occupants: [...holders, ...blockers].map((entry) => policyEntryText(t, entry)).join(t("scheduleHeld.separator")),
    yielders: namesText(t, holders),
    blockers: namesText(t, blockers),
  };
}

function listText(values: unknown): string {
  return Array.isArray(values) ? values.join(", ") : "";
}
