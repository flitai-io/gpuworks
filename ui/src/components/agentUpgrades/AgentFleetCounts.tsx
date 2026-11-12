import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import type { AgentFleetSummary } from "../../api";
import { Button } from "../ui/button";

export default function AgentFleetCounts({ summary, fleetSearch }: {
  summary: AgentFleetSummary;
  fleetSearch?: (group: string) => string;
}) {
  const { t } = useTranslation("nodes");
  const groups = summary.upgrade === undefined ? ["align", "check"] as const : ["upgrade", "downgrade", "different_build", "check"] as const;
  return <>{groups.map(group => {
    const count = summary[group] ?? 0;
    if (!count) return null;
    const label = t(`fleet.count.${group}`, { count });
    return fleetSearch ? <Button key={group} asChild variant="link" size="sm">
      <Link to={{ search: fleetSearch(group) }}>{label}</Link>
    </Button> : <span key={group}>{label}</span>;
  })}</>;
}
