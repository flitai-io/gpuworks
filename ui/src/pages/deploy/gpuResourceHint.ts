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

// The server's evaluated verdict on one card for the request being formed,
// when there is a current one for this node.
function gpuCandidate(gpu: GPU, context?: GPUResourceContext) {
  if (!context || context.pending || context.candidates?.node_uuid !== context.nodeUUID) return undefined;
  return context.candidates.items.find((item) => item.index === gpu.index);
}

// Whether the card can be picked: the inventory says it takes work, or the
// server evaluated it as able to host this very request - a card shared with
// processes outside the platform takes no whole-card work, yet the server
// offers it to a memory-limited partition that fits beside them.
export function gpuPickable(gpu: GPU, context?: GPUResourceContext): boolean {
  if (cardPickable(gpu)) return true;
  return gpu.index >= 0 && gpuCandidate(gpu, context)?.available === true;
}

// Resource feedback annotates a card; the inventory state, or the server's
// verdict that the card hosts this request, gates selection (gpuPickable).
export function gpuResourceHint(gpu: GPU, context?: GPUResourceContext) {
  if (!context || !gpuPickable(gpu, context)) return undefined;
  const item = gpuCandidate(gpu, context);
  if (!item) return { unavailable: false, text: context.t("workbench.gpu.notEvaluated") };
  if (item.available) {
    // What choosing the card entails, as the server notes it (a card shared
    // with processes outside the platform); the card stays pickable.
    const note = item.note;
    if (!note) return undefined;
    return { unavailable: false, text: context.t(`workbench.gpu.resourceNote.${note.code}`, { ...note.params, defaultValue: note.code }) };
  }
  const reason = item.reason;
  const detail = reason && context.t(`workbench.gpu.resourceReason.${reason.code}`, { ...reason.params, defaultValue: "" });
  return { unavailable: true, text: context.t("workbench.gpu.resourcesUnavailable", { reason: detail || scheduleText(context.t, reason) }) };
}
