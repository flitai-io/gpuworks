import { useTranslation } from "react-i18next";
import type { ProviderClusterPool } from "../../api";
import { Badge } from "../../components/ui/badge";
import { Checkbox } from "../../components/ui/checkbox";

/** How a GPU node pool reads in a list: its name, cards and node count. */
export function poolText(t: (key: string, options?: Record<string, unknown>) => string, p: ProviderClusterPool): string {
  return t("clusters.pool", { name: p.name, model: p.gpu_model || t("clusters.mixedModels"), count: p.gpu_count, nodes: p.nodes });
}

/**
 * The GPU node pools of one cloud cluster, each with a checkbox: the pools an
 * entry holds (or will hold), of any vendors. A pool the entry already holds
 * says so. Which selections are allowed is the server's to say; it refuses the
 * rest with a product error the caller shows.
 */
export default function KubePoolPicker({ pools, selected, onChange, disabled = false, label }: {
  pools: ProviderClusterPool[];
  selected: readonly string[];
  onChange: (ids: string[]) => void;
  disabled?: boolean;
  /** The group's accessible name. */
  label: string;
}) {
  const { t } = useTranslation("providers");
  const toggle = (id: string, on: boolean) => onChange(on ? [...selected.filter((s) => s !== id), id] : selected.filter((s) => s !== id));
  return (
    <div role="group" aria-label={label} className="flex flex-col gap-1.5">
      {pools.map((p) => (
        <label key={p.id} className="flex flex-wrap items-center gap-2">
          <Checkbox checked={selected.includes(p.id)} disabled={disabled} aria-label={poolText(t, p)} onCheckedChange={(checked) => toggle(p.id, checked === true)} />
          <span>{poolText(t, p)}</span>
          {p.in_entry && <Badge status="success">{t("clusters.inEntry")}</Badge>}
        </label>
      ))}
    </div>
  );
}
