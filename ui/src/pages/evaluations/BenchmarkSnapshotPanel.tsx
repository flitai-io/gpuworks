import { Alert, Card, Collapse, Descriptions, Space, Tag, Typography } from "antd";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { useState } from "react";
import type { BenchmarkDatasetProfile, BenchmarkRun, BenchmarkTargetSnapshot } from "../../api";
import { agenticDesignPrefixReuseRate } from "./agenticWorkload";

function value(value: unknown) {
	if (value == null || value === "") return "—";
	if (Array.isArray(value)) return value.length ? value.join(" ") : "—";
	if (typeof value === "object") return JSON.stringify(value);
	return String(value);
}

function gpuSummary(snapshot?: BenchmarkTargetSnapshot) {
	if (!snapshot?.gpus?.length) return snapshot?.num_gpus ? `${snapshot.num_gpus}` : "—";
	const groups = new Map<string, number>();
	for (const gpu of snapshot.gpus) {
		const label = [gpu.name || gpu.vendor || "GPU", gpu.arch_id ? `(${gpu.arch_id})` : ""].filter(Boolean).join(" ");
		groups.set(label, (groups.get(label) || 0) + 1);
	}
	return [...groups.entries()].map(([label, count]) => `${label} × ${count}`).join(" · ");
}

export function benchmarkDatasetProfile(run: BenchmarkRun, profiles: Record<string, BenchmarkDatasetProfile>) {
	return run.dataset?.profile || profiles[run.dataset?.kind || ""];
}

export function benchmarkReusesSamples(run: BenchmarkRun, profile?: BenchmarkDatasetProfile) {
	if (!profile?.sample_count || run.dataset?.kind === "random") return false;
	const concurrency = Array.isArray(run.search?.concurrency) ? run.search?.concurrency : [run.search?.max_concurrency || run.search?.concurrency || 1];
	const maximum = Math.max(1, ...concurrency.map(Number).filter(Number.isFinite));
	return maximum * Number(run.search?.requests_per_point_multiplier || 10) > profile.sample_count;
}

export function BenchmarkTargetCard({ run }: { run: BenchmarkRun }) {
	const { t } = useTranslation("evaluations");
	const snapshot = run.target_snapshot;
	const deploymentName = snapshot?.deployment_name || (run.deployment_id ? t("utility.targetDeployment", { id: run.deployment_id }) : "—");
	const deployment = run.target_kind === "instance" && !run.deployment_deleted && !run.instance_deleted
		? <Link to={`/deployments?deployment=${run.deployment_id}`}>{deploymentName}</Link>
		: deploymentName;
	const designPrefixReuseRate = agenticDesignPrefixReuseRate(run.dataset);
	return <Card data-report-target size="small" title={t("detail.target.title")}>
		{!snapshot && <Alert style={{ marginBottom: 12 }} type="info" showIcon message={t("detail.target.legacyMissing")} />}
		<Descriptions size="small" column={{ xs: 1, sm: 2, xl: 3 }}>
			<Descriptions.Item label={t("detail.target.kind")}>{t(run.target_kind === "instance" ? "detail.target.managed" : "detail.target.external")}</Descriptions.Item>
			{run.target_kind === "instance" && <Descriptions.Item label={t("detail.target.deployment")}><Space size={6}>{deployment}{run.deployment_deleted && <Tag color="default">{t("detail.target.deleted")}</Tag>}</Space></Descriptions.Item>}
			<Descriptions.Item label={t("detail.target.endpoint")}><span data-report-endpoint>{snapshot?.base_url || run.base_url || "—"}</span></Descriptions.Item>
			<Descriptions.Item label={t("detail.target.model")}>{snapshot?.served_name || snapshot?.model || run.model}</Descriptions.Item>
			<Descriptions.Item label={t("detail.target.loadOrigin")}>{snapshot?.load_origin || run.load_origin}</Descriptions.Item>
			<Descriptions.Item label={t("detail.target.gpu")}>{gpuSummary(snapshot) || (run.num_gpus ? String(run.num_gpus) : "—")}</Descriptions.Item>
			{designPrefixReuseRate != null && <Descriptions.Item label={t("agentic.result.designReuse")}>{(designPrefixReuseRate * 100).toFixed(1)}%</Descriptions.Item>}
		</Descriptions>
	</Card>;
}

export function BenchmarkSnapshotPanel({ run, expanded = false }: { run: BenchmarkRun; expanded?: boolean }) {
	const { t } = useTranslation("evaluations");
	const [activeKeys, setActiveKeys] = useState<string[]>([]);
	const snapshot = run.target_snapshot;
	const raw = { target_snapshot: snapshot, dataset: run.dataset, search: run.search, tokenizer_ref: run.tokenizer_ref, keep_detail: run.keep_detail, detail_cap_mb: run.detail_cap_mb };
	const rawText = JSON.stringify(raw, null, 2);
	return <Collapse activeKey={expanded ? ["snapshot"] : activeKeys} onChange={(keys) => setActiveKeys(typeof keys === "string" ? [keys] : keys)} items={[{
		key: "snapshot", label: t("detail.snapshot"), children: <Space direction="vertical" size={16} style={{ width: "100%" }}>
			<Typography.Paragraph data-report-exclude copyable={{ text: rawText }} style={{ marginBottom: 0 }}>{t("detail.snapshotCopy")}</Typography.Paragraph>
			{!snapshot ? <Alert type="info" showIcon message={t("detail.target.legacyMissing")} /> : <Descriptions title={t("detail.snapshotSection.target")} size="small" bordered column={2}>
				<Descriptions.Item label={t("detail.target.model")}>{value(snapshot.model_source_id || snapshot.model)}</Descriptions.Item>
				<Descriptions.Item label={t("detail.snapshotField.engine")}>{value(snapshot.engine)}</Descriptions.Item>
				<Descriptions.Item label={t("detail.snapshotField.image")} span={2}>{value(snapshot.image)}</Descriptions.Item>
				<Descriptions.Item label={t("detail.snapshotField.commandMode")}>{value(snapshot.command_mode)}</Descriptions.Item>
				<Descriptions.Item label={t("detail.snapshotField.gpuIndexes")}>{value(snapshot.gpu_indexes)}</Descriptions.Item>
				<Descriptions.Item label={t("detail.snapshotField.entrypoint")} span={2}><Typography.Text code data-report-config="entrypoint">{value(snapshot.entrypoint)}</Typography.Text></Descriptions.Item>
				<Descriptions.Item label={t("detail.snapshotField.argv")} span={2}><Typography.Text code data-report-config="argv">{value(snapshot.argv)}</Typography.Text></Descriptions.Item>
				<Descriptions.Item label={t("detail.snapshotField.env")} span={2}><pre data-report-config="env" style={{ margin: 0, whiteSpace: "pre-wrap" }}>{JSON.stringify(snapshot.env || {}, null, 2)}</pre></Descriptions.Item>
				<Descriptions.Item label={t("detail.snapshotField.dockerArgs")} span={2}><Typography.Text code data-report-config="docker_args">{value(snapshot.docker_args)}</Typography.Text></Descriptions.Item>
			</Descriptions>}
			<Descriptions title={t("detail.snapshotSection.dataset")} size="small" bordered column={2}>
				<Descriptions.Item label={t("wizard.dataset")}>{value(run.dataset?.kind)}</Descriptions.Item>
				<Descriptions.Item label={t("detail.snapshotField.datasetConfig")}><Typography.Text code data-report-config="dataset">{JSON.stringify(run.dataset)}</Typography.Text></Descriptions.Item>
			</Descriptions>
			<Descriptions title={t("detail.snapshotSection.search")} size="small" bordered column={1}>
				<Descriptions.Item label={t("detail.snapshotField.searchConfig")}><pre data-report-config="search" style={{ margin: 0, whiteSpace: "pre-wrap" }}>{JSON.stringify(run.search || {}, null, 2)}</pre></Descriptions.Item>
			</Descriptions>
			<Descriptions title={t("detail.snapshotSection.tokenizer")} size="small" bordered column={1}>
				<Descriptions.Item label="tokenizer_ref"><span data-report-config={run.tokenizer_ref ? "tokenizer" : undefined}>{run.tokenizer_ref || t("detail.snapshotField.estimateMode")}</span></Descriptions.Item>
			</Descriptions>
			<Collapse ghost items={[{ key: "raw", label: t("detail.rawJSON"), children: <pre data-report-config="raw" style={{ whiteSpace: "pre-wrap" }}>{rawText}</pre> }]} />
		</Space>,
	}]} />;
}
