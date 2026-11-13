import { useState } from "react";
import { Container, ContainerHeader, ContainerBody } from "../../components/ui/container";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { Accordion, AccordionItem, AccordionTrigger, AccordionContent } from "../../components/ui/accordion";
import { Notice } from "../../ui-kit/Notice";
import { useTranslation } from "react-i18next";
import { usePolling } from "../../api";
import { schedulingApi, type BoardNode, type TimelineWindowOption } from "../../schedulingApi";
import { Spinner } from "../../ui-kit/Spinner";
import TimeRangePicker from "../../components/TimeRangePicker";
import { formatTime } from "../../i18n/datetime";
import type { TimeRangeSelection } from "../../timeRange";
import { ScopeHint, SharingTable } from "./TimelineStats";
import { askOf, offeredQuickRanges, selectionOfWindow } from "./timelineRange";
import { NodeHistoryChart } from "./GpuRowTimeline";
import { windowLabel } from "./WindowPicker";
import EmptyState from "../../components/EmptyState";

export default function ResidencyTimeline({ nodes }: { nodes: BoardNode[] }) {
  const { t } = useTranslation("scheduling");
  if (!nodes.length) return <EmptyState kind="nodes" compact description={t("timeline.noNodes")}/>;
  return <NodeTimeline nodes={nodes} key={nodes.map(node => node.uuid).join(",")}/>;
}

function NodeTimeline({ nodes }: { nodes: BoardNode[] }) {
  const { t } = useTranslation("scheduling");
  const [picked, setPicked] = useState<TimeRangeSelection>();
  const [windows, setWindows] = useState<TimelineWindowOption[]>([]);
  const ask = askOf(picked, windows);
  const uuids = nodes.map(node => node.uuid);
  const timeline = usePolling(() => schedulingApi.timeline({ nodes: uuids, ...ask }),
    picked?.kind === "custom" ? 0 : 60000, [uuids.join(","), JSON.stringify(ask)]);
  const data = timeline.data;
  const value = picked ?? (data && selectionOfWindow(data.window));
  const empty = data && !data.nodes.some(node => node.gpus.some(gpu => gpu.segments.length || gpu.summary?.length));
  return <>
    <Container className="history-chart mt-3 rounded-2xl">
      <ContainerHeader className="py-2"><h2 className="text-title font-semibold">{t("timeline.title")}</h2>
        {data && value && <TimeRangePicker size="small" quickRanges={offeredQuickRanges(data.windows)} value={value}
          onChange={next => { if (next) { setWindows(data.windows); setPicked(next); } }}/>}
      </ContainerHeader>
      <ContainerBody className="rounded-xl p-0">
        {timeline.error && <Notice tone={data ? "warning" : "danger"} title={data ? t("timeline.refreshFailed", { detail: timeline.error.message }) : t("timeline.loadFailed")} action={<Button size="sm" onClick={()=>void timeline.refetch()}>{t("page.retry")}</Button>}>{!data && timeline.error.message}</Notice>}
        {empty && <p className="px-3 py-2 text-label text-fg-muted">{data.window.custom
          ? t("timeline.noneInRange", { from: formatTime(data.window.from), to: formatTime(data.window.to) })
          : t("timeline.noneInWindow", { span: windowLabel(t, data.window) })}</p>}
        {data ? nodes.map(node => <NodeHistoryChart key={node.uuid} node={node} timeline={data}/>) : !timeline.error && <Spinner label={t("timeline.loading")}/>}
      </ContainerBody>
    </Container>
    {data && <Accordion className="mt-3"><AccordionItem value="sharing">
      <AccordionTrigger><span className="flex grow items-center justify-between gap-2">{t("nodeChart.sharing")}<Badge size="sm">{data.deployments.length} {t("queues.deployments", { count: data.deployments.length })}</Badge></span></AccordionTrigger>
      <AccordionContent><p className="mb-2 text-label text-fg-muted">{t("nodeChart.range", { from: formatTime(data.window.from), to: formatTime(data.window.to) })} <ScopeHint scope={data.scope}/></p>
      {!data.switches_available && <p className="text-label text-fg-muted">{t("timeline.switchesUnavailable")}</p>}
      <SharingTable rows={data.deployments} switchesAvailable={data.switches_available}/></AccordionContent>
    </AccordionItem></Accordion>}
  </>;
}
