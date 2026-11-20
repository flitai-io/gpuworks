import { cn } from "../../lib/utils";
import { useTranslation } from "react-i18next";
import { Badge } from "../../components/ui/badge";
import { MetricCard, type MetricTone } from "../../components/ui/metric-card";
import { formatTime } from "../../i18n/datetime";
import type { NetworkTestRun } from "../../networkTypes";
import { isCardKind } from "./cardTest";
import { testScopeText } from "./networkDisplay";

export default function NetworkTestOverview({ run }: { run: NetworkTestRun }) {
  const { t } = useTranslation("networks");
  const card = isCardKind(run.kind);
  const metrics: { label: string; value: number | string; tone?: MetricTone }[] = [
    { label: t("test.overview.nodes"), value: run.node_count },
    { label: t(card ? "test.overview.nodePairs" : "test.overview.directedPairs"), value: run.pair_count },
  ];
  if (run.status === "done") {
    if (card) {
      metrics.push({ label: t("test.overview.cardPairs"), value: run.rail_count },
        { label: t("test.overview.reachableCards"), value: run.rail_passed_count, tone: run.rail_passed_count ? "positive" : "default" });
    } else {
      const failed = run.pairs.length - run.passed_count - run.partial_count;
      metrics.push({ label: t("test.status.passed"), value: run.passed_count, tone: run.passed_count ? "positive" : "default" },
        { label: t("test.status.partial"), value: run.partial_count, tone: run.partial_count ? "warning" : "default" },
        { label: t("test.status.failed"), value: failed, tone: failed ? "critical" : "default" });
      if (run.kind === "rdma_tool" && run.rail_count > 0) {
        metrics.push({ label: t("test.overview.passedRails"), value: `${run.rail_passed_count} / ${run.rail_count}` });
      }
    }
  }
  return (
    <header className="@container grid gap-2">
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
        <div className="flex items-center gap-2">
          <h3 className="text-body font-semibold">{t("test.overview.title")}</h3>
          <Badge status={run.status === "interrupted" ? "warning" : "neutral"}>{t(`test.overview.state.${run.status}`)}</Badge>
        </div>
        <span className="text-label text-fg-muted">{t("test.overview.startedAt")} <time dateTime={run.started_at}>{formatTime(run.started_at)}</time></span>
      </div>
      <p className="break-words text-body text-fg-muted">{[t(`test.kindName.${run.kind}`), testScopeText(t, run), t(`test.patternName.${run.pattern}`)].join(" · ")}</p>
      <div className="flex flex-wrap items-stretch gap-2">
        {metrics.map(metric => <MetricCard key={metric.label} {...metric}
          className={cn("min-w-0 self-stretch bg-muted p-2.5", typeof metric.value === "string" ? "flex-[2_1_10rem]" : "flex-[1_1_5rem]",
            "data-[tone=positive]:bg-success-surface data-[tone=warning]:bg-warning-surface data-[tone=critical]:bg-danger-surface")}
          labelClassName="whitespace-normal text-label text-fg-muted" valueClassName="whitespace-normal break-all text-title" />)}
      </div>
    </header>
  );
}
