import { useCallback, useMemo } from "react";
import { useTranslation } from "react-i18next";
import type { MonitorEvent, MonitorSeries } from "../../api";
import TimeSeriesChart from "../../components/TimeSeriesChart";
import { colors } from "../../theme/tokens";

// Only capacity-changing state transitions ever appear in `events` (the
// server pre-filters progress events out, plan §3.1: "曲线上的事件标注只画
// 改变容量的状态迁移"), so every entry here is drawn as a markLine.
const EVENT_COLOR: Record<MonitorEvent["kind"], string> = {
  instance_running: colors.successText,
  instance_failed: colors.dangerText,
  instance_stopped: colors.muted,
  sleep: colors.chartTeal,
  wake: colors.successText,
  node_offline: colors.dangerText,
  node_online: colors.successText,
};

export default function ClusterTokensChart({
  prompt,
  generation,
  events,
  group,
}: {
  prompt: MonitorSeries;
  generation: MonitorSeries;
  events: MonitorEvent[];
  group?: string;
}) {
  const { t } = useTranslation("monitoring");
  const series = useMemo(() => {
    return [
      { labels: { ...prompt.labels, metric: "prompt" }, points: prompt.points ?? [] },
      { labels: { ...generation.labels, metric: "generation" }, points: generation.points ?? [] },
    ];
  }, [prompt, generation]);
  const markLines = useMemo(
    () => events.map((event) => ({ time: event.time_ms, label: event.label ?? event.kind, color: EVENT_COLOR[event.kind] ?? colors.chartTick })),
    [events],
  );
  // Memoized so TimeSeriesChart's option-rebuild effect (which lists
  // seriesName in its dependency array) does not re-run on every render of
  // this component - only when the labels it actually reads change.
  const seriesName = useCallback(
    (labels: Record<string, string>) => (labels.metric === "prompt" ? t("overview.clusterTokensPrompt") : t("overview.clusterTokensGeneration")),
    [t],
  );

  return <TimeSeriesChart series={series} height={220} unit="tok/s" seriesName={seriesName} markLines={markLines} group={group} />;
}
