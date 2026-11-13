import type { TFunction } from "i18next";
import type { GPU } from "../../api";
import { gpuStateReason } from "../../components/GPUHealth";
import { cardKey, cardOrdinal, cardPickable } from "../../nodes/cardIdentity";
import { gpuResourceHint, type GPUResourceContext } from "./gpuResourceHint";

/**
 * The options of a GPU multi-select for one node. Every card is listed so
 * the operator sees the whole node; one that cannot be picked (no device
 * number, or the server says it takes no work) is greyed with the reason.
 * Evaluated numbered cards keep their index so an invalidated selection
 * remains removable. Unnumbered cards retain their own distinct keys.
 */
export function gpuSelectOptions(gpus: GPU[] | undefined, tc: TFunction, context?: GPUResourceContext): { value: number | string; label: string; disabled: boolean }[] {
  return (gpus ?? []).map((gpu) => {
    const pickable = cardPickable(gpu);
    const reason = pickable ? gpuResourceHint(gpu, context)?.text : gpuStateReason(tc, gpu);
    const base = `GPU ${cardOrdinal(gpu)} · ${gpu.name}`;
    return {
      value: pickable || (context && gpu.index >= 0) ? gpu.index : cardKey(gpu),
      label: reason ? `${base} · ${reason}` : base,
      disabled: !pickable && !context?.selected?.includes(gpu.index),
    };
  });
}
