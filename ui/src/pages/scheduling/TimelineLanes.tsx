import { useId, useMemo, useState, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";
import type { TimelineGPU, TimelineSegment, TimelineState } from "../../schedulingApi";
import { formatTime } from "../../i18n/datetime";
import { Tooltip } from "../../components/ui/tooltip";
import { colors } from "../../theme/tokens";
import { CHART_WIDTH, LANE_HEIGHT, blockOf, cardIntervals, xOf, type Span, type Tick } from "./timelineChart";
import { durationText } from "../durationText";
import { BoundaryLine, HatchPattern, summaryText } from "./TimelineSummary";
import { deploymentColor } from "./palette";

type Block = { x: number; width: number };

export function CardTrack({ gpu, span, ticks }: { gpu: TimelineGPU; span: Span; ticks: Tick[] }) {
  const { t } = useTranslation("scheduling");
  const id = useId().replace(/:/g, "");
  const [active, setActive] = useState<{ block: Block; text: string } | null>(null);
  const intervals = useMemo(() => cardIntervals(gpu.segments, span), [gpu.segments, span.fromMs, span.toMs]);
  const deployments = [...new Set(gpu.segments.map(s => s.deployment_id))];
  const describe = (s: TimelineSegment) => t("timeline.segmentText", {
    deployment: s.deployment, state: s.state === "asleep" && s.level ? t("timeline.state.asleepLevel", { level: s.level }) : t(`timeline.state.${s.state}`),
    from: formatTime(s.from), to: formatTime(s.to),
  }) + " · " + durationText(t, (Date.parse(s.to) - Date.parse(s.from)) / 1000, "elapsed");
  const hoverable = (block: Block, text: string) => ({
    tabIndex: 0, role: "img", "aria-label": text,
    onMouseEnter: () => setActive({ block, text }), onMouseLeave: () => setActive(null),
    onFocus: () => setActive({ block, text }), onBlur: () => setActive(null),
    onKeyDown: (event: KeyboardEvent<SVGElement>) => {
      if (event.key === "Escape" && active) { event.stopPropagation(); setActive(null); }
    },
  });
  function paint(state: TimelineState, dep: number) {
    if (state === "switching") return "transparent";
    return state === "awake" ? deploymentColor(dep) : `url(#${id}-${dep}-${state})`;
  }
  return <div className="timeline-track" role="group" aria-label={t("timeline.gpuPlain", { index: gpu.index })}>
    <svg viewBox={`0 0 ${CHART_WIDTH} ${LANE_HEIGHT}`} preserveAspectRatio="none">
      <defs>
        <HatchPattern id={id + "-summary"}/>
        {deployments.map(dep => {
          const color = deploymentColor(dep);
          return <g key={dep}>
            <pattern id={`${id}-${dep}-asleep`} width={7} height={7} patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
              <rect width={7} height={7} fill={color} fillOpacity={0.18}/><rect width={3} height={7} fill={color}/>
            </pattern>
            <pattern id={`${id}-${dep}-starting`} width={6} height={6} patternUnits="userSpaceOnUse">
              <rect width={6} height={6} fill={color} fillOpacity={0.18}/><circle cx={3} cy={3} r={1.5} fill={color}/>
            </pattern>
          </g>;
        })}
      </defs>
      <rect width={CHART_WIDTH} height={LANE_HEIGHT} fill={colors.chartGrid} opacity={0.5}/>
      {ticks.map(({ x }) => <line key={x} x1={x} x2={x} y1={0} y2={LANE_HEIGHT} stroke={colors.white}/>)}
      {gpu.summary?.map(slice => {
        const block = blockOf(slice, span);
        const text = [t("timeline.summary.legend"), ...slice.deployments.map(d => d.deployment + " · " + summaryText(t, slice, d))].join("\n");
        return <g key={slice.from}>
          <rect {...block} y={2} height={LANE_HEIGHT - 4} fill={colors.chartSlate}/>
          <rect {...block} y={2} height={LANE_HEIGHT - 4} className="timeline-block timeline-summary-block" fill={`url(#${id}-summary)`} {...hoverable(block, text)}/>
        </g>;
      })}
      {intervals.map(({ from, to, residents }) => {
        const x = xOf(from, span);
        const block = { x, width: xOf(to, span) - x };
        const first = residents[0];
        const peers = [...new Map(residents.filter(s => s.state === first.state).map(s => [s.deployment_id, s])).values()];
        const height = (LANE_HEIGHT - 4) / peers.length;
        const switching = first.state === "switching";
        return <g key={from} className={switching ? "timeline-switching" : undefined}>
          {peers.map((s, i) => <rect key={s.deployment_id} {...block} y={2 + i * height} height={height}
            fill={paint(s.state, s.deployment_id)} stroke={switching ? deploymentColor(s.deployment_id) : undefined}
            vectorEffect="non-scaling-stroke" data-state={s.state}/>)}
          <rect {...block} className="timeline-block" y={0} height={LANE_HEIGHT} fill="transparent" {...hoverable(block, residents.map(describe).join("\n"))}/>
        </g>;
      })}
      {gpu.summarized_before && <BoundaryLine x={xOf(Date.parse(gpu.summarized_before), span)}/>}
    </svg>
    {intervals.map(({ from, to, residents }) => {
      const primary = residents[0];
      if (primary.state !== "awake" || residents.some(s => s.state === "awake" && s.deployment_id !== primary.deployment_id)) return null;
      const left = xOf(from, span) / 10, width = xOf(to, span) / 10 - left;
      return <span aria-hidden="true" key={from} className="track-caption" style={{ left: left + "%", width: width + "%" }}><span>{primary.deployment}</span></span>;
    })}
    {active && <Tooltip forceOpen content={active.text} className="pointer-events-none w-max max-w-[min(24rem,calc(100vw-24px))] whitespace-pre-line wrap-anywhere">
      <span aria-hidden="true" tabIndex={-1} className="pointer-events-none absolute inset-y-0 w-px" style={{ left: (active.block.x + active.block.width / 2) / 10 + "%" }}/>
    </Tooltip>}
  </div>;
}
