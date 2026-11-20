import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
dom.window.HTMLElement.prototype.setPointerCapture = () => {};
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLHtmlElement", "HTMLTextAreaElement", "HTMLInputElement", "HTMLButtonElement", "AbortController", "AbortSignal", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
class EventSourceStub { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: EventSourceStub, configurable: true });

const load = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const networksLocale = load("networks");
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { networks: networksLocale, common: load("common") } },
  defaultNS: "common",
  ns: ["networks", "common"],
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { ConfigProvider } = await import("antd");
const apiModule = await import("../src/api.ts");
const { FeedbackHost } = await import("../src/ui-kit/feedback/index.tsx");
const NetworksPage = (await import("../src/pages/networks/NetworksPage.tsx")).default;
const { default: NetworkDetailDrawer } = await import("../src/pages/networks/NetworkDetailDrawer.tsx");
const { useParams: useRouteParams } = await import("react-router-dom");
// The detail is a drawer over the list now; this stands in for the old page
// route, opening the drawer on the network the path names.
function NetworkDetailPage() {
  const id = Number(useRouteParams().id);
  const [tab, setTab] = React.useState<string>();
  return <NetworkDetailDrawer id={id} tab={tab} onTab={setTab} onClose={() => {}} onChanged={() => {}} />;
}
const NodeNetworksCard = (await import("../src/components/NodeNetworksCard.tsx")).default;
const NetworkTestResult = (await import("../src/pages/networks/NetworkTestResult.tsx")).default;

const detailFixture = JSON.parse(fs.readFileSync(new URL("./fixtures/network-detail.json", import.meta.url), "utf8"));
const typeRoles = {
  host_ethernet: ["storage_sync", "compute", "serving"],
  host_rdma: ["storage_sync", "compute", "serving"],
  accelerator_rdma: ["compute"],
};
const n = networksLocale;

type API = typeof apiModule.api;
const originals: Partial<API> = {};
function stub<K extends keyof API>(key: K, impl: API[K]) {
  if (!(key in originals)) (originals as Record<string, unknown>)[key] = apiModule.api[key];
  apiModule.api[key] = impl;
}
after(() => { closeChannels(); dom.window.close(); });
afterEach(() => {
  cleanup();
  for (const [key, value] of Object.entries(originals)) (apiModule.api as Record<string, unknown>)[key] = value;
  for (const key of Object.keys(originals)) delete (originals as Record<string, unknown>)[key];
});

function wrap(element: React.ReactElement, path = "/", pattern = "/") {
  return render(
    <I18nextProvider i18n={i18n}><FeedbackHost />
      <MemoryRouter initialEntries={[path]}>
        <Routes><Route path={pattern} element={element} /></Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

// The management network exactly as GET /api/v1/networks lists it.
const managementItem = {
  id: 0, name: "management", description: "", roles: ["storage_sync", "compute", "serving"], type: "host_ethernet",
  cidr: "", interface: "", builtin: true, attachments: [], resolved_node_count: 2, warning_node_count: 0,
  created_at: "0001-01-01T00:00:00Z", updated_at: "0001-01-01T00:00:00Z",
};

function userItem() {
  const { nodes: _nodes, role_options: _r, type_options: _t, type_roles: _tr, test_supported: _s, latest_test: _l, ...network } = detailFixture;
  return network;
}

function nodeNetworks(management: Record<string, unknown>) {
  return {
    networks: [{ role: "serving", network_id: 0, network_name: "management", network_type: "host_ethernet", attachment_id: 0, source: "management", interface: "eth0", ip: "172.25.0.11", hcas: [], warnings: [] }],
    serving_forward: null, inventory_reported: true, interfaces: [], rdma_ports: [],
    management: { interface: "eth0", ip: "172.25.0.11", roles_off: [], behind_nat: false, ...management },
  };
}

test("the management network is listed first and cannot be edited or deleted", async () => {
  stub("networks", async () => ({ items: [managementItem, userItem()], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }) as never);
  wrap(<NetworksPage />);
  const link = await screen.findByRole("link", { name: n.management.name });
  const row = link.closest("tr")!;
  assert.ok(within(row).getByText(n.management.attachedTo));
  assert.ok(within(row).getByText(n.management.selector));
  assert.ok(within(row).getByRole("button", { name: n.actions.detail }));
  assert.equal(within(row).queryByRole("button", { name: n.actions.edit }), null);
  assert.equal(within(row).queryByRole("button", { name: n.actions.delete }), null);
  const userRow = screen.getByRole("link", { name: "ib-a" }).closest("tr")!;
  assert.ok(within(userRow).getByRole("button", { name: n.actions.edit }));
});

const mgmtNodes = [
  { node_uuid: "a", node_name: "gpu-a", labels: { site: "lab" }, roles: ["compute", "serving"], attachment_id: 0, source: "management", interface: "eth0", ip: "172.25.0.11", hcas: [], tools: [], warnings: [] },
  { node_uuid: "b", node_name: "gpu-b", labels: { site: "lab", pool: "cpu" }, roles: [], attachment_id: 0, source: "management", interface: "eth0", ip: "172.25.0.12", hcas: [], tools: [], warnings: [], roles_off: ["storage_sync", "compute", "serving"] },
  { node_uuid: "c", node_name: "h200-c", labels: { site: "nebius" }, roles: ["storage_sync", "compute", "serving"], attachment_id: 0, source: "management", interface: "bond0", ip: "10.96.0.9", hcas: [], tools: [], warnings: [] },
];

function stubManagement(role: "admin" | "member") {
  let loads = 0;
  stub("me", async () => ({ role }) as never);
  // The rows carry their labels; the table must not list every node.
  stub("nodes", async () => { throw new Error("the management table fetched every node"); });
  stub("network", async () => { loads++; return { ...managementItem, role_options: [], type_options: [], test_supported: true, latest_test: null, nodes: mgmtNodes } as never; });
  return () => loads;
}

const roleSwitch = (node: string, role: "serving" | "storage_sync" | "compute") =>
  screen.getByRole("switch", { name: `${node} ${n.management.roleSwitch[role]}` });

test("following a node's link does not also expand its row, in either node table", async () => {
  // No routes: the link only changes the location, so the table stays to be read.
  const drawer = (id: number) => render(
    <I18nextProvider i18n={i18n}><FeedbackHost /><MemoryRouter><NetworkDetailDrawer id={id} onTab={() => {}} onClose={() => {}} onChanged={() => {}} /></MemoryRouter></I18nextProvider>,
  );
  // Taken once the table has settled: an earlier render's link may be gone.
  const followDoesNotExpand = (name: string) => {
    const link = screen.getAllByRole("link", { name })[0];
    const row = link.closest("tr") as HTMLElement;
    fireEvent.click(link);
    assert.equal(within(row).getByRole("button", { name: "Expand row" }).getAttribute("aria-expanded"), "false", `${name}'s row stays folded`);
  };
  stubManagement("admin");
  drawer(0);
  await waitFor(() => assert.equal(roleSwitch("gpu-a", "compute").hasAttribute("disabled"), false));
  followDoesNotExpand("gpu-a");
  cleanup();
  stub("network", async () => detailFixture);
  drawer(detailFixture.id);
  await screen.findByRole("link", { name: "h20-05" });
  followDoesNotExpand("h20-05");
});

test("members see the management network's per-node roles read-only", async () => {
  stubManagement("member");
  wrap(<NetworkDetailPage />, "/networks/0", "/networks/:id");
  assert.ok(await screen.findByText(n.management.intro.title));
  const a = screen.getByRole("link", { name: "gpu-a" }).closest("tr")! as HTMLElement;
  assert.ok(within(a).getByText("eth0 · 172.25.0.11"));
  // A node's labels sit in its row's expanded detail.
  await userEvent.click(within(a).getByRole("button", { name: /Show details|Expand row/ }));
  assert.ok(await within(a.nextElementSibling as HTMLElement).findByText("site=lab"));
  // Storage sync is allowed on gpu-a but a dedicated network carries it.
  assert.equal(roleSwitch("gpu-a", "storage_sync").getAttribute("aria-checked"), "true");
  // The cell keeps a short mark that fits the role column; the full wording is its title.
  assert.equal(within(a).getByText(n.management.dedicatedShort).getAttribute("title"), n.management.dedicated);
  // What a switch does is on its header: a dotted, focusable title with a tooltip, no icon.
  const header = screen.getByText(n.role.storage_sync, { selector: ".header-help" });
  header.focus();
  await waitFor(() => assert.ok(screen.getAllByRole("tooltip").some((tip) => tip.textContent === n.management.roleTip.storage_sync)));
  assert.equal(roleSwitch("gpu-b", "serving").getAttribute("aria-checked"), "false");
  for (const role of ["serving", "storage_sync", "compute"] as const) assert.equal(roleSwitch("gpu-b", role).getAttribute("aria-disabled"), "true");
  assert.equal(screen.queryAllByRole("checkbox").length, 0, "no bulk selection for members");
  assert.equal(screen.queryByRole("button", { name: n.management.bulk.apply }), null);
  for (const name of [n.actions.edit, n.actions.delete, n.actions.attach]) {
    assert.equal(screen.queryByRole("button", { name }), null, `${name} is hidden`);
  }
});

test("the management network runs internal tests like any other network", async () => {
  stubManagement("admin");
  const calls: unknown[][] = [];
  stub("startNetworkTest", async (id, body) => {
    calls.push([id, body]);
    return { id: 9, status: "running", kind: "tcp", scope: "nodes", bandwidth: true, node_count: 2, pair_count: 2, passed_count: 0, pairs: [], started_at: "2026-09-26T10:00:00Z" } as never;
  });
  wrap(<NetworkDetailPage />, "/networks/0", "/networks/:id");
  await userEvent.click(await screen.findByRole("tab", { name: n.tabs.test }));
  assert.equal(screen.queryByText(n.test.unsupported), null);
  assert.equal(screen.queryByText(n.test.kindRdmaTool), null, "an Ethernet network has no ib_write_bw test");
  const user = userEvent.setup();
  await chooseNodeScope(user);
  const picker = screen.getByRole("combobox", { name: n.test.nodes });
  for (const name of ["gpu-a", "h200-c"]) {
    await user.type(picker, name);
    await user.click(await screen.findByRole("option", { name }));
  }
  if (picker.getAttribute("aria-expanded") === "true") await user.keyboard("{Escape}");
  await userEvent.click(screen.getByRole("button", { name: n.test.start }));
  await waitFor(() => assert.equal(calls.length, 1));
  const [id, body] = calls[0] as [number, Record<string, unknown>];
  assert.equal(id, 0);
  assert.equal(body.scope, "nodes");
  assert.deepEqual(body.node_uuids, ["a", "c"]);
});

async function chooseNodeScope(user: ReturnType<typeof userEvent.setup>) {
  screen.getByRole("combobox", { name: n.test.scope }).focus();
  await user.keyboard("{Enter}");
  await user.click(await screen.findByRole("option", { name: n.test.scopeNodes }));
}

test("internal test node search recovers from no results and submits only the selected network nodes", async () => {
  stubManagement("admin");
  const calls: unknown[][] = [];
  stub("startNetworkTest", async (id, body) => {
    calls.push([id, body]);
    return { id: 10, status: "running", kind: "tcp", pattern: "mesh", scope: "nodes", bandwidth: true,
      node_count: 2, pair_count: 2, passed_count: 0, pairs: [], started_at: "2026-09-26T10:00:00Z" } as never;
  });
  const user = userEvent.setup();
  wrap(<NetworkDetailPage />, "/networks/0", "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  await chooseNodeScope(user);
  const picker = screen.getByRole("combobox", { name: n.test.nodes }) as HTMLInputElement;
  await user.type(picker, "missing-node");
  await waitFor(() => assert.equal(screen.queryAllByRole("option").length, 0));
  assert.deepEqual(calls, [], "searching alone must not start a test");

  await user.clear(picker);
  await user.type(picker, "gpu-a");
  assert.ok(await screen.findByRole("option", { name: "gpu-a" }));
  assert.equal(screen.queryByRole("option", { name: "gpu-b" }), null, "search filters the network candidates");
  await user.click(await screen.findByRole("option", { name: "gpu-a" }));
  if (picker.getAttribute("aria-expanded") === "true") await user.keyboard("{Escape}");
  await user.click(screen.getByRole("button", { name: n.test.start }));
  assert.ok(await screen.findByText(n.test.nodesRequired));
  assert.deepEqual(calls, [], "one real selection is insufficient");

  await user.click(picker);
  await user.type(picker, "h200");
  assert.ok(await screen.findByRole("option", { name: "h200-c" }));
  await user.click(await screen.findByRole("option", { name: "h200-c" }));
  if (picker.getAttribute("aria-expanded") === "true") await user.keyboard("{Escape}");
  await user.click(screen.getByRole("button", { name: n.test.start }));
  await waitFor(() => assert.deepEqual(calls, [[0, {
    kind: "tcp", pattern: "mesh", scope: "nodes", bandwidth: true, seconds: 3, node_uuids: ["a", "c"],
  }]]));
});

test("network pair dialogs restore their trigger after Escape and Close without retaining another pair", async () => {
  const user = userEvent.setup();
  // JSDOM does not emit CSS animationend; browser E2E exercises real motion.
  wrap(<ConfigProvider theme={{ token: { motion: false } }}>
    <NetworkTestResult network={detailFixture} run={detailFixture.latest_test} onChanged={() => {}} />
  </ConfigProvider>);
  for (const [index, closeWithEscape] of [[0, true], [1, false], [0, false], [1, true]] as const) {
    const pair = detailFixture.latest_test.pairs[index];
    const other = detailFixture.latest_test.pairs[1 - index];
    const title = `${pair.source_name} → ${pair.target_name}`;
    const label = pair.passed ? `${pair.gbps.toFixed(1)} Gb/s` : n.test.failed;
    const trigger = screen.getByRole("button", { name: `${title}: ${label}` });
    trigger.focus();
    await user.keyboard("{Enter}");
    const dialog = await screen.findByRole("dialog", { name: title });
    assert.ok(within(dialog).getByText(pair.target));
    assert.equal(within(dialog).queryByText(other.target), null, "only the selected direction's address is shown");
    const close = within(dialog).getAllByRole("button", { name: n.actions.close })
      .find((element) => element.textContent === n.actions.close)!;
    assert.ok(close, "the visible footer Close action exists");
    close.focus();
    assert.ok(document.activeElement !== trigger, "focus has entered the modal before closing");
    if (closeWithEscape) fireEvent.keyDown(close, { key: "Escape", code: "Escape", keyCode: 27 });
    else await user.click(close);
    await waitFor(() => assert.ok(!screen.queryByRole("dialog", { name: title })));
    await waitFor(() => assert.ok(document.activeElement === trigger, "focus returns to the exact pair trigger"));
    assert.ok(!screen.queryByText(pair.target), "closed pair content is destroyed");
  }
});

test("an administrator flips one node's role in place", async () => {
  const loads = stubManagement("admin");
  const calls: unknown[][] = [];
  stub("updateManagementRoles", async (uuids, allow, deny) => { calls.push([uuids, allow, deny]); return { updated: 1 }; });
  wrap(<NetworkDetailPage />, "/networks/0", "/networks/:id");
  await screen.findByRole("link", { name: "gpu-a" });
  await waitFor(() => assert.equal(roleSwitch("gpu-a", "compute").hasAttribute("disabled"), false));
  await userEvent.click(roleSwitch("gpu-a", "compute"));
  await waitFor(() => assert.deepEqual(calls, [[["a"], [], ["compute"]]]));
  await userEvent.click(roleSwitch("gpu-b", "serving"));
  await waitFor(() => assert.deepEqual(calls[1], [["b"], ["serving"], []]));
  await waitFor(() => assert.equal(loads(), 3), "the page reloads after each change");
});

test("an administrator filters by search and label, selects the result and sets roles in bulk", async () => {
  stubManagement("admin");
  const searches: unknown[][] = [];
  stub("searchNetworkTargets", async (id, kind, query) => {
    searches.push([id, kind, query]);
    return { ids: ["a", "b"] };
  });
  const calls: unknown[][] = [];
  stub("updateManagementRoles", async (uuids, allow, deny) => { calls.push([uuids, allow, deny]); return { updated: 2 }; });
  wrap(<NetworkDetailPage />, "/networks/0", "/networks/:id");
  await screen.findByRole("link", { name: "gpu-a" });
  const apply = await screen.findByRole("button", { name: n.management.bulk.apply });
  assert.ok(apply.hasAttribute("disabled"), "nothing selected yet");

  const search = screen.getByRole("searchbox", { name: n.management.search });
  assert.ok(search.closest(".resource-table-toolbar"));
  await userEvent.type(search, "site=lab");
  assert.ok(await screen.findByText("Showing 2 of 3 nodes"));
  assert.deepEqual(searches, [[0, "nodes", "site=lab"]]);
  assert.equal(screen.queryByRole("link", { name: "h200-c" }), null);
  await userEvent.click(screen.getByRole("button", { name: "Select filtered (2)" }));
  assert.ok(screen.getByText("2 nodes selected"));
  assert.ok(apply.hasAttribute("disabled"), "no role chosen yet");

  const choose = async (role: "serving" | "compute", label: string) => {
    await userEvent.click(screen.getByRole("combobox", { name: `${n.management.bulk.title} ${n.role[role]}` }));
    const options = await screen.findAllByRole("option", { name: label });
    await userEvent.click(options[options.length - 1]);
  };
  await choose("compute", n.management.bulk.deny);
  await choose("serving", n.management.bulk.allow);
  await waitFor(() => assert.equal(apply.hasAttribute("disabled"), false));
  await userEvent.click(apply);
  await waitFor(() => assert.equal(screen.getAllByRole("button", { name: n.management.bulk.apply }).length, 2, "the confirmation is open"));
  await userEvent.click(screen.getAllByRole("button", { name: n.management.bulk.apply }).at(-1)!);
  await waitFor(() => assert.deepEqual(calls, [[["a", "b"], ["serving"], ["compute"]]]));
  assert.ok(await screen.findByText("Updated 2 nodes"));
});

test("the label filter narrows the table and a failed bulk change shows the error", async () => {
  stubManagement("admin");
  stub("updateManagementRoles", async () => { throw new Error("node gone"); });
  wrap(<NetworkDetailPage />, "/networks/0", "/networks/:id");
  await screen.findByRole("link", { name: "gpu-a" });
  for (const button of screen.getAllByRole("button", { name: /Show details|Expand row/ })) await userEvent.click(button);
  await screen.findAllByText("site=lab");
  await userEvent.click(screen.getByRole("combobox", { name: n.management.labelFilter }));
  await userEvent.click((await screen.findAllByRole("option", { name: "pool=cpu" })).at(-1)!);
  await userEvent.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByRole("link", { name: "gpu-a" }), null));
  assert.ok(screen.getByRole("link", { name: "gpu-b" }));
  await userEvent.click(screen.getByRole("button", { name: "Select filtered (1)" }));
  await userEvent.click(screen.getByRole("combobox", { name: `${n.management.bulk.title} ${n.role.serving}` }));
  await userEvent.click((await screen.findAllByRole("option", { name: n.management.bulk.allow })).at(-1)!);
  await userEvent.click(screen.getByRole("button", { name: n.management.bulk.apply }));
  await userEvent.click(screen.getAllByRole("button", { name: n.management.bulk.apply }).at(-1)!);
  assert.ok(await screen.findByText(/node gone/));
});

test("an administrator switches a management role off and sees the node's new state", async () => {
  stub("me", async () => ({ role: "admin" }) as never);
  stub("nodeNetworks", async () => nodeNetworks({ behind_nat: true, remote_addr: "203.0.113.7" }) as never);
  const calls: unknown[][] = [];
  stub("setManagementRoles", async (uuid, off) => {
    calls.push([uuid, off]);
    return nodeNetworks({ roles_off: off }) as never;
  });
  wrap(<NodeNetworksCard nodeUUID="uuid-a" />);
  assert.ok(await screen.findByText(n.management.nat));
  assert.ok(screen.getByText("203.0.113.7"));
  assert.ok(screen.getByRole("link", { name: n.management.name }), "the loaded management network is named in the viewer's language");
  const compute = screen.getByRole("switch", { name: n.management.roleSwitch.compute });
  assert.equal(compute.getAttribute("aria-checked"), "true");
  await waitFor(() => assert.equal(compute.getAttribute("aria-disabled"), null));
  await userEvent.click(compute);
  await waitFor(() => assert.deepEqual(calls, [["uuid-a", ["compute"]]]));
  await waitFor(() => assert.equal(screen.getByRole("switch", { name: n.management.roleSwitch.compute }).getAttribute("aria-checked"), "false"));
  await userEvent.click(screen.getByRole("switch", { name: n.management.roleSwitch.compute }));
  await waitFor(() => assert.deepEqual(calls[1], ["uuid-a", []]));
});

test("a failed role change keeps the switch and shows the error", async () => {
  stub("me", async () => ({ role: "admin" }) as never);
  stub("nodeNetworks", async () => nodeNetworks({}) as never);
  stub("setManagementRoles", async () => { throw new Error("database is locked"); });
  wrap(<NodeNetworksCard nodeUUID="uuid-a" />);
  const serving = await screen.findByRole("switch", { name: n.management.roleSwitch.serving });
  await waitFor(() => assert.equal(serving.getAttribute("aria-disabled"), null));
  await userEvent.click(serving);
  assert.ok(await screen.findByText(/database is locked/));
  assert.equal(screen.getByRole("switch", { name: n.management.roleSwitch.serving }).getAttribute("aria-checked"), "true");
});

test("members see the management roles read-only, and an old agent has no address yet", async () => {
  stub("me", async () => ({ role: "member" }) as never);
  stub("nodeNetworks", async () => ({ ...nodeNetworks({ interface: "", ip: "", roles_off: ["storage_sync"] }), networks: [] }) as never);
  wrap(<NodeNetworksCard nodeUUID="uuid-old" />);
  assert.ok(await screen.findByText(n.management.notReported));
  const storage = screen.getByRole("switch", { name: n.management.roleSwitch.storage_sync });
  assert.equal(storage.getAttribute("aria-checked"), "false");
  for (const role of ["serving", "storage_sync", "compute"] as const) {
    assert.equal(screen.getByRole("switch", { name: n.management.roleSwitch[role] }).getAttribute("aria-disabled"), "true");
  }
  assert.equal(screen.queryByText(n.management.nat), null);
});
