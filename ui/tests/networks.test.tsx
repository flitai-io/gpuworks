import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import type { NetworkNode } from "../src/networkTypes";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { closeNodePicker, findNodeOption, unavailable } from "./helpers/nodePicker.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLHtmlElement", "SVGElement", "Element", "Node", "MutationObserver", "NodeFilter", "ShadowRoot", "AbortController", "AbortSignal"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
// The list reads again on server events; this stands in for the shared
// stream so a test can send one.
class Events {
  static listeners = new Map<string, Set<EventListener>>();
  static emit(type: string, data: Record<string, string>) {
    for (const listener of Events.listeners.get(type) ?? []) listener(new dom.window.MessageEvent(type, { data: JSON.stringify(data) }) as unknown as Event);
  }
  addEventListener(type: string, listener: EventListener) {
    const listeners = Events.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    Events.listeners.set(type, listeners);
  }
  removeEventListener(type: string, listener: EventListener) { Events.listeners.get(type)?.delete(listener); }
  close() {}
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });

const load = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const networksLocale = load("networks");
const settingsLocale = load("settings");
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { networks: networksLocale, settings: settingsLocale, common: load("common") }, zh: Object.fromEntries(["networks", "settings", "common"].map(ns => [ns, JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/zh/${ns}.json`, import.meta.url), "utf8"))])) },
  defaultNS: "common",
  ns: ["networks", "settings", "common"],
  interpolation: { escapeValue: false },
});

const { act, cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const apiModule = await import("../src/api.ts");
const NetworksPage = (await import("../src/pages/networks/NetworksPage.tsx")).default;
const NetworkTopologyGuide = (await import("../src/pages/networks/topology/NetworkTopologyGuide.tsx")).default;
const { NetworkDetailRedirect } = await import("../src/pages/networks/networkLinks.tsx");
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

const detailFixture = JSON.parse(fs.readFileSync(new URL("./fixtures/network-detail.json", import.meta.url), "utf8"));
const typeRoles = {
  host_ethernet: ["storage_sync", "compute", "serving"],
  host_rdma: ["storage_sync", "compute", "serving"],
  accelerator_rdma: ["compute"],
  accelerator_hccs: ["compute"],
};
const n = networksLocale;

type API = typeof apiModule.api;
const originals: Partial<API> = {};
function stub<K extends keyof API>(key: K, impl: API[K]) {
  if (!(key in originals)) (originals as Record<string, unknown>)[key] = apiModule.api[key];
  apiModule.api[key] = impl;
}
after(() => { closeChannels(); dom.window.close(); });
afterEach(async () => {
  cleanup();
  await i18n.changeLanguage("en");
  for (const [key, value] of Object.entries(originals)) (apiModule.api as Record<string, unknown>)[key] = value;
  for (const key of Object.keys(originals)) delete (originals as Record<string, unknown>)[key];
});

const { useLocation } = await import("react-router-dom");
function Where() { const l = useLocation(); return <div data-testid="where">{l.pathname}{l.search}</div>; }

function renderAt(path: string, element: React.ReactElement, pattern = path) {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path={pattern} element={<>{element}<Where /></>} />
          <Route path="/networks/guide" element={<NetworkTopologyGuide />} />
          <Route path="/networks/:id" element={<div>detail route</div>} />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

// A node's bootstrap and HCAs sit in its row's expanded detail: open every
// node row, then read a row's detail from the row after it.
async function expandNodeRows() {
  for (const button of await screen.findAllByRole("button", { name: /Show details|Expand row/ })) await userEvent.click(button);
}
const detailOf = (row: HTMLElement) => row.nextElementSibling as HTMLElement;
// The detail is a drawer, itself a dialog: a modal is the other one.
const modalDialog = () => waitFor(() => {
  const found = screen.getAllByRole("dialog").find((el) => el.getAttribute("data-placement") === "center" || el.classList.contains("ant-modal") || el.closest(".ant-modal"));
  assert.ok(found, "the modal is open");
  return found as HTMLElement;
});

function listItem(overrides: Record<string, unknown> = {}) {
  const { nodes: _nodes, role_options: _r, type_options: _t, type_roles: _tr, test_supported: _s, latest_test: _l, ...network } = detailFixture;
  return { ...network, ...overrides };
}

test("network list shows the empty state and the fallback note", async () => {
  stub("networks", async () => ({ items: [], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }));
  renderAt("/networks", <NetworksPage />);
  await waitFor(() => assert.ok(screen.getByText(n.empty)));
  assert.ok(screen.getByText(n.fallbackNote));
  assert.equal(screen.queryByRole("group", { name: n.topology.diagram.simpleLabel }), null);
  await userEvent.click(screen.getByRole("link", { name: n.topology.guide }));
  assert.ok(screen.getByRole("heading", { name: n.topology.guide }));
  assert.ok(screen.getByRole("group", { name: n.topology.diagram.simpleLabel }));
  await userEvent.click(screen.getByRole("link", { name: n.title }));
  await waitFor(() => assert.ok(screen.getByText(n.empty)));
});

test("invalid network IDs open no detail, while zero opens the management network", async () => {
  const asked: number[] = [];
  stub("networks", async () => ({ items: [], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }));
  stub("network", async (id: number) => { asked.push(id); return { ...detailFixture, id }; });
  for (const raw of ["", " ", "invalid", "-1", "0x0", "1.5", "3e2", "9007199254740993"]) {
    renderAt(`/networks?network=${encodeURIComponent(raw)}`, <NetworksPage />, "/networks");
    await waitFor(() => assert.ok(screen.getByText(n.empty)));
    assert.deepEqual(asked, [], `invalid id "${raw}" must not request a resource`);
    assert.equal(screen.queryByRole("dialog"), null);
    cleanup();
  }
  renderAt("/networks?network=0", <NetworksPage />, "/networks");
  await waitFor(() => assert.deepEqual(asked, [0]));
});

test("the resource name reopens its network after switching from Chinese to English", async () => {
  const name = "inference-fabric";
  stub("networks", async () => ({ items: [listItem({ name })], roles: detailFixture.role_options, types: detailFixture.type_options, type_roles: typeRoles }));
  stub("network", async () => ({ ...detailFixture, name }));
  await i18n.changeLanguage("zh");
  const user = userEvent.setup();
  renderAt("/networks", <NetworksPage />);
  await user.click(await screen.findByRole("link", { name }));
  const drawer = await screen.findByRole("dialog");
  await within(drawer).findByRole("link", { name: "h20-07" });
  await user.click(within(drawer).getByRole("button", { name: i18n.t("common:actions.close") }));
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0));
  await act(async () => { await i18n.changeLanguage("en"); });
  await user.click(await screen.findByRole("link", { name }));
  const reopened = await screen.findByRole("dialog");
  assert.ok(await within(reopened).findByRole("tab", { name: n.tabs.test }));
  assert.ok(within(reopened).getByRole("link", { name: "h20-07" }));
  assert.equal(screen.getByTestId("where").textContent, `/networks?network=${detailFixture.id}`);
});

test("an old detail address opens the list's drawer on the tab it named", async () => {
  stub("networks", async () => ({ items: [listItem()], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }));
  stub("network", async () => detailFixture);
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[`/networks/${detailFixture.id}?tab=test`]}>
        <Routes>
          <Route path="/networks/:id" element={<NetworkDetailRedirect />} />
          <Route path="/networks" element={<><NetworksPage /><Where /></>} />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
  await waitFor(() => assert.equal(screen.getByTestId("where").textContent, `/networks?network=${detailFixture.id}&tab=test`));
  const tab = await screen.findByRole("tab", { name: n.tabs.test });
  await waitFor(() => assert.equal(tab.getAttribute("aria-selected"), "true"));
});

test("the overview shows attachment sources on Ethernet and keeps unmatched attachments in management", async () => {
  const attachments = [...detailFixture.attachments, { id: 99, target: "label", label_key: "rack", label_value: "unassigned", hcas: [], created_at: detailFixture.created_at }];
  const network = { ...detailFixture, type: "host_ethernet", attachments };
  stub("network", async () => network);
  const removed: number[] = [];
  stub("detachNetwork", async (_id, attachmentID) => {
    removed.push(attachmentID);
    return { ...network, attachments: detailFixture.attachments };
  });
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  const manage = await screen.findByRole("button", { name: n.detail.manageLabels });
  assert.equal(screen.queryByRole("dialog", { name: n.detail.manageLabels }), null);
  const section = screen.getByRole("heading", { name: "Loaded nodes (2)" }).closest("section")!;
  assert.ok(within(section).getByText("2 labels"));
  assert.ok(within(section).getByRole("table"));
  const nodeToolbar = within(section).getByRole("searchbox", { name: n.detail.searchEthernetNodes }).closest(".resource-table-toolbar");
  assert.ok(nodeToolbar);
  assert.ok(within(nodeToolbar as HTMLElement).getByRole("button", { name: n.detail.attachNodes }));
  assert.equal(screen.queryByText("rack=unassigned"), null);
  assert.ok(screen.getByRole("columnheader", { name: n.nodeTable.source }));
  const direct = screen.getByRole("link", { name: "h20-07" }).closest("tr") as HTMLElement;
  assert.ok(within(direct).getByText(n.detail.directNode));
  const actions = within(direct).getByRole("button", { name: n.actions.edit }).closest("td")!;
  assert.equal(actions, direct.lastElementChild);
  assert.equal(actions.style.position, "sticky");
  const byLabel = screen.getByRole("link", { name: "h20-05" }).closest("tr") as HTMLElement;
  assert.ok(within(byLabel).getByText("Label pool=h20-a"));
  const toggle = within(byLabel).getByRole("button", { name: /Show details|Expand row/ });
  assert.equal(toggle.closest("td"), byLabel.firstElementChild);
  await userEvent.click(within(byLabel).getByText("Label pool=h20-a"));
  assert.equal(toggle.getAttribute("aria-expanded"), "true");
  assert.ok(within(detailOf(byLabel)).getByText("bond0 · 10.20.1.15"));

  await userEvent.click(manage);
  const labels = await screen.findByRole("dialog", { name: n.detail.manageLabels });
  const key = within(labels).getByRole("textbox", { name: n.attach.labelKey }) as HTMLInputElement;
  const value = within(labels).getByRole("textbox", { name: n.attach.labelValue }) as HTMLInputElement;
  assert.equal(key.value, "");
  const unmatched = within(labels).getByText("rack=unassigned").closest("tr") as HTMLElement;
  await userEvent.click(within(unmatched).getByRole("button", { name: n.actions.edit }));
  assert.equal(key.value, "rack");
  assert.equal(value.value, "unassigned");
  assert.ok(within(labels).getByRole("button", { name: n.actions.save }));
  await userEvent.click(within(unmatched).getByRole("button", { name: n.actions.detach }));
  const popover = await waitFor(() => {
    const element = document.querySelector<HTMLElement>('[data-slot="popover-content"]');
    assert.ok(element);
    return within(element);
  });
  await userEvent.click(popover.getByRole("button", { name: n.actions.detach }));
  await waitFor(() => assert.deepEqual(removed, [99]));
  assert.equal(within(labels).queryByText("rack=unassigned"), null);
  assert.equal(key.value, "");
  await userEvent.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByRole("dialog", { name: n.detail.manageLabels }), null));
});

test("an unresolved direct attachment keeps its overrides and can be detached without removing a resolved node", async () => {
  const orphan = { ...detailFixture.attachments.find((a: { target: string }) => a.target === "node"),
    id: 99, node_uuid: "unresolved-uuid", node_name: "", interface: "bond9", ip: "192.0.2.9", hcas: ["mlx5_9:1"] };
  stub("network", async () => ({ ...detailFixture, attachments: [...detailFixture.attachments, orphan] }));
  const removed: number[][] = [];
  stub("detachNetwork", async (id, attachmentID) => {
    removed.push([id, attachmentID]);
    return detailFixture;
  });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  const label = i18n.t("networks:detail.unresolvedAttachments", { count: 1 });
  const toggle = await screen.findByRole("button", { name: label });
  assert.equal(toggle.getAttribute("aria-expanded"), "false");
  await user.click(toggle);
  const row = screen.getByText("unresolved-uuid").closest("tr")!;
  assert.ok(within(row).getByText(/bond9 · 192\.0\.2\.9 · mlx5_9:1/));
  assert.ok(within(row).getByRole("button", { name: n.actions.edit }));
  await user.click(within(row).getByRole("button", { name: n.actions.detach }));
  assert.deepEqual(removed, []);
  const prompt = await screen.findByText(n.actions.confirmDetach);
  const popover = prompt.closest<HTMLElement>('[data-slot="popover-content"]')!;
  await user.click(within(popover).getByRole("button", { name: n.actions.detach }));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: label }), null));
  assert.deepEqual(removed, [[detailFixture.id, orphan.id]]);
  assert.ok(screen.getByRole("heading", { name: "Loaded nodes (2)" }));
  assert.ok(screen.getByRole("link", { name: "h20-07" }));
});

test("an older test poll cannot restore direct attachment actions after a label takes over", async (t) => {
  t.mock.timers.enable({ apis: ["setInterval"] });
  const direct = detailFixture.attachments.find((a: { target: string }) => a.target === "node");
  const label = detailFixture.attachments.find((a: { target: string }) => a.target === "label");
  const running = { ...detailFixture, latest_test: { ...detailFixture.latest_test, status: "running" } };
  const sourceByLabel = {
    ...running,
    attachments: [label],
    nodes: running.nodes.map((node: NetworkNode) => node.node_uuid === direct.node_uuid
      ? { ...node, source: "label", attachment_id: label.id, label_key: label.label_key, label_value: label.label_value }
      : node),
  };
  const oldRead = Promise.withResolvers<typeof running>();
  let reads = 0;
  stub("network", async () => ++reads === 1 ? running : oldRead.promise);
  const removed: number[][] = [];
  stub("detachNetwork", async (id, attachmentID) => { removed.push([id, attachmentID]); return sourceByLabel; });

  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  const nodeRow = () => screen.getByRole("link", { name: direct.node_name }).closest("tr")!;
  await screen.findByRole("link", { name: direct.node_name });
  assert.ok(within(nodeRow()).getByText(n.detail.directNode));
  act(() => t.mock.timers.tick(2000));
  assert.equal(reads, 2, "the poll is in flight before the user removes the direct attachment");
  await userEvent.click(within(nodeRow()).getByRole("button", { name: n.actions.detach }));
  const confirm = (await screen.findByText(n.actions.confirmDetach)).closest<HTMLElement>('[data-slot="popover-content"]')!;
  await userEvent.click(within(confirm).getByRole("button", { name: n.actions.detach }));
  await within(nodeRow()).findByText("Label pool=h20-a");
  await act(async () => oldRead.resolve(running));

  assert.deepEqual(removed, [[running.id, direct.id]]);
  assert.ok(within(nodeRow()).getByText("Label pool=h20-a"));
  assert.equal(within(nodeRow()).queryByRole("button", { name: n.actions.edit }), null);
  assert.equal(within(nodeRow()).queryByRole("button", { name: n.actions.detach }), null);
  assert.ok(screen.getByRole("heading", { name: "Loaded nodes (2)" }));
  await userEvent.click(screen.getByRole("button", { name: n.detail.manageLabels }));
  const manager = await screen.findByRole("dialog", { name: n.detail.manageLabels });
  assert.ok(within(manager).getByText("pool=h20-a"));
});

test("a delayed detach result belongs to the network where the action started", async () => {
  const response = Promise.withResolvers<typeof detailFixture>();
  const requested: number[] = [];
  stub("network", async (id) => ({ ...detailFixture, id, name: `network-${id}` }));
  stub("detachNetwork", async (id) => { requested.push(id); return response.promise; });
  const user = userEvent.setup();
  const drawer = (id: number) => (
    <I18nextProvider i18n={i18n}><MemoryRouter>
      <NetworkDetailDrawer id={id} onClose={() => {}} onChanged={() => {}} onTab={() => {}} />
    </MemoryRouter></I18nextProvider>
  );
  const view = render(drawer(1));
  const direct = (await screen.findByRole("link", { name: "h20-07" })).closest("tr")!;
  await user.click(within(direct).getByRole("button", { name: n.actions.detach }));
  const confirm = (await screen.findByText(n.actions.confirmDetach)).closest<HTMLElement>('[data-slot="popover-content"]')!;
  await user.click(within(confirm).getByRole("button", { name: n.actions.detach }));
  assert.deepEqual(requested, [1]);
  view.rerender(drawer(2));
  await screen.findByText("network-2");
  await act(async () => response.resolve(detailFixture));
  assert.ok(screen.getByText("network-2"));
  assert.ok(screen.getByRole("link", { name: "h20-07" }));
});

test("detaching in the drawer reads the list again, so its attachments column follows", async () => {
  let listReads = 0;
  stub("networks", async () => { listReads++; return { items: [listItem()], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }; });
  stub("network", async () => detailFixture);
  stub("detachNetwork", async () => ({ ...detailFixture, attachments: detailFixture.attachments.slice(1) }));
  renderAt(`/networks?network=${detailFixture.id}`, <NetworksPage />, "/networks");
  const direct = (await screen.findByRole("link", { name: "h20-07" })).closest("tr")!;
  const detach = within(direct).getByRole("button", { name: n.actions.detach });
  const before = listReads;
  fireEvent.click(detach);
  const ok = await waitFor(() => {
    const found = [...document.querySelectorAll('[data-slot="popover-content"]')].flatMap((pop) => within(pop as HTMLElement).queryAllByRole("button", { name: n.actions.detach }));
    if (!found.length) throw new Error("no confirm");
    return found.at(-1) as HTMLElement;
  });
  fireEvent.click(ok);
  await waitFor(() => assert.ok(listReads > before, "the list is read again"));
});

test("the list reads again on network and node changes but not on a node's routine status report", async () => {
  let listReads = 0;
  stub("networks", async () => { listReads++; return { items: [listItem()], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }; });
  renderAt("/networks", <NetworksPage />);
  await screen.findByRole("link", { name: detailFixture.name });
  await waitFor(() => assert.equal(listReads, 1));

  Events.emit("node", { key: "", reason: "status" });
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.equal(listReads, 1, "a status report does not read the list");

  Events.emit("node", { key: "n1", reason: "labels" });
  await waitFor(() => assert.equal(listReads, 2, "a node change reads the list"));
  Events.emit("network", { key: "1", reason: "attached" });
  await waitFor(() => assert.equal(listReads, 3, "a network change reads the list"));
});

test("network list surfaces load errors", async () => {
  stub("networks", async () => { throw new Error("boom 500"); });
  renderAt("/networks", <NetworksPage />);
  await waitFor(() => assert.ok(screen.getByText(/boom 500/)));
});

test("network list renders roles, type, selector, attachments and a warning summary", async () => {
  stub("networks", async () => ({
    items: [listItem(), listItem({ id: 2, name: "storage", roles: ["storage_sync", "serving"], type: "host_ethernet", cidr: "", attachments: [], warning_node_count: 0, resolved_node_count: 0 })],
    roles: ["storage_sync", "compute", "serving"],
    types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles,
  }));
  renderAt("/networks", <NetworksPage />);
  await waitFor(() => assert.ok(screen.getByRole("link", { name: "ib-a" })));
  const row = screen.getByRole("link", { name: "ib-a" }).closest("tr") as HTMLElement;
  assert.ok(within(row).getByText(n.role.compute));
  assert.ok(within(row).getByText(n.type.host_rdma));
  assert.ok(within(row).getByText("Bootstrap CIDR 10.20.0.0/16"));
  assert.ok(within(row).getByText("pool=h20-a"));
  assert.ok(within(row).getByText("h20-07"));
  assert.ok(within(row).getByRole("img", { name: "2 node(s) have warnings; open the details to see why." }));
  const other = screen.getByRole("link", { name: "storage" }).closest("tr") as HTMLElement;
  assert.equal(within(other).queryByRole("img", { name: /warnings/ }), null, "no warning mark without warnings");
  assert.ok(within(other).getByText(n.selector.none));
});

test("large attachment lists show direct node and label counts separately", async () => {
  const nodes = Array.from({ length: 64 }, (_, i) => ({ id: i + 1, target: "node", node_name: `worker-${i}` }));
  const label = { id: 65, target: "label", label_key: "pool", label_value: "training" };
  stub("networks", async () => ({
    items: [listItem({ attachments: [...nodes, label], resolved_node_count: 100 })],
    roles: ["compute"], types: ["host_rdma"], type_roles: typeRoles,
  }));
  renderAt("/networks", <NetworksPage />);
  const summary = await screen.findByRole("link", { name: "64 nodes · 1 label" });
  assert.equal(summary.getAttribute("href"), `/networks?network=${detailFixture.id}`);
  assert.equal(screen.queryByText("worker-0"), null);
  assert.ok(screen.getByText("100"));
});

test("creating a network sends the server dictionary values and opens the detail", async () => {
  const bodies: unknown[] = [];
  stub("networks", async () => ({ items: [], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }));
  stub("createNetwork", async (body) => { bodies.push(body); return { ...detailFixture, id: 7 }; });
  const user = userEvent.setup();
  renderAt("/networks", <NetworksPage />);
  await user.click(await screen.findByRole("button", { name: n.actions.create }));
  const dialog = await modalDialog();
  assert.ok(within(dialog).getByRole("button", { name: i18n.t("actions.close") }));
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  await waitFor(() => assert.ok(within(dialog).getByText(n.form.nameRequired)));
  const roleGroup = within(dialog).getByRole("group", { name: new RegExp(n.form.roles) });
  assert.equal(roleGroup.getAttribute("aria-required"), "true");
  assert.equal(roleGroup.getAttribute("aria-invalid"), "true");
  assert.deepEqual((roleGroup.getAttribute("aria-describedby") ?? "").split(" "), [`${roleGroup.id}-description`, `${roleGroup.id}-error`]);
  assert.equal(bodies.length, 0, "validation blocks the request");

  await user.type(within(dialog).getByRole("textbox", { name: n.form.name }), "ib-a");
  await user.click(within(dialog).getByRole("checkbox", { name: n.role.compute }));
  assert.ok(within(dialog).getByText(n.form.typeHint));
  assert.equal(within(dialog).queryByText(n.form.typeHelp), null, "what each type means waits behind the help mark");
  await user.click(within(dialog).getByText(n.type.host_rdma));
  // An RDMA network asks for nothing: the interface glob is gone and the
  // bootstrap CIDR is an advanced option.
  assert.ok(within(dialog).getByText(n.form.rdmaInferred));
  assert.equal(within(dialog).queryByLabelText(n.form.interface), null);
  await user.click(within(dialog).getByText(n.form.advanced));
  await user.type(within(dialog).getByLabelText(n.form.bootstrapCidr), "10.20.0.0/16");
  within(dialog).getByRole("textbox", { name: n.form.name }).focus();
  await user.keyboard("{Enter}");
  assert.equal(bodies.length, 0, "Enter in a field does not create the network");
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  // The created network opens in the drawer over the list.
  await waitFor(() => assert.match(screen.getByTestId("where").textContent ?? "", /\?network=\d+/));
  assert.deepEqual(bodies, [{ name: "ib-a", description: "", roles: ["compute"], type: "host_rdma", cidr: "10.20.0.0/16", interface: "" }]);
});

test("an RDMA network is created with nothing filled in", async () => {
  const bodies: unknown[] = [];
  stub("networks", async () => ({ items: [], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }));
  stub("createNetwork", async (body) => { bodies.push(body); return { ...detailFixture, id: 7 }; });
  const user = userEvent.setup();
  renderAt("/networks", <NetworksPage />);
  await user.click(await screen.findByRole("button", { name: n.actions.create }));
  const dialog = await modalDialog();
  await user.type(within(dialog).getByRole("textbox", { name: n.form.name }), "weiw1");
  await user.click(within(dialog).getByRole("checkbox", { name: n.role.compute }));
  await user.click(within(dialog).getByText(n.type.host_rdma));
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0], { name: "weiw1", description: "", roles: ["compute"], type: "host_rdma", cidr: "", interface: "" });
});

for (const networkType of ["accelerator_rdma", "accelerator_hccs"] as const) {
  test(`an accelerator network carries compute traffic only and asks for a bootstrap subnet at most (${networkType})`, async () => {
    const bodies: unknown[] = [];
    stub("networks", async () => ({ items: [], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma", "accelerator_hccs"], type_roles: typeRoles }));
    stub("createNetwork", async (body) => { bodies.push(body); return { ...detailFixture, id: 7 }; });
    const user = userEvent.setup();
    renderAt("/networks", <NetworksPage />);
    await user.click(await screen.findByRole("button", { name: n.actions.create }));
    const dialog = await modalDialog();
    await user.type(within(dialog).getByRole("textbox", { name: n.form.name }), "npu");
    // On a host network every role can be ticked.
    await user.click(within(dialog).getByRole("checkbox", { name: n.role.storage_sync }));
    await user.click(within(dialog).getByRole("checkbox", { name: n.role.compute }));
    assert.equal((within(dialog).getByRole("checkbox", { name: n.role.serving }).getAttribute("aria-disabled") === "true"), false);

    await user.click(within(dialog).getByText(n.type[networkType]));
    // Storage sync and serving are greyed out and say why; the role already
    // ticked that the type cannot carry is dropped.
    await waitFor(() => assert.equal((within(dialog).getByRole("checkbox", { name: n.role.storage_sync }).getAttribute("aria-disabled") === "true"), true));
    assert.equal((within(dialog).getByRole("checkbox", { name: n.role.serving }).getAttribute("aria-disabled") === "true"), true);
    assert.equal(within(dialog).getByRole("checkbox", { name: n.role.storage_sync }).getAttribute("aria-checked") == "true", false);
    assert.equal((within(dialog).getByRole("checkbox", { name: n.role.compute }).getAttribute("aria-checked") === "true"), true);
    assert.ok(within(dialog).getByLabelText(`${n.role.serving}: ${i18n.t("networks:form.roleNotOnType")}`), "the reason is reachable by keyboard and screen reader");
    assert.ok(within(dialog).getByText(n.form.acceleratorInferred));
    assert.equal(within(dialog).queryByLabelText(n.form.interface), null);
    await user.click(within(dialog).getByText(n.form.advanced));
    assert.ok(within(dialog).getByText(n.form.acceleratorBootstrapCidrHint));
    await user.type(within(dialog).getByLabelText(n.form.bootstrapCidr), "198.51.100.0/24");
    await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
    await waitFor(() => assert.equal(bodies.length, 1));
    assert.deepEqual(bodies[0], { name: "npu", description: "", roles: ["compute"], type: networkType, cidr: "198.51.100.0/24", interface: "" });
  });

}

for (const networkType of ["accelerator_rdma", "accelerator_hccs"] as const) {
  test(`${networkType} shows the card IPs, diagnostics and actions for its fabric`, async () => {
    const hccs = networkType === "accelerator_hccs";
    const fabric = hccs ? "HCCS" : "RoCE";
    const layer = hccs ? "hccs" : "roce";
    const subnet = hccs ? "192.0.2" : "203.0.113";
    const calls: unknown[] = [];
    const npu = {
      ...detailFixture, type: networkType,
      attachments: detailFixture.attachments.map((a: { target: string }) => a.target === "node" ? { ...a, hcas: ["npu3"] } : a),
      nodes: detailFixture.nodes.map((node: { node_name: string }, i: number) => ({
        ...node, link_layer: layer,
        warnings: [{ code: "hca_unusable", params: { value: "npu3" } }],
        hcas: [{ name: "npu3", ip: `${subnet}.${i === 0 ? 59 : 82}`, netdev: "", link_layer: layer, state: "DOWN", gap: "inactive", speed_gbps: 0 }],
      })),
    };
    stub("network", async () => npu);
    stub("nodeNetworks", async () => ({
      networks: [], inventory_reported: true, interfaces: [], serving_forward: null, management: { interface: "", ip: "", roles_off: [], behind_nat: false },
      rdma_ports: [{ device: "mlx5_0", port: 1, state: "ACTIVE", link_layer: "InfiniBand", rate_gbps: 400, netdev: "", ip: "" }],
      accelerator_ports: [
        { name: "npu3", index: 3, ip: "203.0.113.59", netmask: "255.255.0.0", state: "UP", speed_gbps: 200 },
        { name: "npu4", index: 4, hccs_ip: "192.0.2.60", ip: "203.0.113.60", netmask: "255.255.0.0", state: "UP", speed_gbps: 200 },
        { name: "npu6", index: 6, hccs_ip: "192.0.2.62", hccs_gap: "inactive", ip: "203.0.113.62", netmask: "255.255.0.0", state: "DOWN", speed_gbps: 0, gap: "inactive" },
        { name: "npu7", index: 7, hccs_gap: "no_ipv4", ip: hccs ? "203.0.113.63" : "", netmask: "", state: "UP", speed_gbps: 200, gap: "no_ipv4" },
      ],
    }));
    stub("editNetworkHCAs", async (id, body) => { calls.push([id, body]); return npu; });
    const user = userEvent.setup();
    renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
    await waitFor(() => assert.ok(screen.getByText("Loaded nodes (2)")));
    assert.ok(screen.getByText(n.detail.acceleratorEditHint));
    assert.equal(screen.queryByText(n.detail.acceleratorInferredHelp), null, "how rails are picked waits behind the help mark");
    assert.ok(screen.getAllByText(n.type[networkType]).length > 0);
    assert.ok(screen.getByRole("columnheader", { name: n.nodeTable.bootstrapCards }));
    assert.ok(screen.getByRole("searchbox", { name: n.detail.searchCardNodes }));
    const warning = i18n.t("networks:cardWarning.hca_unusable", { value: "npu3", fabric });
    const nodeWarnings = screen.getAllByRole("img", { name: warning });
    for (const mark of nodeWarnings) assert.equal(mark.style.color, "var(--warning)");
    await expandNodeRows();
    const row = screen.getByText(`${subnet}.59`).closest("tr") as HTMLElement;
    const cards = within(row).getByRole("list", { name: i18n.t("networks:nodeTable.cardAddresses", { fabric }) });
    assert.equal(within(cards).getByRole("img", { name: n.rail.gap.inactive }).style.color, "var(--warning)");
    assert.equal(within(row).queryByText(fabric, { exact: true }), null, "the field label already names the fabric");
    assert.ok(screen.getByText(`${n.node.card.name} · npu3`));
    await user.click(within(row).getByRole("button", { name: i18n.t("networks:rail.remove", { hca: "npu3" }) }));
    assert.ok(await screen.findByText(i18n.t("networks:rail.removeCardTitle", { hca: "npu3" })));
    await user.click(screen.getByRole("button", { name: n.actions.cancel }));
    await user.click(within(row).getByRole("button", { name: new RegExp(n.rail.addCard) }));
    // Cards are offered, not the host's RDMA ports; the one in use is left out
    // and a card that cannot work says why.
    assert.ok(await screen.findByText(`npu4 · ${subnet}.60`));
    assert.ok(screen.getByText(`npu6 · ${subnet}.62 · ${n.rail.gap.inactive}`));
    assert.ok(screen.getByText(`npu7 · — · ${i18n.t("networks:rail.cardMissingIP", { fabric })}`));
    assert.equal(screen.queryByText(/^npu3 ·/), null);
    assert.equal(screen.queryByText(/^mlx5_0:1/), null);
    await user.click(screen.getByText(`npu4 · ${subnet}.60`));
    await waitFor(() => assert.equal(calls.length, 1));
    assert.deepEqual((calls[0] as [number, { op: string; hca: string }])[1].hca, "npu4");
  });
}

test("network detail lists each node's bootstrap and HCAs with their IPs", async () => {
  stub("network", async () => detailFixture);
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Loaded nodes (2)")));
  assert.ok(screen.getByText(n.detail.rdmaEditHint));
  assert.equal(screen.queryByText(n.detail.rdmaInferredHelp), null, "how HCAs are picked is not on the page");
  await userEvent.setup().hover(screen.getByRole("button", { name: "More information" }));
  assert.equal((await screen.findByRole("tooltip")).textContent, n.detail.rdmaInferredHelp);
  assert.equal(screen.queryByRole("columnheader", { name: n.nodeTable.endpoint }), null, "RDMA shows bootstrap, not interface / IP");
  const collapsed = screen.getByRole("link", { name: "h20-05" }).closest("tr")!;
  assert.match(collapsed.textContent ?? "", /10\.20\.1\.15/);
  assert.match(collapsed.textContent ?? "", /4 HCA ports/i);
  assert.equal(screen.queryByText("mlx5_3:1"), null);
  await expandNodeRows();
  assert.ok(screen.getAllByText(n.nodeTable.bootstrap).length > 0, "each node's detail names its bootstrap");
  const h5 = screen.getByRole("link", { name: "h20-05" }).closest("tr")!;
  const h5Detail = detailOf(h5);
  assert.ok(within(h5).getByRole("img", { name: "Several Compute networks reach this node at the same rank. ib-a was attached first and is used; not used: legacy. Attach one of them to this node directly to choose explicitly." }));
  assert.ok(within(h5).getByText("Label pool=h20-a"));
  assert.ok(within(h5Detail).getByText("bond0 · 10.20.1.15"));
  for (const hca of ["mlx5_0:1", "mlx5_1:1", "mlx5_2:1", "mlx5_3:1"]) assert.ok(within(h5Detail).getByText(hca));
  // InfiniBand without IPoIB has no HCA IP.
  assert.equal(within(h5Detail).getAllByText("—").length, 4);
  assert.equal(within(h5Detail).getAllByText(n.rail.auto).length, 2, "inferred bootstrap and HCAs");
  const resolved = screen.getByText("bond0 · 10.20.1.17").closest("tr") as HTMLElement;
  assert.ok(resolved);
  // The node's own warning is on its name, in the row above its detail.
  assert.ok(within(resolved.previousElementSibling as HTMLElement).getByRole("img", { name: "Override mlx5_9:1 (hca) is not in the node's last reported inventory." }));
  assert.ok(within(resolved).getByRole("img", { name: n.rail.gap.unknown }));
  assert.equal(within(resolved).getAllByText(n.rail.manual).length, 2);
  assert.match(resolved.textContent ?? "", /mlx5_0(?!:1)/, "the configured HCA remains visible alongside the resolved port");
});

test("removing an HCA asks whether it leaves every node or one", async () => {
  const calls: unknown[] = [];
  stub("network", async () => detailFixture);
  stub("editNetworkHCAs", async (id, body) => { calls.push([id, body]); return detailFixture; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Loaded nodes (2)")));
  await expandNodeRows();
  const h5 = detailOf(screen.getByRole("link", { name: "h20-05" }).closest("tr")!);
  await user.click(within(h5).getByRole("button", { name: "Remove mlx5_2:1" }));
  await user.click(await screen.findByRole("button", { name: n.rail.confirm }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [1, { op: "remove", hca: "mlx5_2:1", node_uuid: undefined }], "every node by default");

  await user.click(within(h5).getByRole("button", { name: "Remove mlx5_3:1" }));
  await user.click(await screen.findByRole("checkbox", { name: /From h20-05 only/ }));
  await user.click(screen.getAllByRole("button", { name: n.rail.confirm }).at(-1) as HTMLElement);
  await waitFor(() => assert.equal(calls.length, 2));
  assert.deepEqual(calls[1], [1, { op: "remove", hca: "mlx5_3:1", node_uuid: "uuid-h5" }]);
});

test("adding an HCA offers the node's other ports and reset goes back to auto", async () => {
  const calls: unknown[] = [];
  stub("network", async () => detailFixture);
  stub("nodeNetworks", async () => ({
    networks: [], inventory_reported: true, interfaces: [], serving_forward: null, management: { interface: "", ip: "", roles_off: [], behind_nat: false },
    rdma_ports: [
      { device: "mlx5_0", port: 1, state: "ACTIVE", link_layer: "InfiniBand", rate_gbps: 400, netdev: "", ip: "" },
      { device: "mlx5_bond_0", port: 1, state: "ACTIVE", link_layer: "Ethernet", rate_gbps: 25, netdev: "ens3f0", ip: "", gap: "no_ipv4" },
    ],
  }));
  stub("editNetworkHCAs", async (id, body) => { calls.push([id, body]); return detailFixture; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Loaded nodes (2)")));
  await expandNodeRows();
  const h7 = screen.getByText("bond0 · 10.20.1.17").closest("tr") as HTMLElement;
  await user.click(within(h7).getByRole("button", { name: new RegExp(n.rail.add) }));
  // The ports already in use are not offered; a port that cannot work says why.
  const option = await screen.findByText(`mlx5_bond_0:1 · — · ${n.rail.gap.no_ipv4}`);
  assert.equal(screen.queryByText(/^mlx5_0:1 ·/), null);
  await user.click(option);
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [1, { op: "add", hca: "mlx5_bond_0:1", node_uuid: "uuid-h7" }]);
  // In the detail the bootstrap and the HCAs are entries of one cell; reset the HCAs' own.
  const railsCell = within(h7).getByText("mlx5_9:1").closest("dd") as HTMLElement;
  // The button's loading icon may still be fading out after the add.
  await user.click(within(railsCell).getByRole("button", { name: new RegExp(n.rail.reset) }));
  await waitFor(() => assert.equal(calls.length, 2));
  assert.deepEqual(calls[1], [1, { op: "reset", node_uuid: "uuid-h7" }]);
});

test("the bootstrap can be pinned to one of the node's interfaces", async () => {
  const calls: unknown[] = [];
  stub("network", async () => detailFixture);
  stub("nodeNetworks", async () => ({
    networks: [], inventory_reported: true, rdma_ports: [], serving_forward: null, management: { interface: "", ip: "", roles_off: [], behind_nat: false },
    interfaces: [{ name: "bond0", state: "up", mtu: 9000, speed_mbps: 200000, mac: "", addrs: ["10.20.1.15/16"] }, { name: "eth9", state: "down", mtu: 1500, speed_mbps: 0, mac: "", addrs: [] }],
  }));
  stub("attachNetwork", async (id, body) => { calls.push([id, body]); return detailFixture; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Loaded nodes (2)")));
  // h20-05 is on the network by label: pinning its bootstrap attaches the node.
  await expandNodeRows();
  const h5 = detailOf(screen.getByRole("link", { name: "h20-05" }).closest("tr")!);
  await user.click(within(h5).getByRole("button", { name: n.rail.changeBootstrap }));
  const option = await screen.findByText("bond0 · 10.20.1.15/16");
  assert.equal(screen.queryByText(/^eth9/), null, "interfaces without an address are not offered");
  await user.click(option);
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [1, { node_uuid: "uuid-h5", interface: "bond0" }]);
});

test("warning reasons are reachable from the keyboard", async () => {
  stub("network", async () => detailFixture);
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Loaded nodes (2)")));
  const reason = "Override mlx5_9:1 (hca) is not in the node's last reported inventory.";
  const mark = screen.getByRole("img", { name: reason });
  for (let i = 0; i < 120 && document.activeElement !== mark; i++) await user.tab();
  assert.equal(document.activeElement, mark, "the warning mark is in the tab order");
  // Tabbing past the earlier mark leaves its tooltip mounted but hidden.
  await waitFor(() => assert.ok(screen.getAllByRole("tooltip").some((tip) => tip.textContent === reason)));
});

// stubNodes answers the attach dialog's picker the way GET /nodes does with
// page parameters (one searched page) and GET /nodes/{uuid}; listing every
// node is refused.
function stubNodes(list: Array<{ uuid: string; name?: string; hostname?: string }>) {
  stub("nodes", async () => { throw new Error("the attach dialog fetched every node"); });
  stub("nodesPage", async (query: { q?: string }) => {
    const q = (query.q ?? "").toLowerCase();
    const items = list.filter((node) => !q || (node.name ?? "").toLowerCase().includes(q) || node.uuid.includes(q));
    return { items, page: 1, page_size: 50, total: items.length, all: list.length, labels: [] } as never;
  });
  stub("node", async (uuid: string) => {
    const node = list.find((item) => item.uuid === uuid);
    if (!node) throw new Error("not found");
    return node as never;
  });
}

test("network detail shows a not-found error", async () => {
  stub("network", async () => { throw new Error("Network not found."); });
  renderAt("/networks/9", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Network not found.")));
});

test("attaching several nodes at once reports the ones that failed", async () => {
  const calls: unknown[] = [];
  stub("network", async () => detailFixture);
  stubNodes([
    { uuid: "uuid-a", name: "ascend-31", hostname: "a" }, { uuid: "uuid-b", name: "ascend-32", hostname: "b" }, { uuid: "uuid-h7", name: "h20-07", hostname: "h7" },
  ]);
  stub("attachNetworkNodes", async (id, uuids) => {
    calls.push([id, uuids]);
    return { network: detailFixture, results: [{ node_uuid: "uuid-a", status: "attached" }, { node_uuid: "uuid-b", status: "failed", code: "NETWORK_ATTACHMENT_TARGET_INVALID", error: "node not found" }] } as never;
  });
  const warnings: string[] = [];
  const antd = await import("../src/ui-kit/feedback/index.tsx");
  const originalWarning = antd.message.warning;
  antd.message.warning = ((content: unknown) => { warnings.push(String(content)); return Promise.resolve(true) as never; }) as never;
  try {
    const user = userEvent.setup();
    renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
    await user.click(await screen.findByRole("button", { name: n.detail.attachNodes }));
    const dialog = await modalDialog();
    assert.ok(within(dialog).getByText(n.attach.rdmaInferred));
    assert.equal(within(dialog).queryByText(n.attach.modeLabel), null);
    const picker = within(dialog).getByRole("combobox");
    await user.click(picker);
    await user.click(await screen.findByText("ascend-31"));
    await user.click(await screen.findByText("ascend-32"));
    // A node already attached directly cannot be picked again.
    assert.ok(unavailable(await findNodeOption(picker, "h20-07")));
    await closeNodePicker(picker);
    await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
    await waitFor(() => assert.equal(calls.length, 1));
    assert.deepEqual(calls[0], [1, ["uuid-a", "uuid-b"]]);
    await waitFor(() => assert.equal(warnings.length, 1));
    assert.match(warnings[0], /Attached 1 nodes; failed: ascend-32（node not found）/);
  } finally {
    antd.message.warning = originalWarning;
  }
});

test("the label manager can add the first label without switching modes", async () => {
  const bodies: unknown[] = [];
  const network = { ...detailFixture, attachments: [], nodes: [] };
  stub("network", async () => network);
  stub("attachNetwork", async (_id, body) => {
    bodies.push(body);
    return { ...network, attachments: [{ ...detailFixture.attachments[0], ...body }] };
  });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("button", { name: n.detail.manageLabels }));
  const dialog = await screen.findByRole("dialog", { name: n.detail.manageLabels });
  await user.type(within(dialog).getByRole("textbox", { name: n.attach.labelKey }), "rack");
  await user.type(within(dialog).getByLabelText(n.attach.labelValue), "r3");
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0], { label_key: "rack", label_value: "r3" });
  assert.ok(await within(dialog).findByText("rack=r3"));
  assert.equal((within(dialog).getByRole("textbox", { name: n.attach.labelKey }) as HTMLInputElement).value, "");
});

test("network label rules can only be edited from the label manager", async () => {
  stub("network", async () => detailFixture);
  const calls: unknown[] = [];
  stub("updateNetworkLabel", async (id, attachmentID, body) => {
    calls.push([id, attachmentID, body]);
    return { ...detailFixture, nodes: detailFixture.nodes.slice(1) };
  });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  const row = (await screen.findByRole("link", { name: "h20-05" })).closest("tr")!;
  assert.equal(row.lastElementChild?.textContent, "—");
  await user.click(screen.getByRole("button", { name: n.detail.manageLabels }));
  const dialog = await screen.findByRole("dialog", { name: n.detail.manageLabels });
  const rule = within(dialog).getByText("pool=h20-a").closest("tr")!;
  await user.click(within(rule).getByRole("button", { name: n.actions.edit }));
  assert.equal((within(dialog).getByRole("textbox", { name: n.attach.labelKey }) as HTMLInputElement).value, "pool");
  const value = within(dialog).getByLabelText(n.attach.labelValue) as HTMLInputElement;
  assert.equal(value.value, "h20-a");
  await user.clear(value);
  await user.type(value, "h20-b");
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  await waitFor(() => assert.deepEqual(calls, [[detailFixture.id, 1, { label_key: "pool", label_value: "h20-b" }]]));
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByRole("dialog", { name: n.detail.manageLabels }), null));
  await screen.findByRole("link", { name: "h20-07" });
  assert.equal(screen.queryByRole("link", { name: "h20-05" }), null);
});

test("editing a node attachment prefills and sends its overrides", async () => {
  const calls: unknown[] = [];
  stub("network", async () => detailFixture);
  stubNodes([{ uuid: "uuid-h7", name: "h20-07", hostname: "h7" }]);
  stub("nodeNetworks", async () => ({ networks: [], inventory_reported: true, interfaces: [{ name: "bond0", state: "up", mtu: 9000, speed_mbps: 200000, mac: "", addrs: ["10.20.1.17/16"] }], rdma_ports: [{ device: "mlx5_0", port: 1, state: "ACTIVE", link_layer: "InfiniBand", rate_gbps: 400, netdev: "" }] }));
  stub("updateNetworkAttachment", async (id, attachmentID, body) => { calls.push([id, attachmentID, body]); return detailFixture; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Loaded nodes (2)")));
  const attachmentRow = screen.getByRole("link", { name: "h20-07" }).closest("tr")!;
  await user.click(within(attachmentRow).getByRole("button", { name: n.actions.edit }));
  const dialog = await modalDialog();
  assert.ok(within(dialog).getByText(n.attach.editBootstrapTitle));
  assert.equal((within(dialog).getByRole("combobox", { name: n.attach.interfaceIB }) as HTMLInputElement).value, "bond0");
  const iface = within(dialog).getByRole("combobox", { name: n.attach.interfaceIB });
  await user.clear(iface);
  await user.type(iface, "bond");
  await waitFor(() => assert.equal(iface.getAttribute("aria-expanded"), "true"));
  await user.keyboard("{Escape}");
  assert.ok(dialog.isConnected, "Escape closes interface suggestions, not the editor");
  assert.equal((iface as HTMLInputElement).value, "bond");
  await user.clear(iface);
  await user.type(iface, "bond-custom");
  await user.tab();
  await waitFor(() => assert.equal((iface as HTMLInputElement).value, "bond-custom", "closing suggestions preserves a manually typed interface"));
  const ip = within(dialog).getByLabelText(n.attach.ip);
  await user.type(ip, "10.20.1.17");
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [1, 3, { interface: "bond-custom", ip: "10.20.1.17", hcas: ["mlx5_0", "mlx5_9:1"] }]);
});

test("node networks card shows loaded networks and the reported inventory", async () => {
  stub("nodeNetworks", async () => ({
    networks: [{ role: "compute", network_id: 1, network_name: "ib-a", network_type: "host_rdma", attachment_id: 1, source: "label", label_key: "pool", label_value: "h20-a", interface: "bond0", ip: "10.20.1.15", hcas: [{ name: "mlx5_0:1", ip: "", netdev: "", link_layer: "infiniband", state: "ACTIVE", speed_gbps: 400 }], link_layer: "infiniband", warnings: [{ code: "conflict", params: { role: "compute", winner: "ib-a", losers: "legacy" } }] }],
    inventory_reported: true,
    interfaces: [{ name: "bond0", state: "up", mtu: 9000, speed_mbps: 200000, mac: "", addrs: ["10.20.1.15/16"] }],
    rdma_ports: [{ device: "mlx5_0", port: 1, state: "ACTIVE", link_layer: "InfiniBand", rate_gbps: 400, netdev: "" }],
    serving_forward: { address: "10.40.0.5:19443", mode: "unreachable", reason: "dial tcp: i/o timeout", at: "2026-09-26T01:00:00Z" },
    management: { interface: "eth0", ip: "172.25.0.15", roles_off: [], behind_nat: false },
  }));
  render(<I18nextProvider i18n={i18n}><MemoryRouter><NodeNetworksCard nodeUUID="uuid-h5" /></MemoryRouter></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByRole("link", { name: "ib-a" })));
  assert.equal(screen.getByRole("link", { name: "ib-a" }).getAttribute("href"), "/networks?network=1", "the link opens the drawer directly");
  assert.ok(screen.getByRole("img", { name: /Several Compute networks/ }));
  assert.ok(screen.getByText("10.20.1.15/16"));
  assert.ok(screen.getByText("200G"));
  assert.equal(screen.getAllByText("mlx5_0:1", { selector: "code" }).length, 2, "loaded HCA and the inventory port");
  assert.ok(screen.getByText("10.40.0.5:19443"));
  const tunnel = screen.getByLabelText("Unreachable: dial tcp: i/o timeout");
  tunnel.focus();
  await waitFor(() => assert.ok(screen.getAllByRole("tooltip").some((tip) => tip.textContent === "dial tcp: i/o timeout")));
  assert.ok(screen.getByText("400G"));
  assert.ok(screen.getAllByText(n.linkLayer.infiniband).some((el) => !!el.closest('[data-slot="badge"]')), "the detected link layer is tagged beside the loaded HCAs");
});

test("node networks card explains an agent without inventory and a node without networks", async () => {
  stub("nodeNetworks", async () => ({ networks: [], inventory_reported: false, interfaces: [], rdma_ports: [], serving_forward: null, management: { interface: "", ip: "", roles_off: [], behind_nat: false } }));
  render(<I18nextProvider i18n={i18n}><MemoryRouter><NodeNetworksCard nodeUUID="old" /></MemoryRouter></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText(n.node.none)));
  assert.ok(screen.getByText(n.node.notReported));
});

test("node networks card surfaces load errors", async () => {
  stub("nodeNetworks", async () => { throw new Error("node gone"); });
  render(<I18nextProvider i18n={i18n}><MemoryRouter><NodeNetworksCard nodeUUID="gone" /></MemoryRouter></I18nextProvider>);
  await waitFor(() => assert.ok(screen.getByText("node gone")));
});

// An accelerator RDMA network with `count` nodes of `cards` cards each.
function cardNetwork(count: number, cards: number, latest: unknown = null) {
  const nodes = Array.from({ length: count }, (_, i) => ({
    ...detailFixture.nodes[0], node_uuid: `uuid-${i}`, node_name: `node-${String(i).padStart(2, "0")}`, interface: "br0", ip: `172.30.8.${i + 1}`, link_layer: "roce",
    warnings: [], tools: [],
    hcas: Array.from({ length: cards }, (_, c) => ({ name: `npu${c}`, ip: `172.20.${i}.${c}`, netdev: "", link_layer: "roce", state: "UP", speed_gbps: 200 })),
  }));
  return { ...detailFixture, type: "accelerator_rdma", nodes, latest_test: latest };
}

function cardRail(rail: string, target: string, reachable: boolean) {
  const base = { rail, source_ip: "172.20.0.3", target_rail: target, target_ip: "172.20.1.3", gbps: 0 };
  return reachable ? { ...base, reachable: true, rtt_ms: 0.254 } : { ...base, code: "unreachable", error: "no answer within the time limit" };
}

// A node pair as the server returns it: card_shape is the server's reading of the rails.
function cardPair(source: number, target: number, shape: string, reach: (a: string, b: string) => boolean, extra: Record<string, unknown> = {}) {
  const cards = ["npu3", "npu4"];
  return {
    card_shape: shape,
    source_uuid: `uuid-${source}`, source_name: `node-0${source}`, target_uuid: `uuid-${target}`, target_name: `node-0${target}`, target: `172.30.8.${target + 1}`,
    status: "partial", passed: false, reachable: true, rtt_ms_p50: 0, rtt_ms_p99: 0, gbps: 0,
    rails: cards.flatMap((a) => cards.map((b) => cardRail(a, b, reach(a, b)))), ...extra,
  };
}

// A finished card run as the server returns it: the verdict and the counts of pinged card pairs are its own.
function cardRun(kind: string, pattern: string, verdict: string, pairs: ReturnType<typeof cardPair>[]) {
  const pinged = pairs.flatMap((p) => p.rails).filter((r) => r.rail && (r as { target_rail?: string }).target_rail);
  return {
    id: 9, status: "done", kind, pattern, scope: "all", bandwidth: false, node_count: 3, pair_count: pairs.length, passed_count: 0, partial_count: pairs.length,
    rail_count: pinged.length, rail_passed_count: pinged.filter((r) => "reachable" in r).length, card_shape: verdict, node_uuids: ["uuid-0", "uuid-1", "uuid-2"], pairs, started_at: "2026-09-30T04:00:00Z", finished_at: "2026-09-30T04:00:10Z",
  };
}

// The server's estimate for a request, as tests/estimate answers.
function estimateOf(nodes: number, pairs: number, rounds: number, probes: number) {
  return { kind: "card_ping", pattern: "ring", node_count: nodes, pair_count: pairs, card: { rounds, probes, seconds: rounds * 3, max_seconds: 600 } };
}

for (const language of ["en", "zh"]) {
  for (const layer of ["hccs", "future_fabric"]) {
    test(`network detail names HCCS and retains unknown link layers (${language}, ${layer})`, async () => {
      await i18n.changeLanguage(language);
      const detail = structuredClone(detailFixture);
      detail.type = "accelerator_hccs";
      detail.nodes = detail.nodes.map((node: Record<string, unknown>, index: number) => ({
        ...node, link_layer: layer,
        hcas: [{ name: "npu0", ip: `192.9.2.${199 - index * 2}`, netdev: "", link_layer: layer, state: "UP", speed_gbps: 200 }],
      }));
      stub("network", async () => detail);
      renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
      const title = detail.name;
      const dialog = await screen.findByRole("dialog", { name: title });
      const expand = i18n.t("networks:detail.expand");
      for (const button of await within(dialog).findAllByRole("button", { name: new RegExp(`${expand}|Expand row|展开行`) })) fireEvent.click(button);
      const addressTitle = i18n.t("networks:nodeTable.cardAddresses", { fabric: "HCCS" });
      assert.ok(within(dialog).getAllByText(addressTitle, { selector: "dt" }).length > 0);
      if (layer === "hccs") assert.equal(within(dialog).queryByText("HCCS", { exact: true }), null);
      else assert.ok(within(dialog).getAllByText(layer, { selector: "dd" }).length > 0);
      assert.doesNotMatch(dialog.textContent ?? "", /linkLayer\./, "a future protocol must retain its raw label instead of a translation key");
    });
  }
}

test("overview searches resolved nodes by server IDs, resets paging and cancels superseded searches", async () => {
  const nodes = Array.from({ length: 64 }, (_, i) => ({
    ...detailFixture.nodes[0], node_uuid: `u${i}`, node_name: `worker-${String(i + 1).padStart(2, "0")}`,
  }));
  let detailReads = 0;
  const calls: string[] = [];
  let staleSignal: AbortSignal | undefined;
  let finishStale: (() => void) | undefined;
  stub("network", async () => { detailReads++; return { ...detailFixture, nodes }; });
  stub("searchNetworkTargets", async (id, kind, q, signal) => {
    assert.equal(id, detailFixture.id);
    assert.equal(kind, "nodes");
    calls.push(q);
    if (q === "old") {
      staleSignal = signal;
      return new Promise(resolve => { finishStale = () => resolve({ ids: ["u44"] }); });
    }
    return { ids: q === "mlx5" ? nodes.slice(0, 30).map(node => node.node_uuid) : [] };
  });
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  const input = await screen.findByRole("searchbox", { name: n.detail.searchNodes });
  assert.deepEqual(calls, []);
  const table = screen.getByRole("link", { name: "worker-01" }).closest("table")!;
  assert.equal(within(table).getAllByRole("link").length, 20, "the first page has 20 node rows");
  for (let page = 1; page < 4; page++) await userEvent.click(screen.getByRole("button", { name: "Next page" }));
  assert.ok(within(table).getByRole("link", { name: "worker-64" }));
  assert.equal(within(table).getAllByRole("link").length, 4);
  assert.equal(screen.queryByRole("link", { name: "worker-01" }), null);
  fireEvent.change(input, { target: { value: "s" } });
  fireEvent.change(input, { target: { value: "old" } });
  await waitFor(() => assert.equal(typeof finishStale, "function"));
  fireEvent.change(input, { target: { value: "  mlx5  " } });
  assert.equal(staleSignal?.aborted, true);
  await screen.findByRole("link", { name: "worker-01" });
  finishStale?.();
  await waitFor(() => assert.ok(screen.getByText("30 items")));
  assert.equal(screen.queryByRole("link", { name: "worker-45" }), null);
  assert.deepEqual(calls, ["old", "mlx5"]);
  fireEvent.change(input, { target: { value: "nothing" } });
  await screen.findByText(n.detail.noMatchingNodes);
  fireEvent.change(input, { target: { value: "mlx5" } });
  await screen.findByRole("link", { name: "worker-01" });
  assert.deepEqual(calls, ["old", "mlx5", "nothing"]);
  fireEvent.change(input, { target: { value: "" } });
  await screen.findByText("64 items");
  assert.equal(detailReads, 1);
});

test("label target search retries errors and keeps returned labels editable", async () => {
  let tries = 0;
  stub("network", async () => detailFixture);
  stub("searchNetworkTargets", async (_id, kind, q) => {
    assert.equal(kind, "attachments");
    if (q === "absent") return { ids: [] };
    assert.equal(q, "pool");
    if (++tries === 1) throw new Error("temporarily unavailable");
    return { ids: ["1"] };
  });
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  assert.equal(screen.queryByRole("searchbox", { name: n.detail.searchLabels }), null);
  await userEvent.click(await screen.findByRole("button", { name: n.detail.manageLabels }));
  const input = await screen.findByRole("searchbox", { name: n.detail.searchLabels });
  fireEvent.change(input, { target: { value: "pool" } });
  const error = await screen.findByRole("alert");
  assert.match(error.textContent ?? "", /temporarily unavailable/);
  assert.equal(screen.queryByText(n.detail.noMatchingAttachments), null);
  await userEvent.click(within(error).getByRole("button", { name: "Retry" }));
  const target = await screen.findByText("pool=h20-a", { selector: "td *" });
  assert.ok(within(target.closest("tr")!).getByRole("button", { name: n.actions.edit }));
  assert.equal(tries, 2);
  fireEvent.change(input, { target: { value: "absent" } });
  await screen.findByText(n.detail.noMatchingAttachments);
  await userEvent.keyboard("{Escape}");
  assert.ok(await screen.findByRole("link", { name: "h20-05" }));
});

test("role and attachment-source filters send multi-select values with the search across pages", async () => {
  const nodes = Array.from({ length: 42 }, (_, index) => ({
    ...detailFixture.nodes[0], node_uuid: `u-${index}`, node_name: `worker-${index}`,
  }));
  stub("network", async () => ({ ...detailFixture, roles: ["compute", "serving"], nodes }));
  const requests: unknown[] = [];
  stub("searchNetworkTargets", async (_id, kind, q, _signal, filters) => {
    assert.equal(kind, "nodes");
    requests.push([q, filters?.roles, filters?.source]);
    return { ids: ["u-41"] };
  });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  const input = await screen.findByRole("searchbox", { name: n.detail.searchNodes });
  await user.click(screen.getByRole("button", { name: "Next page" }));
  const roleButton = screen.getByRole("button", { name: `Filter by ${n.nodeTable.roles}` });
  await user.click(roleButton);
  await user.click(await screen.findByRole("checkbox", { name: n.role.compute }));
  await user.click(screen.getByRole("checkbox", { name: n.role.serving }));
  await user.keyboard("{Escape}");
  await screen.findByRole("link", { name: "worker-41" });
  assert.deepEqual(requests.at(-1), ["", ["compute", "serving"], []]);
  const sourceButton = screen.getByRole("button", { name: `Filter by ${n.nodeTable.source}` });
  await user.click(sourceButton);
  await user.click(await screen.findByRole("checkbox", { name: n.source.label }));
  await user.keyboard("{Escape}");
  fireEvent.change(input, { target: { value: "mlx5" } });
  await waitFor(() => assert.deepEqual(requests.at(-1), ["mlx5", ["compute", "serving"], ["label"]]));
  await user.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => assert.deepEqual(requests.at(-1), ["mlx5", [], []]));
  fireEvent.change(input, { target: { value: "" } });
  assert.ok(await screen.findByRole("link", { name: "worker-0" }));
});
