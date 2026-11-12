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

test("renders each node with its name and hostname in the node column", async () => {
  stubApi([onlineNode, offlineNode]);
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  assert.ok(screen.getByText("192.168.10.11"));
  assert.ok(screen.getByText("k8s-3-910c-host"));
  assert.ok(screen.getByText("192.0.2.129"));
});

test("offline Agent versions are historical, including when the session closed before the node state updated", async () => {
  const restarting: Node = { ...offlineNode, uuid: "restarting", name: "restarting", agent_version: "0.9.6", agent_eligibility: "busy" };
  stubApi([offlineNode, { ...onlineNode, agent_eligibility: "offline" }, restarting]);
  renderPage();
  await screen.findByText(onlineNode.name!);
  const versionCell = rowOf(onlineNode.name!).getByText("0.9.7").closest("td")!;
  assert.ok(within(versionCell).getByText("Last known version").closest('[data-slot="badge"]'));
  const restartingRow = rowOf("restarting");
  assert.ok(restartingRow.getByText("In upgrade batch"));
  assert.ok(restartingRow.getByText("Last known version"));
  const unreported = rowOf(offlineNode.name!);
  assert.ok(unreported.getByText("Not reported").closest('[data-slot="badge"]'));
  assert.equal(unreported.queryByText("Last known version"), null);
  assert.equal(screen.queryByText("Upgrade available"), null);
});

test("dismissing the fleet notice follows the account and target while leaving the table available", async () => {
  stubApi([onlineNode]);
  const nodes = api.nodesPage;
  let target = "v1.2.0";
  api.nodesPage = async query => ({
    ...await nodes(query),
    agent_fleet: { target, align: 1, upgrade: 1, check: 0 },
  });
  const ignore = { name: "Dismiss this notice" };
  const first = renderPage(101);
  fireEvent.click(await screen.findByRole("button", ignore));
  assert.ok(screen.getByRole("combobox", { name: "Agent status" }));
  assert.ok(screen.getByText(onlineNode.name!));
  first.unmount();
  const same = renderPage(101);
  await screen.findByText(onlineNode.name!);
  assert.equal(screen.queryByRole("button", ignore), null);
  same.unmount();
  const other = renderPage(102);
  await screen.findByRole("button", ignore);
  other.unmount();
  target = "v1.3.0";
  renderPage(101);
  await screen.findByRole("button", ignore);
});

test("the latest-rejection column is gone from the table; the drawer keeps the detail", async () => {
  stubApi([offlineNode]);
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("k8s-3-910c-host")));
  assert.ok(!screen.queryByText("Latest rejection"), `"Latest rejection" is still shown`);
  assert.ok(!screen.queryByText("Recent Agent handshake rejections"), `"Recent Agent handshake rejections" is still shown`);
  // The reason only surfaces in the detail drawer, never as a table column or
  // a page-level event card.
  assert.ok(!screen.queryByText("bad_token"), `"bad_token" is still shown`);
});

test("the table uses a fixed layout with explicit widths; the name leads frozen left and actions trail frozen right", async () => {
  stubApi([onlineNode]);
  const { container } = renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  const table = container.querySelector("table") as HTMLTableElement;
  assert.equal(table.style.tableLayout, "fixed");
  const cols = Array.from(table.querySelectorAll("thead th"));
  assert.ok(screen.getByRole("columnheader", { name: /Agent version/i }));
  assert.equal(cols.length, 12);
  assert.equal((cols[1] as HTMLElement).style.width, "220px");
  assert.equal((cols[cols.length - 1] as HTMLElement).style.width, "110px");
  const headers = screen.getAllByRole("columnheader");
  assert.equal(headers[1].textContent, "Name");
  assert.equal((headers[1] as HTMLElement).style.left, "48px");
  assert.equal(headers.at(-1)?.textContent, "Actions");
  assert.equal((headers.at(-1) as HTMLElement).style.right, "0px");
});

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

test("the name opens the detail; row actions are Terminal and a More dropdown; Terminal is disabled offline", async () => {
  stubApi([onlineNode, offlineNode]);
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  const onlineRow = rowOf("gpu-a100-node-01");
  onlineRow.getByRole("button", { name: "gpu-a100-node-01" });
  const onlineTerminal = onlineRow.getByRole("button", { name: /Terminal/ }) as HTMLButtonElement;
  assert.equal(onlineTerminal.disabled, false);
  assert.ok(onlineRow.queryByRole("button", { name: /Resource group/ }) === null, "Expected the element to be absent");
  onlineRow.getByRole("button", { name: "More" }).click();
  for (const item of [/Details/, /Edit labels/, /Delete/]) await waitFor(() => screen.getByRole("menuitem", { name: item }));

  const offlineRow = rowOf("k8s-3-910c-host");
  const offlineTerminal = offlineRow.getByRole("button", { name: /Terminal/ }) as HTMLButtonElement;
  assert.equal(offlineTerminal.disabled, true);
  // Delete is no longer an inline button that forced the row to wrap.
  assert.ok(offlineRow.queryByRole("button", { name: /Delete/ }) === null, "Expected the element to be absent");
});

test("delete lives behind the More dropdown and asks for confirmation", async () => {
  stubApi([{ ...onlineNode, has_ssh_credential: undefined } as unknown as Node]);
  let deleted: string | undefined;
  api.deleteNode = async (uuid: string) => {
    deleted = uuid;
    return { status: "deleted", agent_uninstalled: true };
  };
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  const row = rowOf("gpu-a100-node-01");
  row.getByRole("button", { name: "More" }).click();
  const menuItem = await waitFor(() => screen.getByRole("menuitem", { name: /Delete/ }));
  menuItem.click();
  await waitFor(() => assert.ok(deleteModal()));
  assert.ok(screen.getByText("Stop deployment instances on this node first."));
  assert.ok(screen.getByText("Normal removal uninstalls the agent over SSH before deleting the node record."));
  assert.ok(screen.getByRole("checkbox", { name: /Force removal/ }));
  // Cancel must leave the node untouched. (Whether the dialog's leave motion
  // finishes is antd's own concern in jsdom; assert only the functional bit.)
  deleteModalButtons().find((b) => b.textContent === "Cancel")!.click();
  await waitFor(() => assert.equal(deleted, undefined));
});

test("confirming the dialog deletes the node and reports success", async () => {
  stubApi([onlineNode]);
  let deleted: string | undefined;
  api.deleteNode = async (uuid: string) => {
    deleted = uuid;
    return { status: "deleted", agent_uninstalled: true };
  };
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  const row = rowOf("gpu-a100-node-01");
  row.getByRole("button", { name: "More" }).click();
  (await waitFor(() => screen.getByRole("menuitem", { name: /Delete/ }))).click();
  await waitFor(() => assert.ok(deleteModal()));
  deleteModalButtons().find((b) => b.textContent === "Delete")!.click();
  await waitFor(() => assert.equal(deleted, onlineNode.uuid));
  await waitFor(() => assert.ok(screen.getByText("Node deleted")));
});

test("force removal is explicit and warns that the remote agent may still run", async () => {
  stubApi([onlineNode]);
  let request: { uuid: string; force: boolean } | undefined;
  api.deleteNode = async (uuid: string, force = false) => {
    request = { uuid, force };
    return { status: "deleted", agent_uninstalled: false };
  };
  renderPage();
  await waitFor(() => screen.getByText("gpu-a100-node-01"));
  rowOf("gpu-a100-node-01").getByRole("button", { name: "More" }).click();
  (await waitFor(() => screen.getByRole("menuitem", { name: /Delete/ }))).click();
  await waitFor(() => assert.ok(deleteModal()));
  const checkbox = screen.getByRole("checkbox", { name: /Force removal/ }) as HTMLInputElement;
  assert.equal(checkbox.checked, false);
  checkbox.click();
  await waitFor(() => assert.equal(checkbox.checked, true));
  assert.ok(screen.getByRole("alert").textContent?.includes("agent may still be running"));
  deleteModalButtons().find((b) => b.textContent === "Delete")!.click();
  await waitFor(() => assert.deepEqual(request, { uuid: onlineNode.uuid, force: true }));
  await waitFor(() => screen.getByText("Management records deleted; the agent may still be running"));
});

test("normal deletion without SSH retains the dialog and offers an explicit force retry", async () => {
  stubApi([{ ...onlineNode, has_ssh_credential: false }]);
  const requests: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    assert.equal(init?.method, "DELETE");
    const url = String(input);
    requests.push(url);
    if (url.endsWith("?force=true")) return Response.json({ status: "deleted", agent_uninstalled: false });
    return new Response('{"code":"NODE_DELETE_SSH_REQUIRED","error":"normal deletion requires saved SSH credentials to uninstall the agent; node retained. Configure SSH and retry, or uninstall the agent manually and use force=true to remove only its management record"}', { status: 409, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  renderPage();
  await waitFor(() => screen.getByText("gpu-a100-node-01"));
  rowOf("gpu-a100-node-01").getByRole("button", { name: "More" }).click();
  (await waitFor(() => screen.getByRole("menuitem", { name: /Delete/ }))).click();
  const dialog = within(await waitFor(() => deleteModal()));
  assert.match(dialog.getByRole("alert").textContent ?? "", /Configure SSH credentials for normal deletion/);
  assert.ok(dialog.getByText("Normal removal uninstalls the agent over SSH before deleting the node record."));
  const checkbox = dialog.getByRole("checkbox", { name: /Force removal/ }) as HTMLInputElement;
  assert.equal(checkbox.checked, false);
  dialog.getByRole("button", { name: "Delete", exact: true }).click();
  const explanation = "Normal deletion requires SSH to uninstall the agent, but this node has no saved SSH credentials. The node record was retained. Configure SSH credentials and retry, or uninstall the agent manually and then select Force removal to delete its management record.";
  await screen.findByText(`Failed to delete node: ${explanation}`);
  assert.deepEqual(requests, [`/api/v1/nodes/${onlineNode.uuid}`]);
  assert.ok(deleteModal());
  const retainedRow = within(screen.getByRole("table", { hidden: true })).getByText("gpu-a100-node-01").closest("tr");
  assert.ok(retainedRow, "failed deletion retained the node row");
  checkbox.click();
  await waitFor(() => assert.equal(checkbox.checked, true));
  assert.ok(dialog.getByText("The agent may still be running on the node. Stop it manually when the node is accessible."));
  const previousMessages = new Set(screen.queryAllByText("Management records deleted; the agent may still be running"));
  dialog.getByRole("button", { name: "Delete", exact: true }).click();
  await waitFor(() => assert.deepEqual(requests, [`/api/v1/nodes/${onlineNode.uuid}`, `/api/v1/nodes/${onlineNode.uuid}?force=true`]));
  await waitFor(() => {
    const newMessage = screen.queryAllByText("Management records deleted; the agent may still be running")
      .find((element) => !previousMessages.has(element));
    assert.ok(newMessage, "the current deletion reported that its agent remains installed");
  });
});

test("SSH uninstall failure explains recovery steps while keeping the deletion dialog open", async () => {
  stubApi([onlineNode]);
  const requests: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    assert.equal(init?.method, "DELETE");
    requests.push(String(input));
    return new Response('{"code":"NODE_UNINSTALL_UNCONFIRMED","error":"agent uninstall over SSH was not confirmed; node and SSH credentials retained. Fix SSH and retry, or uninstall the agent manually and use force=true to remove only its management record: SSH unreachable","params":{"detail":"SSH unreachable"}}', { status: 409, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  renderPage();
  await waitFor(() => screen.getByText("gpu-a100-node-01"));
  rowOf("gpu-a100-node-01").getByRole("button", { name: "More" }).click();
  (await waitFor(() => screen.getByRole("menuitem", { name: /Delete/ }))).click();
  within(await waitFor(() => deleteModal())).getByRole("button", { name: "Delete", exact: true }).click();
  const explanation = "Normal deletion could not confirm that the agent was uninstalled over SSH. The node and SSH credentials were retained. Fix the SSH connection or credentials and retry, or uninstall the agent manually and then select Force removal to delete its management record. Details: SSH unreachable";
  await screen.findByText(`Failed to delete node: ${explanation}`);
  assert.deepEqual(requests, [`/api/v1/nodes/${onlineNode.uuid}`]);
  assert.ok(deleteModal());
  const retainedRow = within(screen.getByRole("table", { hidden: true })).getByText("gpu-a100-node-01").closest("tr");
  assert.ok(retainedRow, "failed deletion retained the node row");
});

test("uninstall failure keeps the dialog open and allows an explicit force retry", async () => {
  stubApi([onlineNode]);
  const requests: boolean[] = [];
  api.deleteNode = async (_uuid: string, force = false) => {
    requests.push(force);
    if (!force) throw new Error("SSH unreachable; node retained");
    return { status: "deleted", agent_uninstalled: false };
  };
  renderPage();
  await waitFor(() => screen.getByText("gpu-a100-node-01"));
  rowOf("gpu-a100-node-01").getByRole("button", { name: "More" }).click();
  (await waitFor(() => screen.getByRole("menuitem", { name: /Delete/ }))).click();
  await waitFor(() => assert.ok(deleteModal()));
  deleteModalButtons().find((b) => b.textContent === "Delete")!.click();
  await waitFor(() => screen.getByText(/Failed to delete node: SSH unreachable/));
  assert.deepEqual(requests, [false]);
  assert.ok(deleteModal());
  (screen.getByRole("checkbox", { name: /Force removal/ }) as HTMLInputElement).click();
  within(deleteModal()).getByRole("button", { name: "Delete", exact: true }).click();
  await waitFor(() => assert.deepEqual(requests, [false, true]));
});

test("the delete dialog prevents duplicate submissions while SSH uninstall is pending", async () => {
  stubApi([onlineNode]);
  let finish!: () => void;
  let requests = 0;
  api.deleteNode = async () => {
    requests++;
    await new Promise<void>((resolve) => { finish = resolve; });
    return { status: "deleted", agent_uninstalled: true };
  };
  renderPage();
  await waitFor(() => screen.getByText("gpu-a100-node-01"));
  rowOf("gpu-a100-node-01").getByRole("button", { name: "More" }).click();
  (await waitFor(() => screen.getByRole("menuitem", { name: /Delete/ }))).click();
  await waitFor(() => assert.ok(deleteModal()));
  deleteModalButtons().find((b) => b.textContent === "Delete")!.click();
  await waitFor(() => assert.equal(requests, 1));
  const checkbox = screen.getByRole("checkbox", { name: /Force removal/ }) as HTMLInputElement;
  await waitFor(() => assert.equal(checkbox.disabled, true));
  deleteModalButtons().find((b) => b.textContent === "Delete")!.click();
  assert.equal(requests, 1);
  assert.equal(deleteModalButtons().find((b) => b.textContent === "Cancel")!.disabled, true);
  finish();
  await waitFor(() => screen.getByText("Node deleted"));
});

test("the detail drawer renders identity rows and the accelerator table", async () => {
  stubApi([{ ...onlineNode, gpus: [gpuInfo(0), gpuInfo(1)] }]);
  api.node = async (uuid: string) => ({ ...onlineNode, uuid, token: "3267d3b0a4684093b8393fc6eeeacea0cdff34ae15108322", gpus: [gpuInfo(0), gpuInfo(1)] }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  screen.getByRole("button", { name: "gpu-a100-node-01" }).click();
  // Identity block: UUID and the node name with its rename affordance.
  await waitFor(() => assert.ok(screen.getByText(onlineNode.uuid)));
  // The hostname shows both as the table's secondary line and the drawer title.
  assert.ok(screen.getAllByText("192.168.10.11").length >= 2);
  // Accelerator table, on the GPU tab: one row per GPU with the model as a link.
  fireEvent.click(await screen.findByRole("tab", { name: /GPUs/ }));
  await waitFor(() => assert.equal(screen.getAllByRole("link", { name: "A100-SXM4-80GB" }).length, 2));
  // The detail response still carries the node token, but the drawer no
  // longer shows it or offers to regenerate it.
  const drawer = screen.queryByRole("dialog")!;
  assert.ok(!drawer.textContent?.includes("3267d3b0a4684093b8393fc6eeeacea0cdff34ae15108322"), "token is not shown");
  assert.ok(drawer.querySelector(".ant-typography-copy") === null, "Expected the element to be absent");
  assert.ok(screen.queryByRole("button", { name: /regenerate/i }) === null, "Expected the element to be absent");
});

test("ordinary node detail shows its last report without administrator diagnostics", async () => {
  for (const last_seen_at of [new Date(2026, 10, 12, 8, 15).toISOString(), undefined]) {
    const node = { ...onlineNode, last_seen_at };
    stubApi([node]);
    api.me = async () => ({ role: "member" }) as never;
    renderPage();
    await screen.findByRole("button", { name: node.name });
    fireEvent.click(screen.getByRole("button", { name: node.name }));
    await screen.findByText(node.uuid);
    const drawer = screen.getByRole("dialog");
    const report = within(drawer).getByRole("row", { name: /Last successful report/ });
    assert.match(report.textContent ?? "", last_seen_at ? /2026-11-12 08:15/ : /Not observed/);
    assert.equal(within(drawer).queryByRole("button", { name: "Check connection" }), null);
    await act(async () => cleanup());
  }
});

test("the label filter ORs values of one key, ANDs across keys, and explains an empty match", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const nodeA = { ...onlineNode, labels: { pool: "a", zone: "x" } };
  const nodeB = { ...offlineNode, labels: { pool: "b", zone: "y" } };
  const nodeC = { ...onlineNode, uuid: "33333333-3333-3333-3333-333333333333", name: "unlabeled-node", hostname: "192.168.10.13", labels: {} };
  stubApi([nodeA, nodeB, nodeC]);
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("unlabeled-node")));
  const filter = screen.getByRole("combobox", { name: "Filter by label" });

  const pick = async (pair: string) => {
    await user.click(filter);
    await user.type(filter, pair);
    let option: Element | null = null;
    await waitFor(() => assert.ok((option = document.querySelector(`.ant-select-item-option[title="${pair}"]`))));
    fireEvent.click(option!);
  };

  await pick("pool=a");
  await waitFor(() => assert.ok(!screen.queryByText("unlabeled-node"), `"unlabeled-node" is still shown`));
  assert.ok(screen.getByText("gpu-a100-node-01"));
  assert.ok(!screen.queryByText("k8s-3-910c-host"), `"k8s-3-910c-host" is still shown`);

  await pick("pool=b");
  await waitFor(() => assert.ok(screen.getByText("k8s-3-910c-host")));
  assert.ok(screen.getByText("gpu-a100-node-01"));

  await pick("zone=y");
  await waitFor(() => assert.ok(!screen.queryByText("gpu-a100-node-01"), `"gpu-a100-node-01" is still shown`));
  assert.ok(screen.getByText("k8s-3-910c-host"));

  // Dropping pool=b leaves pool=a AND zone=y, which no node satisfies; the
  // empty state names the filter, not an empty fleet.
  fireEvent.click(document.querySelector('.ant-select-selection-item[title="pool=b"] .ant-select-selection-item-remove')!);
  await waitFor(() => assert.ok(screen.getByText("No nodes match the search or the selected labels")));
  assert.ok(!screen.queryByText("k8s-3-910c-host"), `"k8s-3-910c-host" is still shown`);
  assert.deepEqual(pageQueries.at(-1)?.label, ["pool=a", "zone=y"], "the filter is the server's");
  assert.equal(pageQueries.at(-1)?.page, 1);
});

test("the node list asks the server for one page and counts matches against the fleet", async () => {
  const fleet = Array.from({ length: 23 }, (_, i) => ({
    ...onlineNode,
    uuid: `node-${String(i).padStart(2, "0")}`,
    name: `node-${String(i).padStart(2, "0")}`,
    hostname: `10.0.0.${i}`,
    labels: { pool: i % 2 ? "odd" : "even" },
  }));
  stubApi(fleet);
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("node-00")));
  assert.deepEqual(pageQueries[0], { extra: { agent_upgrade: ["all"] }, page: 1, page_size: 20, q: "", label: [], state: [] });
  assert.ok(screen.getByText("23/23 nodes"));
  assert.ok(!screen.queryByText("node-20"), `"node-20" is still shown`);

  fireEvent.click(screen.getByRole("button", { name: "Next page" }));
  await waitFor(() => assert.ok(screen.getByText("node-22")));
  assert.equal(pageQueries.at(-1)?.page, 2);
  assert.ok(!screen.queryByText("node-00"), `"node-00" is still shown`);

  const search = screen.getByRole("searchbox", { name: "Search by name, hostname or ID" });
  fireEvent.change(search, { target: { value: "node-1" } });
  fireEvent.keyDown(search, { key: "Enter", code: "Enter", keyCode: 13 });
  await waitFor(() => assert.ok(screen.getByText("10/23 nodes")));
  assert.equal(pageQueries.at(-1)?.q, "node-1");
  assert.equal(pageQueries.at(-1)?.page, 1, "a new search starts from the first page");
  assert.ok(screen.getByText("node-10"));

  // The label options come from the server's fleet-wide facets, not the page.
  const filter = screen.getByRole("combobox", { name: "Filter by label" });
  fireEvent.mouseDown(filter);
  await waitFor(() => assert.ok(document.querySelector('.ant-select-item-option[title="pool=odd"]')));
  assert.ok(document.querySelector('.ant-select-item-option[title="pool=even"]'));
});

test("a deep link opens the named node even when it is not on the first page", async () => {
  const fleet = Array.from({ length: 25 }, (_, i) => ({ ...onlineNode, uuid: `deep-${i}`, name: `deep-${i}`, hostname: `10.1.0.${i}`, labels: {} }));
  stubApi(fleet);
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[{ pathname: "/nodes", state: { nodeUUID: "deep-0" } }]}>
        <NodesPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
  await waitFor(() => assert.ok(screen.queryByRole("dialog")));
  await waitFor(() => assert.ok(within(screen.queryByRole("dialog") as HTMLElement).getAllByText(/deep-0/).length > 0));
});

test("the instance topology link opens its node on the GPU tab with the topology; the list still opens on the overview", async () => {
  const node = { ...onlineNode, gpus: [gpuInfo(0), gpuInfo(1)] };
  const other = { ...onlineNode, uuid: "node-other", name: "other-node", hostname: "10.0.0.9", gpus: [gpuInfo(0)] };
  stubApi([node, other]);
  api.node = async (uuid: string) => ({ ...(uuid === other.uuid ? other : node), uuid }) as never;
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[{ pathname: "/nodes", state: { nodeUUID: node.uuid, tab: "accelerators", highlight: { gpus: [0, 1], links: [[0, 1]], numa: [0] } } }]}>
        <NodesPage />
      </MemoryRouter>
    </I18nextProvider>,
  );
  await waitFor(() => assert.ok(screen.queryByRole("dialog")));
  const drawer = within(screen.queryByRole("dialog") as HTMLElement);
  await waitFor(() => assert.equal(drawer.getByRole("tab", { name: /GPUs/ }).getAttribute("aria-selected"), "true"));
  assert.ok(document.querySelector(".node-gpu-table"));
  assert.ok(drawer.getByRole("heading", { name: "Accelerator topology" }));
  // Another node opened from the list afterwards starts on its overview.
  fireEvent.click(drawer.getByRole("button", { name: "Close" }));
  await waitFor(() => assert.ok(screen.queryByRole("dialog") === null, "Expected the element to be absent"));
  screen.getByRole("button", { name: other.name }).click();
  await waitFor(() => assert.ok(screen.getByText(other.uuid)));
  const next = within(screen.queryByRole("dialog") as HTMLElement);
  assert.equal(next.getByRole("tab", { name: "Overview" }).getAttribute("aria-selected"), "true");
});

test("a failed node page shows the error with a retry", async () => {
  stubApi([onlineNode]);
  let calls = 0;
  const serve = api.nodesPage;
  api.nodesPage = async (query) => {
    calls += 1;
    if (calls === 1) throw new Error("nodes unavailable");
    return serve(query);
  };
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("nodes unavailable")));
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
});

test("the detail drawer lists the node labels where the resource group used to be", async () => {
  const labeled = { ...onlineNode, labels: { resource_group: "pool-a", owner: "ml" } };
  stubApi([labeled]);
  api.node = async () => labeled as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText("gpu-a100-node-01")));
  screen.getByRole("button", { name: "gpu-a100-node-01" }).click();
  await waitFor(() => assert.ok(screen.queryByRole("dialog")));
  const drawer = within(screen.queryByRole("dialog") as HTMLElement);
  await waitFor(() => assert.ok(drawer.getByText("resource_group=pool-a")));
  assert.ok(drawer.getByText("owner=ml"));
  assert.ok(drawer.queryByText("Resource group") === null, "Expected the element to be absent");
});

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
