import type { TFunction } from "i18next";

import type { EvaluateResponse } from "../../api";
import { diskRegistrationModelID, placementRefusalText } from "./deploySchedule";

// A node the automatic placement looked at and turned down, with the reason
// the scheduler gave, already in the viewer's language.
export interface NodeRefusal {
  uuid: string;
  name: string;
  reason: string;
  /** Only present for a local-model disk refusal with a server model ID. */
  modelId?: number;
}

// Reasons the eligibility pass gives before any plan is tried: the node is
// offline, cordoned, off the template's chip or outside the node
// requirements. They say less about this deployment than a refused plan does,
// so they list after the nodes that were planned.
const EligibilityCodes = new Set([
  "ineligible",
  "node_offline",
  "node_cordoned",
  "node_requirements_unmet",
  "node_requirements_invalid",
  "vendor_mismatch",
  "chip_mismatch",
  "runtime_unsupported",
  "runtime_unreported",
  "accelerator_feature_unsupported",
  "accelerator_device_selection_required",
]);

type Candidate = NonNullable<EvaluateResponse["candidates"]>[number];

function eligibilityOnly(candidate: Candidate): boolean {
  const reason = candidate.reason;
  return typeof reason === "object" && reason !== null && EligibilityCodes.has(reason.code ?? "");
}

/**
 * The nodes an evaluation turned down for the draft. Empty when the server
 * judged no node at all (nothing online matches) or when a node was selected.
 */
export function nodeRefusals(
  t: TFunction<"deploy">,
  result: EvaluateResponse | null | undefined,
): NodeRefusal[] {
  if (!result || result.selected_node) return [];
  const eligibility = new Map(result.eligible_nodes?.map((node) => [node.uuid, node]));
  return (result.candidates ?? [])
    .filter((candidate) => !candidate.ok)
    .map((candidate, index) => ({ candidate, index }))
    .sort((a, b) => Number(eligibility.get(b.candidate.node_uuid)?.requirements_match === true) - Number(eligibility.get(a.candidate.node_uuid)?.requirements_match === true)
      || Number(eligibilityOnly(a.candidate)) - Number(eligibilityOnly(b.candidate)) || a.index - b.index)
    .map(({ candidate }) => {
      const primaryReason = placementRefusalText(t, candidate.reason);
      const modelId = diskRegistrationModelID(primaryReason ? candidate.reason : candidate.plan?.message);
      return {
        uuid: candidate.node_uuid,
        name: candidate.node_name || candidate.node_uuid,
        ...(modelId !== undefined ? { modelId } : {}),
        reason: [...new Set([
          primaryReason || placementRefusalText(t, candidate.plan?.message),
          ...(eligibility.get(candidate.node_uuid)?.reasons ?? []).map((reason) => placementRefusalText(t, reason)),
        ].filter(Boolean))].join(" "),
      };
    });
}

// No diagnostic means an older server, invalid conditions, or no explicit
// conditions. Never infer matching from the first refusal (vendor can mask it).
export function nodeConditionSummary(result: EvaluateResponse | null | undefined) {
  const nodes = result?.eligible_nodes;
  if (!nodes?.length || nodes.some((node) => typeof node.requirements_match !== "boolean")) return null;
  const matched = nodes.filter((node) => node.requirements_match);
  const hardwareOnly = matched.length > 0 && matched.every((node) => !node.eligible && node.reasons?.length && node.reasons.every((reason) =>
    typeof reason !== "string" && (reason.code === "chip_mismatch" || reason.code === "vendor_mismatch")));
  const eligible = new Set(matched.filter((node) => node.eligible).map((node) => node.uuid));
  // Placement can stop at its trial budget or first success. An absent
  // candidate was not evaluated, so it cannot count as a failed plan.
  const planned = new Map((result?.candidates ?? []).filter((node) => eligible.has(node.node_uuid)).map((node) => [node.node_uuid, node]));
  return {
    total: nodes.length, matched, eligible: eligible.size, hardwareOnly,
    tried: planned.size, passed: [...planned.values()].filter((node) => node.ok).length,
    untried: eligible.size - planned.size,
  };
}
