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
      // Every source the fleet has, whatever the filters. No node of this file joined through an external
      // platform, so the only one is "added by hand" (the source filter itself is in tests/nodeSource.test.tsx).
      sources: nodes.length ? [{ value: "manual", nodes: nodes.length }] : [],
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
  assert.equal(cols.length, 13, "master's twelve and the Source column");
  assert.equal((cols[1] as HTMLElement).style.width, "220px");
  assert.equal((cols[cols.length - 1] as HTMLElement).style.width, "110px");
  const headers = screen.getAllByRole("columnheader");
  assert.equal(headers[1].textContent, "Name");
  assert.equal((headers[1] as HTMLElement).style.left, "48px");
  assert.equal(headers.at(-1)?.textContent, "Actions");
  assert.equal((headers.at(-1) as HTMLElement).style.right, "0px");
  // Where a node came from follows what it has (its GPUs and its agent's version) and leads its labels.
  assert.deepEqual(headers.slice(3, 7).map((header) => header.textContent), ["GPU", "Agent version", "Source", "System labels"]);
  assert.equal((headers[5] as HTMLElement).style.width, "190px");
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
  // No source in the address: the request names none, beside the agent upgrade filter the list always sends.
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

async function openDrawer(node: Node) {
  stubApi([node]);
  api.node = async (uuid: string) => ({ ...node, uuid }) as never;
  renderPage();
  await waitFor(() => assert.ok(screen.getByText(node.name!)));
  screen.getByRole("button", { name: node.name! }).click();
  await waitFor(() => assert.ok(screen.getByText(node.uuid)));
  return within(screen.queryByRole("dialog") as HTMLElement);
}

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
