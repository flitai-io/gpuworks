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
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLBodyElement", "HTMLHtmlElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
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
  resources: { en: { networks: networksLocale, settings: settingsLocale, common: load("common") } },
  defaultNS: "common",
  ns: ["networks", "settings", "common"],
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const apiModule = await import("../src/api.ts");
const NetworksPage = (await import("../src/pages/networks/NetworksPage.tsx")).default;
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

const { useLocation } = await import("react-router-dom");
function Where() { const l = useLocation(); return <div data-testid="where">{l.pathname}{l.search}</div>; }

function renderAt(path: string, element: React.ReactElement, pattern = path) {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path={pattern} element={<>{element}<Where /></>} />
          <Route path="/networks/:id" element={<div>detail route</div>} />
        </Routes>
      </MemoryRouter>
    </I18nextProvider>,
  );
}

// A node's bootstrap and HCAs sit in its row's expanded detail: open every
// node row, then read a row's detail from the row after it.
async function expandNodeRows() {
  for (const button of await screen.findAllByRole("button", { name: n.detail.expand })) await userEvent.click(button);
}
const detailOf = (row: HTMLElement) => row.nextElementSibling as HTMLElement;
// The detail is a drawer, itself a dialog: a modal is the other one.
const modalDialog = () => waitFor(() => {
  const found = screen.getAllByRole("dialog").find((el) => el.classList.contains("ant-modal") || el.closest(".ant-modal"));
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
  assert.ok(screen.getByRole("group", { name: n.topology.diagram.simpleLabel }), "the page explains the topology");
});

test("an empty ?network= opens no detail, while ?network=0 is the management network", async () => {
  const asked: number[] = [];
  stub("networks", async () => ({ items: [], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }));
  stub("network", async (id: number) => { asked.push(id); return { ...detailFixture, id }; });
  renderAt("/networks?network=", <NetworksPage />, "/networks");
  await waitFor(() => assert.ok(screen.getByText(n.empty)));
  assert.deepEqual(asked, [], "an empty value is no network");
  assert.equal(document.querySelector('[role="dialog"]'), null);
  cleanup();
  renderAt("/networks?network=0", <NetworksPage />, "/networks");
  await waitFor(() => assert.deepEqual(asked, [0]));
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

test("detaching in the drawer reads the list again, so its attachments column follows", async () => {
  let listReads = 0;
  stub("networks", async () => { listReads++; return { items: [listItem()], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }; });
  stub("network", async () => detailFixture);
  stub("detachNetwork", async () => ({ ...detailFixture, attachments: detailFixture.attachments.slice(1) }));
  renderAt(`/networks?network=${detailFixture.id}`, <NetworksPage />, "/networks");
  const detach = (await screen.findAllByRole("button", { name: n.actions.detach }))[0];
  const before = listReads;
  fireEvent.click(detach);
  const ok = await waitFor(() => {
    const found = [...document.querySelectorAll(".ant-popover:not(.ant-popover-hidden)")].flatMap((pop) => within(pop as HTMLElement).queryAllByRole("button", { name: n.actions.detach }));
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

test("creating a network sends the server dictionary values and opens the detail", async () => {
  const bodies: unknown[] = [];
  stub("networks", async () => ({ items: [], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }));
  stub("createNetwork", async (body) => { bodies.push(body); return { ...detailFixture, id: 7 }; });
  const user = userEvent.setup();
  renderAt("/networks", <NetworksPage />);
  await user.click(await screen.findByRole("button", { name: n.actions.create }));
  const dialog = await modalDialog();
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  await waitFor(() => assert.ok(within(dialog).getByText(n.form.nameRequired)));
  assert.equal(bodies.length, 0, "validation blocks the request");

  await user.type(within(dialog).getByLabelText(n.form.name), "ib-a");
  await user.click(within(dialog).getByLabelText(n.role.compute));
  await user.click(within(dialog).getByText(n.type.host_rdma));
  // An RDMA network asks for nothing: the interface glob is gone and the
  // bootstrap CIDR is an advanced option.
  assert.ok(within(dialog).getByText(n.form.rdmaInferred));
  assert.equal(within(dialog).queryByLabelText(n.form.interface), null);
  await user.click(within(dialog).getByText(n.form.advanced));
  await user.type(within(dialog).getByLabelText(n.form.bootstrapCidr), "10.20.0.0/16");
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
  await user.type(within(dialog).getByLabelText(n.form.name), "weiw1");
  await user.click(within(dialog).getByLabelText(n.role.compute));
  await user.click(within(dialog).getByText(n.type.host_rdma));
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0], { name: "weiw1", description: "", roles: ["compute"], type: "host_rdma", cidr: "", interface: "" });
});

test("an accelerator network carries compute traffic only and asks for a bootstrap subnet at most", async () => {
  const bodies: unknown[] = [];
  stub("networks", async () => ({ items: [], roles: ["storage_sync", "compute", "serving"], types: ["host_ethernet", "host_rdma", "accelerator_rdma"], type_roles: typeRoles }));
  stub("createNetwork", async (body) => { bodies.push(body); return { ...detailFixture, id: 7 }; });
  const user = userEvent.setup();
  renderAt("/networks", <NetworksPage />);
  await user.click(await screen.findByRole("button", { name: n.actions.create }));
  const dialog = await modalDialog();
  await user.type(within(dialog).getByLabelText(n.form.name), "npu");
  // On a host network every role can be ticked.
  await user.click(within(dialog).getByLabelText(n.role.storage_sync));
  await user.click(within(dialog).getByLabelText(n.role.compute));
  assert.equal((within(dialog).getByLabelText(n.role.serving) as HTMLInputElement).disabled, false);

  await user.click(within(dialog).getByText(n.type.accelerator_rdma));
  // Storage sync and serving are greyed out and say why; the role already
  // ticked that the type cannot carry is dropped.
  await waitFor(() => assert.equal((within(dialog).getByRole("checkbox", { name: n.role.storage_sync }) as HTMLInputElement).disabled, true));
  assert.equal((within(dialog).getByRole("checkbox", { name: n.role.serving }) as HTMLInputElement).disabled, true);
  assert.equal((within(dialog).getByRole("checkbox", { name: n.role.storage_sync }) as HTMLInputElement).checked, false);
  assert.equal((within(dialog).getByRole("checkbox", { name: n.role.compute }) as HTMLInputElement).checked, true);
  assert.ok(within(dialog).getByLabelText(`${n.role.serving}: ${n.form.roleNotOnType}`), "the reason is reachable by keyboard and screen reader");
  assert.ok(within(dialog).getByText(n.form.acceleratorInferred));
  assert.equal(within(dialog).queryByLabelText(n.form.interface), null);
  await user.click(within(dialog).getByText(n.form.advanced));
  assert.ok(within(dialog).getByText(n.form.acceleratorBootstrapCidrHint));
  await user.type(within(dialog).getByLabelText(n.form.bootstrapCidr), "198.51.100.0/24");
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0], { name: "npu", description: "", roles: ["compute"], type: "accelerator_rdma", cidr: "198.51.100.0/24", interface: "" });
});

test("an accelerator network's rails are picked from the node's cards", async () => {
  const calls: unknown[] = [];
  const npu = {
    ...detailFixture, type: "accelerator_rdma",
    nodes: detailFixture.nodes.map((node: { node_name: string }, i: number) => ({
      ...node, link_layer: "roce",
      hcas: [{ name: "npu3", ip: i === 0 ? "203.0.113.59" : "203.0.113.82", netdev: "", link_layer: "roce", state: "UP", speed_gbps: 200 }],
    })),
  };
  stub("network", async () => npu);
  stub("nodeNetworks", async () => ({
    networks: [], inventory_reported: true, interfaces: [], serving_forward: null, management: { interface: "", ip: "", roles_off: [], behind_nat: false },
    rdma_ports: [{ device: "mlx5_0", port: 1, state: "ACTIVE", link_layer: "InfiniBand", rate_gbps: 400, netdev: "", ip: "" }],
    accelerator_ports: [
      { name: "npu3", index: 3, ip: "203.0.113.59", netmask: "255.255.0.0", state: "UP", speed_gbps: 200 },
      { name: "npu4", index: 4, ip: "203.0.113.60", netmask: "255.255.0.0", state: "UP", speed_gbps: 200 },
      { name: "npu6", index: 6, ip: "203.0.113.62", netmask: "255.255.0.0", state: "DOWN", speed_gbps: 0, gap: "inactive" },
      { name: "npu7", index: 7, ip: "", netmask: "", state: "UP", speed_gbps: 200, gap: "no_ipv4" },
    ],
  }));
  stub("editNetworkHCAs", async (id, body) => { calls.push([id, body]); return npu; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Resolved nodes (2)")));
  assert.ok(screen.getByText(n.detail.acceleratorInferred));
  assert.ok(screen.getAllByText(n.type.accelerator_rdma).length > 0);
  await expandNodeRows();
  const row = screen.getByText("203.0.113.59").closest("tr") as HTMLElement;
  await user.click(within(row).getByRole("button", { name: new RegExp(n.rail.add) }));
  // Cards are offered, not the host's RDMA ports; the one in use is left out
  // and a card that cannot work says why.
  assert.ok(await screen.findByText("npu4 · 203.0.113.60"));
  assert.ok(screen.getByText(`npu6 · 203.0.113.62 · ${n.rail.gap.inactive}`));
  assert.ok(screen.getByText(`npu7 · — · ${n.rail.gap.no_ipv4}`));
  assert.equal(screen.queryByText(/^npu3 ·/), null);
  assert.equal(screen.queryByText(/^mlx5_0:1/), null);
  await user.click(screen.getByText("npu4 · 203.0.113.60"));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual((calls[0] as [number, { op: string; hca: string }])[1].hca, "npu4");
});

test("network detail lists each node's bootstrap and HCAs with their IPs", async () => {
  stub("network", async () => detailFixture);
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Resolved nodes (2)")));
  assert.ok(screen.getByText(n.detail.rdmaInferred));
  assert.equal(screen.queryByRole("columnheader", { name: n.nodeTable.endpoint }), null, "RDMA shows bootstrap, not interface / IP");
  await expandNodeRows();
  assert.ok(screen.getAllByText(n.nodeTable.bootstrap).length > 0, "each node's detail names its bootstrap");
  const h5 = screen.getAllByText("h20-05")[0].closest("tr") as HTMLElement;
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
  assert.ok(screen.getByText("Overrides: bond0 · mlx5_0,mlx5_9:1"));
});

test("removing an HCA asks whether it leaves every node or one", async () => {
  const calls: unknown[] = [];
  stub("network", async () => detailFixture);
  stub("editNetworkHCAs", async (id, body) => { calls.push([id, body]); return detailFixture; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Resolved nodes (2)")));
  await expandNodeRows();
  const h5 = detailOf(screen.getAllByText("h20-05")[0].closest("tr") as HTMLElement);
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
  await waitFor(() => assert.ok(screen.getByText("Resolved nodes (2)")));
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
  await waitFor(() => assert.ok(screen.getByText("Resolved nodes (2)")));
  // h20-05 is on the network by label: pinning its bootstrap attaches the node.
  await expandNodeRows();
  const h5 = detailOf(screen.getAllByText("h20-05")[0].closest("tr") as HTMLElement);
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
  await waitFor(() => assert.ok(screen.getByText("Resolved nodes (2)")));
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
  const antd = await import("antd");
  const originalWarning = antd.message.warning;
  antd.message.warning = ((content: unknown) => { warnings.push(String(content)); return Promise.resolve(true) as never; }) as never;
  try {
    const user = userEvent.setup();
    renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
    await user.click(await screen.findByRole("button", { name: n.actions.attach }));
    const dialog = await modalDialog();
    assert.ok(within(dialog).getByText(n.attach.rdmaInferred));
    await user.click(within(dialog).getByRole("combobox"));
    await user.click(await screen.findByText("ascend-31"));
    await user.click(await screen.findByText("ascend-32"));
    // A node already attached directly cannot be picked again.
    const attached = screen.getAllByText("h20-07").find((el) => el.closest(".ant-select-item-option")) as HTMLElement;
    assert.ok(attached.closest(".ant-select-item-option-disabled"));
    await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
    await waitFor(() => assert.equal(calls.length, 1));
    assert.deepEqual(calls[0], [1, ["uuid-a", "uuid-b"]]);
    await waitFor(() => assert.equal(warnings.length, 1));
    assert.match(warnings[0], /Attached 1 nodes; failed: ascend-32（node not found）/);
  } finally {
    antd.message.warning = originalWarning;
  }
});

test("attaching to a label sends only the label target", async () => {
  const bodies: unknown[] = [];
  stub("network", async () => detailFixture);
  stubNodes([]);
  stub("attachNetwork", async (_id, body) => { bodies.push(body); return detailFixture; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("button", { name: n.actions.attach }));
  const dialog = await modalDialog();
  await user.click(within(dialog).getByText(n.attach.modeLabel));
  await user.type(within(dialog).getByLabelText(n.attach.labelKey), "rack");
  await user.type(within(dialog).getByLabelText(n.attach.labelValue), "r3");
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0], { label_key: "rack", label_value: "r3" });
});

test("editing a node attachment prefills and sends its overrides", async () => {
  const calls: unknown[] = [];
  stub("network", async () => detailFixture);
  stubNodes([{ uuid: "uuid-h7", name: "h20-07", hostname: "h7" }]);
  stub("nodeNetworks", async () => ({ networks: [], inventory_reported: true, interfaces: [{ name: "bond0", state: "up", mtu: 9000, speed_mbps: 200000, mac: "", addrs: ["10.20.1.17/16"] }], rdma_ports: [{ device: "mlx5_0", port: 1, state: "ACTIVE", link_layer: "InfiniBand", rate_gbps: 400, netdev: "" }] }));
  stub("updateNetworkAttachment", async (id, attachmentID, body) => { calls.push([id, attachmentID, body]); return detailFixture; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await waitFor(() => assert.ok(screen.getByText("Resolved nodes (2)")));
  const attachmentRow = screen.getByText("Overrides: bond0 · mlx5_0,mlx5_9:1").closest("tr") as HTMLElement;
  await user.click(within(attachmentRow).getByRole("button", { name: n.actions.edit }));
  const dialog = await modalDialog();
  assert.ok(within(dialog).getByText(n.attach.editBootstrapTitle));
  assert.equal((within(dialog).getByLabelText(n.attach.interfaceIB) as HTMLInputElement).value, "bond0");
  const ip = within(dialog).getByLabelText(n.attach.ip);
  await user.type(ip, "10.20.1.17");
  await user.click(within(dialog).getByRole("button", { name: n.actions.save }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [1, 3, { interface: "bond0", ip: "10.20.1.17", hcas: ["mlx5_0", "mlx5_9:1"] }]);
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
  assert.ok(screen.getAllByText(n.linkLayer.infiniband).some((el) => el.classList.contains("ant-tag")), "the detected link layer is tagged beside the loaded HCAs");
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

test("internal test tab shows the latest result as a pair matrix", async () => {
  stub("network", async () => detailFixture);
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  await waitFor(() => assert.ok(screen.getByText(/All pairs · pairs 1 passed \/ 0 partial \/ 1 failed/)));
  assert.ok(screen.getByText(/label pool=h20-a/));
  assert.ok(screen.getByText(n.test.rdmaNote), "IB networks say RDMA bandwidth is not measured");
  const pass = screen.getByLabelText("h20-05 → h20-07: 23.4 Gb/s");
  const fail = screen.getByLabelText("h20-07 → h20-05: Failed");
  assert.ok(pass);
  // The reason is in the problem summary, not only behind a hover.
  const why = "Connection failed: dial tcp 10.20.1.15:41000: i/o timeout";
  assert.ok(screen.getByRole("cell", { name: why }));
  fail.focus();
  await waitFor(() => assert.ok(screen.getAllByRole("tooltip").some((tip) => tip.textContent === why)));
  // Enter opens the pair.
  fireEvent.keyDown(fail, { key: "Enter" });
  const dialog = await modalDialog();
  assert.ok(within(dialog).getByText("h20-07 → h20-05"));
  assert.ok(within(dialog).getByText("10.20.1.15:41000"));
  assert.ok(within(dialog).getByText(why));
});

test("starting a label-scoped test sends the scope and polls while it runs", async () => {
  const bodies: unknown[] = [];
  let loads = 0;
  const running = { ...detailFixture, latest_test: { ...detailFixture.latest_test, status: "running", pairs: [], node_count: 3, pair_count: 6 } };
  stub("network", async () => { loads++; return loads === 1 ? detailFixture : running; });
  stub("startNetworkTest", async (_id, body) => { bodies.push(body); return running.latest_test; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  await user.click(await screen.findByText(n.test.scopeLabel));
  await user.type(screen.getByLabelText(n.test.labelKey), "rack");
  await user.type(screen.getByLabelText(n.test.labelValue), "r3");
  await user.click(screen.getByRole("switch"));
  await user.click(screen.getAllByRole("button", { name: n.test.start })[0]);
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0], { kind: "tcp", pattern: "mesh", scope: "label", bandwidth: false, seconds: 3, label_key: "rack", label_value: "r3" });
  await waitFor(() => assert.ok(screen.getByText("Testing 3 nodes, 6 pairs…")));
  const before = loads;
  await new Promise((r) => setTimeout(r, 2300));
  assert.ok(loads > before, "a running test is polled");
  assert.equal((screen.getAllByRole("button", { name: n.test.start })[0] as HTMLButtonElement).disabled, true);
});

test("serving networks explain that they have no internal test", async () => {
  stub("network", async () => ({ ...detailFixture, roles: ["serving"], test_supported: false, latest_test: null }));
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  await waitFor(() => assert.ok(screen.getByText(n.test.unsupported)));
});

test("the test matrix sorts nodes by name and flags pairs far below the fastest one", async () => {
  const pair = (s: string, t2: string, gbps: number) => ({ source_uuid: s, source_name: s, target_uuid: t2, target_name: t2, target: `${t2}:1`, status: "passed", passed: true, reachable: true, rtt_ms_p50: 0.2, rtt_ms_p99: 0.3, gbps });
  const latest = { ...detailFixture.latest_test, pair_count: 2, passed_count: 2, pairs: [pair("zeta", "alpha", 17.5), pair("alpha", "zeta", 0.9)] };
  stub("network", async () => ({ ...detailFixture, latest_test: latest }));
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  await waitFor(() => assert.ok(screen.getByLabelText("zeta → alpha: 17.5 Gb/s")));
  const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
  assert.deepEqual(headers.slice(1), ["alpha", "zeta"], "columns are sorted by node name");
  const slow = screen.getByLabelText("alpha → zeta: 0.9 Gb/s");
  slow.focus();
  await waitFor(() => assert.ok(screen.getAllByRole("tooltip").some((tip) => tip.textContent?.includes(n.test.slow))));
});

test("an RDMA bandwidth test names nodes without ib_write_bw and shows per-rail results", async () => {
  const bodies: unknown[] = [];
  const rdmaRun = {
    ...detailFixture.latest_test, kind: "rdma_tool", pair_count: 1, passed_count: 1, rail_count: 2, rail_passed_count: 2,
    pairs: [{ source_uuid: "uuid-h5", source_name: "h20-05", target_uuid: "uuid-h7", target_name: "h20-07", target: "10.20.1.17", status: "passed", passed: true, reachable: true, rtt_ms_p50: 0, rtt_ms_p99: 0, gbps: 715.2,
      rails: [{ rail: "mlx5_0:1", target_rail: "mlx5_0:1", gbps: 358.7 }, { rail: "mlx5_1:1", target_rail: "mlx5_1:1", gbps: 356.5 }] }],
  };
  stub("network", async () => ({ ...detailFixture, latest_test: rdmaRun }));
  stub("startNetworkTest", async (_id, body) => { bodies.push(body); return rdmaRun as never; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  await waitFor(() => assert.ok(screen.getByText(/RDMA bandwidth · .* HCAs 2\/2 passed/)));
  await user.click(screen.getByText(n.test.kindRdmaTool));
  await waitFor(() => assert.ok(screen.getByText(/These nodes have no ib_write_bw, so their pairs will fail: h20-07/)));
  assert.equal(screen.queryByText(n.test.bandwidth), null, "rdma tests always measure bandwidth");
  await user.click(screen.getAllByRole("button", { name: n.test.start })[0]);
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0], { kind: "rdma_tool", pattern: "mesh", scope: "all", bandwidth: true, seconds: 3 });
  await user.click(screen.getByLabelText("h20-05 → h20-07: 715.2 Gb/s"));
  const dialog = await modalDialog();
  assert.ok(within(dialog).getByText("356.5 Gb/s"));
  assert.equal(within(dialog).getAllByText("mlx5_1:1").length, 2, "source and target rail");
});

// The 2026-09-28 lab case: two 25G rails work, a 1G port without an address
// fails on every pair. The summary names the port and removes it in one click.
test("a partially failing RDMA run names the failing HCA and removes it", async () => {
  const calls: unknown[] = [];
  const rail = (name: string, src: string, dst: string, gbps: number) => ({ rail: name, source_ip: src, target_rail: name, target_ip: dst, gbps });
  const gidless = { rail: "rocep189s0f0:1", target_rail: "rocep189s0f0:1", gbps: 0, code: "no_roce_v2_gid", side: "target", error: "RoCE port has no IPv4 RoCE v2 GID" };
  const pair = (s: string, sn: string, t2: string, tn: string) => ({
    source_uuid: s, source_name: sn, target_uuid: t2, target_name: tn, target: "100.64.29.210", status: "partial", passed: false, reachable: true, rtt_ms_p50: 0, rtt_ms_p99: 0, gbps: 48.9,
    rails: [rail("rocep61s0f0:1", "100.64.29.209", "100.64.29.210", 24.4), rail("rocep61s0f2:1", "100.64.29.213", "100.64.29.214", 24.5), gidless],
  });
  const run = { ...detailFixture.latest_test, kind: "rdma_tool", pattern: "ring", pair_count: 2, passed_count: 0, partial_count: 2, rail_count: 6, rail_passed_count: 4,
    pairs: [pair("uuid-a", "ascend-31", "uuid-b", "ascend-32"), pair("uuid-b", "ascend-32", "uuid-a", "ascend-31")] };
  stub("network", async () => ({ ...detailFixture, latest_test: run }));
  stub("editNetworkHCAs", async (id, body) => { calls.push([id, body]); return detailFixture; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  await waitFor(() => assert.ok(screen.getByText(/Ring · pairs 0 passed \/ 2 partial \/ 0 failed · HCAs 4\/6 passed/)));
  // A ring is listed pair by pair, not as a matrix.
  assert.ok(screen.getByRole("columnheader", { name: n.test.source }));
  assert.ok(screen.getByLabelText("ascend-31 → ascend-32: 48.9 Gb/s · 2/3"));
  const why = "This RoCE port on ascend-32 has no IPv4 address on its interface, so no RoCE v2 connection";
  const row = screen.getByRole("cell", { name: why }).closest("tr") as HTMLElement;
  assert.ok(within(row).getByText("rocep189s0f0:1"));
  assert.ok(within(row).getByText("1 pairs"));
  await user.click(within(row).getByRole("button", { name: n.rail.removeShort }));
  await user.click(await screen.findByRole("button", { name: n.rail.confirm }));
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], [1, { op: "remove", hca: "rocep189s0f0:1", node_uuid: undefined }]);
});

test("more than 16 nodes default to a ring", async () => {
  const bodies: unknown[] = [];
  const many = Array.from({ length: 17 }, (_, i) => ({ ...detailFixture.nodes[0], node_uuid: `u${i}`, node_name: `n${i}` }));
  stub("network", async () => ({ ...detailFixture, nodes: many, latest_test: null }));
  stub("startNetworkTest", async (_id, body) => { bodies.push(body); return detailFixture.latest_test as never; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  assert.ok(await screen.findByText(n.test.patternHint.ring));
  await user.click(screen.getAllByRole("button", { name: n.test.start })[0]);
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.equal((bodies[0] as { pattern: string }).pattern, "ring");
});

test("nodes without a bootstrap address or HCAs are named before a test", async () => {
  const nodes = [{ ...detailFixture.nodes[0], ip: "", interface: "" }, { ...detailFixture.nodes[1], hcas: [] }];
  stub("network", async () => ({ ...detailFixture, nodes, latest_test: null }));
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  assert.ok(await screen.findByText("These nodes have no bootstrap address and their pairs will fail: h20-05."));
  await user.click(screen.getByText(n.test.kindRdmaTool));
  assert.ok(await screen.findByText("These nodes have no usable HCA and their pairs will fail: h20-07."));
});

test("an ethernet network runs TCP tests and keeps the throughput switch", async () => {
  const bodies: unknown[] = [];
  const eth = { ...detailFixture, type: "host_ethernet", latest_test: null };
  stub("network", async () => eth);
  stub("startNetworkTest", async (_id, body) => { bodies.push(body); return detailFixture.latest_test as never; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  await waitFor(() => assert.ok(screen.getByText(n.test.none)));
  assert.equal(screen.queryByText(n.test.kindRdmaTool), null);
  await user.click(screen.getByRole("switch"));
  await user.click(screen.getAllByRole("button", { name: n.test.start })[0]);
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0], { kind: "tcp", pattern: "mesh", scope: "all", bandwidth: false, seconds: 3 });
});

test("a compute network offers only the TCP and RDMA tool tests", async () => {
  stub("network", async () => ({ ...detailFixture, latest_test: null }));
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  assert.deepEqual(detailFixture.roles, ["compute"], "the fixture is a compute network");
  assert.ok(await screen.findByText(n.test.kindTcp));
  assert.ok(screen.getByText(n.test.kindRdmaTool));
  assert.equal(screen.getAllByRole("radio").filter((r) => (r as HTMLInputElement).name === "kind" || r.closest(".ant-form-item")?.textContent?.includes(n.test.kind)).length, 2, "only the TCP and RDMA tool kinds");
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

test("an accelerator network offers card tests and shows the server's estimate", async () => {
  const bodies: unknown[] = [];
  const asked: unknown[] = [];
  stub("network", async () => cardNetwork(24, 8));
  stub("estimateNetworkTest", async (_id, body) => {
    asked.push(body);
    if (body.kind === "card_matrix" && body.pattern === "mesh") return estimateOf(24, 276, 96, 17664) as never;
    if (body.kind === "card_matrix") return estimateOf(24, 24, 8, 1536) as never;
    return estimateOf(24, 24, 1, 192) as never;
  });
  stub("startNetworkTest", async (_id, body) => { bodies.push(body); return cardRun("card_ping", "ring", "full", []) as never; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  // Same-named cards are the default kind; more than 16 nodes default to a ring.
  assert.ok(await screen.findByText(n.test.cardNote.card_ping));
  assert.equal(screen.queryByText(n.test.kindRdmaTool), null);
  assert.ok(await screen.findByText(/^24 nodes, 24 pairs: 192 probes in 1 rounds, at most 3 s\./));
  assert.deepEqual(asked.at(-1), { kind: "card_ping", pattern: "ring", scope: "all" });
  // A card test has no throughput to measure.
  assert.equal(screen.queryByRole("switch"), null);
  assert.equal(screen.queryByLabelText(n.test.seconds), null);

  await user.click(screen.getByText(n.test.kindCardMatrix));
  assert.ok(await screen.findByText(/^24 nodes, 24 pairs: 1536 probes in 8 rounds, at most 24 s\./));
  await user.click(screen.getByText(n.test.patternName.mesh));
  assert.ok(await screen.findByText(/^24 nodes, 276 pairs: 17664 probes in 96 rounds, at most 4\.8 minutes\./));
  assert.ok(screen.getByText(n.test.cardPatternHint.mesh));
  assert.deepEqual(asked.at(-1), { kind: "card_matrix", pattern: "mesh", scope: "all" });
  assert.equal((screen.getAllByRole("button", { name: n.test.start })[0] as HTMLButtonElement).disabled, false);
  await user.click(screen.getAllByRole("button", { name: n.test.start })[0]);
  await waitFor(() => assert.equal(bodies.length, 1));
  // What starts is the request that was estimated.
  assert.deepEqual(bodies[0], asked.at(-1));

  // The bootstrap TCP test is still there, with its own controls, and asks for no estimate.
  const before = asked.length;
  await user.click(screen.getByText(n.test.kindTcp));
  assert.ok(await screen.findByText(n.test.acceleratorNote));
  assert.ok(screen.getByRole("switch"));
  await new Promise((r) => setTimeout(r, 300));
  assert.equal(asked.length, before);
});

test("a card test the server refuses cannot be started and says why", async () => {
  let resolveRing!: (value: ReturnType<typeof estimateOf>) => void;
  const ringEstimate = new Promise<ReturnType<typeof estimateOf>>((resolve) => { resolveRing = resolve; });
  let rejectMesh!: (reason: Error) => void;
  const meshEstimate = new Promise<never>((_resolve, reject) => { rejectMesh = reject; });
  let ringRequested = false;
  let meshRequested = false;
  let starts = 0;
  stub("network", async () => cardNetwork(60, 8));
  stub("estimateNetworkTest", async (_id, body) => {
    if (body.kind === "card_matrix" && body.pattern === "mesh") { meshRequested = true; return meshEstimate; }
    if (body.scope === "label") return estimateOf(12, 12, 8, 768) as never;
    if (body.kind === "card_matrix") { ringRequested = true; return ringEstimate as never; }
    return estimateOf(60, 60, 8, 3840) as never;
  });
  stub("startNetworkTest", async () => { starts++; return cardRun("card_matrix", "mesh", "full", []) as never; });
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  // Until the server has answered, the form says it is asking.
  await user.click(await screen.findByText(n.test.kindCardMatrix));
  assert.ok(screen.getByText(n.test.cardEstimating));
  await waitFor(() => assert.ok(ringRequested));
  resolveRing(estimateOf(60, 60, 8, 3840));
  assert.ok(await screen.findByText(/^60 nodes, 60 pairs: 3840 probes in 8 rounds, at most 24 s\./), "sixty nodes as a ring are eight rounds");
  const start = screen.getAllByRole("button", { name: n.test.start })[0] as HTMLButtonElement;
  await user.click(screen.getByText(n.test.patternName.mesh));
  assert.ok(screen.getByText(n.test.cardEstimating));
  await waitFor(() => assert.ok(meshRequested));
  rejectMesh(new Error("This test takes 240 rounds, up to 720 seconds, over the 600-second limit; narrow the scope, or use a ring or same-named cards."));
  assert.ok(await screen.findByText(/^Cannot start: This test takes 240 rounds, up to 720 seconds/));
  assert.equal(start.disabled, true);
  await user.click(start);
  assert.equal(starts, 0, "a server-refused test cannot be started");
  // A label's nodes are the server's to resolve: nothing is asked until a key is given.
  await user.click(screen.getByText(n.test.patternName.ring));
  await user.click(screen.getByText(n.test.scopeLabel));
  assert.ok(await screen.findByText(n.test.cardEstimating));
  await user.type(screen.getByLabelText(n.test.labelKey), "rack");
  assert.ok(await screen.findByText(/^12 nodes, 12 pairs: 768 probes in 8 rounds, at most 24 s\./));
  assert.equal(start.disabled, false);
});

test("a card matrix of two nodes says how to pick cards and shows the grid", async () => {
  const run = cardRun("card_matrix", "mesh", "diagonal", [cardPair(0, 1, "diagonal", (a, b) => a === b)]);
  stub("network", async () => cardNetwork(2, 2, run));
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  assert.ok(await screen.findByText(n.test.card.verdict.matrix.diagonal));
  assert.ok(screen.getByText(/1 node pairs · 2\/4 card pairs reachable/));
  // One node pair is shown as its grid at once: source cards down, target cards across.
  const grid = screen.getByRole("columnheader", { name: "node-00 \\ node-01" }).closest("table") as HTMLElement;
  assert.ok(within(grid).getByLabelText("npu3 → npu3: Reachable"));
  assert.ok(within(grid).getByLabelText("npu4 → npu4: Reachable"));
  const miss = within(grid).getByLabelText("npu3 → npu4: Unreachable");
  assert.ok(within(grid).getByLabelText("npu4 → npu3: Unreachable"));
  miss.focus();
  await waitFor(() => assert.ok(screen.getAllByRole("tooltip").some((tip) => tip.textContent === "172.20.0.3 → 172.20.1.3 · No answer within the time limit")));
  const hit = within(grid).getByLabelText("npu3 → npu3: Reachable");
  hit.focus();
  await waitFor(() => assert.ok(screen.getAllByRole("tooltip").some((tip) => tip.textContent === "172.20.0.3 → 172.20.1.3 · 0.25 ms")));
});

test("a card matrix of several nodes names the node pair that differs from the rest", async () => {
  const diagonal = (a: string, b: string) => a === b;
  const run = cardRun("card_matrix", "mesh", "diagonal", [cardPair(0, 1, "diagonal", diagonal), cardPair(0, 2, "diagonal", diagonal), cardPair(1, 2, "mixed", (a, b) => a === "npu3" && b === "npu4")]);
  stub("network", async () => cardNetwork(3, 2, run));
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  assert.ok(await screen.findByText(n.test.card.verdict.matrix.diagonal));
  assert.ok(screen.getByText("Node pairs that differ from the rest (1): node-01 ↔ node-02. This is usually the wiring or configuration fault to find."));
  // The overview has the pair on both sides of the diagonal.
  assert.equal(screen.getAllByLabelText("node-00 ↔ node-01: Same-named only").length, 2);
  const odd = screen.getAllByLabelText("node-01 ↔ node-02: Partly reachable 1/4");
  assert.equal(odd.length, 2);
  assert.ok(screen.getByText(n.test.card.openHint));
  // Enter opens the pair's cards.
  fireEvent.keyDown(odd[0], { key: "Enter" });
  const dialog = await modalDialog();
  assert.ok(within(dialog).getByText("node-01 ↔ node-02"));
  assert.ok(within(dialog).getByLabelText("npu3 → npu4: Reachable"));
  assert.ok(within(dialog).getByLabelText("npu3 → npu3: Unreachable"));
  await user.click(within(dialog).getAllByRole("button", { name: n.actions.close }).at(-1) as HTMLElement);
  await waitFor(() => assert.equal(screen.queryByLabelText("npu3 → npu4: Reachable"), null));
});

test("a same-named card test lists each pair of cards and explains a pair that was not tested", async () => {
  const healthy = { ...cardPair(0, 1, "full", () => true), rails: [cardRail("npu3", "npu3", true), cardRail("npu4", "npu4", true)], status: "passed", passed: true };
  const broken = { ...cardPair(1, 2, "mixed", () => true), rails: [cardRail("npu3", "npu3", true), cardRail("npu4", "npu4", false), { rail: "npu7", source_ip: "172.20.1.7", gbps: 0, code: "no_peer_rail", side: "source", error: "no target rail to pair with" }] };
  const offline = { ...cardPair(2, 0, "untested", () => true), rails: [], status: "failed", reachable: false, code: "offline", side: "target", error: "target is offline" };
  stub("network", async () => cardNetwork(3, 2, cardRun("card_ping", "ring", "full", [healthy, broken, offline] as never)));
  const user = userEvent.setup();
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  // The server's verdict is the healthy shape; the pairs that differ from it are named.
  assert.ok(await screen.findByText(n.test.card.verdict.ping.full));
  // The card without a namesake was never pinged: four pairs were, three answered.
  assert.ok(screen.getByText(/3 node pairs · 3\/4 card pairs reachable/));
  assert.ok(screen.getByText(/^Node pairs that differ from the rest \(2\): node-01 ↔ node-02, node-02 ↔ node-00\./));
  assert.ok(screen.getByLabelText("node-00 ↔ node-01: All same-named"));
  const notTested = screen.getByLabelText("node-02 ↔ node-00: Not tested");
  notTested.focus();
  await waitFor(() => assert.ok(screen.getAllByRole("tooltip").some((tip) => tip.textContent === "node-00 is offline")));
  await user.click(screen.getByLabelText("node-01 ↔ node-02: Partly reachable 1/2"));
  const dialog = await modalDialog();
  assert.ok(within(dialog).getByText("0.25 ms"));
  assert.ok(within(dialog).getByText("No answer within the time limit"));
  assert.ok(within(dialog).getByText("This card of node-01 has no namesake on the other node"));
});

test("a card test that is running or was never run says so", async () => {
  stub("network", async () => cardNetwork(3, 2));
  stub("estimateNetworkTest", async () => estimateOf(3, 3, 1, 6) as never);
  const user = userEvent.setup();
  const first = renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  assert.ok(await screen.findByText(n.test.none));
  first.unmount();
  stub("network", async () => cardNetwork(3, 2, { ...cardRun("card_matrix", "mesh", "", []), status: "running", node_count: 3, pair_count: 3, card_shape: undefined }));
  renderAt("/networks/1", <NetworkDetailPage />, "/networks/:id");
  await user.click(await screen.findByRole("tab", { name: n.tabs.test }));
  assert.ok(await screen.findByText("Testing 3 nodes, 3 pairs…"));
  assert.equal((screen.getAllByRole("button", { name: n.test.start })[0] as HTMLButtonElement).disabled, true);
});
