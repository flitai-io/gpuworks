import { useTranslation } from "react-i18next";
import type { SchedulingOverview } from "../../schedulingApi";
import { awakeFill, stateFill, tokenVars } from "./cellColor";
import { CELL_STATES, type CellState } from "./overviewModel";

export function HeatmapSwatch({ state }: { state: CellState | "switching" }) {
  const background = state === "awake" ? awakeFill(100) : state === "switching" ? undefined : stateFill[state];
  return <span className={`hm-swatch is-${state}`} style={{...tokenVars(), background}} aria-hidden="true"/>;
}

/**
 * HeatmapLegend names the colours once for the whole heatmap: a state each,
 * the awake blue deepening with the share of the card claimed, an outline for a
 * card changing state, and, while a deployment is searched, the ring on its cards.
 */
export default function HeatmapLegend({ searching, summary }: { searching: boolean; summary: SchedulingOverview["summary"] }) {
  const { t } = useTranslation("scheduling");
  return (
    <ul className="hm-legend" style={tokenVars()} aria-label={t("nodes.legend.title")}>
      {CELL_STATES.map((state) => (
        <li key={state}>
          <HeatmapSwatch state={state}/>
          <span>{t(`states.${state}`)}</span> <strong>{summary[state]}</strong>
        </li>
      ))}
      <li>
        <span className="hm-swatch" style={{ background: `linear-gradient(90deg, ${awakeFill(1)}, ${awakeFill(100)})`, width: 28 }} aria-hidden="true" />
        {t("heatmap.legendShade")}
      </li>
      <li>
        <HeatmapSwatch state="switching"/>
        {t("heatmap.legendSwitching")}
      </li>
      {searching && (
        <li>
          <span className="hm-swatch is-hit" aria-hidden="true" />
          {t("heatmap.legendHit")}
        </li>
      )}
    </ul>
  );
}
