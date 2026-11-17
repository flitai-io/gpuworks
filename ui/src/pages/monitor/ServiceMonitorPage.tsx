import { useCallback, useMemo, useRef, useState } from "react";
import { Alert, Button, Card, Col, Row, Skeleton, Space, Tabs, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { Link, useNavigate, useParams } from "react-router-dom";
import { APIError } from "../../apiError";
import { engineReportsNoMetrics, type MonitorDeploymentCharts } from "../../api";
import PageHeader from "../../components/PageHeader";
import TimeSeriesChart, { type RightAxis } from "../../components/TimeSeriesChart";
import AsOfCaption from "./AsOfCaption";
import InfoTitle from "./InfoTitle";
import { serviceSeriesName } from "./monitorSeries";
import PdCard from "./PdCard";
import ReplicaTable from "./ReplicaTable";
import LatencyBreakdown from "./LatencyBreakdown";
import LiveRequestsOverview from "./LiveRequestsOverview";
import ServiceHealthCard from "./ServiceHealthCard";
import ServiceLogStream from "./ServiceLogStream";
import TimeRangeControl, { PickedRangeNote, TimeRangeProvider, useTimeRange } from "./TimeRangeContext";
import { DistributionState, InputOutputHeatmap, UncachedTTFTHeatmap } from "./RequestShapeCards";
import RequestView from "./RequestView";
import { useMonitorDeployment, useMonitorDistributions } from "./useMonitor";

const CHART_KEYS: Array<keyof MonitorDeploymentCharts> = ["latency", "tpot", "concurrency", "kv", "throughput", "stage_time", "preemptions"];
// Height of a chart card's body, shared by the chart and its empty state so a
// card with nothing to draw lines up with its neighbours.
const CHART_HEIGHT = 200;
// The first two charts (TTFT, TPOT) come before the request-shape cards.
const LEADING_CHARTS = 2;
const PERCENT_RANGE: [number, number] = [0, 100];

// The tabs below the always-shown breakdown: the charts, replicas and logs,
// or the requests in flight, or the finished ones.
type ServiceTab = "overview" | "live" | "history";
// Charts the gateway cannot stand in for: when the engine reports nothing,
// they are empty for that reason, not because data has yet to arrive. TPOT
// and stage times are not among them: the gateway measures a TPOT and sums
// up the stage times replies report.
const ENGINE_ONLY_CHARTS = new Set<keyof MonitorDeploymentCharts>(["kv", "preemptions"]);

export default function ServiceMonitorPage() {
  return (
    <TimeRangeProvider>
      <ServiceMonitorPageContent />
    </TimeRangeProvider>
  );
}

function ServiceMonitorPageContent() {
  const { t } = useTranslation("monitoring");
  const { t: tCommon } = useTranslation("common");
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { range } = useTimeRange();
  const [tab, setTab] = useState<ServiceTab>("overview");
  // Memoized so TimeSeriesChart's option-rebuild effect (which lists
  // seriesName in its dependency array) only re-runs when the locale does.
  const seriesName = useCallback((labels: Record<string, string>, index: number) => serviceSeriesName(t, labels, index), [t]);
  // The kv chart reads usage on the left axis and hit rate on the right.
  const kvHitRateAxis = useMemo<RightAxis>(
    () => ({ matches: (labels) => labels.metric === "hit_rate_pct", name: t("metrics.hit_rate_pct"), yRange: PERCENT_RANGE }),
    [t],
  );

  const deploymentId = Number(id);
  const validId = Number.isInteger(deploymentId) && deploymentId > 0;
  // useMonitorDeployment itself skips the request for an invalid id (never
  // calls api.monitorDeployment), so this still renders "not found" without
  // ever depending on what an out-of-range id would have fetched.
  const { data, error, loading, refetch } = useMonitorDeployment(deploymentId, range);
  const distributions = useMonitorDistributions(range, validId ? deploymentId : -1);
  const lastData = useRef(data);
  if (data) lastData.current = data;
  const layoutData = data ?? lastData.current;


  if (!validId) {
    return <Alert type="error" showIcon message={t("deployment.notFound")} />;
  }
  const notFound = error instanceof APIError && error.status === 404;
  // A deleted deployment's page names it by the name it left and points at
  // the history Analytics keeps; its live views are gone with it.
  const deletedName = error instanceof APIError && error.code === "MONITOR_DEPLOYMENT_DELETED" ? String(error.params?.name ?? "") : undefined;
  const title = data?.deployment.name ?? (deletedName || `#${deploymentId}`);

  const renderChart = (key: keyof MonitorDeploymentCharts) => {
    if (!layoutData) return null;
    const engineOnly = engineReportsNoMetrics(layoutData.sources);
    const series = layoutData.charts[key] ?? [];
    const hasData = series.some((entry) => entry.points?.length > 0);
    return (
      <Col key={key} xs={24} md={12} xl={8}>
        <Card
          size="small"
          title={<InfoTitle title={t(`deployment.charts.${key}`)} help={t(`deployment.${key}Help`)} />}
        >
          {hasData ? (
            <TimeSeriesChart
              series={series}
              height={CHART_HEIGHT}
              group="monitor-service"
              seriesName={seriesName}
              {...(key === "kv" ? { leftName: t("metrics.usage_pct"), yRange: PERCENT_RANGE, rightAxis: kvHitRateAxis } : {})}
            />
          ) : (
            <Typography.Text
              type="secondary"
              style={{ height: CHART_HEIGHT, display: "flex", alignItems: "center", justifyContent: "center", textAlign: "center" }}
            >
              {t(engineOnly && ENGINE_ONLY_CHARTS.has(key) ? "deployment.charts.engineOnly" : "deployment.charts.notCollected")}
            </Typography.Text>
          )}
        </Card>
      </Col>
    );
  };

  return (
    <div>
      <PageHeader
        title={title}
        description={
          data
            ? t("deployment.subtitle", {
                engine: data.deployment.engine,
                placement: data.deployment.placement,
                ready: data.deployment.replicas.ready,
                desired: data.deployment.replicas.desired,
                id: data.deployment.id,
              })
            : undefined
        }
        parents={[<Link key="list" to="/monitor">{t("overview.title")}</Link>]}
        toolbar={<TimeRangeControl />}
        extra={
          <>
            <Button onClick={() => navigate("/monitor")}>{t("deployment.backToMonitor")}</Button>
            {deletedName === undefined && (
              <Link to={`/deployments?deployment=${deploymentId}`}>
                <Button type="primary">{t("deployment.goToDeployment")}</Button>
              </Link>
            )}
          </>
        }
      />

      {deletedName !== undefined && (
        <Alert
          type="info"
          showIcon
          message={t("deployment.deleted.title", { name: deletedName || `#${deploymentId}` })}
          description={t("deployment.deleted.help")}
          action={
            <Link to={`/analytics/usage?range=30d&deployment=${deploymentId}`}>
              <Button size="small">{t("deployment.deleted.openAnalytics")}</Button>
            </Link>
          }
        />
      )}
      {notFound && deletedName === undefined && <Alert type="error" showIcon message={t("deployment.notFound")} />}
      {error && !notFound && (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message={t("deployment.loadFailed")}
          description={error.message}
          action={<Button size="small" onClick={() => refetch()}>{tCommon("actions.retry")}</Button>}
        />
      )}
      {loading && !data && <Skeleton active paragraph={{ rows: 8 }} />}

      {layoutData && (
        <div hidden={!data}>
          <AsOfCaption asOf={layoutData.as_of} stale={layoutData.stale} />
          <PickedRangeNote />

          {layoutData.deployment.replicas.ready === 0 && layoutData.deployment.replicas.desired > 0 &&
            (layoutData.replicas.length === 0 || layoutData.replicas.some((replica) => replica.state !== "asleep" && replica.state !== "stopped")) && (
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 16 }}
              message={t("deployment.noReadyReplicas")}
              description={t("deployment.noReadyReplicasDetail")}
            />
          )}
          {layoutData.pd && <PdCard pd={layoutData.pd} />}

          <div className="monitor-service-top">
            <Card size="small">
              <LatencyBreakdown deploymentId={deploymentId} range={range} pd={layoutData.pd != null} />
            </Card>
            <Card size="small">
              <ServiceHealthCard data={layoutData} />
            </Card>
          </div>
          <LiveRequestsOverview deploymentId={deploymentId} replicas={layoutData.replicas} />

          <Tabs
            activeKey={tab}
            onChange={(next) => setTab(next as ServiceTab)}
            items={[
              { key: "overview", label: t("deployment.tabs.overview") },
              { key: "live", label: t("requests.live") },
              { key: "history", label: t("requests.history") },
            ]}
          />
          {tab !== "overview" && (
            <Card title={<InfoTitle title={t("requests.title")} help={t("requests.help")} />}>
              <RequestView view={tab} deploymentId={deploymentId} range={range} replicas={layoutData.replicas} pd={layoutData.pd != null} />
            </Card>
          )}
          {tab === "overview" && (
            <>
              {layoutData.alignment && (
                <Card size="small" style={{ marginBottom: 16 }} title={<InfoTitle title={t("deployment.alignment")} help={t("deployment.alignmentHelp")} />}>
                  <Space size={16} wrap>
                    <Typography.Text>{t("deployment.alignmentProxy")}: {layoutData.alignment.proxy_requests}</Typography.Text>
                    <Typography.Text>{t("deployment.alignmentEngine")}: {layoutData.alignment.engine_completed}</Typography.Text>
                    <Typography.Text>{t("deployment.alignmentErrors")}: {layoutData.alignment.client_errors}</Typography.Text>
                    <Typography.Text>{t("deployment.alignmentServerErrors")}: {layoutData.alignment.server_errors}</Typography.Text>
                    <Typography.Text>{t("deployment.alignmentInFlight")}: {layoutData.alignment.in_flight}</Typography.Text>
                  </Space>
                  {/* A transient gap (requests still in flight, counters
                      sampled mid-window) is not a loss; only a discrepancy the
                      server has seen persist gets the red sentence. */}
                  {layoutData.alignment.discrepancy_persistent && (
                    <Typography.Text type="danger" style={{ display: "block", marginTop: 6 }}>
                      {t("deployment.alignmentLost", { count: layoutData.alignment.lost })}
                    </Typography.Text>
                  )}
                </Card>
              )}

              <Row gutter={[16, 16]}>
                {CHART_KEYS.slice(0, LEADING_CHARTS).map(renderChart)}
                <Col xs={24} md={12} xl={8}>
                  <Card size="small" title={<InfoTitle title={t("distribution.inputOutput")} help={t("distribution.help.inputOutput")} />}>
                    <DistributionState state={distributions} height={CHART_HEIGHT}>
                      {(dist) => <InputOutputHeatmap dist={dist} height={CHART_HEIGHT} />}
                    </DistributionState>
                  </Card>
                </Col>
                <Col xs={24} md={12} xl={8}>
                  <Card size="small" title={<InfoTitle title={t("distribution.uncachedTTFT")} help={t("distribution.help.uncachedTTFT")} />}>
                    <DistributionState state={distributions} height={CHART_HEIGHT}>
                      {(dist) => <UncachedTTFTHeatmap dist={dist} height={CHART_HEIGHT} />}
                    </DistributionState>
                  </Card>
                </Col>
                {CHART_KEYS.slice(LEADING_CHARTS).map(renderChart)}
              </Row>
              <Card
                style={{ marginTop: 16 }}
                title={<InfoTitle title={t("deployment.replicas")} help={t("deployment.replicasHelp")} />}
              >
                <ReplicaTable replicas={layoutData.replicas} deploymentId={deploymentId} loading={loading && !data} />
              </Card>

              <Card style={{ marginTop: 16 }} title={t("deployment.logs.title")}>
                <ServiceLogStream logInstances={layoutData.log_instances ?? []} range={range} />
              </Card>
            </>
          )}
        </div>
      )}
    </div>
  );
}
