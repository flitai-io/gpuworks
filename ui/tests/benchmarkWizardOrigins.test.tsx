import assert from "node:assert/strict";
import { afterEach, beforeEach, mock, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "HTMLBodyElement", "HTMLInputElement", "HTMLTextAreaElement", "Document", "KeyboardEvent"] as const) {
	Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });

const { cleanup, render, screen, fireEvent, within, waitFor, act } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: BenchmarkWizard } = await import("../src/pages/evaluations/BenchmarkWizard.tsx");
const { MemoryRouter } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { default: EvaluationsPage } = await import("../src/pages/EvaluationsPage.tsx");
const locale = (await import("../src/i18n/locales/en/evaluations.json")).default;
const i18n = createInstance();
await i18n.use(initReactI18next).init({
	lng: "en", defaultNS: "evaluations", interpolation: { escapeValue: false },
	resources: { en: { evaluations: locale } },
});

Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "EventSource", { value: class { addEventListener() {} removeEventListener() {} close() {} }, configurable: true });

type BenchmarkMeta = import("../src/api.ts").BenchmarkMeta;
type Deployment = import("../src/api.ts").Deployment;
type Instance = import("../src/api.ts").Instance;
type ComputeNode = import("../src/api.ts").Node;
type WizardPreset = import("../src/pages/evaluations/BenchmarkWizard.tsx").WizardPreset;
const meta: BenchmarkMeta = {
	modes: [
		{ value: "saturation", label: { en: "Throughput Limit Probe" } },
		{ value: "sla", label: { en: "SLA Maximum Concurrency" } },
		{ value: "grid", label: { en: "Parameter Sweep" } },
	],
	mode_defaults: {
		saturation: { concurrency: 1, max_concurrency: 32, isl: [4096], osl: [1024], requests_per_point_multiplier: 10, min_throughput_gain: .05 },
		sla: { concurrency: 1, max_concurrency: 32, isl: [4096], osl: [1024], requests_per_point_multiplier: 10, objective: "rps" },
		grid: { concurrency: [1, 2, 4], isl: [4096], osl: [1024], requests_per_point_multiplier: 10 },
	},
	datasets: [{ value: "random", label: { en: "Fixed-Length Random" } }], dataset_profiles: {},
	agentic_presets: [],
	agentic_defaults: { preset: "agentic-80k-v1", turns: 13, subsequent_turn_input_tokens: 753, output_tokens: 220, seed: 42, concurrency: [1, 2, 4, 8], sessions_per_point: { "1": 4, "2": 8, "4": 8, "8": 16 }, sessions_per_concurrency: 2, min_sessions_per_point: 4 },
	sla_constraints: [], objectives: [], statuses: [], tokenizer_schemes: {},
	detail_cap_mb: { default: 64, min: 1, max: 512, unit: "MB" },
	load_defaults: { warmup_requests: 1, max_warmup_requests: 1000, request_timeout_s: 600, min_request_timeout_s: 10, max_request_timeout_s: 7200, burstiness: 1, point_duration_s: 60, min_point_duration_s: 5, max_point_duration_s: 3600, min_open_loop_requests: 20, max_request_rate: 10000, max_request_rates: 32, max_burstiness: 100, max_in_flight: 100000, ignore_eos: true, ignore_eos_datasets: ["random", "agentic"] },
};
const deployment = { id: 7, name: "Test service", node_uuid: "historical", desired_state: "active", operations: [{ type: "openai.chat", endpoint: "/v1/chat/completions", renderer: "chat" }] } as Deployment;
const node = (uuid: string, extra: Partial<ComputeNode> = {}) => ({ uuid, name: uuid, hostname: uuid, state: "online", caps: ["bench"], ...extra } as ComputeNode);
const instance = (node_uuid: string, extra: Partial<Instance> = {}) => ({ uuid: `instance-${node_uuid}`, deployment_id: 7, node_uuid, state: "running", sleep_level: 0, node_rank: 0, api_port: 8000, ...extra } as Instance);
let details: Deployment;
let detailCalls: number[];
let nodeCalls: string[];
beforeEach(() => {
	details = { ...deployment, instances: [instance("leader")] };
	detailCalls = [];
	nodeCalls = [];
	mock.method(api, "deployment", async (id: number) => { detailCalls.push(id); return details; });
	mock.method(api, "node", async (uuid: string) => { nodeCalls.push(uuid); return node(uuid); });
});
afterEach(() => { cleanup(); mock.restoreAll(); });
const props = (overrides: Partial<React.ComponentProps<typeof BenchmarkWizard>> = {}) => ({ open: true, meta, deployments: [deployment], nodes: [node("historical"), node("leader"), node("unrelated")], preset: { deploymentID: 7 }, onClose() {}, onCreated() {}, ...overrides });
const wizard = (overrides: Partial<React.ComponentProps<typeof BenchmarkWizard>> = {}) => <I18nextProvider i18n={i18n}><BenchmarkWizard {...props(overrides)} /></I18nextProvider>;
async function originChoices() {
	const user = userEvent.setup();
	await user.click(await screen.findByRole("combobox", { name: locale.wizard.loadOrigin }));
	return user;
}

test("managed origin uses actual running leaders instead of historical or unrelated nodes", async () => {
	render(wizard());
	const user = await originChoices();
	await screen.findByText("Target node · leader");
	assert.equal(screen.queryByText("Target node · historical"), null);
	assert.equal(screen.queryByText("Compute node · unrelated"), null);
	// Rendering on a busy runner may span a legitimate refresh interval.
	assert.ok(detailCalls.length > 0);
	assert.deepEqual([...new Set(detailCalls)], [7], "only the selected deployment is fetched");
	await user.keyboard("{ArrowDown}{Enter}");
	assert.ok(screen.getByText("Target node · leader"));
});

test("only awake serving leaders qualify, including replicas and the PD decode role", async () => {
	details.instances = [instance("leader"), instance("replica"), instance("stopped", { state: "stopped" }), instance("sleeping", { sleep_level: 1 }), instance("sleep-ordered", { desired_sleep: 1 }), instance("follower", { dist_role: "follower", node_rank: 1, headless: true }), instance("headless", { headless: true }), instance("rank-one", { node_rank: 1 }), instance("no-port", { api_port: 0 }), instance("prefill", { role: "prefill" }), instance("decode", { role: "decode" })];
	const nodes = details.instances.map((item) => node(item.node_uuid));
	render(wizard({ nodes }));
	await originChoices();
	await screen.findByText("Target node · decode");
	assert.ok(screen.getByText("Target node · leader"));
	assert.ok(screen.getByText("Target node · replica"));
	for (const uuid of ["stopped", "sleeping", "sleep-ordered", "follower", "headless", "rank-one", "no-port", "prefill"]) assert.equal(screen.queryByText(`Target node · ${uuid}`), null);
});

test("missing leader metadata is fetched only for eligible runtime nodes", async () => {
	render(wizard({ nodes: [node("historical")] }));
	await originChoices();
	await screen.findByText("Target node · leader");
	assert.ok(nodeCalls.length > 0);
	assert.deepEqual([...new Set(nodeCalls)], ["leader"], "refreshes only look up eligible runtime nodes");
});

test("managed origins wait for selected detail and safely fall back after a load error", async () => {
	let reject!: (reason: Error) => void;
	mock.method(api, "deployment", () => new Promise<Deployment>((_, fail) => { reject = fail; }));
	const sourceRun = { id: 1, name: "Prior", mode: "grid", target_kind: "instance", deployment_id: 7, load_origin: "leader", dataset: { kind: "random" }, search: {} } as import("../src/api.ts").BenchmarkRun;
	render(wizard({ preset: { sourceRun } }));
	await originChoices();
	assert.equal(screen.queryByText("Target node · leader"), null);
	reject(new Error("offline"));
	await screen.findByText(locale.wizard.originLoadFailed);
	await screen.findAllByText("Server");
	assert.equal(screen.queryByText("Target node · leader"), null);
});

test("a stale deployment response cannot replace the newly selected target", async () => {
	let resolveFirst!: (value: Deployment) => void;
	mock.method(api, "deployment", (id: number) => id === 7 ? new Promise<Deployment>((resolve) => { resolveFirst = resolve; }) : Promise.resolve({ ...deployment, id, name: "Second service", instances: [instance("second")] }));
	const second = { ...deployment, id: 8, name: "Second service" };
	const view = render(wizard({ deployments: [deployment, second], nodes: [node("leader"), node("second")] }));
	await screen.findByRole("combobox", { name: locale.wizard.service });
	view.rerender(wizard({ deployments: [deployment, second], nodes: [node("leader"), node("second")], preset: { deploymentID: 8 } }));
	const user = await originChoices();
	await screen.findByText("Target node · second");
	resolveFirst(details);
	await user.keyboard("{Escape}");
	await originChoices();
	assert.equal(screen.queryByText("Target node · leader"), null);
	assert.ok(screen.getByText("Target node · second"));
});

test("an instance update resets an origin that no longer has a running leader", async () => {
	const view = render(wizard());
	const user = await originChoices();
	await user.click(await screen.findByText("Target node · leader"));
	details = { ...deployment, instances: [instance("leader", { state: "stopped" })] };
	view.rerender(wizard({ deployments: [{ ...deployment, runtime_state: "stopped" }] }));
	await screen.findAllByText("Server");
	await originChoices();
	assert.equal(screen.queryByText("Target node · leader"), null);
});

test("external targets retain any online benchmark-capable node origin", async () => {
	const sourceRun = { id: 2, name: "External", mode: "grid", target_kind: "external", load_origin: "unrelated", dataset: { kind: "random" }, search: {} } as import("../src/api.ts").BenchmarkRun;
	render(wizard({ preset: { sourceRun }, nodes: [node("unrelated"), node("offline", { state: "offline" }), node("no-bench", { caps: [] })] }));
	await originChoices();
	assert.ok(screen.getAllByText("Compute node · unrelated").length);
	assert.equal(screen.queryByText("Compute node · offline"), null);
	assert.ok(screen.getByText("Compute node · no-bench").closest(".ant-select-item-option-disabled"));
	assert.deepEqual(detailCalls, []);
});

test("a valid rerun node stays selected while missing node metadata loads", async () => {
	let resolveNode!: (value: ComputeNode) => void;
	mock.method(api, "node", () => new Promise<ComputeNode>((resolve) => { resolveNode = resolve; }));
	const sourceRun = { id: 3, name: "Prior", mode: "grid", target_kind: "instance", deployment_id: 7, load_origin: "leader", dataset: { kind: "random" }, search: {} } as import("../src/api.ts").BenchmarkRun;
	render(wizard({ preset: { sourceRun }, nodes: [] }));
	await waitFor(() => assert.ok(resolveNode));
	const origin = screen.getByRole("combobox", { name: locale.wizard.loadOrigin }).closest(".ant-select")!;
	assert.match(origin.textContent || "", /leader/);
	resolveNode(node("leader"));
	await screen.findByText("Target node · leader");
	assert.match(origin.textContent || "", /Target node · leader/);
});

test("selected-origin polling makes one detail request per tick and stops when closed", async () => {
	const realSetInterval = globalThis.setInterval;
	let refresh!: () => void;
	let pollTimer!: ReturnType<typeof setInterval>;
	mock.method(globalThis, "setInterval", (callback: () => void, delay?: number, ...args: unknown[]) => {
		if (delay !== 10000) return realSetInterval(callback, delay, ...args);
		refresh = callback;
		// Control just the wizard's polling clock, leaving RTL and React timers real.
		pollTimer = realSetInterval(() => {}, 2 ** 30);
		return pollTimer;
	});
	const clearIntervalSpy = mock.method(globalThis, "clearInterval");
	const deployments = [deployment, { ...deployment, id: 8, name: "Other service" }];
	const nodes = [node("historical"), node("unrelated")];
	const view = render(wizard({ deployments, nodes }));
	const user = await originChoices();
	await user.click(await screen.findByText("Target node · leader"));
	assert.deepEqual(detailCalls, [7], "one initial selected-target request");
	assert.deepEqual(nodeCalls, ["leader"], "one initial eligible-node lookup");

	details = { ...deployment, instances: [instance("leader", { state: "stopped" })] };
	await act(async () => { refresh(); });
	const origin = screen.getByRole("combobox", { name: locale.wizard.loadOrigin }).closest(".ant-select")!;
	await waitFor(() => assert.match(origin.textContent || "", /Server/));
	assert.deepEqual(detailCalls, [7, 7], "one selected-target request on the first tick");
	assert.deepEqual(nodeCalls, ["leader"], "a stopped instance triggers no node lookup");

	details = { ...deployment, instances: [instance("leader")] };
	await act(async () => { refresh(); });
	await originChoices();
	await screen.findByText("Target node · leader");
	assert.deepEqual(detailCalls, [7, 7, 7], "one selected-target request on the second tick");
	assert.deepEqual(nodeCalls, ["leader", "leader"], "the restored leader refreshes only its own node");

	view.rerender(wizard({ deployments, nodes, open: false }));
	assert.ok(clearIntervalSpy.mock.calls.some((call) => call.arguments[0] === pollTimer), "closing clears the polling timer");
	assert.deepEqual(detailCalls, [7, 7, 7], "closing does not fetch another deployment");
});

test("a request priority applies from the server only: a node origin calls the instance directly", async () => {
	mock.method(api, "priorityClasses", async () => ({
		min_value: -100, max_value: 100, policies: [], default_policy: "", ceiling_policies: [],
		items: [{ name: "normal", display_name: "", description: "", value: 0, builtin: true, default: true, deletable: false, created_at: "" }],
	}));
	render(wizard());
	const priority = await screen.findByRole("combobox", { name: locale.wizard.priorityClass });
	const disabled = () => !!priority.closest(".ant-select")?.classList.contains("ant-select-disabled");
	await waitFor(() => assert.equal(disabled(), false));
	screen.getByText(locale.wizard.priorityClassHelp);
	const user = await originChoices();
	await screen.findByText("Target node · leader");
	await user.click(screen.getAllByText("Target node · leader").at(-1)!);
	await screen.findByText(locale.wizard.priorityClassGateway);
	assert.equal(disabled(), true);
});
