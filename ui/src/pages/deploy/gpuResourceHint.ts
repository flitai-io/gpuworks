import type { TFunction } from "i18next";
import type { GPU, GPUCandidates } from "../../api";
import { cardPickable } from "../../nodes/cardIdentity";
import { scheduleText } from "./deploySchedule";

export type GPUResourceContext = {
  nodeUUID: string;
  candidates?: GPUCandidates;
  pending?: boolean;
  selected?: number[];
  t: TFunction<"deploy">;
};

// Resource feedback annotates a card; only its inventory state gates selection.
export function gpuResourceHint(gpu: GPU, context?: GPUResourceContext) {
  if (!context || !cardPickable(gpu)) return undefined;
  const item = !context.pending && context.candidates?.node_uuid === context.nodeUUID
    ? context.candidates.items.find((item) => item.index === gpu.index)
    : undefined;
  if (!item) return { unavailable: false, text: context.t("workbench.gpu.notEvaluated") };
  if (item.available) return undefined;
  const reason = item.reason;
  const detail = reason && context.t(`workbench.gpu.resourceReason.${reason.code}`, { ...reason.params, defaultValue: "" });
  return { unavailable: true, text: context.t("workbench.gpu.resourcesUnavailable", { reason: detail || scheduleText(context.t, reason) }) };
}
