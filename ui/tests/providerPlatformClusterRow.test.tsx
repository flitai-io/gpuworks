import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { account, cloudEntry, kubeEntry, servePlatforms } from "./helpers/platformFixtures.ts";
import { chooseRowAction, confirmDialog, rowActions } from "./helpers/rowActions.ts";

// A Kubernetes cluster connected on its own, as a row of the External
// Platforms page: where its entry stands (ui/src/pages/kubernetes/KubeEntryStatus.tsx),
// what its row opens to (ui/src/pages/platforms/StandaloneClusterDetail.tsx),
// what an administrator and a viewer get in it, and the ?open= address that
// opens the page on one row.

const en = await createI18n(["providers", "kubernetes"], "en");
const zh = await createI18n(["providers", "kubernetes"], "zh");
const languages = { en: en.i18n, zh: zh.i18n };
const k = en.locale.kubernetes;
const p = en.locale.providers;
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { api } = await import("../src/api.ts");
const { cardReturnText } = await import("../src/kubeClusters.ts");
const { FeedbackHost } = await import("../src/ui-kit/feedback/index.tsx");
import type { ProviderCluster } from "../src/api";

const original = { ...api };
// A cluster group is found by its entry with CSS.escape, which every browser has and jsdom does not.
(globalThis as unknown as { CSS?: unknown }).CSS ??= { escape: (value: string) => value.replace(/[^\w-]/g, (char) => `\\${char}`) };
afterEach(() => { cleanup(); Object.assign(api, original); });

const LAB_COMMAND = "kubectl apply -f 'https://gpuworks.example/kube/manifest.yaml?token=lab-token'";
const ali = account(3, "ali-prod", "aliyun", { instance_count: 5, node_count: 4 });
const pods = account(1, "Pods", "runpod", { instance_count: 2 });
// Three nodes joined; their agents reported two with NVIDIA cards and one with Ascend.
const prod = kubeEntry({ vendors: [{ vendor: "nvidia", nodes: 2 }, { vendor: "ascend", nodes: 1 }] });
const lab = kubeEntry({ id: 2, uuid: "c-2", name: "lab", namespace: "gpu-lab", node_selector: "", cluster_uid: "", node_count: 0, vendors: [], idle_return_seconds: -1, apply_command: LAB_COMMAND });
const doks = kubeEntry({ id: 4, uuid: "c-4", name: "doks", cluster_uid: "", node_count: 0, vendors: [], stalled: true });
const retired = kubeEntry({ id: 3, uuid: "c-3", name: "retired", revoked: true, node_count: 0, vendors: [], idle_return_seconds: 7200 });
// Connected from ali-prod's cluster c-gpu: listed under the account.
const ack = cloudEntry({ node_count: 4, cluster_uid: "0b9d6c1e-0000-4000-8000-00000000000a" });
const ackCluster: ProviderCluster = {
  external_id: "c-gpu", name: "ack-gpu", region: "cn-shanghai", version: "1.35.7-aliyun.1", type: "ack_managed", type_label: { en: "ACK managed", zh: "ACK 托管版" },
  state: "running", running: true, public_api: true, connectable: false, entry: { uuid: ack.uuid, name: ack.name },
  gpu_pools: [{ id: "np-l20", name: "l20", gpu_model: "NVIDIA L20", gpu_count: 1, nodes: 4, vendor: "nvidia", in_entry: true }],
};

function renderPage(address = "/providers", language: keyof typeof languages = "en") {
  const view = render(<I18nextProvider i18n={languages[language]}><FeedbackHost /><MemoryRouter initialEntries={[address]}><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
  return { view, user: userEvent.setup({ document: dom.window.document }) };
}
// A row's name is plain text in its cell.
const rowOf = async (name: string) => (await screen.findByText(name, { selector: "td" })).closest("tr") as HTMLElement;
const statusCell = async (name: string) => within(await rowOf(name)).getAllByRole("cell")[screen.getAllByRole("columnheader").findIndex((header) => ["Status", "状态"].includes(header.textContent ?? ""))];
/** The rows whose detail is open, by name. */
const opened = () => screen.queryAllByRole("button", { name: "Collapse row" }).map((button) => within(button.closest("tr") as HTMLElement).getAllByRole("cell")[1].textContent);
/** The opened row's facts, term to definition, in the order listed: the description list that starts with how the cluster was connected. */
const facts = (first: string = p.standalone.connectedBy) => {
  const list = screen.getByText(first, { selector: "dt" }).closest("dl") as HTMLElement;
  const terms = within(list).getAllByRole("term").map((term) => term.textContent ?? "");
  const values = within(list).getAllByRole("definition").map((value) => value.textContent ?? "");
  assert.equal(terms.length, values.length, "every term has its value");
  return Object.fromEntries(terms.map((term, index) => [term, values[index]]));
};
/** The opened row's detail: what follows the row. */
const detailOf = async (name: string) => (await rowOf(name)).nextElementSibling as HTMLElement;
const states = {
  en: { joined: "Active", waiting: "Not connected yet", stalled: "Not connected for long", troubleshoot: "Troubleshoot", revoked: "Revoked" },
  zh: { joined: "正常", waiting: "未接入", stalled: "长时间未接入", troubleshoot: "排查", revoked: "已吊销" },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} a cluster's row says where its entry stands: joined, waiting for its first node, waiting too long (with a way to troubleshoot), or revoked`, async () => {
    const copy = states[language];
    servePlatforms(api, { providers: [], clusters: [prod, lab, doks, retired] });
    renderPage("/providers", language);

    assert.equal((await statusCell("prod-gpu")).textContent, copy.joined);
    const waiting = await statusCell("lab");
    assert.equal(waiting.textContent, copy.waiting);
    assert.equal(within(waiting).queryAllByRole("button").length, 0, "the server did not call it stalled");
    const stalled = await statusCell("doks");
    // Named as the status filter names it.
    assert.ok(within(stalled).getByText(copy.stalled));
    assert.ok(within(stalled).getByRole("button", { name: copy.troubleshoot }));
    assert.equal((await statusCell("retired")).textContent, copy.revoked);
  });
}

test("a cluster the server calls stalled offers the commands and causes to look at, in its namespace", async () => {
  servePlatforms(api, { providers: [], clusters: [kubeEntry({ ...doks, namespace: "gpu-edge" })] });
  const { user } = renderPage();
  await user.click(within(await rowOf("doks")).getByRole("button", { name: k.clusterUid.troubleshoot }));
  assert.ok(await screen.findByText(k.stalled.title));
  assert.ok(screen.getByText(k.stalled.intro));
  for (const key of ["pull", "runtimeClass", "selector", "connect"] as const) assert.ok(screen.getByText(k.stalled[key]));
  const commands = screen.getByLabelText(k.stalled.copy, { selector: "pre" }).textContent ?? "";
  assert.ok(commands.includes("kubectl -n gpu-edge describe daemonset gpuworks-agent"), commands);
  await user.click(screen.getByRole("button", { name: k.stalled.copy }));
  assert.equal(await navigator.clipboard.readText(), commands);
});

test("a cluster's row opens to how it was connected and what it selects, laid out flat as terms and values with no card around them; once nodes joined their count, by the vendor of their cards, links to them and no command is repeated", async () => {
  servePlatforms(api, { providers: [], clusters: [prod] });
  const { user } = renderPage();
  await user.click(within(await rowOf("prod-gpu")).getByRole("button", { name: "Expand row" }));

  await screen.findByText(p.standalone.connectedBy);
  const shown = facts();
  assert.deepEqual(Object.keys(shown), [p.standalone.connectedBy, k.drawer.nodeSelector, k.column.namespace, k.column.cardReturn, k.column.createdAt, p.clusters.field.nodes], "six facts in one list: no vendor of the entry's own, its nodes report theirs");
  const detail = await detailOf("prod-gpu");
  assert.equal(detail.querySelectorAll('[data-slot="card"], [data-slot="container"]').length, 0, "no card or tray");
  assert.equal(within(detail).queryAllByRole("heading").length, 0);
  assert.equal(shown[p.standalone.connectedBy], p.standalone.byKubectl);
  assert.equal(shown[k.drawer.nodeSelector], "nvidia.com/gpu.present=true");
  assert.equal(shown[k.column.namespace], "gpuworks-system");
  assert.equal(shown[k.column.cardReturn], "Returned after 10 min idle");
  // The server's 2026-10-19T08:30:00Z in the viewer's time zone.
  assert.match(shown[k.column.createdAt] ?? "", /^2026-10-(18|19|20) \d{2}:\d{2}$/);

  assert.equal(shown[p.clusters.field.nodes], "3 nodes · NVIDIA ×2 · Ascend ×1");
  assert.equal(within(detail).getByRole("link", { name: "3 nodes · NVIDIA ×2 · Ascend ×1" }).getAttribute("href"), "/nodes?source=cluster%3Ac-1", "the node list filtered to this cluster as their source");
  assert.equal(screen.queryAllByLabelText(k.actions.copyCommand, { selector: "pre" }).length, 0, "the cluster is connected: no command to run");
  assert.equal(screen.queryAllByText(p.standalone.waiting).length, 0);
});

test("until a node joins, the opened row gives the command to run and copies it; a cluster without a selector takes all nodes and one that keeps its cards says so", async () => {
  servePlatforms(api, { providers: [], clusters: [lab] });
  const { user } = renderPage();
  await user.click(within(await rowOf("lab")).getByRole("button", { name: "Expand row" }));

  assert.ok(await screen.findByText(p.standalone.waiting));
  const shown = facts();
  assert.equal(shown[k.drawer.nodeSelector], p.standalone.allNodes);
  assert.equal(shown[k.column.namespace], "gpu-lab");
  assert.equal(shown[k.column.cardReturn], k.returnPolicy.keep);
  const detail = screen.getByText(p.standalone.waiting).parentElement as HTMLElement;
  assert.equal(within(detail).getByLabelText(k.actions.copyCommand, { selector: "pre" }).textContent, LAB_COMMAND);
  await user.click(within(detail).getByRole("button", { name: k.actions.copyCommand }));
  assert.equal(await navigator.clipboard.readText(), LAB_COMMAND);
  assert.equal(shown[p.clusters.field.nodes], "0 nodes");
  assert.equal(within(await detailOf("lab")).queryAllByRole("link").length, 0, "no node to view yet");
});

test("a revoked cluster no node joined says the token is revoked and gives no command", async () => {
  servePlatforms(api, { providers: [], clusters: [retired] });
  const { user } = renderPage();
  await user.click(within(await rowOf("retired")).getByRole("button", { name: "Expand row" }));

  assert.ok(await screen.findByText(p.standalone.revoked));
  assert.equal(facts()[k.column.cardReturn], "Returned after 2 h idle");
  assert.equal(facts()[p.clusters.field.nodes], "0 nodes");
  assert.equal(screen.queryAllByText(p.standalone.waiting).length, 0);
  assert.equal(screen.queryAllByLabelText(k.actions.copyCommand, { selector: "pre" }).length, 0);
  assert.equal(document.body.textContent?.includes("secret-token"), false, "a revoked token is not shown again");
});

test("in zh the opened row of a cluster reads in Chinese", async () => {
  servePlatforms(api, { providers: [], clusters: [prod, lab, retired] });
  const { user } = renderPage("/providers", "zh");
  await user.click(within(await rowOf("prod-gpu")).getByRole("button", { name: "展开行" }));
  await screen.findByText("接入方式", { selector: "dt" });
  assert.deepEqual(facts("接入方式"), {
    "接入方式": "kubectl apply（自行执行）", "节点选择器": "nvidia.com/gpu.present=true", "命名空间": "gpuworks-system",
    "卡归还": facts("接入方式")["卡归还"], "创建时间": facts("接入方式")["创建时间"], "已接入节点": "3 个节点 · NVIDIA ×2 · 昇腾 ×1",
  });
  assert.ok(screen.getByRole("link", { name: "3 个节点 · NVIDIA ×2 · 昇腾 ×1" }));

  await user.click(within(await rowOf("lab")).getByRole("button", { name: "展开行" }));
  assert.ok(await screen.findByText("还没有节点接入。在集群里执行下面的命令，选中的 GPU 节点约半分钟后出现在「计算节点」页。"));
  assert.ok(screen.getByText("全部节点"));

  await user.click(within(await rowOf("retired")).getByRole("button", { name: "展开行" }));
  assert.ok(await screen.findByText("接入令牌已吊销，新节点不能再用接入命令加入。"));
});

test("the return policy reads in seconds, minutes or hours, whichever divides evenly", () => {
  const t = en.i18n.getFixedT("en", "kubernetes");
  assert.equal(cardReturnText(t, 45), "Returned after 45 s idle");
  assert.equal(cardReturnText(t, 90), "Returned after 90 s idle");
  assert.equal(cardReturnText(t, 600), "Returned after 10 min idle");
  assert.equal(cardReturnText(t, 3600), "Returned after 1 h idle");
  assert.equal(cardReturnText(t, 0), "Returned after 0 s idle");
  assert.equal(cardReturnText(t, -1), "Kept (never returned)");
});

test("?open=k<uuid> opens the page with that cluster's row open, and only that row", async () => {
  servePlatforms(api, { providers: [ali, pods], clusters: [prod, lab] });
  renderPage("/providers?open=kc-2");
  assert.ok(await screen.findByText(p.standalone.waiting), "the row of lab is open on its detail");
  assert.deepEqual(opened(), ["lab"]);
});

test("?open=p<id> opens an account's row on what it holds, read at once and only for that row; another account opened by hand reads its own", async () => {
  const listings: number[] = [];
  const dev = account(7, "ali-dev", "aliyun", { instance_count: 1 });
  servePlatforms(api, { providers: [ali, dev, pods], clusters: [prod, ack] });
  Object.assign(api, { providerClusters: async (id: number) => { listings.push(id); return { items: id === 3 ? [ackCluster] : [], total: id === 3 ? 1 : 0 }; } });
  const { user } = renderPage("/providers?open=p3");
  await waitFor(() => assert.deepEqual(opened(), ["ali-prod"]));
  assert.ok(await screen.findByRole("rowgroup", { name: "ack-gpu" }), "the account's cluster, read from the cloud");
  assert.deepEqual(listings, [3]);
  assert.equal(screen.queryAllByRole("tab").length, 0, "no tabs to open on");

  await user.click(within(await rowOf("ali-dev")).getByRole("button", { name: "Expand row" }));
  await waitFor(() => assert.deepEqual(opened(), ["ali-prod", "ali-dev"]));
  await waitFor(() => assert.deepEqual(listings, [3, 7]));
});

test("&cluster=<entry> with ?open=p<id> brings that cluster's group into view once the cloud has answered, and only once", async () => {
  const scrolled: string[] = [];
  const original = dom.window.HTMLElement.prototype.scrollIntoView;
  // jsdom does no layout: what is asked of the browser is recorded.
  dom.window.HTMLElement.prototype.scrollIntoView = function (this: HTMLElement) { scrolled.push(this.getAttribute("aria-label") ?? this.tagName); };
  try {
    const other = { ...ackCluster, external_id: "c-other", name: "ack-other", entry: undefined, connectable: true, gpu_pools: ackCluster.gpu_pools.map((pool) => ({ ...pool, in_entry: false })) };
    let answer!: (clusters: unknown) => void;
    servePlatforms(api, { providers: [ali], clusters: [ack] });
    Object.assign(api, { providerClusters: () => new Promise((resolve) => { answer = resolve; }) });
    renderPage(`/providers?open=p3&cluster=${ack.uuid}`);
    await waitFor(() => assert.deepEqual(opened(), ["ali-prod"]));
    assert.deepEqual(scrolled, [], "nothing to bring into view before the cloud answers");
    answer({ items: [other, ackCluster], total: 2 });
    await screen.findByRole("rowgroup", { name: "ack-gpu" });
    await waitFor(() => assert.deepEqual(scrolled, ["ack-gpu"]));
    assert.equal(screen.queryAllByRole("alert").length, 0);
  } finally {
    dom.window.HTMLElement.prototype.scrollIntoView = original;
  }
});

for (const address of ["/providers?open=p3&cluster=missing", "/providers?open=p1&cluster=e-5", "/providers?cluster=e-5"]) {
  test(`${address} names no cluster group to bring into view: the page opens as it would without it, without an error`, async () => {
    servePlatforms(api, { providers: [ali, pods], clusters: [prod, ack] });
    Object.assign(api, { providerClusters: async (id: number) => ({ items: id === 3 ? [ackCluster] : [], total: id === 3 ? 1 : 0 }) });
    renderPage(address);
    await screen.findByText("prod-gpu");
    const open = new URLSearchParams(address.split("?")[1]).get("open");
    await waitFor(() => assert.deepEqual(opened(), open === "p3" ? ["ali-prod"] : open === "p1" ? ["Pods"] : []));
    if (open === "p3") await screen.findByRole("rowgroup", { name: "ack-gpu" });
    if (open === "p1") assert.ok(await screen.findByText(p.instances.empty));
    assert.equal(screen.queryAllByRole("alert").length, 0);
  });
}

for (const address of ["/providers?open=p999", "/providers?open=kmissing", "/providers?open=c-1", "/providers?open=p3x"]) {
  test(`${address} names no row in the list: the list opens whole with nothing opened and no error`, async () => {
    servePlatforms(api, { providers: [ali, pods], clusters: [prod, lab] });
    renderPage(address);
    await screen.findByText("prod-gpu");
    assert.deepEqual(screen.getAllByRole("row").slice(1).map((row) => within(row).getAllByRole("cell")[1].textContent), ["ali-prod", "Pods", "prod-gpu", "lab"]);
    assert.deepEqual(opened(), []);
    assert.equal(screen.queryAllByRole("alert").length, 0);
  });
}

test("a viewer sees the clusters and their state, but no actions and no enrollment token, even in an opened row that waits for nodes", async () => {
  servePlatforms(api, { providers: [], clusters: [prod, lab, retired], admin: false });
  const { user } = renderPage();
  const row = await rowOf("lab");
  assert.equal((await statusCell("prod-gpu")).textContent, k.status.active);
  for (const action of [k.actions.edit, k.actions.copyCommand, en.locale.common.actions.more]) {
    assert.equal(screen.queryAllByRole("button", { name: action }).length, 0, `${action} is an administrator's`);
  }
  assert.deepEqual(await rowActions(row), { shown: [], folded: [] }, "nothing to revoke, delete or read the manifest of");
  await user.click(within(row).getByRole("button", { name: "Expand row" }));
  assert.ok(await screen.findByText(p.standalone.waiting));
  assert.equal(screen.queryAllByLabelText(k.actions.copyCommand, { selector: "pre" }).length, 0);
  assert.equal(document.body.textContent?.includes("token="), false);
});

test("an administrator's actions on a cluster row are those of an entry made by hand, and revoking one reloads the list", async () => {
  const server = servePlatforms(api, { providers: [ali], clusters: [prod, lab] });
  const revoked: string[] = [];
  Object.assign(api, { revokeKubeCluster: async (uuid: string) => { revoked.push(uuid); server.clusters = [{ ...prod, revoked: true, state: "revoked" as const }, lab]; return { ...prod, revoked: true, state: "revoked" as const }; } });
  const { user } = renderPage();
  const row = await rowOf("prod-gpu");
  await waitFor(() => assert.deepEqual(
    within(row).getAllByRole("button").map((button) => button.getAttribute("aria-label")),
    ["Expand row", k.actions.edit, k.actions.copyCommand, en.locale.common.actions.more],
  ));
  assert.deepEqual(await rowActions(row), {
    shown: [k.actions.edit, k.actions.copyCommand, en.locale.common.actions.more],
    folded: [k.actions.manifest, k.actions.revoke, k.actions.delete],
  }, "editing the entry, but no node pools, apply or remove: gpuworks has no way into a cluster connected by hand");
  const account = within(await rowOf("ali-prod"));
  assert.equal(account.queryAllByRole("button", { name: k.actions.revoke }).length + account.queryAllByRole("button", { name: en.locale.common.actions.more }).length, 0, "an account's row keeps its own actions, none folded");
  assert.equal(account.getAllByRole("button", { name: p.table.sync }).length, 1);

  const before = server.reads.clusters;
  await chooseRowAction(user, row, k.actions.revoke);
  await confirmDialog(user, k.actions.revoke);
  await waitFor(() => assert.deepEqual(revoked, ["c-1"]));
  await waitFor(() => assert.equal(server.reads.clusters, before + 1, "the cluster list is read again"));
  await waitFor(async () => assert.equal((await statusCell("prod-gpu")).textContent, k.status.revoked));
});

test("a cluster's name is its name and nothing else: clicking it opens neither a dialog nor the row; a cluster has no account to synchronize, and its Edit edits the entry, not an account", async () => {
  servePlatforms(api, { providers: [ali], clusters: [prod, lab] });
  const { user } = renderPage();
  const row = await rowOf("prod-gpu");
  const name = within(row).getAllByRole("cell")[1];
  assert.equal(name.textContent, "prod-gpu");
  assert.equal(within(name).queryAllByRole("button").length + within(name).queryAllByRole("link").length, 0);
  await user.click(name);
  assert.equal(screen.queryAllByRole("dialog").length, 0);
  assert.deepEqual(opened(), []);
  await waitFor(() => assert.ok(within(row).getByRole("button", { name: k.actions.edit }), "the administrator's actions have arrived"));
  assert.equal(within(row).queryAllByRole("button", { name: p.table.sync }).length, 0, "Sync is an account's");
  assert.equal(within(await rowOf("ali-prod")).getAllByRole("button", { name: p.table.sync }).length, 1);
  // Both rows have an Edit, each for what the row is.
  assert.equal(within(row).getAllByRole("button", { name: k.actions.edit }).length, 1);
  assert.equal(within(await rowOf("ali-prod")).getAllByRole("button", { name: p.table.edit }).length, 1);
  await user.click(within(row).getByRole("button", { name: k.actions.edit }));
  const drawer = within(await screen.findByRole("dialog", { name: k.drawer.editTitle }));
  assert.equal((drawer.getByLabelText(/^Name( \*)?$/) as HTMLInputElement).value, "prod-gpu", "the cluster entry's form, on this entry");
  assert.equal(screen.getAllByRole("dialog").length, 1, "and no account dialog");
});

test("deleting a cluster from its row takes it off the list", async () => {
  const server = servePlatforms(api, { providers: [ali], clusters: [prod, lab] });
  const deleted: string[] = [];
  Object.assign(api, { deleteKubeCluster: async (uuid: string) => { deleted.push(uuid); server.clusters = [prod]; } });
  const { user } = renderPage();
  await chooseRowAction(user, await rowOf("lab"), k.actions.delete);
  await confirmDialog(user, k.actions.delete);
  await waitFor(() => assert.deepEqual(deleted, ["c-2"]));
  await waitFor(() => assert.equal(screen.queryAllByText("lab").length, 0));
  assert.ok(screen.getByText("External platforms: 2 · Compute nodes: 7"));
});

// The server names the entries that may be of the same cluster and use the same namespace (master's
// namespace_shared_with): a local cluster's row says so beside its name, with whom and in which namespace on hover;
// one sharing with nobody carries nothing.
for (const language of ["en", "zh"] as const) {
  test(`in ${language} a local cluster that shares its namespace with another entry says so beside its name, and one that does not says nothing`, async () => {
    const copy = language === "en" ? en.locale.kubernetes : zh.locale.kubernetes;
    const shared = kubeEntry({ id: 5, uuid: "c-5", name: "lab-a", namespace: "gpuworks-system", namespace_shared_with: ["lab-b", "lab-c"] });
    servePlatforms(api, { providers: [], clusters: [shared, lab] });
    const { user } = renderPage("/providers", language);
    const badge = await screen.findByText(copy.namespaceShared.badge);
    const row = badge.closest("tr") as HTMLElement;
    const name = within(row).getAllByRole("cell")[1];
    assert.ok(within(name).getByText("lab-a"), "the badge sits in the name's cell");
    assert.equal(within(name).queryAllByRole("button").length + within(name).queryAllByRole("link").length, 0, "the name is still plain text");
    assert.equal(within(await rowOf("lab")).queryAllByText(copy.namespaceShared.badge).length, 0, "an entry sharing with nobody carries nothing");
    await user.hover(badge);
    const hint = copy.namespaceShared.hint.replace("{{names}}", "lab-b, lab-c").replace("{{namespace}}", "gpuworks-system");
    assert.ok(await screen.findByText(hint), "on hover it names the others and the namespace");
  });
}
