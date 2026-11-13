import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { TimelineState } from "../../schedulingApi";
import { HeatmapSwatch } from "./HeatmapLegend";

export default function ResidencyLegend({ items, children }: {
  items: { state: TimelineState | "free"; label: string }[];
  children?: ReactNode;
}) {
  const { t } = useTranslation("scheduling");
  return <div className="residency-legend" role="group" aria-label={t("timeline.legend")}>
    {items.map(({ state, label }) => <span key={state}>
      {state === "switching" ? <HeatmapSwatch state={state}/> : <i aria-hidden="true" data-state={state}/>}
      {label}
    </span>)}
    {children}
  </div>;
}
