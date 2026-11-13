import { MetricStat } from "./InferenceMetricsPanel";
import { Col, Row } from "antd";
import { Alert } from "../pages/DeploymentControls";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, engineReportsNoMetrics, usePolling, type MonitorDeployment, type MonitorRange, type MonitorSeries } from "../api";

// The server's signal sources distinguish an engine still starting from one
// for which the gateway is the only source (ADR 0039).
export function useGatewayOnlyMetrics(deploymentId: number | undefined, sinceMS: number, pollMs: number, allRunning: boolean) {
  const range = monitorRangeCovering(sinceMS);
  const interval = deploymentId == null ? 0 : pollMs;
  const { data } = usePolling(
    async () => deploymentId == null ? null : { range, snapshot: await api.monitorDeployment(deploymentId, range) },
    interval,
    [deploymentId, range, interval],
  );
  const snapshot = data?.range === range ? data.snapshot : null;
  if (deploymentId == null || snapshot?.deployment.id !== deploymentId) return null;
  if (allRunning && Object.keys(snapshot.sources ?? {}).length === 0) return snapshot;
  return engineReportsNoMetrics(snapshot.sources) ? snapshot : null;
}

export default function GatewayMetricsNotice({ data }: { data: MonitorDeployment }) {
  const { t } = useTranslation("deployments");
  const deploymentId = data.deployment.id;
  const toServiceMonitor = <Link to={`/monitor/d/${deploymentId}`}>{t("metrics.openServiceMonitor")}</Link>;
  if (Object.keys(data.sources ?? {}).length === 0) {
    return (
      <Alert
        type="info"
        showIcon
        message={t("metrics.noEngineNoTraffic")}
        description={
          <>
            {t("metrics.noEngineNoTrafficDescription")} {toServiceMonitor}
          </>
        }
      />
    );
  }

  const ttft = latest(data.charts.latency, "ttft_p95");
  const e2e = latest(data.charts.latency, "e2e_p95");
  const generation = latest(data.charts.throughput, "generation_tok_s");
  const inflight = latest(data.charts.concurrency, "running");
  return (
    <div>
      <Alert
        type="info"
        showIcon
        message={t("metrics.engineReportsNone")}
        description={
          <>
            {t("metrics.engineReportsNoneDescription")} {toServiceMonitor}
          </>
        }
        style={{ marginBottom: 12 }}
      />
      <Row gutter={[12, 12]}>
        <Stat title={t("metrics.gatewayTTFT")} value={ttft} suffix="ms" />
        <Stat title={t("metrics.gatewayE2E")} value={e2e} suffix="ms" />
        <Stat title={t("metrics.totalGenerationThroughput")} value={generation} suffix="tok/s" precision={1} />
        <Stat title={t("metrics.gatewayInflight")} value={inflight} />
      </Row>
    </div>
  );
}

// The deployment monitor answers fixed windows ending now; the drawer's
// window maps to the shortest one that reaches as far back. A minute of slack
// absorbs the milliseconds between resolving a quick range and rendering, so
// "15 minutes" stays 15m rather than spilling into 1h.
const MONITOR_RANGES: Array<[MonitorRange, number]> = [
  ["15m", 15 * 60 * 1000],
  ["1h", 60 * 60 * 1000],
  ["6h", 6 * 60 * 60 * 1000],
  ["24h", 24 * 60 * 60 * 1000],
  ["168h", 7 * 24 * 60 * 60 * 1000],
];

export function monitorRangeCovering(sinceMS: number): MonitorRange {
  return (MONITOR_RANGES.find(([, ms]) => ms + 60 * 1000 >= sinceMS) ?? MONITOR_RANGES[MONITOR_RANGES.length - 1])[0];
}

function Stat({ title, value, suffix, precision = 0 }: { title: string; value?: number; suffix?: string; precision?: number }) {
  return (
    <Col xs={24} sm={12} xl={6}>
      <MetricStat title={title} value={value} precision={precision} suffix={suffix} />
    </Col>
  );
}

// latest is the newest point of a chart line, summed across the line's
// series (one per replica where the chart is per-instance).
function latest(series: MonitorSeries[], metric: string): number | undefined {
  let at = -1;
  let total: number | undefined;
  for (const entry of series) {
    if (entry.labels.metric !== metric) continue;
    const point = entry.points[entry.points.length - 1];
    if (!point) continue;
    if (point[0] > at) {
      at = point[0];
      total = point[1];
    } else if (point[0] === at) {
      total = (total ?? 0) + point[1];
    }
  }
  return total;
}
