import { useEffect, useMemo, useRef, useState } from "react";
import { QuestionCircleOutlined, SearchOutlined, UploadOutlined } from "@ant-design/icons";
import { Alert, Button, Card, Checkbox, Col, Collapse, Drawer, Form, Input, InputNumber, Modal, Radio, Row, Select, Space, Steps, Tooltip, Typography, Upload, message } from "antd";
import type { BenchmarkMeta, BenchmarkRun, Deployment, DeploymentListItem, Node } from "../../api";
import { api } from "../../api";
import { APIError } from "../../apiError";
import { estimateOpenLoop, estimateRequests, initialBenchmarkDataset, modeFormReset, normalizeEndpointURL, parseNumbers, parseRates } from "./evaluationUtils";
import { formatTime } from "../../i18n/datetime";
import { localizedText } from "../../i18n/localizedText";
import { useTranslation } from "react-i18next";
import { OutboundHostAccess, useOutboundHostAccess } from "../../components/OutboundHostAccess";
import {
	agenticSessionsPerPoint,
	summarizeAgenticLoad,
} from "./agenticWorkload";
import { hasOperation } from "../../modelInference";
import { benchmarkOriginNodes } from "./benchmarkOrigins";
import AgenticLoadSummary from "./AgenticLoadSummary";
import TokenizerSourceFields from "./TokenizerSourceFields";
import { ENGINE_REF, type TokenizerSource } from "./tokenizerSource";
import { tokenizerFormState, tokenizerReference, tokenizerSourceOrDefault } from "./tokenizerSource";

type Values = {
	name?: string;
	mode: BenchmarkRun["mode"];
	target_kind: "instance" | "external";
	deployment_id?: number;
	base_url?: string;
	api_key?: string;
	model?: string;
	load_origin: string;
	num_gpus?: number;
	dataset: string;
	agentic_mean_input?: number;
	agentic_preset?: string;
	agentic_turns?: number;
	isl: string;
	osl: string;
	concurrency: string;
	max_concurrency?: number;
	min_throughput_gain?: number;
	requests_per_point_multiplier: number;
	objective?: "output_tps" | "total_tps" | "rps";
	keep_detail: boolean;
	detail_cap_mb: number;
	warmup_requests: number;
	request_timeout_s: number;
	ignore_eos: boolean;
	load_mode: "closed" | "open";
	request_rates?: string;
	burstiness?: number;
	max_in_flight?: number;
	point_duration_s?: number;
	tokenizer_source: TokenizerSource;
	tokenizer_repo?: string;
	tokenizer_ref?: string;
	[key: string]: unknown;
};

export type WizardPreset = { mode?: BenchmarkRun["mode"]; deploymentID?: number; sourceRun?: BenchmarkRun; maxConcurrency?: number };

// Admission refusals the user may override. They are matched by code and never
// by message text, so reworded copy cannot silently disable the dialog.
const CONFLICT_CODES = new Set(["BENCHMARK_RUN_CONFLICT", "BENCHMARK_SERVER_ACTIVE"]);

// Per-user limits of earlier runs divided throughput by concurrency; they are
// no longer offered, so a rerun leaves them out and says so.
const LEGACY_SLA_KEYS = ["max_per_user_total_tps", "min_per_user_total_tps", "max_per_user_output_tps", "min_per_user_output_tps"] as const;

// The created run records only the reference the automatic source resolved to,
// so the notice names what it will use: the target's tokenize endpoint
// alone, or the target's endpoint before a fallback that is a repository, a
// copy fetched back to the server, or a file on the node that will generate
// the load.
function automaticTokenizerNoticeKey(run: BenchmarkRun): string {
	let reference = run.tokenizer_ref || "";
	if (reference === ENGINE_REF) return "wizard.tokenizerAutoEngine";
	if (reference.startsWith(ENGINE_REF)) reference = reference.slice(ENGINE_REF.length);
	if (reference.startsWith("hf://") || reference.startsWith("ms://")) return "wizard.tokenizerAutoRepository";
	return run.load_origin === "server" ? "wizard.tokenizerAutoFetched" : "wizard.tokenizerAutoNode";
}

export default function BenchmarkWizard({ open, meta, deployments, nodes, onSearchDeployments, onSearchNodes, preset, onClose, onCreated }: {
	open: boolean;
	meta: BenchmarkMeta;
	/** Deployments that can serve now; with onSearchDeployments, one server-searched page. */
	deployments: DeploymentListItem[];
	/** Online nodes; with onSearchNodes, one server-searched page. */
	nodes: Node[];
	onSearchDeployments?: (query: string) => void;
	onSearchNodes?: (query: string) => void;
	preset?: WizardPreset;
	onClose: () => void;
	onCreated: (run: BenchmarkRun) => void;
}) {
	const { t, i18n } = useTranslation("evaluations");
	const language = i18n.resolvedLanguage || i18n.language;
	const [form] = Form.useForm<Values>();
	const [step, setStep] = useState(0);
	const [models, setModels] = useState<string[]>([]);
	const [probing, setProbing] = useState(false);
	const [submitting, setSubmitting] = useState(false);
	const [probeNotice, setProbeNotice] = useState<string>();
	const [importedDataset, setImportedDataset] = useState<BenchmarkRun["dataset"]>();
	const initializedPreset = useRef("");
	const mode = Form.useWatch("mode", form) || "saturation";
	const targetKind = Form.useWatch("target_kind", form) || "instance";
	const deploymentID = Form.useWatch("deployment_id", form);
	const [loadOrigin, baseURL] = Form.useWatch((values: Values) => [values.load_origin, values.base_url], { form, preserve: true }) || [];
	const access = useOutboundHostAccess([baseURL || ""], open && targetKind === "external" && loadOrigin === "server");
	const keepDetail = Form.useWatch("keep_detail", form);
	const dataset = Form.useWatch("dataset", form) || "random";
	const concurrencyRaw = Form.useWatch("concurrency", form) || "1";
	const agenticMeanInput = Form.useWatch("agentic_mean_input", form) || meta.agentic_presets.find((item) => item.value === meta.agentic_defaults.preset)?.mean_input_tokens || 80_000;
	const agenticTurns = Form.useWatch("agentic_turns", form) || meta.agentic_defaults.turns;
	const agenticConcurrencies = useMemo(() => parseNumbers(concurrencyRaw), [concurrencyRaw]);
	const agenticSummary = useMemo(() => summarizeAgenticLoad(agenticMeanInput, agenticTurns, meta, agenticConcurrencies.length ? agenticConcurrencies : meta.agentic_defaults.concurrency), [agenticConcurrencies, agenticMeanInput, agenticTurns, meta]);
	const tokenizerSource = Form.useWatch("tokenizer_source", form) || (targetKind === "instance" ? "auto" : "estimate");
	const tokenizerRef = Form.useWatch("tokenizer_ref", form);
	const islRaw = Form.useWatch("isl", form) || "1";
	const oslRaw = Form.useWatch("osl", form) || "1";
	const maxConcurrency = Form.useWatch("max_concurrency", form);
	const multiplier = Form.useWatch("requests_per_point_multiplier", form) || 10;
	const loadMode = Form.useWatch("load_mode", form) || "closed";
	const openLoop = mode === "grid" && loadMode === "open";
	const ratesRaw = Form.useWatch("request_rates", form) || "";
	const pointDuration = Form.useWatch("point_duration_s", form) || meta.load_defaults.point_duration_s;
	const ignoreEOSOffered = meta.load_defaults.ignore_eos_datasets.includes(dataset);
	const [legacySLA, setLegacySLA] = useState(false);
	const datasetProfile = dataset === "imported" ? importedDataset?.profile : meta.dataset_profiles?.[dataset];

	useEffect(() => {
		if (!open) {
			initializedPreset.current = "";
			return;
		}
		const presetKey = JSON.stringify([preset?.mode, preset?.deploymentID, preset?.sourceRun?.id, preset?.maxConcurrency]);
		if (initializedPreset.current === presetKey) return;
		initializedPreset.current = presetKey;
		const source = preset?.sourceRun;
		const initialTargetKind: "instance" | "external" = source?.target_kind || "instance";
		const initialDataset = initialBenchmarkDataset(source?.dataset?.kind);
		const managedTarget = initialTargetKind === "instance";
		// The tokenizer source is a creation-time input rather than run state, so
		// a rerun restores it from the reference the previous run resolved to,
		// and a fresh managed run starts on the automatic source.
		const sourceTokenizer = tokenizerFormState(source?.tokenizer_ref, managedTarget);
		const initialTokenizerSource = tokenizerSourceOrDefault(managedTarget, (initialDataset === "agentic" || initialDataset === "random"), source?.tokenizer_ref, !!source);
		const initialMode = preset?.mode || source?.mode || "saturation";
		const defaults = modeFormReset(meta, initialMode);
		const sourceSearch = { ...(source?.search || {}) };
		for (const key of LEGACY_SLA_KEYS) delete sourceSearch[key];
		setLegacySLA(LEGACY_SLA_KEYS.some((key) => source?.search?.[key] != null));
		form.resetFields();
		form.setFieldsValue({
			...defaults,
			...sourceSearch,
			name: source ? `${source.name} - ${t("wizard.rerunSuffix")}` : undefined,
			target_kind: initialTargetKind,
			deployment_id: preset?.deploymentID || source?.deployment_id,
			base_url: source?.base_url,
			model: source?.model,
			// Runs never store "auto" — the server resolves it at creation — but a
			// legacy row that does falls back to the server origin, the only
			// behavior "auto" has there anyway.
			load_origin: source?.load_origin && source.load_origin !== "auto" ? source.load_origin : "server",
			num_gpus: source?.num_gpus || undefined,
			dataset: initialDataset,
			agentic_preset: source?.dataset?.preset || meta.agentic_defaults.preset,
			agentic_mean_input: source?.dataset?.mean_input_tokens || meta.agentic_presets.find((item) => item.value === meta.agentic_defaults.preset)?.mean_input_tokens,
			agentic_turns: source?.dataset?.turns || meta.agentic_defaults.turns,
			isl: source?.search?.isl?.join(",") || defaults.isl,
			osl: source?.search?.osl?.join(",") || defaults.osl,
			concurrency: source ? (source.dataset?.kind === "agentic" && source.search?.fixed_concurrency?.length ? source.search.fixed_concurrency.join(", ") : Array.isArray(source.search?.concurrency) ? source.search?.concurrency.join(",") : String(source.search?.concurrency || 1)) : defaults.concurrency,
			max_concurrency: preset?.maxConcurrency || source?.search?.max_concurrency || defaults.max_concurrency,
			min_throughput_gain: source?.search?.min_throughput_gain ?? defaults.min_throughput_gain,
			requests_per_point_multiplier: source?.search?.requests_per_point_multiplier || defaults.requests_per_point_multiplier,
			objective: source?.search?.objective || defaults.objective,
			keep_detail: source?.keep_detail || false,
			detail_cap_mb: source?.detail_cap_mb || meta.detail_cap_mb.default,
			// Runs created before these settings existed rerun with today's
			// defaults rather than their implicit old behaviour.
			warmup_requests: source?.search?.warmup_requests ?? meta.load_defaults.warmup_requests,
			request_timeout_s: source?.search?.request_timeout_s || meta.load_defaults.request_timeout_s,
			ignore_eos: source?.dataset?.ignore_eos ?? meta.load_defaults.ignore_eos,
			load_mode: source?.search?.request_rates?.length ? "open" : "closed",
			request_rates: source?.search?.request_rates?.join(",") || "",
			burstiness: source?.search?.burstiness || meta.load_defaults.burstiness,
			max_in_flight: source?.search?.max_in_flight || 0,
			point_duration_s: source?.search?.point_duration_s || meta.load_defaults.point_duration_s,
			tokenizer_source: initialTokenizerSource,
			tokenizer_repo: sourceTokenizer.repository,
			tokenizer_ref: sourceTokenizer.reference,
		});
		setStep(preset?.mode || preset?.deploymentID || source ? 1 : 0);
		setModels(source?.model ? [source.model] : []);
		setProbeNotice(undefined);
		setImportedDataset(undefined);
	}, [form, meta, open, preset]);

	// The choices are one searched page; a deployment or node chosen earlier,
	// or named by a preset, must keep its name when a later search leaves it
	// out, so every one seen is remembered and missing ones are looked up.
	const [knownDeployments, setKnownDeployments] = useState(() => new Map<number, DeploymentListItem>());
	const [knownNodes, setKnownNodes] = useState(() => new Map<string, Node>());
	useEffect(() => {
		if (deployments.length) setKnownDeployments((known) => new Map([...known, ...deployments.map((item) => [item.id, item] as const)]));
	}, [deployments]);
	useEffect(() => {
		if (nodes.length) setKnownNodes((known) => new Map([...known, ...nodes.map((item) => [item.uuid, item] as const)]));
	}, [nodes]);
	const selectedDeployment = deploymentID ? knownDeployments.get(deploymentID) : undefined;
	const listedDeployment = deployments.find((item) => item.id === deploymentID);
	const [deploymentDetail, setDeploymentDetail] = useState<Deployment>();
	const [originLoading, setOriginLoading] = useState(false);
	const [originError, setOriginError] = useState(false);
	useEffect(() => {
		setDeploymentDetail(undefined);
		setOriginError(false);
		if (!open || targetKind !== "instance" || !deploymentID) {
			setOriginLoading(false);
			return;
		}
		let alive = true;
		let request = 0;
		const refresh = async () => {
			const current = ++request;
			setOriginLoading(true);
			try {
				const deployment = await api.deployment(deploymentID);
				if (!alive || current !== request) return;
				setDeploymentDetail(deployment);
				setKnownDeployments((known) => new Map(known).set(deployment.id, deployment));
				setOriginError(false);
			} catch {
				if (!alive || current !== request) return;
				setDeploymentDetail(undefined);
				setOriginError(true);
			} finally {
				if (alive && current === request) setOriginLoading(false);
			}
		};
		void refresh();
		// Only the selected deployment is refreshed, including instance changes
		// that leave its list-row summary unchanged.
		const timer = setInterval(() => { void refresh(); }, 10000);
		return () => { alive = false; clearInterval(timer); };
	}, [open, targetKind, deploymentID, listedDeployment]);
	const targetNodeUUIDs = useMemo(() => deploymentDetail && deploymentDetail.id === deploymentID
		? benchmarkOriginNodes(deploymentDetail.instances || []) : [], [deploymentDetail, deploymentID]);
	const targetNodeUUID = loadOrigin && targetNodeUUIDs.includes(loadOrigin) ? loadOrigin : targetNodeUUIDs[0];
	useEffect(() => {
		if (!open || targetKind !== "instance") return;
		let alive = true;
		for (const uuid of targetNodeUUIDs) {
			// Current search results already carry fresh metadata. Refresh pinned
			// runtime nodes too, so a cached online state cannot outlive a disconnect.
			if (nodes.some((node) => node.uuid === uuid)) continue;
			api.node(uuid).then((node) => {
				if (alive) setKnownNodes((known) => new Map(known).set(node.uuid, node));
			}).catch(() => {
				if (alive && form.getFieldValue("load_origin") === uuid) form.setFieldValue("load_origin", "server");
			});
		}
		return () => { alive = false; };
	}, [open, targetKind, targetNodeUUIDs, nodes, form]);
	const deploymentOptions = useMemo(() => {
		const shown = selectedDeployment && !deployments.some((item) => item.id === selectedDeployment.id) ? [selectedDeployment, ...deployments] : deployments;
		return shown.map((deployment) => ({ value: deployment.id, label: deployment.name, disabled: !hasOperation(deployment.operations, "openai.chat") }));
	}, [deployments, selectedDeployment]);
	const originOptions = useMemo(() => {
		// The server is the default origin for both target kinds: a managed
		// deployment is measured through the gateway the way clients reach it,
		// which is also the only end-to-end path for multi-instance and PD
		// deployments. A node is chosen to measure one of its instances directly.
		const options: Array<{ value: string; label: string; disabled?: boolean }> = [{ value: "server", label: "Server" }];
		// The target's own node and the chosen origin lead, whether or not the
		// current search page holds them.
		const pinned = [...targetNodeUUIDs, loadOrigin].map((uuid) => (uuid ? knownNodes.get(uuid) : undefined)).filter((node): node is Node => !!node);
		const listed = new Set<string>();
		for (const node of [...pinned, ...nodes].filter((item) => item.state === "online")) {
			if (listed.has(node.uuid) || (targetKind === "instance" && !targetNodeUUIDs.includes(node.uuid))) continue;
			listed.add(node.uuid);
			options.push({ value: node.uuid, label: `${t(targetNodeUUIDs.includes(node.uuid) ? "wizard.targetNode" : "wizard.computeNode")} · ${node.name || node.hostname || node.uuid}`, disabled: !node.caps?.includes("bench") });
		}
		return options;
	}, [nodes, knownNodes, targetNodeUUIDs, targetKind, loadOrigin, t]);

	useEffect(() => {
		if (!open || targetKind !== "instance" || originLoading || (!originError && (!deploymentDetail || deploymentDetail.id !== deploymentID))) return;
		if (!loadOrigin || loadOrigin === "server") return;
		const node = knownNodes.get(loadOrigin);
		// A valid leader may be absent from the searched node page. Keep the
		// selection while its metadata loads rather than silently changing paths.
		if (!targetNodeUUIDs.includes(loadOrigin) || (node && (node.state !== "online" || !node.caps?.includes("bench")))) {
			form.setFieldValue("load_origin", "server");
		}
	}, [open, targetKind, originLoading, originError, deploymentDetail, deploymentID, targetNodeUUIDs, knownNodes, loadOrigin, form]);

	const estimate = estimateRequests(mode, parseNumbers(concurrencyRaw), multiplier, maxConcurrency, dataset === "random" ? parseNumbers(islRaw).length : 1, dataset === "random" || dataset === "multiturn" ? parseNumbers(oslRaw).length : 1);
	const openEstimate = openLoop ? estimateOpenLoop(parseRates(ratesRaw), pointDuration, meta.load_defaults.min_open_loop_requests, dataset === "random" ? parseNumbers(islRaw).length : 1, dataset === "random" || dataset === "multiturn" ? parseNumbers(oslRaw).length : 1) : undefined;
	const estimateText = openEstimate
		? t("wizard.estimateOpenLoop", { requests: openEstimate.requests.toLocaleString(), minutes: openEstimate.minutes })
		: dataset === "agentic"
		? t("agentic.estimate", { points: agenticSummary.points, sessions: agenticSummary.sessions, requests: agenticSummary.requests })
		: typeof estimate === "number"
		? t("wizard.estimate", { samples: estimate.toLocaleString(), min: Math.max(1, Math.ceil(estimate / 60)), max: Math.max(2, Math.ceil(estimate / 20)) })
		: t("wizard.estimateRange", { low: estimate.low.toLocaleString(), high: estimate.high.toLocaleString(), min: Math.max(1, Math.ceil(estimate.low / 60)), max: Math.max(2, Math.ceil(estimate.high / 20)) });
	const plannedConcurrency = mode === "grid" ? Math.max(1, ...parseNumbers(concurrencyRaw)) : Math.max(1, Number(maxConcurrency || parseNumbers(concurrencyRaw)[0] || 1));
	const reusesSamples = dataset !== "random" && !!datasetProfile?.sample_count && plannedConcurrency * multiplier > datasetProfile.sample_count;

	const chooseMode = (nextMode: BenchmarkRun["mode"]) => {
		form.setFieldsValue({ ...modeFormReset(meta, nextMode), dataset: "random", load_mode: "closed" });
		setStep(1);
	};

	const probe = async () => {
		const values = form.getFieldsValue();
		if (!values.base_url) return message.warning(t("wizard.urlRequired"));
		const normalized = normalizeEndpointURL(values.base_url);
		form.setFieldValue("base_url", normalized.url);
		setProbing(true);
		try {
			await access.authorize();
			const result = await api.benchmarkModels({ node_uuid: values.load_origin, url: normalized.url, api_key: values.api_key });
			setModels(result.models);
			form.setFieldValue("model", result.models[0]);
			if (!values.tokenizer_repo) form.setFieldValue("tokenizer_repo", result.models[0]);
			setProbeNotice(result.notice || (normalized.strippedV1 ? t("wizard.v1Removed") : t("wizard.probeSuccess", { count: result.models.length })));
		} catch (error) {
			setProbeNotice(undefined);
			message.error((error as Error).message);
		} finally { setProbing(false); }
	};

	// A conflicting create is a decision to put to the user rather than an error
	// to report: the dialog names what the new run would contend with, and the
	// same create is retried with the override once they accept.
	const confirmConflict = (error: APIError, retry: () => void) => {
		const params = error.params || {};
		const kind = String(params.kind || "server_active");
		const originLabel = originOptions.find((item) => item.value === form.getFieldValue("load_origin"))?.label;
		const targetNode = targetNodeUUID ? knownNodes.get(targetNodeUUID) : undefined;
		Modal.confirm({
			title: t("wizard.conflictTitle"),
			content: <Space direction="vertical" size={4}>
				<Typography.Text>
					{t(`wizard.conflictKind.${kind}`, {
						origin: originLabel || form.getFieldValue("load_origin"),
						deployment: selectedDeployment?.name || `#${form.getFieldValue("deployment_id")}`,
						node: targetNode?.name || targetNode?.hostname || targetNodeUUID || "",
					})}
				</Typography.Text>
				<Typography.Text type="secondary">{t("wizard.conflictRun", { id: params.run_id, name: String(params.run_name || "").trim() || t("wizard.unnamedRun") })}</Typography.Text>
				{Number(params.count || 1) > 1 && <Typography.Text type="secondary">{t("wizard.conflictMore", { count: params.count })}</Typography.Text>}
			</Space>,
			okText: t("wizard.conflictContinue"),
			cancelText: t("wizard.conflictCancel"),
			onOk: retry,
		});
	};

	const submit = async (allowConflict = false) => {
		setSubmitting(true);
		try {
			const values = await form.validateFields();
			const concurrency = parseNumbers(values.concurrency);
			const rates = parseRates(values.request_rates);
			const isl = parseNumbers(values.isl), osl = parseNumbers(values.osl);
			if ((openLoop ? !rates.length : !concurrency.length) || (dataset === "random" && (!isl.length || !osl.length)) || (dataset === "multiturn" && !osl.length)) throw new Error(t("wizard.numbersRequired"));
			if (values.dataset === "imported" && !importedDataset) throw new Error(t("wizard.datasetRequired"));
			const searchISL = dataset === "agentic" ? [agenticSummary.meanInputTokens] : dataset === "random" ? isl : [isl[0] || meta.mode_defaults[mode].isl[0]];
			const searchOSL = dataset === "agentic" ? [meta.agentic_defaults.output_tokens] : dataset === "random" || dataset === "multiturn" ? osl : [osl[0] || meta.mode_defaults[mode].osl[0]];
			const search: Record<string, unknown> = openLoop
				? { request_rates: rates, burstiness: values.burstiness, max_in_flight: values.max_in_flight || undefined, point_duration_s: values.point_duration_s, isl: searchISL, osl: searchOSL }
				: mode === "grid"
				? { concurrency, isl: searchISL, osl: searchOSL }
				: { concurrency: concurrency[0], max_concurrency: dataset === "agentic" ? Math.max(...concurrency) : values.max_concurrency, isl: [searchISL[0]], osl: [searchOSL[0]] };
			search.warmup_requests = values.warmup_requests ?? 0;
			search.request_timeout_s = values.request_timeout_s || meta.load_defaults.request_timeout_s;
			if (mode === "sla" || mode === "grid") for (const field of meta.sla_constraints) if (values[field.key] != null) search[field.key] = values[field.key];
			Object.assign(search, { min_throughput_gain: values.min_throughput_gain, objective: values.objective, requests_per_point_multiplier: values.requests_per_point_multiplier });
			if (dataset === "agentic" && !openLoop) Object.assign(search, { fixed_concurrency: concurrency, sessions_per_point: agenticSessionsPerPoint(meta, concurrency) });
			const ignoreEOS = ignoreEOSOffered ? { ignore_eos: !!values.ignore_eos } : {};
			const endpoint = normalizeEndpointURL(values.base_url || "").url;
			await access.authorize();
			const created = await api.createBenchmark({
				name: values.name?.trim() || `${localizedText(meta.modes.find((item) => item.value === mode)?.label, language) || t("wizard.defaultName")} ${formatTime(new Date())}`,
				target_kind: values.target_kind, deployment_id: values.deployment_id, base_url: endpoint || undefined,
				api_key: values.api_key, model: values.model, load_origin: values.load_origin, num_gpus: values.num_gpus,
				dataset: importedDataset || (values.dataset === "agentic" ? { kind: "agentic", preset: values.agentic_preset, mean_input_tokens: agenticMeanInput, turns: agenticTurns, seed: meta.agentic_defaults.seed, ...ignoreEOS } : { kind: values.dataset, input_tokens: isl[0], output_tokens: osl[0], ...ignoreEOS }),
				mode, search, keep_detail: values.keep_detail, detail_cap_mb: values.detail_cap_mb,
				tokenizer_source: values.tokenizer_source,
				tokenizer_ref: tokenizerReference(values.tokenizer_source, values.tokenizer_repo, values.tokenizer_ref, meta.tokenizer_schemes),
				// Sent only after the user has seen the conflict this create
				// would introduce.
				allow_conflict: allowConflict || undefined,
			});
			onCreated(created);
			message.success(t("wizard.created"));
			if (values.tokenizer_source === "auto" && created.tokenizer_ref) {
				const fallback = created.tokenizer_ref.startsWith(ENGINE_REF) ? created.tokenizer_ref.slice(ENGINE_REF.length) : created.tokenizer_ref;
				const notice = t(automaticTokenizerNoticeKey(created), { ref: fallback, path: fallback });
				message.info(created.tokenizer_ref.startsWith(ENGINE_REF) && fallback ? t("wizard.tokenizerAutoEngineFirst", { fallback: notice }) : notice);
			}
		} catch (error) {
			// Admission conflicts are a decision to put to the user, not an
			// error to report: the same create is retried with the override.
			if (error instanceof APIError && CONFLICT_CODES.has(error.code)) {
				confirmConflict(error, () => { void submit(true); });
				return;
			}
			message.error((error as Error).message);
		} finally { setSubmitting(false); }
	};

	return (
			<Drawer title={t("wizard.title")} width={760} open={open} onClose={onClose} destroyOnHidden={false} footer={<Space style={{ width: "100%", justifyContent: "space-between" }}><Typography.Text type="secondary">{estimateText}</Typography.Text><Space><Button disabled={step === 0} onClick={() => setStep(step - 1)}>{t("wizard.previous")}</Button>{step < 2 ? <Button type="primary" onClick={async () => { try { if (step === 1) await form.validateFields(targetKind === "instance" ? ["deployment_id", "load_origin"] : ["base_url", "model", "load_origin"]); setStep(step + 1); } catch { /* form renders errors */ } }}>{t("wizard.next")}</Button> : <Button type="primary" loading={submitting} onClick={() => { void submit(); }}>{t("wizard.start")}</Button>}</Space></Space>}>
			<Steps current={step} items={[{ title: t("wizard.step.question") }, { title: t("wizard.step.target") }, { title: t("wizard.step.load") }]} style={{ marginBottom: 24 }} />
			<Form form={form} layout="vertical">
				<Form.Item name="mode" hidden><Input /></Form.Item>
				<div hidden={step !== 0}>
					<Row gutter={[12, 12]}>{meta.modes.map((item) => <Col xs={24} md={8} key={item.value}><Card hoverable onClick={() => chooseMode(item.value as BenchmarkRun["mode"])} title={localizedText(item.label, language)} style={{ height: "100%" }}><Typography.Text type="secondary">{localizedText(item.description, language)}</Typography.Text></Card></Col>)}</Row>
				</div>
				<div hidden={step !== 1}>
					<Form.Item name="name" label={t("wizard.name")}><Input placeholder={t("wizard.autoGenerated")} /></Form.Item>
					<Form.Item name="target_kind" label={t("wizard.target")}><Radio.Group optionType="button" buttonStyle="solid" options={[{ label: t("wizard.managedInstance"), value: "instance" }, { label: t("wizard.externalEndpoint"), value: "external" }]} onChange={(event) => {
						// Start a newly selected external target from the default origin.
						if (event.target.value === "external") form.setFieldValue("load_origin", "server");
					}} /></Form.Item>
					{targetKind === "instance" ? <Form.Item name="deployment_id" label={t("wizard.service")} rules={[{ required: true, message: t("wizard.serviceRequired") }, { validator: async (_, id) => {
						const selected = knownDeployments.get(id) || deployments.find((item) => item.id === id);
						if (selected && !hasOperation(selected.operations, "openai.chat")) throw new Error(t("wizard.chatRequired"));
					} }]} extra={t("wizard.chatRequired")}><Select showSearch {...(onSearchDeployments ? { filterOption: false, onSearch: onSearchDeployments } : { optionFilterProp: "label" })} options={deploymentOptions} /></Form.Item> : <>
						<Form.Item name="base_url" label={t("wizard.baseUrl")} rules={[{ required: true, whitespace: true }]} extra={t("wizard.baseUrlHelp")}><Input placeholder={t("wizard.baseUrlPlaceholder")} /></Form.Item>
						<OutboundHostAccess access={access} />
						<Form.Item name="api_key" label="API Key"><Input.Password /></Form.Item>
					</>}
					<Form.Item name="load_origin" label={t("wizard.loadOrigin")} extra={targetKind === "instance" && originError ? t("wizard.originLoadFailed") : undefined} rules={[{ required: true }, { validator: async (_, value) => {
						if (targetKind === "instance" && value !== "server" && (originLoading || !originOptions.some((option) => option.value === value && !option.disabled))) throw new Error(t("wizard.originUnavailable"));
					} }]}><Select loading={targetKind === "instance" && originLoading} showSearch {...(onSearchNodes ? { filterOption: false, onSearch: onSearchNodes } : { optionFilterProp: "label" })} options={originOptions} /></Form.Item>
					{targetKind === "external" && <Form.Item label={t("wizard.model")} required><Space.Compact style={{ width: "100%" }}><Form.Item name="model" noStyle rules={[{ required: true, message: t("wizard.modelProbeRequired") }]}><Select showSearch options={models.map((model) => ({ value: model, label: model }))} /></Form.Item><Button icon={<SearchOutlined />} loading={probing} onClick={probe}>{t("wizard.probe")}</Button></Space.Compact></Form.Item>}
					{probeNotice && <Alert showIcon type="success" message={probeNotice} />}
				</div>
				<div hidden={step !== 2}>
					<Alert type="info" showIcon message={t("wizard.currentQuestion", { question: localizedText(meta.modes.find((item) => item.value === mode)?.label, language) })} style={{ marginBottom: 16 }} />
					<Form.Item name="dataset" label={t("wizard.dataset")}><Select onChange={(value) => {
						if (value !== "imported") setImportedDataset(undefined);
						if (value === "agentic") {
							const selected = meta.agentic_presets.find((item) => item.value === meta.agentic_defaults.preset);
							form.setFieldsValue({ agentic_preset: meta.agentic_defaults.preset, agentic_mean_input: selected?.mean_input_tokens, agentic_turns: meta.agentic_defaults.turns, concurrency: meta.agentic_defaults.concurrency.join(", "), max_concurrency: Math.max(...meta.agentic_defaults.concurrency), tokenizer_source: tokenizerSource === "estimate" ? (targetKind === "instance" ? "auto" : "huggingface") : tokenizerSource });
						} else if (value === "random") {
							const defaults = modeFormReset(meta, mode);
							form.setFieldsValue({ concurrency: defaults.concurrency, max_concurrency: defaults.max_concurrency, isl: defaults.isl, osl: defaults.osl, tokenizer_source: tokenizerSource === "estimate" ? (targetKind === "instance" ? "auto" : "huggingface") : tokenizerSource });
						}
					}} options={meta.datasets.map((item) => ({ ...item, label: localizedText(item.label, language) }))} /></Form.Item>
					{dataset === "agentic" && <AgenticLoadSummary summary={agenticSummary} meta={meta} onPresetChange={(value) => {
						const selected = meta.agentic_presets.find((item) => item.value === value);
						if (selected) form.setFieldValue("agentic_mean_input", selected.mean_input_tokens);
					}} />}
					{dataset === "imported" && <Upload accept=".jsonl,.txt" showUploadList={false} beforeUpload={(file) => { void api.importBenchmarkDataset(file).then((result) => { setImportedDataset(result); message.success(t("wizard.samplesImported", { count: result.samples.length })); }).catch((error) => message.error((error as Error).message)); return false; }}><Button icon={<UploadOutlined />}>{t("wizard.importJsonl")}</Button></Upload>}
					{dataset !== "agentic" && dataset !== "random" && datasetProfile && <Alert style={{ marginBottom: 16, marginTop: dataset === "imported" ? 12 : 0 }} type="info" showIcon message={t("wizard.datasetProfile", { count: datasetProfile.sample_count, input: datasetProfile.max_input_tokens || "—", output: datasetProfile.max_output_tokens || "—" })} />}
					<Row gutter={12}>
						{dataset === "random" && <Col span={12}><Form.Item name="isl" label={t("wizard.isl")}><Input /></Form.Item></Col>}
						{(dataset === "random" || dataset === "multiturn") && <Col span={12}><Form.Item name="osl" label={t(dataset === "multiturn" ? "wizard.outputLimit" : "wizard.osl")}><Input /></Form.Item></Col>}
					</Row>
					{legacySLA && <Alert style={{ marginBottom: 16 }} type="warning" showIcon message={t("wizard.legacySLADropped")} />}
					{dataset !== "agentic" && !openLoop && <Row gutter={12}><Col span={12}><Form.Item name="requests_per_point_multiplier" label={t("wizard.samplesPerChannel")} extra={<Space size={6}><span>{t(dataset === "random" ? "wizard.samplesPerChannelRandomHelp" : "wizard.samplesPerChannelDatasetHelp")}</span><Tooltip title={t("wizard.sampleHelp")}><QuestionCircleOutlined /></Tooltip></Space>}><InputNumber min={1} max={1000} style={{ width: "100%" }} /></Form.Item></Col></Row>}
					{openLoop && <Row gutter={12}><Col span={12}><Form.Item name="request_rates" label={<Space size={4}>{t("wizard.requestRates")}<Tooltip title={t("wizard.requestRatesHelp")}><QuestionCircleOutlined /></Tooltip></Space>} rules={[{ validator: (_, value) => { const rates = parseRates(value); return rates.length && rates.length <= meta.load_defaults.max_request_rates && rates.every((rate) => rate <= meta.load_defaults.max_request_rate) ? Promise.resolve() : Promise.reject(new Error(t("wizard.requestRatesInvalid", { max: meta.load_defaults.max_request_rate, count: meta.load_defaults.max_request_rates }))); } }]}><Input placeholder="1, 2, 4, 8" /></Form.Item></Col><Col span={12}><Form.Item name="point_duration_s" label={<Space size={4}>{t("wizard.pointDuration")}<Tooltip title={t("wizard.pointDurationHelp", { min: meta.load_defaults.min_open_loop_requests })}><QuestionCircleOutlined /></Tooltip></Space>}><InputNumber min={meta.load_defaults.min_point_duration_s} max={meta.load_defaults.max_point_duration_s} addonAfter="s" style={{ width: "100%" }} /></Form.Item></Col></Row>}
					{/* An arrival-rate scan has no concurrency list; the field's value stays in the form for switching back. */}
					{!openLoop && <Row gutter={12}><Col span={12}><Form.Item name="concurrency" label={t(dataset === "agentic" ? "agentic.concurrency" : mode === "grid" ? "wizard.concurrencyList" : "wizard.initialConcurrency")} extra={dataset === "agentic" ? t("agentic.concurrencyHelp") : undefined}><Input /></Form.Item></Col>{dataset !== "agentic" && mode !== "grid" && <Col span={12}><Form.Item name="max_concurrency" label={t("wizard.maxConcurrency")}><InputNumber min={1} max={4096} style={{ width: "100%" }} /></Form.Item></Col>}</Row>}
					{reusesSamples && <Alert style={{ marginBottom: 16 }} type="warning" showIcon message={t("wizard.sampleReuseWarning")} />}
					<Collapse items={[{ key: "advanced", label: t("wizard.advanced"), forceRender: true, children: <>
						{mode === "grid" && <Form.Item name="load_mode" label={<Space size={4}>{t("wizard.loadMode")}<Tooltip title={t("wizard.loadModeHelp")}><QuestionCircleOutlined /></Tooltip></Space>}><Radio.Group optionType="button" options={[{ value: "closed", label: t("wizard.loadModeClosed") }, { value: "open", label: t("wizard.loadModeOpen") }]} /></Form.Item>}
						{openLoop && <Row gutter={12}><Col span={12}><Form.Item name="burstiness" label={<Space size={4}>{t("wizard.burstiness")}<Tooltip title={t("wizard.burstinessHelp")}><QuestionCircleOutlined /></Tooltip></Space>}><InputNumber min={0.01} max={meta.load_defaults.max_burstiness} step={0.1} style={{ width: "100%" }} /></Form.Item></Col><Col span={12}><Form.Item name="max_in_flight" label={<Space size={4}>{t("wizard.maxInFlight")}<Tooltip title={t("wizard.maxInFlightHelp")}><QuestionCircleOutlined /></Tooltip></Space>}><InputNumber min={0} max={meta.load_defaults.max_in_flight} style={{ width: "100%" }} /></Form.Item></Col></Row>}
						<Row gutter={12}><Col span={12}><Form.Item name="warmup_requests" label={<Space size={4}>{t("wizard.warmupRequests")}<Tooltip title={t("wizard.warmupRequestsHelp")}><QuestionCircleOutlined /></Tooltip></Space>}><InputNumber min={0} max={meta.load_defaults.max_warmup_requests} style={{ width: "100%" }} /></Form.Item></Col><Col span={12}><Form.Item name="request_timeout_s" label={<Space size={4}>{t("wizard.requestTimeout")}<Tooltip title={t("wizard.requestTimeoutHelp")}><QuestionCircleOutlined /></Tooltip></Space>}><InputNumber min={meta.load_defaults.min_request_timeout_s} max={meta.load_defaults.max_request_timeout_s} addonAfter="s" style={{ width: "100%" }} /></Form.Item></Col></Row>
						{ignoreEOSOffered && !importedDataset && <Form.Item name="ignore_eos" valuePropName="checked"><Checkbox>{t("wizard.ignoreEOS")} <Tooltip title={t("wizard.ignoreEOSHelp")}><QuestionCircleOutlined /></Tooltip></Checkbox></Form.Item>}
						{dataset === "agentic" && <Row gutter={12}><Col span={12}><Form.Item name="agentic_turns" label={t("agentic.turns")} rules={[{ validator: (_, value) => summarizeAgenticLoad(agenticMeanInput, value, meta).firstPromptTokens > 0 ? Promise.resolve() : Promise.reject(new Error(t("agentic.turnsTooHigh"))) }]}><InputNumber min={2} max={64} style={{ width: "100%" }} /></Form.Item></Col><Col span={12}><Form.Item label={t("agentic.seed")}><InputNumber value={meta.agentic_defaults.seed} disabled style={{ width: "100%" }} /></Form.Item></Col></Row>}
						{dataset !== "agentic" && mode === "saturation" && <Form.Item name="min_throughput_gain" label={t("wizard.minGain")} extra={t("wizard.minGainHelp")}><InputNumber min={.001} max={1} step={.01} addonAfter={t("wizard.ratio")} /></Form.Item>}
						{(mode === "sla" || mode === "grid") && <><Form.Item name="objective" label={t("wizard.objective")}><Select options={meta.objectives.map((item) => ({ ...item, label: localizedText(item.label, language) }))} /></Form.Item><Row gutter={12}>{meta.sla_constraints.map((field) => <Col xs={24} md={12} key={field.key}><Form.Item name={field.key} label={`${t(field.direction === "max" ? "wizard.constraintMax" : "wizard.constraintMin", { label: localizedText(field.label, language) })}${field.unit ? ` (${field.unit})` : ""}`}><InputNumber min={field.min} max={field.max} step={field.step} style={{ width: "100%" }} /></Form.Item></Col>)}</Row></>}
						<Form.Item name="keep_detail" valuePropName="checked"><Checkbox>{t("wizard.keepDetail")}</Checkbox></Form.Item>
						{keepDetail && <Form.Item name="detail_cap_mb" label={t("wizard.detailLimit")}><InputNumber min={meta.detail_cap_mb.min} max={meta.detail_cap_mb.max} addonAfter="MB" /></Form.Item>}
						<TokenizerSourceFields source={tokenizerSource} dataset={dataset} targetKind={targetKind} tokenizerRef={tokenizerRef} onUpload={(file) => { void api.importBenchmarkTokenizer(file).then((result) => { form.setFieldValue("tokenizer_ref", result.tokenizer_ref); message.success(t("wizard.tokenizerUploaded")); }).catch((error) => message.error((error as Error).message)); }} />
					</> }]} />
				</div>
			</Form>
		</Drawer>
	);
}
