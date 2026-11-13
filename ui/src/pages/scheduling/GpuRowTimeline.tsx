import { useEffect } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Badge } from "../../components/ui/badge";
import type { BoardNode, SchedulingTimeline, TimelineGPU } from "../../schedulingApi";
import { Table, TableHeader, TableHead, TableBody, TableRow, TableCell } from "../../components/ui/table";
import { formatTime } from "../../i18n/datetime";
import { CardTrack } from "./TimelineLanes";
import ResidencyLegend from "./ResidencyLegend";
import { CHART_WIDTH, tickLabel, ticksOf, lanesOf, windowSpan, stateOrder, type Lane, type Span, type Tick } from "./timelineChart";

import { deploymentColor } from "./palette";
import { durationText } from "../durationText";
import { Tooltip } from "../../components/ui/tooltip";

function GpuRowTimeline({ gpu, lanes, span, ticks }: {
  gpu: TimelineGPU; lanes: Lane[]; span: Span; ticks: Tick[];
}) {
  const { t } = useTranslation("scheduling");
  const { t: tDeploy } = useTranslation("deployments");
  return <div className="residency-history">
    <div className="residency-deployments">{lanes.map(lane => {
      const intervals = lane.segments.filter(s => s.state === "awake").map(s => [Date.parse(s.from), Date.parse(s.to)]).sort((a, b) => a[0] - b[0]);
      let served = 0, end = span.fromMs;
      for (const [from, to] of intervals) {
        served += Math.max(0, Math.min(span.toMs, to) - Math.max(from, end));
        end = Math.max(end, to);
      }
      const detail = [
        !lane.deleted && t("timeline.columns.level") + " · " + tDeploy(`yieldPolicy.policy.name.${lane.policy}`),
        !gpu.truncated && t("timeline.summary.awake", { duration: durationText(t, served / 1000, "elapsed") }),
      ].filter(Boolean).join(" · ");
      const name = lane.deleted ? t("history.deleted", { name: lane.deployment }) : lane.deployment;
      const badge = <Badge size="sm" color={{ base: deploymentColor(lane.deploymentID), onStrong: "#fff" }}>{name}</Badge>;
      const label = lane.deleted ? <span tabIndex={detail ? 0 : undefined}>{badge}</span> : <Link to={`/deployments?deployment=${lane.deploymentID}`}>{badge}</Link>;
      return detail ? <Tooltip key={lane.deploymentID} content={detail}>{label}</Tooltip> : <span key={lane.deploymentID}>{label}</span>;
    })}</div>
    <CardTrack gpu={gpu} span={span} ticks={ticks}/>
  </div>;
}

export function NodeHistoryChart({ node, timeline }: { node: BoardNode; timeline: SchedulingTimeline }) {
  const { t } = useTranslation("scheduling");
  const history = new Map(timeline.nodes.find(n => n.uuid === node.uuid)?.gpus.map(gpu => [gpu.index, gpu]));
  const indices = [...new Set([...node.gpus.map(card => card.index), ...history.keys()])].sort((a, b) => a - b);
  const span = windowSpan(timeline.window);
  const ticks = ticksOf(span);
  const rows = indices.map(index => {
    const gpu = history.get(index);
    return { index, gpu, lanes: gpu ? lanesOf(gpu.segments, timeline.deployments, gpu.summary) : [] };
  });
  useEffect(() => {
    const anchor = window.location.hash.slice(1);
    if (/^gpu-\d+-residency$/.test(anchor)) document.getElementById(anchor)?.scrollIntoView({ block: "nearest" });
  }, [node.uuid]);
  return <>
    <ResidencyLegend items={stateOrder.map(state => ({ state, label: t(`timeline.state.${state}`) }))}>
      {rows.some(row => row.gpu?.summary?.length) && <span><i className="timeline-summary-swatch" aria-hidden="true"/>{t("timeline.summary.legend")}</span>}
    </ResidencyLegend>
    <Table size="md" className="table-fixed" containerClassName="max-w-full">
      <TableHeader><TableRow><TableHead className="history-gpu-col">GPU</TableHead><TableHead>
        <div className="timeline-axis-ticks">{ticks.filter((_, i) => i % 2 === 0).map(tick =>
          <span key={tick.ms} style={{ left: `clamp(20px, ${tick.x / CHART_WIDTH * 100}%, calc(100% - 20px))` }}>{tickLabel(tick.ms, span)}</span>)}</div>
      </TableHead></TableRow></TableHeader>
      <TableBody className="[&>tr]:font-normal">
        {rows.map(({ index, gpu, lanes }) => (
          <TableRow aria-label={t("timeline.gpuAria", { node: node.name, index })} id={`gpu-${index}-residency`} key={index}>
            <TableCell>
              <strong>GPU {index}</strong>
              {gpu?.truncated && <small className="block">{gpu.summarized_before ? t("timeline.summarized", { time: formatTime(gpu.summarized_before) }) : t("timeline.truncated")}</small>}
            </TableCell>
            <TableCell>{gpu && lanes.length > 0
              ? <GpuRowTimeline gpu={gpu} lanes={lanes} span={span} ticks={ticks}/>
              : <div className="residency-history"><small>{gpu?.truncated ? t("timeline.truncated") : t("nodeChart.emptyWindow")}</small><CardTrack gpu={gpu ?? { index, truncated: false, segments: [] }} span={span} ticks={ticks}/></div>}</TableCell>
          </TableRow>
        ))}
        {!rows.length && <TableRow><TableCell colSpan={2}>{t("timeline.noCards")}</TableCell></TableRow>}
      </TableBody>
    </Table>
  </>;
}
