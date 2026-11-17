import { useRef, useState } from "react";
import { Alert, Button, Card, Col, Row, Skeleton } from "antd";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import EmptyState from "../../components/EmptyState";
import PageHeader from "../../components/PageHeader";
import type { MonitorService, MonitorSignal } from "../../api";
import AsOfCaption from "./AsOfCaption";
import ClusterTokensChart from "./ClusterTokensChart";
import MetricExplorer from "./MetricExplorer";
import ServiceTable from "./ServiceTable";
import SignalTile from "./SignalTile";
import { formatLatency, latencyUnit } from "./signalFormat";
import TimeRangeControl, { PickedRangeNote, TimeRangeProvider, useTimeRange } from "./TimeRangeContext";
import InfoTitle from "./InfoTitle";
import RequestLookup from "./RequestLookup";
import { DistributionState, InputOutputHeatmap } from "./RequestShapeCards";
import { useMonitorDistributions, useMonitorOverview } from "./useMonitor";

const DISTRIBUTION_HEIGHT = 260;

const LATENCY_SIGNALS = new Set(["ttft", "tpot"]);

// latencyTileProps switches a latency tile (and its p95/p99 pair together)
// to seconds once it reaches 1 s.
function latencyTileProps(signal: MonitorSignal) {
  const unit = latencyUnit(signal.value, signal.denominator);
  return { unit, formatValue: (value: number) => formatLatency(value, unit) };
}

const SIGNAL_UNIT: Record<string, string | undefined> = {
  ready_replicas: undefined,
  error_rate: "%",
  // request_rate pairs the rate with the gateway's in-flight requests as a
  // bare "a / b" under an "RPS / Concurrency" label: two units would not fit.
  request_rate: undefined,
  token_rate: "tok/s",
  waiting: undefined,
  preemptions: "/s",
};

// A signal narrows the service table to the services it is about where
// that is a fact of each service: ready_replicas to the ones short of
// replicas. The other signals are fleet totals with no per-service verdict
// behind them, so clicking them clears any active filter instead. A
// predicate that matches nothing also clears the filter rather than
// narrowing the table to an empty, confusing blank state.
const SIGNAL_PREDICATE: Partial<Record<string, (s: MonitorService) => boolean>> = {
  ready_replicas: (s) => s.replicas.ready < s.replicas.desired,
};

function filterForSignal(key: string, services: MonitorService[]): number[] | null {
  const predicate = SIGNAL_PREDICATE[key];
  if (!predicate) return null;
  const ids = services.filter(predicate).map((s) => s.deployment_id);
  return ids.length > 0 ? ids : null;
}

function signalDetail(signal: MonitorSignal, t: TFunction): string | undefined {
  const notReady = signal.detail?.not_ready;
  if (signal.key === "ready_replicas" && Array.isArray(notReady) && notReady.length > 0) {
    return t("signals.notReadyDetail", { names: notReady.join(", ") });
  }
  if (signal.key === "error_rate" && signal.detail && "5xx" in signal.detail) {
    return t("signals.errorRateDetail", signal.detail);
  }
  return undefined;
}

// signalTooltip is a native title-attribute hint for a signal whose headline
// number needs a caveat the tile's own label/unit can't carry: ttft/tpot's
// detail.upper_bound (p95/p99 here is the max, across the window, of each
// tick's own quantile, not a single quantile over the whole window - see
// MonitorSignal's doc comment in api.ts), and which of request_rate's two
// unitless numbers is the rate and which the concurrency.
function signalTooltip(signal: MonitorSignal, t: TFunction): string | undefined {
  if (signal.detail?.upper_bound) return t("signals.upperBoundHint");
  if (signal.key === "request_rate") return t("signals.requestRateHint");
  return undefined;
}

export default function MonitorPage() {
  return (
    <TimeRangeProvider>
      <MonitorPageContent />
    </TimeRangeProvider>
  );
}

function MonitorPageContent() {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const { range } = useTimeRange();
  const { data, error, loading, refetch } = useMonitorOverview(range);
  const distributions = useMonitorDistributions(range);
  const lastData = useRef(data);
  if (data) lastData.current = data;
  const layoutData = data ?? lastData.current;

  // The service table's narrowings: the state filter (summary and column
  // funnel) and a signal tile's. Picking either replaces the other; both
  // live here so the table keeps its sort, other filters and page.
  const [stateFilter, setStateFilter] = useState<string[]>([]);
  // A signal filter keeps the signal, not the services it matched when
  // clicked: each refresh narrows the table to the services that match now.
  const [signalFilter, setSignalFilter] = useState<string | null>(null);
  const pickSignal = (key: string, services: MonitorService[]) => {
    setSignalFilter(filterForSignal(key, services) ? key : null);
    setStateFilter([]);
  };
  const pickStates = (states: string[]) => {
    setStateFilter(states);
    if (states.length > 0) setSignalFilter(null);
  };

  return (
    <div>
      <PageHeader title={t("overview.title")} description={t("overview.description")} toolbar={
          <>
            <RequestLookup />
            <TimeRangeControl />
          </>
        }
      />

      {error && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message={t("overview.loadFailed")}
          description={error.message}
          action={<Button size="small" onClick={() => refetch()}>{tCommon("actions.retry")}</Button>}
        />
      )}

      {loading && !data && <Skeleton active paragraph={{ rows: 6 }} />}

      {!loading && !error && data && data.services.length === 0 && <EmptyState kind="deployments" description={t("overview.empty")} />}

      {layoutData && (
        <div hidden={!data}>
          <AsOfCaption asOf={layoutData.as_of} stale={layoutData.stale} />
          <PickedRangeNote />

          <div className="monitor-signal-grid">
            {layoutData.signals.map((signal) => (
              <SignalTile
                key={signal.key}
                signal={signal}
                label={t(`signals.${signal.key}`)}
                {...(LATENCY_SIGNALS.has(signal.key) ? latencyTileProps(signal) : { unit: SIGNAL_UNIT[signal.key] })}
                detail={signalDetail(signal, t)}
                valueLabel={signal.key === "token_rate" ? t("signals.tokenRateInput") : undefined}
                denominatorLabel={signal.key === "token_rate" ? t("signals.tokenRateOutput") : undefined}
                onClick={signal.collected ? () => pickSignal(signal.key, layoutData.services) : undefined}
                tooltip={signalTooltip(signal, t)}
              />
            ))}
          </div>

          <div className="monitor-services-section mt-4">
          <ServiceTable services={layoutData.services} stateCounts={layoutData.state_counts} stateFilter={stateFilter} onStateFilterChange={pickStates}
            signalFilter={signalFilter ? { label: t(`signals.${signalFilter}`), deploymentIds: filterForSignal(signalFilter, layoutData.services) ?? [] } : null} onClearSignalFilter={() => setSignalFilter(null)}
            onRefresh={refetch} loading={loading && !data} />
          </div>

          <Row gutter={[16, 16]} style={{ marginTop: 16 }}>
            <Col xs={24} xl={10}>
              <Card style={{ height: "100%" }} title={<InfoTitle title={t("overview.inputOutput")} help={t("overview.help.inputOutput")} />}>
                <DistributionState state={distributions} height={DISTRIBUTION_HEIGHT}>
                  {(dist) => <InputOutputHeatmap dist={dist} height={DISTRIBUTION_HEIGHT} />}
                </DistributionState>
              </Card>
            </Col>
            <Col xs={24} xl={14}>
              <Card style={{ height: "100%" }} title={t("overview.clusterTokens")}>
                <ClusterTokensChart
                  prompt={layoutData.cluster_tokens.prompt}
                  generation={layoutData.cluster_tokens.generation}
                  events={layoutData.events}
                  group="monitor-overview"
                />
              </Card>
            </Col>
          </Row>

          <div style={{ marginTop: 16 }}>
            <MetricExplorer range={range} />
          </div>
        </div>
      )}
    </div>
  );
}
