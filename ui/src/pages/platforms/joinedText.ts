import type { TFunction } from "i18next";
import type { KubeCluster } from "../../api";
import { vendorsText } from "../../kubeClusters";

/** How many nodes joined through a cluster entry, then by the vendor of their cards: "2 nodes · NVIDIA ×2". */
export function joinedText(t: TFunction<"providers">, tKube: TFunction<"kubernetes">, tCommon: (key: string) => string, entry: KubeCluster): string {
  const count = t("clusters.nodes", { count: entry.node_count });
  return entry.vendors?.length ? `${count} · ${vendorsText(tKube, tCommon, entry.vendors)}` : count;
}
