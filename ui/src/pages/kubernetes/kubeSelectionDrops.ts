import type { KubeSelectionDrop, KubeSelectionDrops } from "../../api";
import { APIError } from "../../apiError";

/** The server refuses a narrower node selection that leaves out nodes still running instances until it is confirmed. */
export const KUBE_SELECTION_DROPS_CODE = "KUBE_SELECTION_DROPS_INSTANCES";

/** The nodes of that refusal, or null when the error is anything else. */
export function selectionDropsOf(error: unknown): KubeSelectionDrops | null {
  if (!(error instanceof APIError) || error.code !== KUBE_SELECTION_DROPS_CODE) return null;
  const nodes = error.params?.nodes;
  const list: KubeSelectionDrop[] = Array.isArray(nodes) ? nodes.filter((n): n is KubeSelectionDrop => !!n && typeof n === "object") : [];
  return {
    // A node the answer lists without deployments still runs instances.
    nodes: list.map((n) => ({ ...n, deployments: Array.isArray(n.deployments) ? n.deployments : [] })),
    verified: error.params?.verified !== false,
  };
}
