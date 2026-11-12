import { useTranslation } from "react-i18next";
import type { AgentFleetSummary } from "../../api";
import AgentFleetCounts from "../../components/agentUpgrades/AgentFleetCounts";
import { Notice } from "../../ui-kit/Notice";
import { Button } from "../../components/ui/button";

export default function ServerAgentNotice({ summary, dismiss }: { summary: AgentFleetSummary; dismiss: () => void }) {
  const { t } = useTranslation("settings");
  const { t: common } = useTranslation("common");
  return <Notice tone="default" title={t("updates.agentFollowup", { version: summary.target })} action={<>
    <Button asChild variant="link" size="sm"><a href="/nodes" onClick={dismiss}>{t("updates.viewNodes")}</a></Button>
    <Button variant="ghost" size="sm" onClick={dismiss}>{common("actions.close")}</Button>
  </>}>
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
      <AgentFleetCounts summary={summary} />
    </div>
  </Notice>;
}
