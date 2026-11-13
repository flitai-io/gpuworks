import { Progress } from "../../ui-kit/Progress";
import { Tooltip } from "../../components/ui/tooltip";
import { useTranslation } from "react-i18next";
import type { SchedulingOverview } from "../../schedulingApi";
import { freeBorder, stateFill } from "./cellColor";
import HeatmapLegend from "./HeatmapLegend";
import { CELL_STATES } from "./overviewModel";

export default function SummaryStrip({ summary, searching }: { summary: SchedulingOverview["summary"]; searching: boolean }) {
  const { t, i18n } = useTranslation("scheduling");
  const percent = new Intl.NumberFormat(i18n.language, { style: "percent", maximumFractionDigits: 1 });
  const segments = CELL_STATES.filter((state) => summary[state] > 0 && summary.cards > 0).map((state) => {
    const count = summary[state];
    const share = count / summary.cards;
    return { state, share, detail: `${t(`states.${state}`)} ${count} / ${summary.cards} · ${percent.format(share)}` };
  });
  const onlineLabel = t("summary.nodesOnline", { online: summary.online_nodes, count: summary.nodes });
  return (
    <div className="scheduling-map-summary" role="group" aria-label={t("summary.cardsTitle")}>
      <div className="flex flex-wrap items-baseline gap-4">
        <div className="flex items-baseline gap-1.5"><strong className="scheduling-fleet-total">{summary.cards}</strong><span>{t("summary.total")}</span></div>
        <div className="flex items-center gap-1.5">
          <span className="text-fg-muted">{onlineLabel}</span>
          {summary.nodes > 0 && (
            <Tooltip content={`${onlineLabel} · ${percent.format(summary.online_nodes / summary.nodes)}`}>
              <span className="block w-11"><Progress label={onlineLabel} value={summary.online_nodes} max={summary.nodes}/></span>
            </Tooltip>
          )}
        </div>
        <span className="text-fg-muted">{t("summary.yields")} <strong>{summary.contention.yields_last_hour}</strong></span>
      </div>
      {segments.length > 0 && (
        <div className="scheduling-state-share flex" role="img" aria-label={segments.map(({ detail }) => detail).join(" · ")}>
          {segments.map(({ state, share, detail }) => (
            <Tooltip key={state} content={detail}>
              <span style={{ width: `${share * 100}%`, backgroundColor: state === "free" ? freeBorder : stateFill[state] }} />
            </Tooltip>
          ))}
        </div>
      )}
      <HeatmapLegend searching={searching} summary={summary} />
    </div>
  );
}
