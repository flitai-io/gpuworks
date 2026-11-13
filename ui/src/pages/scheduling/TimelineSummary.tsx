import type { TFunction } from "i18next";
import type { TimelineSummary, TimelineSummaryDeployment } from "../../schedulingApi";
import { formatTime } from "../../i18n/datetime";
import { colors } from "../../theme/tokens";
import { durationText } from "../durationText";
import { LANE_HEIGHT } from "./timelineChart";

/**
 * summaryText words one summarized slice for one deployment from the numbers
 * the server counted: the slice's time, how often the card changed hands
 * (the card's figure, not the deployment's) and how long the deployment
 * served, slept, switched and started in it (the states it spent no time in
 * are left out).
 */
export function summaryText(t: TFunction<"scheduling">, slice: TimelineSummary, deployment: TimelineSummaryDeployment): string {
  const head = t("timeline.summary.head", { from: formatTime(slice.from), to: formatTime(slice.to), switches: t("timeline.summary.switches", { count: slice.switches }) });
  const spent = (["awake", "asleep", "switching", "starting"] as const)
    .map((state) => [state, deployment[`${state}_seconds`]] as const)
    .filter(([, seconds]) => seconds > 0)
    .map(([state, seconds]) => t(`timeline.summary.${state}`, { duration: durationText(t, seconds, "elapsed") }));
  return spent.length === 0 ? head : [head, t("timeline.summary.own", { parts: spent.join(t("timeline.summary.partSeparator")) })].join(" · ");
}

/** BoundaryLine is the dashed line where summarizing ends, drawn across a lane. */
export function BoundaryLine({ x }: { x: number }) {
  return <line x1={x} x2={x} y1={0} y2={LANE_HEIGHT} stroke={colors.chartSlate} strokeWidth={1.5} strokeDasharray="3 2" vectorEffect="non-scaling-stroke" aria-hidden="true" />;
}

/** HatchPattern is the stripes drawn over a summarized block; id is the pattern's id in the lane's svg. */
export function HatchPattern({ id }: { id: string }) {
  return (
    <pattern id={id} width={6} height={6} patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
      <rect width={2.5} height={6} fill={colors.white} opacity={0.6} />
    </pattern>
  );
}
