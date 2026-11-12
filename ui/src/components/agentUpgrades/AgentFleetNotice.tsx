import { useState } from "react";
import AgentFleetCounts from "./AgentFleetCounts";
import { useSessionUser } from "../sessionUser";
import { useTranslation } from "react-i18next";
import type { AgentFleetSummary } from "../../api";
import { Notice } from "../../ui-kit/Notice";
import { Button } from "../ui/button";

export default function AgentFleetNotice({ summary, failed, retry, fleetSearch }: {
  summary?: AgentFleetSummary;
  failed: boolean;
  retry: () => void;
  fleetSearch: (group: string) => string;
}) {
  const { t } = useTranslation("nodes");
  const userID = useSessionUser()?.id;
  if (failed) return <Notice tone="warning" title={t("fleet.loadError")}
    action={<Button size="sm" variant="ghost" onClick={retry}>{t("fleet.retry")}</Button>} />;
  if (!summary) return null;
  const { target, align, check, run_id } = summary;
  if (!target || target === "dev") return <Notice title={t("fleet.noTarget")} />;
  if (!align && !check && !run_id) return null;
  const key = `gpuworks.agent-notice.${userID}.${target}`;
  return <TargetNotice key={key} storageKey={userID === undefined ? undefined : key} summary={summary} fleetSearch={fleetSearch} />;
}

function TargetNotice({ storageKey, summary, fleetSearch }: {
  storageKey?: string;
  summary: AgentFleetSummary;
  fleetSearch: (group: string) => string;
}) {
  const { t } = useTranslation("nodes");
  const { target, run_id } = summary;
  const [ignored, setIgnored] = useState(() => {
    try { return !!storageKey && window.localStorage.getItem(storageKey) === "1"; }
    catch { return false; }
  });
  if (ignored) return null;
  const ignore = () => {
    setIgnored(true);
    try { if (storageKey) window.localStorage.setItem(storageKey, "1"); }
    catch { /* The notice is still dismissed for this visit. */ }
  };
  return <Notice tone="default" title={t("fleet.target", { target })}
    action={<Button variant="ghost" size="sm" onClick={ignore}>{t("fleet.ignore")}</Button>}>
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
      <AgentFleetCounts summary={summary} fleetSearch={fleetSearch} />
      {!!run_id && <Button asChild variant="link" size="sm">
        <a href={`/nodes?agent_run=${run_id}`}>{t("fleet.progress")}</a>
      </Button>}
    </div>
  </Notice>;
}
