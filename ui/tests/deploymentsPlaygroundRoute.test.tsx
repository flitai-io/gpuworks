import assert from "node:assert/strict";
import { listPage, paged, list } from "./helpers/listPage.ts";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLHtmlElement", "NodeFilter", "HTMLInputElement", "HTMLButtonElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "File", "Blob"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: {
    deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")),
    common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
  } },
  defaultNS: "deployments",
  interpolation: { escapeValue: false },
});

const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { act, cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");

class Events {
  static listeners = new Map<string, Set<EventListener>>();
  addEventListener(type: string, listener: EventListener) {
    const listeners = Events.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    Events.listeners.set(type, listeners);
  }
  removeEventListener(type: string, listener: EventListener) { Events.listeners.get(type)?.delete(listener); }
  close() {}
  static emit(type: string) { for (const listener of Events.listeners.get(type) ?? []) listener(new dom.window.Event(type)); }
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
const { containerTerminalURL, default: DeploymentsPage, deploymentCurl, deploymentTerminalInstances } = await import("../src/pages/DeploymentsPage.tsx");
const originalDeployments = api.deploymentsPage;
const originalDeployment = api.deployment;
const originalRoutingPolicies = api.routingPolicies;
const originalDeleteDeployment = api.deleteDeployment;
afterEach(() => { cleanup(); Events.listeners.clear(); api.deploymentsPage = originalDeployments; api.deployment = originalDeployment; api.routingPolicies = originalRoutingPolicies; api.deleteDeployment = originalDeleteDeployment; });
after(() => { closeChannels(); dom.window.close(); });
function Destination() { const location = useLocation(); return <output>{location.pathname}{location.search}</output>; }

test("Deployments open playground at the declared renderer, not chat", async () => {
  for (const [type, renderer, endpoint] of [["openai.audio.speech", "audio", "/v1/audio/speech"], ["openai.chat", "chat", "/v1/chat/completions"], ["openai.completions", "completion", "/v1/completions"]] as const) {
    api.deploymentsPage = async () => listPage([{ id: 1, name: "deployment", model_name: "model", model_id: 1, served_name: "Qwen/model", engine: "vllm", node_uuid: "node", desired_state: "active", runtime_state: "running", sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 1, template_deviated: false, created_at: "2026-09-05T00:00:00Z", operations: [{ type, renderer, endpoint }] }]);
    const view = render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments"]}><Routes><Route path="/deployments" element={<DeploymentsPage />} /><Route path="/playground/*" element={<Destination />} /></Routes></MemoryRouter></I18nextProvider>);
    const button = await screen.findByRole("button", { name: "Open Playground" });
    button.focus();
    await userEvent.setup().keyboard("{Enter}");
    await waitFor(() => assert.equal(screen.getByRole("status").textContent, `/playground/${renderer}?operation=${type}&model=Qwen%2Fmodel`));
    view.unmount();
  }
});

test("deployment terminal actions use live container references", () => {
  const deployment = {
    id: 1, name: "deployment", model_name: "model", model_id: 1, served_name: "model", engine: "vllm",
    node_uuid: "node/one", desired_state: "active", runtime_state: "running", sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 1,
    template_deviated: false, created_at: "2026-09-05T00:00:00Z", operations: [],
    instances: [
      { uuid: "instance-1", deployment_id: 1, node_uuid: "node/one", state: "running", gpu_indexes: [7], ports: [30000], api_port: 30000, image: "image", message: "", container_id: "container/id", sleep_level: 0 },
      { uuid: "instance-2", deployment_id: 1, node_uuid: "node-two", state: "failed", gpu_indexes: [6], ports: [], api_port: 0, image: "image", message: "failed", container_id: "stopped", sleep_level: 0 },
    ],
  } as never;
  const instances = deploymentTerminalInstances(deployment);
  assert.deepEqual(instances.map((instance) => instance.uuid), ["instance-1"]);
  assert.equal(containerTerminalURL(instances[0]), "/nodes/node%2Fone/terminal?target=container&ref=container%2Fid");
});

test("deployment progress events refresh rows without blocking the table", async () => {
  const deployment = { id: 1, name: "pulling-deployment", model_name: "model", model_id: 1, served_name: "model", engine: "vllm", node_uuid: "node", desired_state: "active", runtime_state: "pulling", sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 1, template_deviated: false, created_at: "2026-09-05T00:00:00Z", operations: [] } as const;
  let calls = 0;
  let finishRefresh: ((value: typeof deployment[]) => void) | undefined;
  api.deploymentsPage = paged(async () => {
    calls += 1;
    if (calls === 1) return [deployment];
    return new Promise<typeof deployment[]>((resolve) => { finishRefresh = resolve; });
  });
  const view = render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments"]}><Routes><Route path="/deployments" element={<DeploymentsPage />} /></Routes></MemoryRouter></I18nextProvider>);
  await screen.findByRole("cell", { name: "pulling-deployment" });

  act(() => Events.emit("instance"));
  await waitFor(() => assert.equal(calls, 2));
  assert.equal(view.container.querySelector(".ant-spin-spinning"), null, "background progress refresh must not block the deployment table");
  assert.equal(screen.getByRole("cell", { name: "pulling-deployment" }).textContent, "pulling-deployment");

  await act(async () => { finishRefresh?.([deployment]); });
});

// Settle a refresh loop whose requests all resolve, then report whether it
// asked for anything else. A coalescer may legitimately run one trailing
// refresh; what it must never do is keep asking after the burst ended.
async function settleRefreshes<T>(
  pending: Array<(value: T) => void>,
  resolveWith: T,
  calls: () => number,
): Promise<number> {
  await act(async () => {
    while (pending.length > 0) pending.shift()?.(resolveWith);
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  return calls();
}

test("bursty image progress cannot keep the initial deployment load spinning", async () => {
  const deployment = { id: 1, name: "pulling-deployment", model_name: "model", model_id: 1, served_name: "model", engine: "vllm", node_uuid: "node", desired_state: "active", runtime_state: "pulling", sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 1, template_deviated: false, created_at: "2026-09-05T00:00:00Z", operations: [] } as const;
  const pending: Array<(value: typeof deployment[]) => void> = [];
  let calls = 0;
  api.deploymentsPage = paged(() => {
    calls += 1;
    return new Promise<typeof deployment[]>((resolve) => { pending.push(resolve); });
  });
  const view = render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments"]}><Routes><Route path="/deployments" element={<DeploymentsPage />} /></Routes></MemoryRouter></I18nextProvider>);
  await waitFor(() => assert.equal(calls, 1));

  act(() => {
    Events.emit("deployment");
    Events.emit("instance");
    Events.emit("modelfile");
  });
  await act(async () => { pending.shift()?.([deployment]); });
  await waitFor(() => assert.equal(calls, 2));

  act(() => {
    Events.emit("deployment");
    Events.emit("instance");
    Events.emit("modelfile");
  });
  await act(async () => { pending.shift()?.([deployment]); });
  await screen.findByRole("cell", { name: "pulling-deployment" });
  assert.equal(view.container.querySelector(".ant-spin-spinning"), null, "initial loading must finish while image progress continues");
  // The burst arrived while a refresh was in flight, so it collapsed into that
  // refresh plus at most one trailing one — never one request per event. The
  // exact count is deliberately a bound: the coalescer starts its trailing
  // refresh immediately once its one-second window has already elapsed, so a
  // slow runner legitimately reaches three.
  assert.ok(calls <= 3, `bursty events must add at most one trailing refresh, saw ${calls} requests`);
  const settled = await settleRefreshes(pending, [deployment], () => calls);
  assert.equal(calls, settled, "bursty events must not leave a refresh loop running");
  view.unmount();
});

test("deployment detail coalesces bursty image progress requests", async () => {
  const deployment = { id: 23, name: "pulling-deployment", model_name: "model", model_id: 1, served_name: "model", engine: "vllm", node_uuid: "node", desired_state: "active", runtime_state: "pulling", sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 1, template_deviated: false, created_at: "2026-09-05T00:00:00Z", operations: [], instances: [] } as const;
  const pending: Array<(value: typeof deployment) => void> = [];
  let detailCalls = 0;
  api.deploymentsPage = async () => listPage([deployment]);
  api.deployment = () => {
    detailCalls += 1;
    return new Promise<typeof deployment>((resolve) => { pending.push(resolve); });
  };
  const view = render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments?deployment=23"]}><Routes><Route path="/deployments" element={<DeploymentsPage />} /></Routes></MemoryRouter></I18nextProvider>);
  await waitFor(() => assert.equal(detailCalls, 1));

  act(() => {
    for (let index = 0; index < 20; index++) {
      Events.emit("deployment");
      Events.emit("instance");
      Events.emit("modelfile");
    }
  });
  assert.equal(detailCalls, 1, "events during the initial request must be coalesced");

  await act(async () => { pending.shift()?.(deployment); });
  await waitFor(() => assert.ok(screen.getAllByText("pulling-deployment").length >= 2));
  // Twenty bursts must collapse into the request that was already running plus
  // at most one trailing refresh. Demanding exactly one made this a wall-clock
  // test: whether the trailing refresh starts immediately or after the window
  // depends on how long the runner took to get here.
  assert.ok(detailCalls <= 2, `a burst must collapse into at most one refresh, saw ${detailCalls} requests`);
  const settledDetail = await settleRefreshes(pending, deployment, () => detailCalls);
  assert.equal(detailCalls, settledDetail, "a burst must not leave detail refreshes running");
  view.unmount();
});

test("deployment detail renders server-owned routing policies and muted help", async () => {
  const deployment = {
    id: 44, name: "replicated-deployment", model_name: "model", model_id: 1, served_name: "model", engine: "vllm",
    node_uuid: "node", desired_state: "active", runtime_state: "running", sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 2,
    routing_policy: "prefix_affinity", template_deviated: false, created_at: "2026-09-05T00:00:00Z", operations: [], instances: [],
  } as const;
  api.deploymentsPage = async () => listPage([deployment]);
  api.deployment = async () => deployment;
  api.routingPolicies = async () => list([
    { value: "least_request", label: { en: "Load balance", zh: "负载均衡" }, description: { en: "Lightest load", zh: "负载最轻" } },
    { value: "prefix_affinity", label: { en: "Prefix affinity", zh: "前缀亲和" }, description: { en: "Reuse prefix", zh: "复用前缀" } },
    { value: "failover", label: { en: "Failover", zh: "主备" }, description: { en: "Use backup", zh: "使用备用" } },
  ]);
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments?deployment=44"]}><Routes><Route path="/deployments" element={<DeploymentsPage />} /></Routes></MemoryRouter></I18nextProvider>);
  assert.ok(await screen.findByText("Prefix affinity"));
  const combo = screen.getByLabelText(i18n.t("deployments:detail.routingPolicy"));
  const help = screen.getByRole("img", { name: "Explain routing policies" });
  assert.equal(help.getAttribute("title"), "Explain routing policies");
  assert.notEqual(help.getAttribute("style")?.includes("orange"), true);
  await userEvent.setup().click(combo);
  assert.ok(await screen.findByText("Failover"));
});

test("copied curl carries the API key header the gateway requires", () => {
  assert.equal(
    deploymentCurl("http://gw:8800", { served_name: "", model_name: "Qwen/Qwen3-8B" }),
    `curl 'http://gw:8800/v1/chat/completions' -H 'Content-Type: application/json' -H "Authorization: Bearer <key>" -d '{"model":"Qwen/Qwen3-8B","messages":[{"role":"user","content":"hello"}]}'`,
  );
  assert.match(deploymentCurl("http://gw", { served_name: "api", model_name: "m" }), /"model":"api"/);
});

test("deployment list exposes the copyable served name and deletes from the more menu", async () => {
  const deployment = { id: 7, name: "templated", model_name: "model", model_id: 1, served_name: "public-name", engine: "vllm", node_uuid: "node", node_name: "gpu3", desired_state: "active", runtime_state: "running", sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 1, template_id: "tpl", template_deviated: true, created_at: "2026-09-05T00:00:00Z", operations: [] } as const;
  api.deploymentsPage = async () => listPage([deployment]);
  const deleted: Array<[number, boolean | undefined]> = [];
  api.deleteDeployment = async (id: number, force?: boolean) => { deleted.push([id, force]); };
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments"]}><Routes><Route path="/deployments" element={<DeploymentsPage />} /></Routes></MemoryRouter></I18nextProvider>);
  await screen.findByRole("button", { name: "templated" });
  const headers = screen.getAllByRole("columnheader").map((cell) => cell.textContent);
  assert.deepEqual(headers.filter(Boolean), ["Name", "Served name", "Model", "Engine / Image", "Compute Node", "State", "Health", "Ready / desired replicas", "Created", "Actions"]);
  const servedName = screen.getByText("public-name");
  assert.ok(servedName.closest("td"), "the wire served_name is visible in the list without opening details");
  assert.equal(screen.queryByText(/Inference Template/), null, "the template badge no longer crowds the name");
  assert.equal(screen.queryByRole("button", { name: /Delete/ }), null, "delete sits behind the more menu");

  const user = userEvent.setup();
  // Typography uses the browser's selection/execCommand fallback on HTTP.
  const originalExecCommand = document.execCommand;
  let copied = "";
  document.execCommand = (command) => {
    assert.equal(command, "copy");
    copied = window.getSelection()?.toString() || "";
    return true;
  };
  try {
    await user.click(within(servedName.closest("td")!).getByRole("button", { name: "Copy" }));
    assert.equal(copied, "public-name");
  } finally {
    document.execCommand = originalExecCommand;
  }
  await user.click(screen.getByRole("button", { name: "Copy curl" }));
  assert.match(await navigator.clipboard.readText(), /-H "Authorization: Bearer <key>"/);

  await user.click(screen.getByRole("button", { name: "More Actions" }));
  await user.click(await screen.findByRole("menuitem", { name: /Delete/ }));
  const dialog = await screen.findByRole("dialog", { name: "Delete deployment “templated”" });
  await user.click(within(dialog).getByRole("button", { name: "Delete" }));
  await waitFor(() => assert.deepEqual(deleted, [[7, false]]));
});

test("deployment detail links its model and every node", async () => {
  const deployment = {
    id: 9, name: "multi-node", model_name: "Qwen/Qwen3-235B", model_id: 42, served_name: "qwen", engine: "vllm",
    node_uuid: "uuid-a", node_name: "gpu-a", node_uuids: ["uuid-a", "uuid-b"], node_names: ["gpu-a", "gpu-b"], nnodes: 2,
    desired_state: "active", runtime_state: "running", sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] }, replicas: 1,
    template_deviated: false, created_at: "2026-09-05T00:00:00Z", operations: [], instances: [],
  } as const;
  api.deploymentsPage = async () => listPage([deployment]);
  api.deployment = async () => deployment as never;
  function NodeDestination() { const location = useLocation(); return <output>{location.pathname}:{(location.state as { nodeUUID?: string } | null)?.nodeUUID}</output>; }
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments?deployment=9"]}><Routes><Route path="/deployments" element={<DeploymentsPage />} /><Route path="/models" element={<Destination />} /><Route path="/nodes" element={<NodeDestination />} /></Routes></MemoryRouter></I18nextProvider>);
  const model = await screen.findByRole("link", { name: "Qwen/Qwen3-235B" });
  assert.equal(model.getAttribute("href"), "/models?detail=42");
  assert.ok(screen.getByRole("link", { name: "gpu-a" }));
  await userEvent.setup().click(screen.getByRole("link", { name: "gpu-b" }));
  await waitFor(() => assert.equal(screen.getByRole("status").textContent, "/nodes:uuid-b"));
});

test("a detail opened by URL recovers from a failed read without reopening the drawer", async () => {
  const record = {
    id: 19, name: "retry-service", model_name: "model", model_id: 1, served_name: "public-name",
    engine: "vllm", node_uuid: "", desired_state: "stopped", runtime_state: "stopped", replicas: 1,
    sleepable: false, health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, created_at: "2026-09-05T00:00:00Z", operations: [], instances: [],
  } as const;
  api.deploymentsPage = async () => listPage([record]);
  let unavailable = true;
  api.deployment = async () => {
    if (unavailable) throw new Error("Connection interrupted");
    return record;
  };
  render(<React.StrictMode><I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments?deployment=19"]}><DeploymentsPage /></MemoryRouter></I18nextProvider></React.StrictMode>);
  await screen.findByText("Connection interrupted");
  unavailable = false;
  const retry = screen.getByRole("button", { name: i18n.t("common:actions.retry") });
  retry.focus();
  await userEvent.setup().keyboard("{Enter}");
  const dialog = within(screen.getByRole("dialog"));
  await dialog.findByRole("heading", { name: /retry-service/ });
  assert.equal(dialog.queryByText("Connection interrupted"), null);
  await userEvent.setup().click(dialog.getByRole("tab", { name: i18n.t("detail.instanceDetails") }));
  assert.ok(await dialog.findByText(i18n.t("detail.noInstances")));
});

test("detail actions open the editor or a clone with the correct deployment, including stopped and deleting services", async () => {
  for (const [state, action, destination] of [
    ["active", "page.edit", "/deploy?edit=7"],
    ["stopped", "page.edit", "/deploy?edit=7"],
    ["deleting", "page.cloneModify", "/deploy?from=7"],
  ] as const) {
    const record = {
      id: 7, name: "qwen", model_name: "qwen", model_id: 1, served_name: "qwen", engine: "vllm",
      node_uuid: "", desired_state: state, runtime_state: "stopped", replicas: 1, sleepable: false,
      health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
      template_deviated: false, created_at: "2026-09-05T00:00:00Z", operations: [], instances: [],
    } as const;
    api.deploymentsPage = async () => listPage([record]);
    api.deployment = async () => record;
    const user = userEvent.setup();
    const view = render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/deployments?deployment=7"]}>
      <Routes><Route path="/deployments" element={<DeploymentsPage />} /><Route path="/deploy" element={<Destination />} /></Routes>
    </MemoryRouter></I18nextProvider>);
    const dialog = within(await screen.findByRole("dialog"));
    const more = await dialog.findByRole("button", { name: i18n.t("detailLayout.more") });
    if (state === "stopped") assert.ok(dialog.getByRole("button", { name: i18n.t("detail.start") }));
    more.focus();
    await user.keyboard("{ArrowDown}");
    if (state === "deleting") {
      const edit = await screen.findByRole("menuitem", { name: i18n.t("page.edit") });
      assert.equal(edit.getAttribute("aria-disabled"), "true");
    }
    await user.click(await screen.findByRole("menuitem", { name: i18n.t(action) }));
    await waitFor(() => assert.equal(screen.getByRole("status").textContent, destination));
    view.unmount();
  }
});
