import type { TFunction } from "i18next";
import type { BenchmarkPoint } from "../../api";
import { format, pointTimingValue } from "./evaluationUtils";

export function advancedPointFacts(point: BenchmarkPoint | undefined, t: TFunction<"evaluations">) {
	const tail = point?.long_tail;
	const evidence = tail?.evidence?.version === 1 ? tail.evidence : undefined;
	const cache = evidence?.all.cache;
	const cacheRate = evidence ? cache && cache.input_tokens > 0 ? cache.cached_tokens / cache.input_tokens : undefined : tail?.kv_hit_rate;
	const speculative = evidence ? undefined : tail?.speculative;
	const sources = (counts?: Record<string, number> | null) => Object.entries(counts || {})
		.filter(([, count]) => count > 0)
		.map(([source, count]) => t(`evidence.source.${source}`, { defaultValue: t("evidence.unknown") }) + ": " + count)
		.join(" · ") || t("evidence.unknown");
	return [
		{ key: "kv", label: t(evidence ? "evidence.cacheWeighted" : "support.kv"), lines: [
			cacheRate == null ? t(evidence ? "evidence.unknown" : "support.kvMissing") : format(cacheRate * 100) + "%",
		] },
		{ key: "spec", label: t("support.speculative"), lines: speculative ? [
			t("support.speculativeValue", { tokens: format(speculative.tokens_per_iteration, 2), rate: format(speculative.acceptance_rate * 100) }),
			t("evidence.legacySpeculative"),
		] : [t(evidence ? "evidence.unknown" : "support.speculativeMissing")] },
		{ key: "length", label: t("support.lengthMethod"), lines: evidence ? [
			t("evidence.input") + ": " + sources(evidence.all.tokens?.input_sources),
			t("evidence.output") + ": " + sources(evidence.all.tokens?.output_sources),
		] : [tail?.approximate_tokens == null ? t("evidence.unknown") : t(tail.approximate_tokens ? "support.estimated" : "support.exact")] },
		{ key: "detail", label: t("support.detail"), lines: [t(point?.detail_path ? "support.download" : "support.detailMissing")] },
	];
}

export function advancedPercentiles(point?: BenchmarkPoint) {
	const tail = point?.long_tail;
	const timing = (metric: "ttft" | "tpot" | "latency", rank: string) => {
		if (tail?.evidence?.version !== 1) return tail?.[`${metric}_ms`]?.[rank];
		if (!point || (rank !== "50" && rank !== "99")) return undefined;
		return pointTimingValue(point, `p${rank}_${metric}`);
	};
	return ["1", "5", "10", "25", "50", "75", "90", "95", "99"].map(rank => ({
		p: "P" + rank, ttft: timing("ttft", rank), tpot: timing("tpot", rank), latency: timing("latency", rank),
		itl: tail?.evidence?.version === 1 ? undefined : tail?.itl_ms?.[rank],
	}));
}
