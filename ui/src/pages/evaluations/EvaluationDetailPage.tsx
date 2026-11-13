import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { flushSync } from "react-dom";
import { ArrowLeftOutlined, CopyOutlined, StopOutlined } from "@ant-design/icons";
import { Alert, Button, Card, Col, Popconfirm, Progress, Row, Segmented, Space, Spin, Typography, message } from "antd";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api, useEventPayload, usePolling, type BenchmarkCurveResponse, type BenchmarkPoint, type BenchmarkRun } from "../../api";
import PageHeader from "../../components/PageHeader";
import TimeSeriesChart from "../../components/TimeSeriesChart";
import { AdvancedResult, Metric } from "../EvaluationSupport";
import BenchmarkWizard, { type WizardPreset } from "./BenchmarkWizard";
import ParetoChart from "./ParetoChart";
import PointResultsTable from "./PointResultsTable";
import VerdictCard from "./VerdictCard";
import { benchmarkSeriesName, bestPoint, metricCardState, pointParameterLabel } from "./evaluationUtils";
import { createRefreshCoalescer } from "./evaluationRefresh";
import { localizedText } from "../../i18n/localizedText";
import { BenchmarkSnapshotPanel, BenchmarkTargetCard, benchmarkDatasetProfile, benchmarkReusesSamples } from "./BenchmarkSnapshotPanel";
import BenchmarkCurveNotice from "./BenchmarkCurveNotice";
import BenchmarkReportMenu from "./BenchmarkReportMenu";
import MetricLabel from "./MetricLabel";
import BenchmarkEvidencePanel from "./BenchmarkEvidencePanel";
import BenchmarkRequestTimeline from "./BenchmarkRequestTimeline";
import useBenchmarkRequestTimeline from "./useBenchmarkRequestTimeline";
import { useWizardTargets } from "./useWizardTargets";
import { downloadChartReport } from "./downloadChartReport";
import { benchmarkCurveSeries, type CurveMetric, type CurveField } from "./benchmarkCurveSeries";
import { paretoData } from "./paretoUtils";
import { benchmarkReportURL } from "./BenchmarkReportMenu";
import { renderBenchmarkReport, downloadBenchmarkReport, type BenchmarkVisualFormat } from "./benchmarkReportExport";


export default function EvaluationDetailPage() {
	const id = Number(useParams().id);
	return <EvaluationDetail key={id} id={id} />;
}

function EvaluationDetail({ id }: { id: number }) {
	const { t, i18n } = useTranslation("evaluations");
	const language = i18n.resolvedLanguage || i18n.language;
	const curveMetrics = useMemo<CurveMetric[]>(() => [
		{ value: "output_tput", label: t("detail.curveMetric.output"), unit: "tok/s" },
		{ value: "total_tput", label: t("detail.curveMetric.total"), unit: "tok/s" },
		{ value: "request_tput", label: t("detail.curveMetric.request"), unit: "req/s" },
		{ value: "ttft_p50", label: "TTFT", unit: "ms" },
		{ value: "tpot_p50", label: "TPOT", unit: "ms" },
		{ value: "latency_p99", label: t("detail.curveMetric.latency"), unit: "ms" },
	], [t]);
	const navigate = useNavigate();
	const [chartMetric, setChartMetric] = useState(curveMetrics[0].value);
	const [selectedSeq, setSelectedSeq] = useState<number>();
	const [wizardPreset, setWizardPreset] = useState<WizardPreset>();
	const reportRef = useRef<HTMLDivElement>(null);
	const exportingRef = useRef(false);
	const [captureWidth, setCaptureWidth] = useState<number>();
	const capturing = captureWidth != null;
	const runQuery = usePolling<BenchmarkRun>(() => api.benchmark(id), 0, [id]);
	const metaQuery = usePolling(api.benchmarkMeta, 0);
	const run = runQuery.data;
	const active = run?.state === "pending" || run?.state === "running";
	const curve = usePolling<BenchmarkCurveResponse>(() => api.benchmarkCurve(id), 0, [id]);
	const wizardOpen = !!wizardPreset;
	const targets = useWizardTargets(wizardOpen);
	const refreshRef = useRef({ run: runQuery.refresh, curve: curve.refresh });
	refreshRef.current = { run: runQuery.refresh, curve: curve.refresh };
	const coalescer = useMemo(() => createRefreshCoalescer(async () => { if (!exportingRef.current) await Promise.all([refreshRef.current.run(), refreshRef.current.curve()]); }, 1000), [id]);
	useEffect(() => () => coalescer.dispose(), [coalescer]);
	useEventPayload("benchmark", (event) => { if (event.key === String(id)) coalescer.trigger(); });
	useEffect(() => {
		if (!active) return;
		const fallback = setInterval(() => coalescer.trigger(), 10000);
		return () => clearInterval(fallback);
	}, [active, coalescer]);

	const best = (run?.points?.length ? bestPoint(run) : undefined) as BenchmarkPoint | undefined;
	const selected = run?.points?.find((point) => point.seq === selectedSeq) || best;
	const timeline = useBenchmarkRequestTimeline(id, run?.id === id && selected?.state !== "running" ? selected?.seq : undefined);
	const liveCards = run ? metricCardState(run, t) : undefined;
	const curveSeriesName = useCallback((labels: Record<string, string>, index: number) => run ? benchmarkSeriesName(run, labels, index, t) : t("common.point", { value: index + 1 }), [run, t]);
	const chart = useMemo(() => benchmarkCurveSeries(curve.data?.points || [], id, chartMetric), [chartMetric, curve.data?.points, id]);
	const curveHasSamples = chart.some((series) => series.points.length > 0);
	const exportVisual = async (format: BenchmarkVisualFormat) => {
		const report = reportRef.current;
		if (!report || exportingRef.current) return;
		exportingRef.current = true;
		// Widen the report to include every selected table column in the exported report.
		const extraWidth = Math.max(0, ...[...report.querySelectorAll<HTMLElement>(".ant-table-content, .ant-table-body")].map((table) => table.scrollWidth - table.clientWidth));
		try {
			flushSync(() => setCaptureWidth(Math.ceil(report.getBoundingClientRect().width + extraWidth)));
			if (format === "html") {
				await downloadChartReport(benchmarkReportURL(id, "html", language), {
					root: report, run: run ?? undefined, curves: curve.data?.points, curveSeriesName, curveMetric: chartMetric, curveMetrics,
					curveTitle: t("detail.curve"), plotted: paretoData(run?.points || []), selectedSeq: selected?.seq,
					fullMetricsLabel: t("pointTable.showFullMetrics"),
				}, id);
			} else {
				const blob = await renderBenchmarkReport(report, format);
				downloadBenchmarkReport(blob, id, format);
			}
		} finally {
			flushSync(() => setCaptureWidth(undefined));
			exportingRef.current = false;
		}
	};

	if (runQuery.loading && !runQuery.data || metaQuery.loading && !metaQuery.data) return <Card><Spin /></Card>;
	if (runQuery.error || metaQuery.error || !run || !metaQuery.data) return <Card><Alert type="error" showIcon message={t("detail.loadFailed")} description={(runQuery.error || metaQuery.error)?.message || t("detail.missing")} action={<Space><Button onClick={() => navigate("/evaluations")}>{t("common.back")}</Button><Button onClick={() => { void downloadChartReport(benchmarkReportURL(id, "html", language), {
        root: null, curveTitle: t("detail.curve"), curveMetrics, curveMetric: chartMetric, curveSeriesName, plotted: [], fullMetricsLabel: t("pointTable.showFullMetrics"),
    }, id).catch(() => message.error(t("agentic.export.failed"))); }}>{t("agentic.export.html")}</Button></Space>} /></Card>;
	const meta = metaQuery.data;
	const progressPct = run.mode === "grid" ? Math.round((Math.max(0, (run.progress?.point_index || 1) - 1) / Math.max(1, run.progress?.point_total || 1)) * 100) : undefined;
	const completed = run.progress?.completed ?? 0;
	const requestTotal = run.progress?.current_point?.request_total;
	const datasetProfile = benchmarkDatasetProfile(run, meta.dataset_profiles || {});
	const reusesSamples = benchmarkReusesSamples(run, datasetProfile);

	return <div>
		<div ref={reportRef} className={capturing ? "benchmark-report-export" : undefined} style={capturing ? { width: captureWidth } : undefined}>
		<PageHeader title={run.name || `Run #${run.id}`} description={`${localizedText(meta.modes.find((item) => item.value === run.mode)?.label, language) || run.mode} · ${run.model}`} parents={[<Link key="list" to="/evaluations">{t("list.breadcrumb.evaluations")}</Link>]} extra={<Space data-report-exclude><Button icon={<ArrowLeftOutlined />} onClick={() => navigate("/evaluations")}>{t("common.back")}</Button><BenchmarkReportMenu runID={run.id} onExportVisual={exportVisual} visualDisabled={runQuery.loading || curve.loading || timeline.loading} /><Button icon={<CopyOutlined />} onClick={() => setWizardPreset({ sourceRun: run })}>{t("detail.rerun")}</Button>{active && <Popconfirm title={t("detail.cancelTitle")} description={t("detail.cancelDescription")} onConfirm={async () => { try { await api.cancelBenchmark(run.id); await runQuery.refetch(); message.success(t("detail.canceled")); } catch (error) { message.error((error as Error).message); } }}><Button danger icon={<StopOutlined />}>{t("detail.cancel")}</Button></Popconfirm>}</Space>} />
			<Space direction="vertical" size={16} style={{ display: "flex" }}>
				<VerdictCard run={run} meta={meta} onRerun={(maxConcurrency) => setWizardPreset({ sourceRun: run, maxConcurrency })} />
				{run.warnings?.map((warning) => <Alert key={warning} type="warning" showIcon message={t(`benchmarkWarning.${warning}`, { defaultValue: warning })} />)}
				{reusesSamples && <Alert type="warning" showIcon message={t("detail.sampleReuseWarning")} />}
				<BenchmarkTargetCard run={run} />
			{run.mode === "grid" && active && <Card size="small"><Progress percent={progressPct} status="active" /><Typography.Text type="secondary">{t("detail.gridProgress", { completed, total: requestTotal ? ` / ${requestTotal}` : "" })}</Typography.Text></Card>}
			<Card size="small" title={liveCards?.label}>
				{liveCards?.point && <Typography.Text type="secondary" style={{ display: "block", marginBottom: 12 }}>{pointParameterLabel(liveCards.point)}{active && requestTotal ? ` · ${t("detail.requestProgress", { completed, total: requestTotal })}` : ""}</Typography.Text>}
				<Row gutter={[10, 10]}>
					<Metric title={<MetricLabel label={t("detail.metric.output")} help="output" />} value={liveCards?.values?.outputTput} suffix="tok/s" /><Metric title={<MetricLabel label={t("detail.metric.request")} help="request" />} value={liveCards?.values?.requestTput} suffix="req/s" /><Metric title={<MetricLabel label={t("detail.metric.interactivity")} help="interactivity" />} value={liveCards?.values?.interactivity} suffix="tok/s/user" /><Metric title={<MetricLabel label={t("detail.metric.total")} help="total" />} value={liveCards?.values?.totalTput} suffix="tok/s" /><Metric title={<MetricLabel label="TTFT P50" help="ttft" />} value={liveCards?.values?.ttftP50} suffix="ms" /><Metric title={<MetricLabel label="TPOT P50" help="tpot" />} value={liveCards?.values?.tpotP50} suffix="ms" /><Metric title={<MetricLabel label={t("detail.metric.success")} help="success" />} value={liveCards?.values?.successRate} suffix="%" /><Metric title={<MetricLabel label={t("detail.metric.latency")} help="latency" />} value={liveCards?.values?.latencyP99} suffix="ms" />
				</Row>
			</Card>
			<Row gutter={[16, 16]}>
					<Col xs={24} xl={12} data-report-curve><Card title={t("detail.curve")} extra={<Segmented<CurveField> size="small" value={chartMetric} onChange={setChartMetric} options={curveMetrics.map(({ value, label }) => ({ value, label }))} />}><BenchmarkCurveNotice warmup={run.progress?.current_point?.warmup} active={active} loading={curve.loading} hasResponse={curve.data != null} hasSamples={curveHasSamples} runState={run.state} completed={run.progress?.completed ?? 0} runError={run.error} error={curve.error} onRetry={() => { void curve.refetch(); }} />{!(curve.loading && curve.data == null) && <TimeSeriesChart series={chart} connectNulls={false} height={280} unit={curveMetrics.find((item) => item.value === chartMetric)?.unit} seriesName={curveSeriesName} resetKey={`${id}:${chartMetric}`} />}</Card></Col>
				<Col xs={24} xl={12} data-report-pareto><Card title={<MetricLabel label={t("detail.pareto")} help="pareto" />}><ParetoChart points={run.points || []} selectedSeq={selected?.seq} onSelect={(point) => setSelectedSeq(point.seq)} /></Card></Col>
			</Row>
			<div data-report-points><PointResultsTable run={run} selectedSeq={selected?.seq} onSelect={(point) => setSelectedSeq(point.seq)} /></div>
			<BenchmarkEvidencePanel run={run} point={selected} />
			{selected && (selected.state === "running" ? <Card title={t("timeline.title")}><Alert type="info" showIcon message={t("timeline.running")} /></Card> : <BenchmarkRequestTimeline key={`${id}:${selected.seq}`} data={timeline.data} loading={timeline.loading} error={timeline.error} onRetry={timeline.refetch} />)}
			<AdvancedResult run={run} point={selected} expanded={capturing} />
				<BenchmarkSnapshotPanel run={run} expanded={capturing} />
		</Space>
		</div>
		<BenchmarkWizard open={wizardOpen} meta={meta} deployments={targets.deployments} nodes={targets.nodes} onSearchDeployments={targets.searchDeployments} onSearchNodes={targets.searchNodes} preset={wizardPreset} onClose={() => setWizardPreset(undefined)} onCreated={(created) => { setWizardPreset(undefined); navigate(`/evaluations/${created.id}`); }} />
	</div>;
}
