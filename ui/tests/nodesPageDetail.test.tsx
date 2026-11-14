import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/nodes" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLHtmlElement", "HTMLInputElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "Document"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
// React's legacy input-event polyfill reaches for IE's attachEvent when the
// label filter's search input gains focus under JSDOM.
Object.defineProperties(dom.window.HTMLElement.prototype, {
  attachEvent: { configurable: true, value() {} },
  detachEvent: { configurable: true, value() {} },
});

// echarts needs a 2d context JSDOM cannot provide; the same catch-all proxy
// the monitor-page tests use keeps chart mounting alive here too.
const anyContext = new Proxy(function () {} as never, {
  get: (_target: never, prop: string | symbol) => {
    if (prop === "canvas") return { width: 300, height: 200, style: {} };
    if (prop === "dpr") return 1;
    if (prop === "measureText") return () => ({ width: 10 });
    return anyContext;
  },
  set: () => true,
  apply: () => anyContext,
});
dom.window.HTMLCanvasElement.prototype.getContext = () => anyContext as CanvasRenderingContext2D;

class Events {
  static listeners = new Map<string, Set<EventListener>>();
  addEventListener(type: string, listener: EventListener) {
    const listeners = Events.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    Events.listeners.set(type, listeners);
  }
  removeEventListener(type: string, listener: EventListener) { Events.listeners.get(type)?.delete(listener); }
  close() {}
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

// rc-overflow (antd Menu's overflow measurement) creates a MessageChannel per
// update and never closes the ports; under node that port pins the event loop
// and the test process never exits. Shim it with timer-based delivery that
// leaves no live handle behind.
class ShimPort {
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  postMessage(data: unknown) { setTimeout(() => this.onmessage?.({ data }), 0); }
}
class ShimMessageChannel { port1 = new ShimPort(); port2 = new ShimPort(); }
Object.defineProperty(globalThis, "MessageChannel", { value: ShimMessageChannel, configurable: true });

const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: {
      nodes: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/nodes.json", import.meta.url), "utf8")),
      common: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8")),
    },
  },
  defaultNS: "nodes",
  interpolation: { escapeValue: false },
});

const { MemoryRouter } = await import("react-router-dom");
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { message } = await import("antd");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { default: requestI18n } = await import("../src/i18n/index.ts");
requestI18n.addResourceBundle("en", "common", i18n.getResourceBundle("en", "common"), true, true);
await requestI18n.changeLanguage("en");
const originalFetch = globalThis.fetch;
const { SessionUserContext } = await import("../src/components/sessionUser.ts");
const { default: NodesPage } = await import("../src/pages/NodesPage.tsx");
const { DETAIL_DRAWER_WIDTH } = await import("../src/components/detail/DetailDrawer.tsx");
const { runtimeLabel } = await import("../src/nodeDisplay.ts");
import type { ListQuery, Node, NodePage } from "../src/api";
import { list } from "./helpers/listPage.ts";

const onlineNode: Node = {
  uuid: "11111111-1111-1111-1111-111111111111",
  state: "online",
  hostname: "192.168.10.11",
  name: "gpu-a100-node-01",
  os: "linux", kernel: "5.15.0", arch: "amd64", cpu_model: "EPYC 7543",
  cpu_cores: 128, mem_total_gib: 1024, disk_total_gib: 2048, gpu_count: 8,
  agent_version: "0.9.7", has_ssh_credential: true, gpus: [],
  // What the server offers a plain Linux node (the runtime selector renders exactly these).
  runtime_choices: ["auto", "docker", "nerdctl", "udocker"],
};

const offlineNode: Node = {
  ...onlineNode,
  uuid: "22222222-2222-2222-2222-222222222222",
  state: "offline",
  hostname: "192.0.2.129",
  name: "k8s-3-910c-host",
  agent_version: undefined,
  last_reject_reason: "bad_token",
  last_reject_at: "2026-09-20T10:00:00Z",
};

function gpuInfo(index: number): Node["gpus"][number] {
  return {
    index,
    uuid: `GPU-FAKE-${index}`,
    name: "A100-SXM4-80GB",
    compute_capability: "8.0",
    arch_family: "ampere",
    vendor: "nvidia",
    memory_total_bytes: 80 << 30,
    numa_node: 0,
    schedulable: true,
  };
}

const originals = {
  me: api.me,
  agentUpgradeRuns: api.agentUpgradeRuns,
  agentUpgradePreview: api.agentUpgradePreview,
  createAgentUpgrade: api.createAgentUpgrade,
  nodes: api.nodes,
  nodesPage: api.nodesPage,
  health: api.health,
  agentRejects: api.agentRejects,
  agentUpgradeErrors: api.agentUpgradeErrors,
  engines: api.engines,
  deleteNode: api.deleteNode,
  node: api.node,
  storageUsage: api.storageUsage,
  storages: api.storages,
  kubeClusters: api.kubeClusters,
  metricsQuery: api.metricsQuery,
  nodeDoctor: api.nodeDoctor,
};

let pageQueries: ListQuery[] = [];

// nodesServer answers GET /nodes?page=… the way the server does: search over
// name, hostname and uuid; label values of one key are alternatives and keys
// must all match; facets and "all" describe the whole fleet.
function nodesServer(nodes: Node[]) {
  return async (query: ListQuery): Promise<NodePage> => {
    pageQueries.push(query);
    const q = query.q?.trim().toLowerCase() ?? "";
    const wanted = new Map<string, Set<string>>();
    for (const pair of query.label ?? []) {
      const at = pair.indexOf("=");
      const key = pair.slice(0, at);
      wanted.set(key, (wanted.get(key) ?? new Set()).add(pair.slice(at + 1)));
    }
    const matched = nodes.filter((node) =>
      (!q || [node.name ?? "", node.hostname, node.uuid].some((name) => name.toLowerCase().includes(q))) &&
      [...wanted].every(([key, values]) => Object.prototype.hasOwnProperty.call(node.labels ?? {}, key) && values.has(node.labels![key])));
    const facets = new Map<string, Set<string>>();
    for (const node of nodes) for (const [key, value] of Object.entries(node.labels ?? {})) facets.set(key, (facets.get(key) ?? new Set()).add(value));
    const start = (query.page - 1) * query.page_size;
    return {
      agent_fleet: { target: "0.9.7", align: 0, check: 0 },
      items: matched.slice(start, start + query.page_size),
      page: query.page, page_size: query.page_size, total: matched.length, all: nodes.length,
      labels: [...facets].sort(([a], [b]) => a.localeCompare(b)).map(([key, values]) => ({ key, values: [...values].sort() })),
    };
  };
}

function stubApi(nodes: Node[]) {
  pageQueries = [];
  api.me = async () => ({ role: "admin" }) as never;
  api.agentUpgradeRuns = async () => ({ items: [], total: 0, page: 1, page_size: 20 });
  api.nodes = async () => { throw new Error("the node page fetched every node"); };
  api.nodesPage = nodesServer(nodes);
  api.node = async (uuid: string) => {
    const node = nodes.find((item) => item.uuid === uuid);
    if (!node) throw new Error("node not found");
    return node;
  };
  api.health = async () => ({ version: "0.9.7" }) as never;
  api.agentRejects = async () => list([{
    token_prefix: "891e71d7",
    reason: "bad_token",
    remote: "192.0.2.230:41374",
    at: "2026-09-28T08:19:00Z",
  }]);
  api.agentUpgradeErrors = async () => ({});
  api.engines = async () => list([]);
}

afterEach(async () => {
  await act(async () => {
    cleanup();
    message.destroy();
  });
  Object.assign(api, originals);
  globalThis.fetch = originalFetch;
});

function renderPage(userID?: number) {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/nodes"]}>
        <SessionUserContext.Provider value={userID === undefined ? undefined : { id: userID, username: "preview", role: "admin" }}>
          <NodesPage />
        </SessionUserContext.Provider>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

function rowOf(text: string) {
  const cell = screen.getByText(text);
  const row = cell.closest("tr");
  assert.ok(row, `table row containing ${text}`);
  return within(row);
}

// The delete dialog is a controlled Modal in the page tree (not the static
// Modal.confirm, whose own React root keeps handles alive under jsdom).
function deleteModal(): HTMLElement {
  const title = Array.from(document.querySelectorAll(".ant-modal-title")).find((el) =>
    el.textContent?.includes("Delete this node?"),
  );
  assert.ok(title, "delete confirm modal rendered");
  return title.closest(".ant-modal") as HTMLElement;
}

function deleteModalButtons(): HTMLButtonElement[] {
  return Array.from(deleteModal().querySelectorAll(".ant-modal-footer button")) as HTMLButtonElement[];
}

test("a Mac's host driver label is shown as is", () => {
  assert.equal(runtimeLabel("host"), "host");
});

test("a Kubernetes node has no runtime selector, only the runtime it runs", async () => {
  const kube = { ...onlineNode, name: "kube-gpu-1", runtime_driver: "kube", runtime_choices: undefined, kube: { cluster_id: 1, node_name: "gpu-1", unschedulable: false } };
  stubApi([kube]);
  api.node = async () => kube as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("kube-gpu-1")));
  screen.getByRole("button", { name: "kube-gpu-1" }).click();
  await waitFor(() => assert.ok(screen.queryByRole("dialog")));
  const drawer = within(screen.queryByRole("dialog") as HTMLElement);
  await waitFor(() => assert.ok(drawer.getByText("kube")));
  assert.equal(screen.queryByRole("dialog")?.querySelector(".ant-select-selection-item")?.textContent ?? null, null);
});

test("the runtime selector of a Mac node offers only the automatic choice", async () => {
  const mac = { ...onlineNode, os: "darwin", arch: "arm64", name: "mac-studio-01", runtime_choices: ["auto"] };
  stubApi([mac]);
  api.node = async () => mac as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("mac-studio-01")));
  screen.getByRole("button", { name: "mac-studio-01" }).click();
  await waitFor(() => assert.ok(screen.queryByRole("dialog")?.querySelector(".ant-select")));
  const optionTexts = async () => {
    const selector = screen.queryByRole("dialog")?.querySelector(".ant-select-selector") as HTMLElement;
    fireEvent.mouseDown(selector);
    await waitFor(() => assert.ok(document.querySelector(".ant-select-dropdown .ant-select-item-option")));
    return [...document.querySelectorAll(".ant-select-dropdown .ant-select-item-option")].map((option) => option.textContent);
  };
  assert.deepEqual(await optionTexts(), ["auto"]);
});

test("the udocker driver is shown as runveil while its wire value stays udocker", async () => {
  assert.equal(runtimeLabel("udocker"), "runveil");
  for (const other of ["auto", "docker", "nerdctl"]) assert.equal(runtimeLabel(other), other);

  const node = { ...onlineNode, runtime_driver: "udocker" };
  stubApi([node]);
  api.node = async (uuid: string) => ({ ...node, uuid, gpus: [] }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  screen.getByRole("button", { name: "gpu-a100-node-01" }).click();
  await waitFor(() => {
    const selected = screen.queryByRole("dialog")?.querySelector(".ant-select-selection-item");
    assert.equal(selected?.textContent, "runveil");
  });
  assert.equal(screen.queryByRole("dialog")?.textContent?.includes("udocker"), false);
});

test("a node on its runveil fallback says which runtime it runs and why Docker was passed over", async () => {
  const node = {
    ...onlineNode, gpu_vendor: "nvidia", runtime_preference: "auto", runtime_driver: "udocker",
    runtime_fallback_from: "docker", runtime_fallback_reason: "gpu_plugin_missing", runtime_fallback_plugin: "NVIDIA Container Toolkit",
  };
  stubApi([node]);
  api.node = async (uuid: string) => ({ ...node, uuid, gpus: [] }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  screen.getByRole("button", { name: "gpu-a100-node-01" }).click();
  await waitFor(() => {
    const selected = screen.queryByRole("dialog")?.querySelector(".ant-select-selection-item");
    assert.equal(selected?.textContent, "auto (now runveil)");
  });
  const drawer = within(screen.queryByRole("dialog") as HTMLElement);
  assert.ok(drawer.getAllByRole("alert").some((alert) =>
    /docker is installed but NVIDIA Container Toolkit is not, so this node uses runveil/.test(alert.textContent ?? "")));
  assert.ok(drawer.getByRole("button", { name: /Copy install command/ }));
});

// The server decides the node is soft-isolated and writes the commands; the
// page words the label and why, and shows and copies the commands as received.
test("a soft-isolated node says so in the detail and offers the server's commands to copy", async () => {
  const append = `kubectl patch clusterpolicy "$(kubectl get clusterpolicy -o jsonpath='{.items[0].metadata.name}')" --type=json -p '[{"op":"add","path":"/spec/toolkit/env/-","value":{"name":"NVIDIA_CONTAINER_RUNTIME_MODE","value":"legacy"}}]'`;
  const hostCommands = ["sudo nvidia-ctk config --in-place --set nvidia-container-runtime.mode=legacy", "sudo systemctl restart containerd"];
  const node = {
    ...onlineNode, gpu_vendor: "nvidia", runtime_driver: "kube", runtime_choices: undefined,
    gpu_isolation: {
      level: "soft", reason: "cdi_names_missing", cards: 2,
      fixes: [
        { code: "gpu_operator_toolkit_legacy_append", commands: [append] },
        { code: "host_toolkit_legacy", commands: hostCommands },
      ],
    },
  };
  stubApi([node]);
  api.node = async (uuid: string) => ({ ...node, uuid, gpus: [] }) as never;
  let copied = "";
  Object.defineProperty(globalThis.navigator, "clipboard", { value: { writeText: async (text: string) => { copied = text; } }, configurable: true });
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  screen.getByRole("button", { name: "gpu-a100-node-01" }).click();
  const drawer = within(await screen.findByRole("dialog"));
  // The label beside the status, and the explanation with its reason.
  await waitFor(() => assert.ok(drawer.getAllByText("GPU devices are soft-isolated").length >= 2));
  const alert = drawer.getAllByRole("alert").find((item) => /soft-isolated/.test(item.textContent ?? ""));
  assert.match(alert?.textContent ?? "", /No CDI spec on the node names its 2 NVIDIA cards/);
  // Each fix shows its commands exactly as the server wrote them, and copies them.
  const fixes = (await drawer.findAllByLabelText("gpu_operator_toolkit_legacy_append")) as HTMLElement[];
  assert.equal(fixes[0].textContent, append);
  assert.equal(drawer.getByLabelText("host_toolkit_legacy").textContent, hostCommands.join("\n"));
  const copyButtons = drawer.getAllByRole("button", { name: "Copy commands" });
  assert.equal(copyButtons.length, 2);
  fireEvent.click(copyButtons[1]);
  await waitFor(() => assert.equal(copied, hostCommands.join("\n")));
  fireEvent.click(copyButtons[0]);
  await waitFor(() => assert.equal(copied, append));
});

test("a strictly isolated node shows no soft-isolation label", async () => {
  stubApi([onlineNode]);
  api.node = async (uuid: string) => ({ ...onlineNode, uuid, gpus: [] }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  screen.getByRole("button", { name: "gpu-a100-node-01" }).click();
  const drawer = within(await screen.findByRole("dialog"));
  await waitFor(() => assert.ok(drawer.getByText(onlineNode.uuid)));
  assert.equal(drawer.queryByText("GPU devices are soft-isolated"), null);
  assert.equal(drawer.queryByRole("button", { name: "Copy commands" }), null);
});

test("the detail drawer shows the node's disk usage and the paths it measures", async () => {
  stubApi([onlineNode]);
  api.node = async (uuid: string) => ({ ...onlineNode, uuid, path_usage: [{ kind: "docker", path: "/var/lib/docker", total_bytes: 200 * 1024 ** 3, available_bytes: 20 * 1024 ** 3 }] }) as never;
  api.storages = async () => list([]);
  api.storageUsage = async () => list([{
    node_uuid: onlineNode.uuid, hostname: onlineNode.hostname, capacity_known: true, total_bytes: 100 * 1024 ** 3, available_bytes: 30 * 1024 ** 3, used_bytes: 70 * 1024 ** 3,
    model_bytes: 40 * 1024 ** 3, image_bytes: 10 * 1024 ** 3, other_bytes: 20 * 1024 ** 3,
  }]);
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  screen.getByRole("button", { name: "gpu-a100-node-01" }).click();
  assert.ok(await screen.findByText("Managed models 40.0 GiB · images 10.0 GiB · other 20.0 GiB · available 30.0 GiB"));
  assert.ok(await screen.findByText("/var/lib/docker"));
  assert.ok(screen.getByText("Docker"));
});

test("the detail drawer offers the accelerator choice only on a host with cards of more than one vendor", async () => {
  const mixed = {
    ...onlineNode, gpu_vendor: "ascend",
    accelerator_vendors: [{ vendor: "ascend", cards: 1, model: "310P3" }, { vendor: "nvidia", cards: 1, model: "AD102 [GeForce RTX 4090]" }],
  };
  stubApi([mixed]);
  api.node = async (uuid: string) => ({ ...mixed, uuid, gpus: [] }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  screen.getByRole("button", { name: "gpu-a100-node-01" }).click();
  await waitFor(() => assert.ok(screen.queryByRole("dialog")?.textContent?.includes("Runtime plugin")));
  const drawer = within(screen.queryByRole("dialog") as HTMLElement);
  // The accelerator choice lives with the cards it chooses between.
  assert.ok(drawer.queryByRole("combobox", { name: "Accelerator" }) === null, "Expected the element to be absent");
  fireEvent.click(drawer.getByRole("tab", { name: /GPUs/ }));
  await waitFor(() => assert.ok(drawer.getByRole("combobox", { name: "Accelerator" })));
  assert.ok(drawer.getAllByRole("alert").some((alert) => /This machine also has NVIDIA ×1/.test(alert.textContent ?? "")));
});

test("a node whose host has one vendor's cards shows no accelerator choice", async () => {
  const single = { ...onlineNode, gpu_vendor: "nvidia", accelerator_vendors: [{ vendor: "nvidia", cards: 8, model: "A100" }] };
  stubApi([single]);
  api.node = async (uuid: string) => ({ ...single, uuid, gpus: [] }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  screen.getByRole("button", { name: "gpu-a100-node-01" }).click();
  await waitFor(() => assert.ok(screen.queryByRole("dialog")?.textContent?.includes("Runtime plugin")));
  const drawer = screen.queryByRole("dialog") as HTMLElement;
  const labels = [...drawer.querySelectorAll(".ant-descriptions-item-label")].map((el) => el.textContent);
  assert.ok(labels.includes("Runtime"), `labels: ${labels.join(", ")}`);
  fireEvent.click(within(drawer).getByRole("tab", { name: /GPUs/ }));
  await waitFor(() => assert.ok(within(drawer).getByText("Summary")));
  const gpuLabels = [...drawer.querySelectorAll(".ant-descriptions-item-label")].map((el) => el.textContent);
  assert.equal(gpuLabels.includes("Accelerator"), false, `labels: ${gpuLabels.join(", ")}`);
  assert.ok(within(drawer).queryByRole("combobox", { name: "Accelerator" }) === null, "Expected the element to be absent");
});

async function openDrawer(node: Node) {
  stubApi([node]);
  api.node = async (uuid: string) => ({ ...node, uuid }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText(node.name!)));
  screen.getByRole("button", { name: node.name! }).click();
  await waitFor(() => assert.ok(screen.getByText(node.uuid)));
  return within(screen.queryByRole("dialog") as HTMLElement);
}

test("the detail drawer opens on the overview, in sections, and is a little wider than before but never wider than the window", async () => {
  const drawer = await openDrawer({ ...onlineNode, gpus: [gpuInfo(0)] });
  assert.equal(drawer.getByRole("tab", { name: "Overview" }).getAttribute("aria-selected"), "true");
  for (const section of ["Node", "Runtime", "Storage", "Network"]) assert.ok(drawer.getByRole("heading", { name: section }), section);
  // The GPU tab carries its card count apart from its label.
  const gpuTab = drawer.getByRole("tab", { name: /GPUs/ });
  assert.match(gpuTab.textContent ?? "", /GPUs\s*1$/);
  // JSDOM drops a min() width from inline styles, so the bound is checked
  // where the drawer takes it from.
  assert.equal(DETAIL_DRAWER_WIDTH, "min(720px, calc(100vw - 32px))");
});

test("the GPU tab leads each card with its model, memory and live readings, and keeps the rest in its expanded row", async () => {
  api.metricsQuery = (async (metric: string) => ({
    items: [{ labels: { gpu: "0" }, points: [[Date.now(), metric === "gpuworks_gpu_utilization" ? 63 : metric === "gpuworks_gpu_memory_used_bytes" ? 20 * 1024 ** 3 : metric === "gpuworks_gpu_temperature_celsius" ? 61 : 280]] }],
  })) as never;
  const drawer = await openDrawer({ ...onlineNode, gpus: [{ ...gpuInfo(0), memory_total_bytes: 80 * 1024 ** 3, pcie_bdf: "0000:8a:00.0", numa_node: 1 }] });
  fireEvent.click(drawer.getByRole("tab", { name: /GPUs/ }));
  const table = (await waitFor(() => document.querySelector(".node-gpu-table") as HTMLElement));
  const headers = [...table.querySelectorAll("thead th")].map((th) => th.textContent).filter(Boolean);
  assert.deepEqual(headers.slice(0, 5), ["#", "Model", "VRAM", "Util.", "Temp."]);
  assert.equal(table.querySelector("thead span[title=\"Temperature\"]")?.textContent, "Temp.", "the full name on hover");
  await waitFor(() => assert.ok(within(table).getByText("63%")));
  assert.ok(within(table).getByText("20.0 / 80 GiB"));
  assert.ok(within(table).getByText("61°C"));
  // PCIe address and NUMA node wait in the expanded row.
  assert.ok(within(table).queryByText("0000:8a:00.0") === null, "Expected the element to be absent");
  const expand = within(table).getByRole("button", { name: "Show details" });
  fireEvent.click(expand);
  await waitFor(() => assert.ok(within(table).getByText("0000:8a:00.0")));
  assert.ok(within(table).getByText("GPU-FAKE-0"));
  assert.equal(within(table).getByRole("button", { name: "Hide details" }).getAttribute("aria-expanded"), "true");
});

test("a capability every card reports alike is listed once, with how many cards share it", async () => {
  const capability = (card: number, status: "ready" | "unavailable") => ({ backend: "ascend-dgpu", display_name: "Ascend dGPU", maturity: "experimental", status, reason: `probe failed: card ${card}` });
  const drawer = await openDrawer({ ...onlineNode, gpus: [gpuInfo(0)], partition_capabilities: [capability(0, "unavailable"), capability(1, "unavailable"), capability(2, "ready")] } as Node);
  fireEvent.click(drawer.getByRole("tab", { name: /GPUs/ }));
  await waitFor(() => assert.ok(drawer.getByText("2 cards")));
  assert.equal(drawer.getAllByText("Ascend dGPU").length, 2, "one row per distinct state");
});

test("a diagnosis opens its own tab", async () => {
  api.nodeDoctor = async () => ({ report: "all checks passed" }) as never;
  const drawer = await openDrawer({ ...onlineNode, gpus: [] });
  assert.ok(drawer.queryByRole("tab", { name: "Diagnosis" }) === null, "Expected the element to be absent");
  fireEvent.click(drawer.getByRole("button", { name: "Diagnose" }));
  await waitFor(() => assert.equal(drawer.getByRole("tab", { name: "Diagnosis" }).getAttribute("aria-selected"), "true"));
});

test("a node event reloads the list in the background, without the table's loading spinner", async () => {
  stubApi([onlineNode]);
  // The page keeps the fetcher it rendered with, so the stub decides per call.
  let serve = nodesServer([onlineNode]);
  api.nodesPage = (query) => serve(query);
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  await waitFor(() => assert.ok(document.querySelector(".ant-spin-spinning") === null, "Expected the element to be absent"));
  const grown = nodesServer([onlineNode, offlineNode]);
  let release!: () => void;
  const held = new Promise<void>((resolve) => { release = resolve; });
  let reloads = 0;
  serve = async (query) => { reloads++; await held; return grown(query); };

  // Watch the whole reload, not one moment of it: a spinner that comes and
  // goes between two checks is still the flicker this guards against.
  let spun = false;
  const watcher = new dom.window.MutationObserver(() => {
    if (document.querySelector(".ant-spin-spinning")) spun = true;
  });
  watcher.observe(document.body, { subtree: true, childList: true, attributes: true, attributeFilter: ["class"] });
  // Every agent's once-a-minute status report reaches the page as one of these.
  for (const listener of Events.listeners.get("node") ?? []) {
    listener(new dom.window.MessageEvent("node", { data: JSON.stringify({ key: onlineNode.uuid, reason: "status" }) }) as unknown as Event);
  }
  await waitFor(() => assert.equal(reloads, 1, "the event did not reload the list"));
  await act(async () => {}); // commit whatever the event's reload rendered
  assert.ok(screen.getByText("gpu-a100-node-01"), "the current rows stay while the reload runs");

  release();
  await waitFor(() => assert.ok(screen.getByText("k8s-3-910c-host")));
  watcher.disconnect();
  assert.equal(spun, false, "the background reload spun the table");
});


test("batch upgrade retains checked nodes across pages and previews only those nodes", async (context) => {
  const fleet = Array.from({ length: 12 }, (_, i) => ({ ...onlineNode, uuid: `batch-${i}`, name: `batch-node-${i}`, hostname: `10.1.0.${i}` }));
  const { saveStoredPageSize, storageKey } = await import("../src/components/resourceTable/columnPrefs.ts");
  const key = storageKey("nodes");
  const previous = dom.window.localStorage.getItem(key);
  context.after(() => { if (previous === null) dom.window.localStorage.removeItem(key); else dom.window.localStorage.setItem(key, previous); });
  saveStoredPageSize("nodes", 10);
  stubApi(fleet);
  const requests: unknown[] = [];
  api.agentUpgradePreview = async (body) => {
    requests.push(body);
    const nodes = [fleet[0], fleet[11]].map((node) => ({ node_uuid: node.uuid, name: node.name!, from_version: node.agent_version, os: node.os, arch: node.arch, transport: "push", eligibility: "eligible" }));
    return { target_version: "0.9.8", manifest_fingerprint: "sealed", node_uuids: nodes.map((node) => node.node_uuid), nodes, counts: { eligible: 2 } };
  };
  renderPage(); await screen.findByText("batch-node-0");
  const batch = screen.getByRole("button", { name: "Upgrade selected Agents" }) as HTMLButtonElement;
  assert.equal(batch.disabled, true);
  fireEvent.click(rowOf("batch-node-0").getByRole("checkbox"));
  await screen.findByText("1 nodes selected");
  fireEvent.click(screen.getByRole("button", { name: "Next page" })); await screen.findByText("batch-node-11");
  fireEvent.click(rowOf("batch-node-11").getByRole("checkbox")); await screen.findByText("2 nodes selected");
  fireEvent.click(screen.getByRole("button", { name: "Previous page" })); await screen.findByText("batch-node-0");
  assert.equal(rowOf("batch-node-0").getByRole("checkbox").getAttribute("aria-checked"), "true");
  fireEvent.click(batch); await screen.findByRole("dialog", { name: "Review Agent upgrade" });
  assert.deepEqual(requests, [{ node_uuids: ["batch-0", "batch-11"] }]);
});

test("clear selection unchecks selected rows and disables batch upgrade", async () => {
  stubApi([onlineNode, offlineNode]); renderPage();
  await screen.findByText(onlineNode.name!);
  fireEvent.click(rowOf(onlineNode.name!).getByRole("checkbox"));
  fireEvent.click(rowOf(offlineNode.name!).getByRole("checkbox"));
  await screen.findByText("2 nodes selected");
  fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
  assert.equal(rowOf(onlineNode.name!).getByRole("checkbox").getAttribute("aria-checked"), "false");
  assert.equal(rowOf(offlineNode.name!).getByRole("checkbox").getAttribute("aria-checked"), "false");
  assert.equal((screen.getByRole("button", { name: "Upgrade selected Agents" }) as HTMLButtonElement).disabled, true);
  assert.ok(!screen.queryByText("2 nodes selected"));
});

test("a node-list refresh failure preserves an open upgrade preview and its retry identity", async () => {
  stubApi([onlineNode]);
  let failList = false;
  const serve = nodesServer([onlineNode]);
  api.nodesPage = async (query) => { if (failList) throw new Error("node list temporarily unavailable"); return serve(query); };
  const submitted: Array<{ idempotency_key: string; node_uuids: string[] }> = [];
  api.agentUpgradePreview = async () => ({ target_version: "0.9.8", manifest_fingerprint: "sealed", node_uuids: [onlineNode.uuid], nodes: [{ node_uuid: onlineNode.uuid, name: onlineNode.name!, from_version: "0.9.7", os: "linux", arch: "amd64", transport: "push", eligibility: "eligible" }], counts: { eligible: 1 } });
  api.createAgentUpgrade = async (body) => { submitted.push(body); throw new Error("create response lost"); };
  renderPage(); await screen.findByText(onlineNode.name!);
  fireEvent.click(rowOf(onlineNode.name!).getByRole("checkbox"));
  fireEvent.click(screen.getByRole("button", { name: "Upgrade selected Agents" }));
  const modal = await screen.findByRole("dialog", { name: "Review Agent upgrade" });
  await within(modal).findByText("0.9.7 → 0.9.8");
  fireEvent.click(within(modal).getByRole("checkbox"));
  fireEvent.click(within(modal).getByRole("button", { name: "Update to this version" }));
  await within(modal).findByText("create response lost");
  failList = true;
  await act(async () => {
    for (const listener of Events.listeners.get("node") ?? []) listener(new dom.window.MessageEvent("node", { data: JSON.stringify({ key: onlineNode.uuid, reason: "status" }) }) as unknown as Event);
  });
  await screen.findByText("node list temporarily unavailable");
  assert.ok(screen.getByRole("dialog", { name: "Review Agent upgrade" }) === modal);
  fireEvent.click(within(modal).getByRole("button", { name: "Update to this version" }));
  await waitFor(() => assert.equal(submitted.length, 2));
  assert.equal(submitted[0].idempotency_key, submitted[1].idempotency_key);
  assert.deepEqual(submitted[1].node_uuids, [onlineNode.uuid]);
  assert.ok(screen.getByRole("button", { name: "Recent Agent upgrades" }));
});

test("an offline node uses doctor and reruns replace the partial report", async () => {
  let calls = 0;
  api.nodeDoctor = async () => {
    calls++;
    return { report: "", checked_at: "2026-11-09T10:00:00Z", summary: "incomplete", checks: [
      { stage: "connection", status: "unavailable", reason: calls === 1 ? "credentials_unavailable" : "not_observed", vantage: "node" },
      { stage: "accelerators", status: "unavailable", reason: "requires_agent", vantage: "node" } ] } as never;
  };
  const drawer = await openDrawer({ ...offlineNode, gpus: [] });
  fireEvent.click(drawer.getByRole("button", { name: "Diagnose" }));
  await waitFor(() => assert.ok(drawer.getByText(/No SSH credentials/)));
  assert.ok(drawer.getByText("This check requires a connected Agent."));
  fireEvent.click(await drawer.findByRole("button", { name: "Run diagnosis again" }));
  await waitFor(() => assert.ok(drawer.getByText(/No reliable evidence/)));
  assert.equal(drawer.queryByText(/No SSH credentials/), null);
  assert.equal(calls, 2);
});

// What the server sends for a node that joined from a Kubernetes cluster: the
// placement is two of its system labels (the cluster by UUID, shown by name),
// not a field the page composes.
const kubeNode: Node = {
  ...onlineNode,
  uuid: "33333333-3333-3333-3333-333333333333", name: "k8s-gpu-01", hostname: "k8s-gpu-01",
  system_labels: [
    { key: "gpuworks.ai/kubernetes-cluster", value: "7c1b6f0e-0000-4000-8000-000000000007", display: "prod-gpu" },
    { key: "gpuworks.ai/kubernetes-node", value: "worker-3", display: "worker-3" },
    { key: "gpuworks.ai/node", value: "33333333-3333-3333-3333-333333333333", display: "k8s-gpu-01" },
  ],
};

test("a node that joined from Kubernetes shows its cluster and Kubernetes node as system labels; no K8s badge is composed", async () => {
  stubApi([onlineNode, kubeNode]);
  let clusterLookups = 0;
  api.kubeClusters = async () => { clusterLookups++; return list([]) as never; };
  renderPage();
  const row = (await screen.findByText("k8s-gpu-01", { selector: "button span, button" })).closest("tr") as HTMLElement;
  await waitFor(() => assert.ok(within(row).getByText("Kubernetes cluster: prod-gpu")));
  assert.ok(within(row).queryByText(/^K8s/) === null, "no K8s badge next to the name");
  assert.equal(clusterLookups, 0, "the page does not look up cluster names itself");

  screen.getByRole("button", { name: "k8s-gpu-01" }).click();
  await waitFor(() => assert.ok(screen.queryByRole("dialog")));
  const drawer = within(screen.queryByRole("dialog") as HTMLElement);
  await waitFor(() => assert.ok(drawer.getByText("Kubernetes cluster: prod-gpu")));
  assert.ok(drawer.getByText("Kubernetes node: worker-3"));
  assert.ok(drawer.queryByText(/^K8s/) === null);
  assert.equal(clusterLookups, 0);
});

test("a node Kubernetes has cordoned carries the platform cordon, not a K8s badge", async () => {
  const cordoned: Node = { ...kubeNode, scheduling: "cordoned", scheduling_by: "platform", cordon_reason: "kube_unschedulable" };
  stubApi([cordoned]);
  renderPage();
  const row = (await screen.findByText("k8s-gpu-01", { selector: "button span, button" })).closest("tr") as HTMLElement;
  await waitFor(() => assert.ok(within(row).getByText("Kubernetes cluster: prod-gpu")));
  assert.ok(within(row).queryByText("K8s cordoned") === null);
  screen.getByRole("button", { name: "k8s-gpu-01" }).click();
  await waitFor(() => assert.ok(screen.queryByRole("dialog")));
  assert.ok(within(screen.queryByRole("dialog") as HTMLElement).queryByText("K8s cordoned") === null);
});
