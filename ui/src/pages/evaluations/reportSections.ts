import type { TFunction } from "i18next";
import { advancedPercentiles, advancedPointFacts } from "./advancedPoint";
import type { ParetoDatum } from "./paretoUtils";
import type { SavedReportContext } from "./reportContext";
import type { BenchmarkPoint, BenchmarkRun } from "../../api";
import { pointLoadLabel, pointMeetsSLA, pointTimingValue } from "./evaluationUtils";
import { reportElement as el, reportText } from "./reportLanguage";

export function legacySections(doc: Document) {
	const sections = new Map<string, HTMLElement>();
	const heading = doc.body.querySelector("h1");
	const loose = el("div");
	let current: HTMLElement = loose;
	for (const node of Array.from(doc.body.children)) {
		if (node === heading) continue;
		if (node.tagName === "H2") {
			current = el("section");
			sections.set(node.textContent || "", current);
		}
		current.append(node);
	}
	// Keep all sections in the document while localizing headings, labels and values.
	doc.body.append(loose, ...sections.values());
	return { heading, loose, sections };
}

// A historical HTML attachment may be the only source when the detail API fails.
export function legacyParetoData(section: HTMLElement | undefined, details: Element[]): ParetoDatum[] {
	const table = section?.querySelector("table");
	if (!table?.tHead || !table.tBodies[0]) return [];
	const columns = Array.from(table.tHead.rows[0].cells, cell => cell.textContent);
	return Array.from(table.tBodies[0].rows).flatMap((row, i) => {
		const detail = details[i];
		if (!detail) return [];
		const value = (name: string) => {
			const raw = row.cells[columns.indexOf(name)]?.textContent?.trim().replace(/%$/, "");
			return raw && Number.isFinite(Number(raw)) ? Number(raw) : undefined;
		};
		const x = value("interactivity_tok_s_user"), y = value("output_tok_s");
		if (x == null || y == null) return [];
		const seq = Number(detail.id.slice("point-".length));
		const loadLabel = detail.querySelector("h3")?.textContent?.match(/\((.*)\)$/)?.[1] ?? `#${seq}`;
		return [{ seq, x, y, loadLabel, successRate: value("success_rate"), total: value("total_tok_s"),
			ttft: value("ttft_p99_ms"), tpot: value("tpot_p50_ms"), latency: value("e2el_p99_ms") }];
	});
}

export function offlinePointTable(section: HTMLElement, pointDetails: Element[], run: BenchmarkRun | undefined, zh: boolean) {
	section.id = "all-points";
	section.className = "card";
	const table = section.querySelector("table");
	if (!table?.tHead || !table.tBodies[0]) return section;
	const scroll = el("div");
	scroll.className = "scroll";
	table.replaceWith(scroll);
	scroll.append(table);
	const header = table.tHead.rows[0];
	header.prepend(el("th", reportText("Point", zh)));
	const extra = ["Request throughput (req/s)", "Peak output throughput (tok/s)", "Peak total throughput (tok/s)", "Average TTFT (ms)", "Average TPOT (ms)", "Average latency (ms)", "Point duration (s)", "SLA", "Verdict point"];
	for (const field of extra) header.append(el("th", reportText(field, zh)));
	const points = new Map(run?.points?.map(p => [p.seq, p]));
	for (const [i, row] of Array.from(table.tBodies[0].rows).entries()) {
		const detail = pointDetails[i];
		const seq = detail && Number(detail.id.slice("point-".length));
		const point = seq == null ? undefined : points.get(seq);
		const th = el("th", "—");
		if (detail && seq != null) {
			row.dataset.seq = String(seq);
			const a = el("a", point ? `#${seq} · ${pointLoadLabel(point)}` : `#${seq}`);
			a.href = "#" + detail.id;
			th.replaceChildren(a);
		}
		row.prepend(th);
		const duration = point?.ended_at && point.started_at ? (Date.parse(point.ended_at) - Date.parse(point.started_at)) / 1000 : undefined;
		const values = [point?.request_tput, point?.max_output_tput || undefined, point?.max_total_tput || undefined,
			point && pointTimingValue(point, "avg_ttft"), point && pointTimingValue(point, "avg_tpot"), point && pointTimingValue(point, "avg_latency"),
			duration != null && duration >= 0 ? duration : undefined,
			point && run?.mode === "sla" ? reportText(pointMeetsSLA(run, point) ? "Met" : "Unmet", zh) : undefined,
			point && point.seq === run?.verdict?.best_point_seq ? reportText("Verdict point", zh) : undefined];
		for (const v of values) row.insertCell().textContent = typeof v === "number" && Number.isFinite(v) ? v.toFixed(3) : typeof v === "string" ? v : "—";
	}
	return section;
}

export function pointEvidence(section: Element, point: BenchmarkPoint | undefined, zh: boolean, t: TFunction<"evaluations">, evaluationURL?: string) {
	const heading = section.querySelector(":scope > h3");
	if (!heading) return;
	const details = el("details");
	section.replaceWith(details);
	details.append(el("summary", heading.textContent), section);
	heading.remove();
	for (const request of section.querySelectorAll<HTMLDetailsElement>("details[id]")) {
		const contents = el("div");
		contents.id = request.id;
		request.removeAttribute("id");
		for (const child of Array.from(request.children)) if (child.tagName !== "SUMMARY") contents.append(child);
		request.append(contents);
	}
	if (!point) return;
	const extra = el("details");
	extra.append(el("summary", reportText("Advanced metrics", zh)));
	const facts = el("table");
	let requestURL: URL | undefined;
	if (point.detail_path && evaluationURL) {
		try {
			const url = new URL(evaluationURL);
			const path = url.pathname.replace(/\/evaluations\/(\d+)\/?$/, `/api/v1/benchmarks/$1/points/${point.seq}/detail`);
			if (["http:", "https:"].includes(url.protocol)) requestURL = new URL(path, url.origin);
		} catch { /* Historical reports can contain an incomplete evaluation URL. */ }
	}
	for (const { key, label, lines } of advancedPointFacts(point, t)) {
		const cell = el("td");
		lines.forEach((line, index) => {
			if (index) cell.append(el("br"));
			cell.append(line);
		});
		if (key === "detail" && requestURL) {
			const link = el("a", lines[0]);
			link.href = requestURL.href;
			cell.replaceChildren(link);
		}
		const spec = point.long_tail?.speculative;
		if (key === "spec" && spec && point.long_tail?.evidence?.version !== 1 && Number.isFinite(spec.tokens_per_iteration) && Number.isFinite(spec.acceptance_rate)) {
			cell.title = t("support.speculativeValue", { tokens: spec.tokens_per_iteration.toFixed(3), rate: (spec.acceptance_rate * 100).toFixed(2) });
		}
		facts.insertRow().append(el("th", label), cell);
	}
	const table = el("table"), head = table.createTHead().insertRow();
	for (const field of ["Percentile", "TTFT (ms)", "TPOT (ms)", "End-to-end latency (ms)", "Chunk interval (ms)"]) head.append(el("th", reportText(field, zh)));
	const body = table.createTBody();
	for (const { p, ttft, tpot, latency, itl } of advancedPercentiles(point)) {
		const row = body.insertRow();
		row.append(el("th", p));
		for (const value of [ttft, tpot, latency, itl]) row.insertCell().textContent = value == null ? "—" : value.toFixed(3);
	}
	extra.append(facts, table);
	section.prepend(extra);
}

export function configuration(run: BenchmarkRun, saved?: SavedReportContext) {
	// Only export configuration fields. Imported workload samples can contain customer prompts.
	const { dataset, search } = run;
	const numbers = (object: object | undefined) => Object.fromEntries(Object.entries(object || {}).filter(([, v]) =>
		typeof v === "number" || typeof v === "boolean" || (Array.isArray(v) && v.every(n => typeof n === "number"))));
	return { dataset: { kind: dataset?.kind, preset: dataset?.preset, ...numbers(dataset), workload_fingerprint: dataset?.workload_fingerprint, profile: saved?.dataset?.profile ?? (dataset?.profile ? numbers(dataset.profile) : undefined) },
		search: { ...numbers(search), sessions_per_point: search?.sessions_per_point, objective: search?.objective }, target_snapshot: saved?.target_snapshot, tokenizer_ref: saved?.tokenizer, keep_detail: run.keep_detail, detail_cap_mb: run.detail_cap_mb };
}
