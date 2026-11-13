import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { account, aliyunKind, cloudCluster, cloudEntry, connectedCluster, instance, kubeEntry, servePlatforms } from "./helpers/platformFixtures.ts";
import { clusterHeader, factValue, facts, groupHeader, groupNames, groupOf } from "./helpers/platformDetail.ts";
import { list } from "./helpers/listPage.ts";
import { chooseRowAction, confirmDialog, findRowAction, rowActions } from "./helpers/rowActions.ts";

// The cluster entries made from a cloud account's clusters, where the console
// keeps them: each cluster's group in the account's table
// (ui/src/pages/platforms/ClusterGroup.tsx in ui/src/pages/platforms/ProviderResources.tsx)
// under the account's row of the External Platforms page. Where an entry
// stands, what it is and how many nodes joined (by the vendor of their cards),
// what an administrator can do to it from the group's header, the entries the
// cloud's listing does not account for, and the page around it.

const en = await createI18n(["providers", "kubernetes"], "en");
const zh = await createI18n(["providers", "kubernetes"], "zh");
const languages = { en: en.i18n, zh: zh.i18n };
const k = en.locale.kubernetes;
const p = en.locale.providers;
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: ProviderResources } = await import("../src/pages/platforms/ProviderResources.tsx");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { formatTime } = await import("../src/i18n/datetime.ts");
const { cardReturnText } = await import("../src/kubeClusters.ts");
const { FeedbackHost } = await import("../src/ui-kit/feedback/index.tsx");
import type { KubeCluster, ProviderKindInfo } from "../src/api";

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

/** An entry of account 3 made from its cluster with that id. */
const entryOf = (uuid: string, name: string, clusterID: string, overrides: Partial<KubeCluster> = {}) =>
  cloudEntry({ uuid, name, source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: clusterID }, ...overrides });
// Three nodes joined: two with NVIDIA cards, one with Ascend, as their agents reported.
const joined = entryOf("e-1", "ack-gpu", "c-gpu", { id: 11, node_count: 3, vendors: [{ vendor: "nvidia", nodes: 2 }, { vendor: "ascend", nodes: 1 }], cluster_uid: "0b9d6c1e-0000-4000-8000-00000000000a" });
const waiting = entryOf("e-2", "ack-lab", "c-lab", { id: 12 });

const user = () => userEvent.setup({ document: dom.window.document });
function renderDetail({ entries = [] as KubeCluster[], admin = false, onChanged = () => {}, kindInfo = aliyunKind as ProviderKindInfo | undefined, language = "en" as keyof typeof languages } = {}) {
  api.providerInstances = (async () => list([])) as never;
  return render(
    <I18nextProvider i18n={languages[language]}>
      {/* The console's frame hosts the confirmations and toasts. */}
      <FeedbackHost />
      <MemoryRouter>
        <ProviderResources provider={account(3, "ali-prod", "aliyun")} kindInfo={kindInfo} refreshKey="1"
          entries={Object.fromEntries(entries.map((entry) => [entry.uuid, entry]))} onEntriesChanged={onChanged} admin={admin} />
      </MemoryRouter>
    </I18nextProvider>,
  );
}
function renderPage(address = "/providers", language: keyof typeof languages = "en") {
  return render(<I18nextProvider i18n={languages[language]}><FeedbackHost /><MemoryRouter initialEntries={[address]}><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
}
/** The cluster groups listed, in order: every group but the account's own instances. */
const clusterNames = async () => (await groupNames()).filter((name) => !["Standalone instances", "独立实例"].includes(name));
/** The entry's actions offered in a group's header: the buttons left in it and the ones folded into More. */
const actionsIn = async (group: HTMLElement) => rowActions(await groupHeader(group));
const NONE = { shown: [], folded: [] };
// What an administrator can do to an entry made from an account's cluster: Edit and Apply stay in the
// header, the rest fold into More, the destructive ones last. gpuworks removes it itself (can_remove), so
// there is no by-hand uninstall.
const ENTRY_ACTIONS = { shown: ["edit", "apply"], folded: ["copyCommand", "manifest", "remove", "revoke", "delete"] } as const;
const entryActions = (language: "en" | "zh" = "en") => {
  const { locale } = language === "en" ? en : zh;
  return { shown: [...ENTRY_ACTIONS.shown.map((action) => locale.kubernetes.actions[action]), locale.common.actions.more], folded: ENTRY_ACTIONS.folded.map((action) => locale.kubernetes.actions[action]) };
};
// The operation dialog is the last one opened.
const modal = () => within(screen.getAllByRole("dialog").at(-1) as HTMLElement);

const groupCopy = {
  en: {
    managed: "ACK managed", joined: "Active", waiting: "Not connected yet",
    labels: ["Cluster ID", "Region", "Version", "API server", "GPU node pools", "Nodes joined", "Namespace", "Card return", "Created"],
    api: "Publicly reachable", nodes: "3 nodes · NVIDIA ×2 · Ascend ×1", none: "0 nodes",
  },
  zh: {
    managed: "ACK 托管版", joined: "正常", waiting: "未接入",
    labels: ["集群 ID", "地域", "版本", "API Server", "GPU 节点池", "已接入节点", "命名空间", "卡归还", "创建时间"],
    api: "公网可访问", nodes: "3 个节点 · NVIDIA ×2 · 昇腾 ×1", none: "0 个节点",
  },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} a connected cluster's group says what the cluster is, where its entry stands, its namespace, card return and creation, and how many nodes joined by vendor, linked to those nodes; one without nodes says 0 and links nowhere; one not connected has no entry facts`, async () => {
    const copy = groupCopy[language];
    const { i18n } = language === "en" ? en : zh;
    api.providerClusters = (async () => list([connectedCluster("c-gpu", "ack-gpu", "e-1"), connectedCluster("c-lab", "ack-lab", "e-2"), cloudCluster("c-free", "ack-free")])) as never;
    renderDetail({ entries: [joined, waiting], language });

    const connected = await groupOf("ack-gpu");
    assert.deepEqual(await clusterHeader(connected), { title: ["ack-gpu", copy.managed], standing: copy.joined });
    assert.deepEqual(await facts(connected), Object.fromEntries(copy.labels.map((label, index) => [label, [
      "c-gpu", "cn-shanghai", "1.35.7-aliyun.1", copy.api, language === "en" ? "l20 · NVIDIA L20 ×1 · 2 nodes" : "l20 · NVIDIA L20 ×1 · 2 个节点",
      copy.nodes, "gpuworks-system", cardReturnText(i18n.getFixedT(language, "kubernetes"), 600), formatTime("2026-10-19T08:30:00Z"),
    ][index]])));
    const nodes = await factValue(connected, copy.labels[5]);
    assert.equal(within(nodes).getByRole("link", { name: copy.nodes }).getAttribute("href"), "/nodes?source=cluster%3Ae-1", "the node list filtered to this entry as their source");
    assert.equal(within(await groupHeader(connected)).queryAllByRole("link").length, 0, "the count is with the facts, not in the header");

    const idle = await groupOf("ack-lab");
    assert.equal((await clusterHeader(idle)).standing, copy.waiting);
    assert.equal((await facts(idle))[copy.labels[5]], copy.none);
    assert.equal(within(idle).queryAllByRole("link").length, 0, "no node to open");
    const free = await groupOf("ack-free");
    assert.equal((await clusterHeader(free)).standing, copy.waiting);
    assert.deepEqual(Object.keys(await facts(free)), copy.labels.slice(0, 5), "no entry: no node count, namespace, card return or creation");
  });
}

test("an administrator gets the entry's actions in a connected cluster's header, beside where it stands, and Change under its pools; a viewer gets neither", async () => {
  api.providerClusters = (async () => list([connectedCluster("c-gpu", "ack-gpu", "e-1"), cloudCluster("c-free", "ack-free")])) as never;
  const admin = renderDetail({ entries: [joined], admin: true });
  const group = await groupOf("ack-gpu");
  assert.deepEqual(await actionsIn(group), entryActions());
  assert.ok(within(await factValue(group, p.clusters.column.gpuPools)).getByRole("button", { name: p.clusters.editPools }), "its pools are changed from the pools' own fact");
  assert.deepEqual(await actionsIn(await groupOf("ack-free")), NONE, "nothing to act on before the cluster is connected");
  assert.ok(within(await groupOf("ack-free")).getByRole("button", { name: p.clusters.connect }));
  admin.unmount();

  renderDetail({ entries: [joined] });
  const seen = await groupOf("ack-gpu");
  assert.equal((await clusterHeader(seen)).standing, k.status.active, "the state is everyone's to see");
  assert.deepEqual(await actionsIn(seen), NONE);
  assert.equal(within(seen).queryAllByRole("button", { name: p.clusters.editPools }).length, 0);
});

test("applying from a cluster's header checks first; a refused check names the step and the cluster's reason, and nothing can be confirmed", async () => {
  api.providerClusters = (async () => list([connectedCluster("c-gpu", "ack-gpu", "e-1")])) as never;
  const ops: boolean[] = [];
  api.kubeClusterOperation = (async (_uuid: string, _op: string, dryRun: boolean) => {
    ops.push(dryRun);
    return {
      dry_run: true, code: "KUBE_CLUSTER_OPERATION_FAILED", error: "Kubernetes API server: forbidden",
      params: { step: "api_server", reason: "clusterroles is forbidden: User \"ram-user\" cannot patch" },
      results: [{ kind: "Namespace", name: "gpuworks-system", action: "create" }, { kind: "ClusterRole", name: "gpuworks-agent-gpuworks-system", action: "failed", error: "forbidden" }],
    };
  }) as never;
  const u = user();
  renderDetail({ entries: [joined], admin: true });
  await u.click(within(await groupOf("ack-gpu")).getByRole("button", { name: k.actions.apply }));
  assert.ok(await modal().findByText(k.operation.failed.api_server));
  assert.ok(modal().getByText(k.operation.applyIntro.replace("{{account}}", "ali-prod").replace("{{cluster}}", "c-gpu")), "which account's credentials and which cluster");
  assert.ok(modal().getByText("clusterroles is forbidden: User \"ram-user\" cannot patch"));
  assert.equal(modal().queryAllByRole("button", { name: k.operation.confirmApply }).length, 0);
  await u.click(modal().getByRole("button", { name: k.operation.retry }));
  await waitFor(() => assert.equal(ops.length, 2));
  assert.deepEqual(ops, [true, true], "only dry runs");
});

test("removing from the cluster checks first, then deletes on confirmation, and the cloud is read again and the page told", async () => {
  let listings = 0;
  api.providerClusters = (async () => { listings += 1; return list([connectedCluster("c-gpu", "ack-gpu", "e-1")]); }) as never;
  const ops: [string, string, boolean][] = [];
  api.kubeClusterOperation = (async (uuid: string, op: string, dryRun: boolean) => {
    ops.push([uuid, op, dryRun]);
    return { dry_run: dryRun, results: [{ kind: "DaemonSet", name: "gpuworks-agent", namespace: "gpuworks-system", action: "delete" },
      { kind: "PriorityClass", name: "gpuworks-inference", action: "kept", reason: "used by gpuworks in namespace gpuworks-npu" }] };
  }) as never;
  const u = user();
  let changed = 0;
  renderDetail({ entries: [joined], admin: true, onChanged: () => { changed += 1; } });
  await chooseRowAction(u, await groupOf("ack-gpu"), k.actions.remove);
  assert.ok(await modal().findByText(k.operation.checkedRemove));
  assert.ok(modal().getByText("used by gpuworks in namespace gpuworks-npu"));
  assert.deepEqual([listings, changed], [1, 0], "a check changes nothing");
  await u.click(modal().getByRole("button", { name: k.operation.confirmRemove }));
  assert.ok(await modal().findByText(k.operation.removed));
  assert.deepEqual(ops, [["e-1", "remove", true], ["e-1", "remove", false]]);
  await waitFor(() => assert.deepEqual([listings, changed], [2, 1]));
});

test("a product error before any object (no source any more) is shown as such", async () => {
  api.providerClusters = (async () => list([connectedCluster("c-gpu", "ack-gpu", "e-1")])) as never;
  api.kubeClusterOperation = (async () => { throw new APIError("KUBE_CLUSTER_NO_SOURCE", undefined, "no source", 409); }) as never;
  const u = user();
  renderDetail({ entries: [joined], admin: true });
  await u.click(within(await groupOf("ack-gpu")).getByRole("button", { name: k.actions.apply }));
  assert.ok(await modal().findByText(en.locale.common.apiError.KUBE_CLUSTER_NO_SOURCE));
  assert.ok(modal().getByText(k.operation.failed.other));
});

// An entry whose cluster the cloud's listing does not account for.
const gone = entryOf("e-9", "ack-old", "c-old", { id: 19, node_count: 2, cluster_uid: "5e0c7a44-0000-4000-8000-00000000000b" });
const elsewhere = cloudEntry({ id: 20, uuid: "e-20", name: "do-prod", source: { provider_id: 8, provider_name: "do", provider_kind: "digitalocean", cluster_id: "c-old" } });
const byHand = kubeEntry({ id: 21, uuid: "e-21", name: "by-hand" });

const goneCopy = {
  en: { labels: ["Cluster ID", "GPU node pools", "Nodes joined", "Namespace", "Card return", "Created"], nodes: "2 nodes", standing: "Active", gone: p.clusters.entryGone, unread: p.clusters.entryUnread, failed: "Cannot read the clusters", empty: "This account has no managed Kubernetes clusters" },
  zh: { labels: ["集群 ID", "GPU 节点池", "已接入节点", "命名空间", "卡归还", "创建时间"], nodes: "2 个节点", standing: "正常", gone: zh.locale.providers.clusters.entryGone, unread: zh.locale.providers.clusters.entryUnread, failed: "无法读取集群", empty: "这个账号下没有托管 Kubernetes 集群" },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} an entry whose cluster the account no longer lists keeps a group saying so, with where it stands; entries of other accounts and ones made by hand are not this account's`, async () => {
    const copy = goneCopy[language];
    api.providerClusters = (async () => list([])) as never;
    renderDetail({ entries: [gone, elsewhere, byHand], admin: true, language });

    const group = await groupOf("ack-old");
    assert.deepEqual(await clusterNames(), ["ack-old"]);
    assert.deepEqual(await clusterHeader(group), { title: ["ack-old"], standing: copy.standing }, "the entry's name; what the cluster was is the cloud's to say, and it no longer does");
    const shown = await facts(group);
    assert.deepEqual(Object.keys(shown), copy.labels, "the cloud id it was made from and the entry's own facts; nothing of a region, version or API server");
    assert.equal(shown[copy.labels[0]], "c-old");
    assert.equal(shown[copy.labels[1]], copy.gone, "where its pools would be");
    assert.equal(within(await factValue(group, copy.labels[2])).getByRole("link", { name: copy.nodes }).getAttribute("href"), "/nodes?source=cluster%3Ae-9");
    assert.equal(within(group).queryAllByRole("checkbox").length, 0, "no pools to tick");
    assert.deepEqual(await actionsIn(group), entryActions(language), "an administrator can still revoke, remove or delete it");
    assert.equal(screen.queryAllByText(copy.empty).length, 0, "the account is not without clusters while it holds an entry");
  });

  test(`in ${language} when the cloud's clusters cannot be read the account's entries are still listed by name, saying their pools cannot be shown`, async () => {
    const copy = goneCopy[language];
    api.providerClusters = (async () => { throw new Error("aliyun: InvalidAccessKeyId.NotFound"); }) as never;
    renderDetail({ entries: [gone, joined], language });

    assert.ok(await screen.findByText(copy.failed));
    assert.ok(screen.getByText("aliyun: InvalidAccessKeyId.NotFound"));
    await groupOf("ack-old");
    assert.deepEqual(await clusterNames(), ["ack-gpu", "ack-old"], "by name");
    for (const name of ["ack-gpu", "ack-old"]) assert.ok(within(await groupOf(name)).getByText(copy.unread));
    assert.deepEqual(await actionsIn(await groupOf("ack-old")), NONE, "a viewer still gets no actions");
    assert.equal(screen.queryAllByText(copy.gone).length, 0, "nothing says the clusters are gone: the cloud was not heard");
  });
}

test("while the cloud is still answering no entry is called gone; a cluster the cloud then lists has its one group", async () => {
  let answer!: (clusters: unknown) => void;
  api.providerClusters = (() => new Promise((resolve) => { answer = resolve; })) as never;
  renderDetail({ entries: [joined] });
  assert.ok(await screen.findByText(p.detail.loadingClusters));
  assert.deepEqual(await clusterNames(), []);
  assert.equal(screen.queryAllByText(goneCopy.en.gone).length, 0);

  answer(list([connectedCluster("c-gpu", "ack-gpu", "e-1")]));
  await groupOf("ack-gpu");
  assert.deepEqual(await clusterNames(), ["ack-gpu"]);
  assert.equal(screen.queryAllByText(goneCopy.en.gone).length, 0);
  assert.equal(screen.queryAllByText(p.detail.loadingClusters).length, 0, "the listing is over");
});

test("an account without clusters or entries of its own says so in place of the groups", async () => {
  api.providerClusters = (async () => list([])) as never;
  renderDetail({ entries: [elsewhere, byHand] });
  assert.ok(await screen.findByText(goneCopy.en.empty));
  assert.deepEqual(await clusterNames(), []);
});

test("an entry whose cluster is gone can still be deleted from its group; the cloud is read again and the page told", async () => {
  let listings = 0;
  api.providerClusters = (async () => { listings += 1; return list([]); }) as never;
  const deleted: string[] = [];
  api.deleteKubeCluster = (async (uuid: string) => { deleted.push(uuid); }) as never;
  const u = user();
  let changed = 0;
  renderDetail({ entries: [gone], admin: true, onChanged: () => { changed += 1; } });
  await chooseRowAction(u, await groupOf("ack-old"), k.actions.delete);
  await confirmDialog(u, k.actions.delete);
  await waitFor(() => assert.deepEqual(deleted, ["e-9"]));
  await waitFor(() => assert.deepEqual([listings, changed], [2, 1]));
});

// The account's row on the page.
const ali = account(3, "ali-prod", "aliyun", { instance_count: 1, node_count: 3 });
/** The detail under the account's row. */
const detailOf = async (name: string) => ((await screen.findByText(name, { selector: "td" })).closest("tr") as HTMLElement).nextElementSibling as HTMLElement;

test("opening an account's row on the page reads its instances and its clusters at once, for that account only; the page's entries say where each cluster's entry stands", async () => {
  const listings: number[] = [];
  servePlatforms(api, { providers: [ali, account(4, "ali-dev", "aliyun")], clusters: [joined] });
  Object.assign(api, {
    providerInstances: async () => list([instance()]),
    providerClusters: async (id: number) => { listings.push(id); return list([connectedCluster("c-gpu", "ack-gpu", "e-1")]); },
  });
  const u = user();
  renderPage();
  await screen.findByText("ali-prod");
  assert.deepEqual(listings, [], "nothing is asked of the cloud before a row opens");
  const row = (await screen.findByText("ali-prod", { selector: "td" })).closest("tr") as HTMLElement;
  await u.click(within(row).getByRole("button", { name: "Expand row" }));
  const group = await groupOf("ack-gpu");
  assert.deepEqual(listings, [3]);
  assert.ok(await within(await detailOf("ali-prod")).findByText("train-01"));
  await waitFor(async () => assert.deepEqual([(await clusterHeader(group)).standing, (await facts(group))["Nodes joined"]], [k.status.active, groupCopy.en.nodes]));
  assert.equal(screen.queryAllByRole("tab").length, 0, "no tabs: instances and clusters are one table");
});

test("when the kind schema cannot be loaded an account whose row says it connects Kubernetes still has its clusters, before any is connected", async () => {
  servePlatforms(api, { providers: [ali], clusters: [], kinds: new Error("schema unavailable") });
  Object.assign(api, { providerClusters: async () => list([cloudCluster("c-free", "ack-free")]) });
  renderPage("/providers?open=p3");
  assert.ok(within(await groupOf("ack-free")).getByRole("button", { name: p.clusters.connect }));
});

test("when the kind schema cannot be loaded an administrator still reaches an account's connected entries", async () => {
  servePlatforms(api, { providers: [ali], clusters: [joined], kinds: new Error("schema unavailable") });
  Object.assign(api, { providerClusters: async () => list([connectedCluster("c-gpu", "ack-gpu", "e-1")]) });
  renderPage("/providers?open=p3");
  const group = await groupOf("ack-gpu");
  await waitFor(async () => assert.deepEqual([(await clusterHeader(group)).standing, (await facts(group))["Nodes joined"]], [k.status.active, groupCopy.en.nodes]));
  await waitFor(async () => assert.ok(await findRowAction(group, k.actions.revoke)));
});

test("an account that already holds an entry shows its clusters even when neither its kind nor its row says it connects Kubernetes; the account beside it is ungrouped", async () => {
  // The account's kind is unknown to this console's schema and its row lists no resources.
  servePlatforms(api, { providers: [account(3, "ali-prod", "newcloud"), account(4, "Other", "newcloud")], clusters: [gone] });
  const listings: number[] = [];
  Object.assign(api, { providerClusters: async (id: number) => { listings.push(id); return list([]); } });
  const u = user();
  renderPage("/providers?open=p3");
  assert.ok(await screen.findByText(goneCopy.en.gone));
  assert.deepEqual(await clusterNames(), ["ack-old"]);

  const other = (await screen.findByText("Other", { selector: "td" })).closest("tr") as HTMLElement;
  await u.click(within(other).getByRole("button", { name: "Expand row" }));
  const detail = await detailOf("Other");
  await waitFor(() => assert.equal(within(detail).getAllByText(p.instances.empty).length, 1));
  assert.equal(within(detail).queryAllByRole("rowgroup", { name: "Standalone instances" }).length, 0, "its instances alone, ungrouped");
  assert.deepEqual(listings, [3], "and its clusters are not asked for");
});

test("a viewer sees where an account's entries stand but gets no actions on them, and no enrollment token", async () => {
  servePlatforms(api, { providers: [ali], clusters: [joined], admin: false });
  Object.assign(api, { providerClusters: async () => list([connectedCluster("c-gpu", "ack-gpu", "e-1"), cloudCluster("c-new", "ack-new")]) });
  renderPage("/providers?open=p3");
  const group = await groupOf("ack-gpu");
  await waitFor(async () => assert.deepEqual([(await clusterHeader(group)).standing, (await facts(group))["Nodes joined"]], [k.status.active, groupCopy.en.nodes]));
  assert.deepEqual(await actionsIn(group), NONE);
  // Connecting a cluster makes an enrollment token: a viewer is not offered it.
  assert.equal(within(await groupOf("ack-new")).queryByRole("button", { name: p.clusters.connect }), null);
  assert.equal(document.body.textContent?.includes("token="), false);
});

test("revoking an entry from its cluster's header reloads the page's two lists and the cloud's, so the account and its entries follow", async () => {
  const server = servePlatforms(api, { providers: [ali], clusters: [joined] });
  const revoked: string[] = [];
  let listings = 0;
  Object.assign(api, {
    providerClusters: async () => { listings += 1; return list([connectedCluster("c-gpu", "ack-gpu", "e-1")]); },
    revokeKubeCluster: async (uuid: string) => { revoked.push(uuid); server.clusters = [{ ...joined, revoked: true, can_apply: false, operation_blocked: "revoked" }]; return server.clusters[0]; },
  });
  const u = user();
  renderPage("/providers?open=p3");
  const group = await groupOf("ack-gpu");
  const before = { ...server.reads };
  await waitFor(async () => assert.ok(await findRowAction(group, k.actions.revoke), "the administrator's actions have arrived"));
  await u.click(await findRowAction(group, k.actions.revoke) as HTMLElement);
  await confirmDialog(u, k.actions.revoke);
  await waitFor(() => assert.deepEqual(revoked, ["e-1"]));
  await waitFor(() => assert.deepEqual(server.reads, { providers: before.providers + 1, clusters: before.clusters + 1 }));
  await waitFor(() => assert.equal(listings, 2));
  await waitFor(async () => assert.ok((await clusterHeader(await groupOf("ack-gpu"))).standing.startsWith(k.status.revoked)));
});

test("an entry the server says shares its namespace with another entry carries a warning beside its cluster's name, whose hint names the others and the namespace", async () => {
  api.providerClusters = (async () => list([connectedCluster("c-gpu", "ack-gpu", "e-1"), connectedCluster("c-lab", "ack-lab", "e-2")])) as never;
  const u = user();
  renderDetail({ entries: [{ ...joined, namespace_shared_with: ["by-hand", "ack-lab"] }, waiting] });
  const group = await groupOf("ack-gpu");
  assert.deepEqual((await clusterHeader(group)).title, ["ack-gpu", "ACK managed", k.namespaceShared.badge]);
  assert.deepEqual((await clusterHeader(await groupOf("ack-lab"))).title, ["ack-lab", "ACK managed"], "an entry the server names no other for has no warning");
  await u.hover(within(await groupHeader(group)).getByText(k.namespaceShared.badge));
  assert.equal((await screen.findByRole("tooltip")).textContent, k.namespaceShared.hint.replace("{{names}}", "by-hand, ack-lab").replace("{{namespace}}", "gpuworks-system"));
});
