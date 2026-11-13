import { getInstanceByDom } from "echarts/core";
import type { BenchmarkCurveResponse, BenchmarkRun } from "../../api";
import i18n from "../../i18n";
import { renderBenchmarkReport, downloadBenchmarkReport } from "./benchmarkReportExport";
import type { CurveField, CurveMetric } from "./benchmarkCurveSeries";
import type { ReportChartData } from "./standaloneCharts";
import type { ParetoDatum } from "./paretoUtils";
import { fetchReportContext } from "./reportContext";
import { processSamples } from "./reportCurves";
import { localizeLegacy, reportElement as el, reportText } from "./reportLanguage";
import { addFullPointColumns } from "./reportPointTable";
import { configuration, legacyParetoData, legacySections, offlinePointTable, pointEvidence } from "./reportSections";

interface ReportView {
	root: HTMLElement | null;
	run?: BenchmarkRun;
	curves?: BenchmarkCurveResponse["points"];
	curveSeriesName: (labels: Record<string, string>, index: number) => string;
	curveMetric: CurveField;
	curveMetrics: CurveMetric[];
	curveTitle: string;
	plotted: ParetoDatum[];
	selectedSeq?: number;
	fullMetricsLabel: string;
}

async function readLegacyHTML(url: URL) {
	const response = await fetch(url.href);
	if (!response.ok || !response.headers.get("content-disposition")?.startsWith("attachment;") ||
		!response.headers.get("content-type")?.startsWith("text/html")) throw new Error("Could not read benchmark report");
	const html = await response.text();
	if (!/^\s*<!doctype html>/i.test(html) || !/<\/html>\s*$/i.test(html)) throw new Error("Incomplete HTML report");
	return new DOMParser().parseFromString(html, "text/html");
}

async function loadCurves(url: URL): Promise<BenchmarkCurveResponse["points"]> {
	const curveURL = new URL(url);
	curveURL.pathname = curveURL.pathname.replace(/\/report$/, "/curve");
	curveURL.search = "";
	try {
		const response = await fetch(curveURL);
		if (response.ok) {
			const result: BenchmarkCurveResponse = await response.json();
			if (Array.isArray(result.points)) return result.points.filter(point => Array.isArray(point?.samples));
		}
	} catch { /* The stored tables remain available without samples. */ }
	return [];
}

function restoreSafeConfiguration(doc: Document, cfg: ReturnType<typeof configuration>, endpoint?: string | null) {
	const target = cfg.target_snapshot as Record<string, unknown> | undefined;
	const visibleEndpoint = doc.querySelector("[data-report-endpoint]");
	if (visibleEndpoint) visibleEndpoint.textContent = endpoint || "—";
	const values: Record<string, unknown> = { ...target, dataset: cfg.dataset, search: cfg.search, tokenizer: cfg.tokenizer_ref, raw: cfg };
	for (const item of doc.querySelectorAll<HTMLElement>("[data-report-config]")) {
		const key = item.dataset.reportConfig!;
		const value = values[key];
		if (Array.isArray(value)) item.textContent = value.length ? value.join(" ") : "—";
		else if (typeof value === "object" && value !== null) item.textContent = JSON.stringify(value, null, item.tagName === "PRE" ? 2 : undefined);
		else item.textContent = value == null ? "—" : String(value);
	}
}

export async function downloadChartReport(href: string, view: ReportView, runID: number) {
	const url = new URL(href, location.href);
	const zh = url.searchParams.get("lang")?.toLowerCase().split("-")[0] === "zh";
	const t = (key: string) => reportText(key, !!zh);
	const host = view.root?.querySelector<HTMLElement>('[data-report-curve] [_echarts_instance_]');
	const legend = host && (getInstanceByDom(host)?.getOption()?.legend as ReportChartData["legend"][])?.[0];
	const [legacy, snapshot, saved, curves] = await Promise.all([
		readLegacyHTML(url), view.root ? renderBenchmarkReport(view.root, "html") : undefined,
		view.run ? fetchReportContext(href, runID) : undefined, view.curves ?? loadCurves(url),
	]);
	const [css, runtime, integrity] = await Promise.all([
		import("./report.css?inline"), import("../../../node_modules/.cache/gpuworks-report/benchmark-report.js?raw"),
		import("../../../node_modules/.cache/gpuworks-report/benchmark-report.sha256.txt?raw"),
	]);
	const doc = snapshot ? new DOMParser().parseFromString(await snapshot.text(), "text/html") : document.implementation.createHTMLDocument(legacy.title);
	doc.documentElement.lang = zh ? "zh" : "en";
	const policy = doc.querySelector<HTMLMetaElement>('meta[http-equiv="Content-Security-Policy"]') ?? doc.createElement("meta");
	policy.httpEquiv = "Content-Security-Policy";
	policy.content = `default-src 'none'; script-src 'sha256-${integrity.default}'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'`;
	if (!policy.isConnected) doc.head.prepend(policy);
	doc.head.append(el("style", css.default));
	const data: ReportChartData = {
		runID, curves, metrics: view.curveMetrics, metric: view.curveMetric, selectedSeq: view.selectedSeq,
		names: Object.fromEntries(curves.map((point, i) => [point.seq, view.curveSeriesName({ point: String(point.seq) }, i)])),
		pareto: view.plotted, legend: legend ? { selected: legend.selected, scrollDataIndex: legend.scrollDataIndex } : undefined,
	};
	const endpoint = Array.from(legacy.querySelectorAll("tr")).find(row => row.cells[0]?.textContent === "Endpoint")?.cells[1]?.textContent;
	const { heading, loose, sections } = legacySections(legacy);
	const ids = Array.from(legacy.querySelectorAll<HTMLElement>('section[id^="point-"]'));
	const evaluationURL = loose.querySelector("a[href]")?.getAttribute("href") ?? undefined;
	if (!snapshot) data.pareto = legacyParetoData(sections.get("Point results"), ids);
	localizeLegacy(legacy, !!zh);
	const archive = el("section");
	archive.className = "report-archive";
	if (!snapshot && heading) archive.append(heading);
	const curveCard = doc.querySelector<HTMLElement>("[data-report-curve]");
	if (curves.length && !curveCard?.querySelector("[_echarts_instance_]")) {
		const card = curveCard ?? el("section");
		if (!curveCard) {
			card.dataset.reportCurve = "";
			card.append(el("h2", view.curveTitle));
			for (const metric of view.curveMetrics) {
				const label = el("label", metric.label);
				const radio = el("input");
				radio.type = "radio";
				label.prepend(radio);
				card.append(label);
			}
			archive.append(card);
		}
		const host = el("div");
		host.dataset.offlineChart = "";
		host.style.height = "280px";
		host.setAttribute("role", "img");
		host.setAttribute("aria-label", view.curveTitle);
		(card.querySelector(".ant-card-body") ?? card).append(host);
	}
	archive.append(el("h2", t("Detailed results")));
	if (loose.childElementCount) archive.append(loose);
	for (const [name, section] of sections) {
		if (name === "Point results") {
			const full = offlinePointTable(section, ids, view.run, !!zh);
			if (!addFullPointColumns(doc.querySelector<HTMLElement>("[data-report-points]"), full, view.fullMetricsLabel)) archive.append(full);
		} else {
			if (name === "Throughput and interactivity") {
				section.querySelectorAll(".chart > svg + p.muted, .chart > svg").forEach(node => node.remove());
				const chart = section.querySelector<HTMLElement>(".chart");
				if (!snapshot && chart && data.pareto.length) {
					chart.dataset.reportPareto = "";
					const host = el("div");
					host.dataset.offlineChart = "";
					host.style.height = "320px";
					chart.prepend(host);
					archive.append(section);
					continue;
				}
			}
			const title = section.querySelector(":scope > h2");
			const details = el("details");
			details.append(el("summary", title?.textContent));
			title?.remove();
			if (name === "Throughput and interactivity" && !section.textContent?.trim() && !section.querySelector("table, [id]")) continue;
			details.append(...section.childNodes);
			archive.append(details);
		}
	}
	const pointText = i18n.getFixedT(zh ? "zh" : "en", "evaluations");
	for (const section of ids) pointEvidence(section, view.run?.points?.find(p => section.id === "point-"+p.seq), !!zh, pointText, evaluationURL);
	if (view.run) restoreSafeConfiguration(doc, configuration(view.run, saved), endpoint);
	if (curves.length) archive.append(processSamples(data, view.curveMetrics, !!zh));
	doc.body.append(archive);
	const controls = doc.querySelectorAll<HTMLInputElement>('[data-report-curve] input[type="radio"]');
	controls.forEach((input, i) => {
		const metric = view.curveMetrics[i];
		if (!metric) return;
		input.name = "offline-curve-metric";
		input.value = metric.value;
		input.defaultChecked = metric.value === view.curveMetric;
	});
	if (curves.length || data.pareto.length) {
		const payload = el("script", JSON.stringify(data).replace(/</g, "\\u003c"));
		payload.type = "application/json";
		payload.id = "benchmark-chart-data";
		const script = el("script", runtime.default);
		doc.body.append(payload, script);
	}
	downloadBenchmarkReport(new Blob(["<!doctype html>\n", doc.documentElement.outerHTML], { type: "text/html;charset=utf-8" }), runID, "html");
}
