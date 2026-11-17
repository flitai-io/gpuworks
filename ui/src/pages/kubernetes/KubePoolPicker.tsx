import { useTranslation } from "react-i18next";
import type { ProviderClusterPool } from "../../api";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger } from "../../components/ui/select";
import { ConsoleIcon } from "../../ui-kit/consoleIcons";
import { Notice } from "../../ui-kit/Notice";

/** How a GPU node pool reads in a list: its name, cards and node count. */
export function poolText(t: (key: string, options?: Record<string, unknown>) => string, p: ProviderClusterPool): string {
  return t("clusters.pool", { name: p.name, model: p.gpu_model || t("clusters.mixedModels"), count: p.gpu_count, nodes: p.nodes });
}

/**
 * The GPU node pools an entry holds (or will hold), as the list of the chosen
 * ones: each can be taken out, and the cluster's other pools added from a
 * select. Nothing is ticked in place, so the list reads as what is chosen
 * rather than as a form left open. The last pool cannot be taken out: an
 * entry holds at least one.
 *
 * `held` are the pools the entry holds now. Taking one of them out says what
 * follows: once the manifest is applied again its nodes leave, and what runs
 * on them stops. Which selections are allowed is the server's to say; it
 * refuses the rest with a product error the caller shows.
 */
export default function KubePoolPicker({ pools, selected, onChange, disabled = false, label, held = [] }: {
  pools: ProviderClusterPool[];
  selected: readonly string[];
  onChange: (ids: string[]) => void;
  disabled?: boolean;
  /** The list's accessible name. */
  label: string;
  held?: readonly string[];
}) {
  const { t } = useTranslation("providers");
  const { t: tKube } = useTranslation("kubernetes");
  const chosen = pools.filter((p) => selected.includes(p.id));
  const rest = pools.filter((p) => !selected.includes(p.id));
  const leaving = pools.filter((p) => held.includes(p.id) && !selected.includes(p.id));
  return (
    <div className="flex flex-col gap-2">
      <ul aria-label={label} className="m-0 flex list-none flex-col divide-y divide-border-subtle rounded-md border border-border p-0">
        {chosen.map((p) => (
          <li key={p.id} className="flex min-h-control-md items-center gap-2 px-3 py-1.5 text-body text-fg-default">
            <span className="min-w-0 flex-1 truncate" title={poolText(t, p)}>{poolText(t, p)}</span>
            {p.in_entry && <Badge size="sm" status="success">{t("clusters.inEntry")}</Badge>}
            {chosen.length > 1 && (
              <Button variant="ghost" size="xs" iconOnly aria-label={tKube("pools.removeOne", { name: p.name })} disabled={disabled} onClick={() => onChange(selected.filter((id) => id !== p.id))}>
                <ConsoleIcon.close size={14} />
              </Button>
            )}
          </li>
        ))}
      </ul>
      {rest.length > 0 && (
        // Picking a pool adds it; the select itself holds no value.
        <Select value="" onValueChange={(id) => { if (id) onChange([...selected, id]); }} disabled={disabled}>
          <SelectTrigger aria-label={tKube("pools.add")} placeholder={tKube("pools.add")} className="w-full" />
          <SelectContent>
            {rest.map((p) => <SelectItem key={p.id} value={p.id}>{poolText(t, p)}</SelectItem>)}
          </SelectContent>
        </Select>
      )}
      {leaving.length > 0 && (
        <Notice tone="warning" title={tKube("pools.removing", { names: leaving.map((p) => p.name).join(tKube("vendors.separator")) })}>{tKube("pools.removingHelp")}</Notice>
      )}
    </div>
  );
}
