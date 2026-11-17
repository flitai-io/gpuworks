import type { TFunction } from "i18next";

import type { InstanceEvent } from "../api";
import { durationText } from "./durationText";
import { paramList as list, paramNumber as num, paramText as text } from "./eventParams";

/**
 * The timeline entries (kind "automation") of a replica moving off a node
 * that stayed offline or is draining, and of the platform picking another
 * place for a unit of an automatically placed deployment (UNIT_PLACED). Their
 * params are names and lists, not counts, so they are worded here rather than
 * by the lifecycle mapping.
 */
export const nodeReplacementEventCodes = [
  "NODE_REPLACED",
  "NODE_REPLACE_REVERTED",
  "NODE_REPLACE_NO_CANDIDATE",
  "NODE_DRAIN_MOVED",
  "UNIT_PLACED",
] as const;

const known = new Set<string>(nodeReplacementEventCodes);

export function isNodeReplacementEvent(code: string): boolean {
  return known.has(code);
}

// what names the replica a node move is about: its number (with the PD role),
// or just "a replica" when the event names none.
function what(t: TFunction<"deployments">, event: InstanceEvent): string {
  const unit = num(event, "unit");
  if (unit == null) return t("nodeReplacement.what.replica");
  const role = text(event, "role");
  const label = role ? `${t(`detail.pd.role.${role}`, { defaultValue: role })} ${unit}` : String(unit);
  return t("nodeReplacement.what.unit", { label });
}

// placedText words UNIT_PLACED: which unit the platform put on which nodes,
// where it was, and - when the nodes it had could not take it - why. The
// reason is a scheduler reason code without its figures, so it is worded by
// cause here; a code this does not know reads as the general cause.
function placedText(t: TFunction<"deployments">, event: InstanceEvent): string {
  const from = list(event, "from").join(", ");
  const reason = text(event, "reason");
  const cause = reason
    ? t("nodeReplacement.placedCause", {
        reason: t(`nodeReplacement.placedReason.${reason}`, { defaultValue: t("nodeReplacement.placedReason.other") }),
      })
    : "";
  const rendered = t("instance.event.UNIT_PLACED", {
    what: what(t, event),
    to: list(event, "to").join(", "),
    from: from ? t("nodeReplacement.placedFrom", { from }) : "",
    cause,
    defaultValue: "",
  });
  return rendered.trim() || event.message?.trim() || "UNIT_PLACED";
}

/** Word one node replacement entry from its code and params. */
export function nodeReplacementEventText(t: TFunction<"deployments">, event: InstanceEvent, code: string): string {
  if (code === "UNIT_PLACED") return placedText(t, event);
  const to = text(event, "to");
  const after = num(event, "after_sec");
  const ucxNodes = list(event, "ucx_netlink_nodes").join(", ");
  const rendered = t(`instance.event.${code}`, {
    from: text(event, "from"),
    to,
    target: to ? t("nodeReplacement.movedTo", { node: to }) : t("nodeReplacement.movedElsewhere"),
    after: after != null ? durationText(t, after) : "",
    what: what(t, event),
    nodes: list(event, "nodes").join(", "),
    defaultValue: "",
  });
  // A PD node the replacement left out for its kernel (the hang the node page
  // lists the fixes for) is named after the sentence, not folded into it.
  const skipped = code === "NODE_REPLACE_NO_CANDIDATE" && ucxNodes ? t("nodeReplacement.ucxNetlinkSkipped", { nodes: ucxNodes }) : "";
  return rendered.trim() ? `${rendered.trim()}${skipped}` : event.message?.trim() || code;
}
