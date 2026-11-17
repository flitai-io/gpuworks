import { useTranslation } from "react-i18next";
import type { MonitorService, MonitorStateCount } from "../../api";
import { Badge } from "../../components/ui/badge";
import { FilterChips } from "../../ui-kit/FilterChips";
import { STATE_BADGE, offeredStates } from "./serviceStates";

const ALL = "all";

/**
 * StateSummary is the service table's state filter as one-choice chips:
 * "All N" and each state the server counted, in the server's order
 * (overview state_counts). It shows the state column filter's own
 * selection: none lights "All", exactly one lights that state, and a
 * multi-state pick from the column lights none (the toolbar chip names it).
 */
export default function StateSummary({ counts, selected, narrowed = false, onSelect }: {
  counts: MonitorStateCount[]; selected: string[];
  /** Another filter (a signal tile's) narrows the table, so "All" is not what it shows. */
  narrowed?: boolean;
  onSelect: (state: MonitorService["state"] | null) => void;
}) {
  const { t } = useTranslation("monitoring");
  const total = counts.reduce((sum, item) => sum + item.count, 0);
  const shown = offeredStates(counts, selected);
  const value = selected.length === 0 ? (narrowed ? "" : ALL) : selected.length === 1 ? selected[0] : "";
  return <FilterChips label={t("stateSummary.title")} value={value}
    onChange={(next) => onSelect(next === ALL ? null : next as MonitorService["state"])}
    items={[
      { value: ALL, label: t("stateSummary.all", { count: total }) },
      ...shown.map((item) => ({
        value: item.state as string,
        label: <Badge variant="plain" status={STATE_BADGE[item.state] ?? "neutral"}>{t("stateSummary.count", { state: t(`states.${item.state}`, { defaultValue: item.state }), count: item.count })}</Badge>,
      })),
    ]} />;
}
