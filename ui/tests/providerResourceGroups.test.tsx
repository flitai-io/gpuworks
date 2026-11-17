import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { account, aliyunKind, cloudCluster, cloudEntry, connectedCluster, instance, runpodKind } from "./helpers/platformFixtures.ts";
import { columnTitles, foldButton, groupHeader, groupNames, groupOf, groupRows, instanceNames } from "./helpers/platformDetail.ts";
import { list } from "./helpers/listPage.ts";

// What an account's row opens to (ui/src/pages/platforms/ProviderResources.tsx):
// one table of what the account holds. An account with managed Kubernetes
// has it grouped by what the machines belong to (ui/src/pages/platforms/GroupRows.tsx):
// its own instances, then each cluster; any other account has its instances
// alone, ungrouped. Here: the groups and their order, folding them, the
// instance group's counts, the bar above the table (Refresh, when the account
// was last synced), a long instance list shown ten at a time, and the
// instance list while it loads, when it is empty and when it fails. What a
// cluster's group holds is in tests/kubeCloudClusters.test.tsx and
// tests/providerClusterEntries.test.tsx; an instance's row in
// tests/providerInstanceRows.test.tsx.

const languages = { en: await createI18n(["providers", "kubernetes"], "en"), zh: await createI18n(["providers", "kubernetes"], "zh") };
const p = languages.en.locale.providers;
const { MemoryRouter } = await import("react-router-dom");
const { act, cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: ProviderResources } = await import("../src/pages/platforms/ProviderResources.tsx");
const { api } = await import("../src/api.ts");
const { formatTime } = await import("../src/i18n/datetime.ts");
type CloudProvider = import("../src/api.ts").CloudProvider;
type KubeCluster = import("../src/api.ts").KubeCluster;
type ProviderCluster = import("../src/api.ts").ProviderCluster;
type ProviderInstance = import("../src/api.ts").ProviderInstance;
type ProviderKindInfo = import("../src/api.ts").ProviderKindInfo;

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

// An Alibaba Cloud account: ECS instances of its own and ACK clusters. The server lists the clusters' nodes with
// neither: once joined they are compute nodes.
const ali = account(3, "ali-prod", "aliyun", { instance_count: 3, node_count: 5 });
// A RunPod account: its kind has no managed Kubernetes.
const pods = account(1, "Pods", "runpod", { instance_count: 2 });
const train1 = instance();
const train2 = instance({ external_id: "i-uf6train02", name: "train-02", node_id: 41, actions: ["power_off"] });
const train3 = instance({ external_id: "i-uf6train03", name: "train-03", power: "stopped", node_id: 42, actions: ["power_on"] });
// Two clusters in the cloud, one connected; and an entry whose cluster the cloud no longer lists.
const ackEntry = cloudEntry({ uuid: "e-1", name: "ack-gpu", node_count: 3, cluster_uid: "0b9d6c1e-0000-4000-8000-00000000000a", source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-gpu" } });
const oldEntry = cloudEntry({ id: 9, uuid: "e-9", name: "ack-old", source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-old" } });
const cloud = [connectedCluster("c-gpu", "ack-gpu", "e-1"), cloudCluster("c-lab", "ack-lab")];

interface Served { instances?: ProviderInstance[] | Error; clusters?: ProviderCluster[] | Error }
/** What the detail reads: the account's instances as of the last sync, and its clusters from the cloud. Counts each read. */
function serve(served: Served = {}) {
  const reads = { instances: [] as number[], clusters: [] as number[] };
  const state = { instances: served.instances ?? [], clusters: served.clusters ?? [] };
  Object.assign(api, {
    providerInstances: async (id: number) => { reads.instances.push(id); if (state.instances instanceof Error) throw state.instances; return list(state.instances); },
    providerClusters: async (id: number) => { reads.clusters.push(id); if (state.clusters instanceof Error) throw state.clusters; return list(state.clusters); },
  });
  return { reads, state };
}

// The page passes Sync for a kind that lists instances (both kinds here do), and says while the account syncs.
interface Mounted { provider?: CloudProvider; kindInfo?: ProviderKindInfo; entries?: KubeCluster[]; language?: keyof typeof languages; refreshKey?: string; onSync?: (() => void) | null; syncing?: boolean }
function view({ provider = ali, kindInfo = provider.kind === "aliyun" ? aliyunKind : runpodKind, entries = [], language = "en", refreshKey = "1", onSync = () => {}, syncing = false }: Mounted = {}) {
  return (
    <I18nextProvider i18n={languages[language].i18n}>
      <MemoryRouter>
        <ProviderResources provider={provider} kindInfo={kindInfo} refreshKey={refreshKey} entries={Object.fromEntries(entries.map((entry) => [entry.uuid, entry]))} onEntriesChanged={() => {}} admin
          onSync={onSync ?? undefined} syncing={syncing} />
      </MemoryRouter>
    </I18nextProvider>
  );
}
function mount(options: Mounted = {}) {
  return { ...render(view(options)), user: userEvent.setup({ document: dom.window.document }) };
}
const table = () => screen.getByRole("table");
/** The count beside the instance group's name: how many instances the account has. */
const countIn = (header: HTMLElement) => within(header).queryAllByText(/^\d+$/).map((count) => count.textContent);

const groupCopy = {
  en: { instances: "Standalone instances", titles: ["Instance", "GPU", "Status", "Region", "Connection", "Actions"], synced: `Last synced: ${formatTime("2026-11-10T07:55:00Z")}`, sync: "Sync", joined: "Nodes joined 2", collapse: "Collapse Standalone instances" },
  zh: { instances: "独立实例", titles: ["实例", "GPU", "状态", "区域", "连接方式", "操作"], synced: `上次同步时间：${formatTime("2026-11-10T07:55:00Z")}`, sync: "同步", joined: "已接入节点 2", collapse: "收起 独立实例" },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} an account with managed Kubernetes has one table grouped by what the machines belong to: its own instances first, then each cluster in the cloud's order, then the entries the cloud no longer lists; a cluster's nodes are not listed`, async () => {
    const copy = groupCopy[language];
    serve({ instances: [train1, train2, train3], clusters: cloud });
    mount({ entries: [ackEntry, oldEntry], language });
    await groupOf("ack-gpu");

    assert.equal(screen.getAllByRole("table").length, 1, "one table, no tabs and no second list");
    assert.equal(screen.queryAllByRole("tab").length, 0);
    assert.deepEqual(await columnTitles(), copy.titles, "one header row for every group");
    assert.deepEqual(await groupNames(), [copy.instances, "ack-gpu", "ack-lab", "ack-old"]);
    // Above the table: Sync, which lists the instances from the vendor again (and with them the clusters), then when that last ran.
    assert.ok(screen.getByRole("button", { name: copy.sync }));
    assert.equal(screen.queryAllByRole("button", { name: /^(Refresh|刷新)$/ }).length, 0, "no separate refresh: a sync brings both up to date");
    assert.ok(screen.getByText(copy.synced));

    // The instance group: what its instances are by the vendor's product (ECS, not ACK), how many there are, and how many are compute nodes.
    const instances = await groupOf(copy.instances);
    const header = await groupHeader(instances);
    assert.equal(within(header).getByRole("button", { name: copy.collapse }).getAttribute("aria-expanded"), "true");
    assert.ok(within(header).getByText("ECS"));
    assert.equal(within(header).queryAllByText("ACK").length, 0, "the clusters have groups of their own");
    assert.deepEqual(countIn(header), ["3"]);
    assert.ok(header.textContent?.includes(copy.joined), header.textContent ?? "");
    assert.deepEqual(await instanceNames(), ["train-01", "train-02", "train-03"]);
    assert.equal((await groupRows(instances)).length, 3, "the group's rows are its instances");

    // A cluster's group is its header and one row of what it is: the nodes themselves are compute nodes, listed there.
    for (const name of ["ack-gpu", "ack-lab", "ack-old"]) assert.equal((await groupRows(await groupOf(name))).length, 1, name);
    assert.equal(document.body.textContent?.includes("{{"), false, "no sentence is left with an unfilled placeholder");
  });
}

test("the instance group counts what the account has: no count while the instances are still being read, no node count while none of them joined", async () => {
  let answer!: (instances: unknown) => void;
  serve({ clusters: cloud });
  Object.assign(api, { providerInstances: () => new Promise((resolve) => { answer = resolve; }) });
  mount();
  const group = await groupOf("Standalone instances");
  const header = await groupHeader(group);
  assert.equal(within(group).getByRole("status").textContent, p.detail.loadingInstances);
  assert.deepEqual(countIn(header), [], "nothing is counted before the list has answered");
  assert.equal(header.textContent?.includes("Nodes joined"), false);

  await act(async () => answer(list([train1, instance({ external_id: "i-2", name: "train-04" })])));
  await waitFor(() => assert.deepEqual(countIn(header), ["2"]));
  assert.equal(header.textContent?.includes("Nodes joined"), false, "no instance has the agent yet: nothing to say");
  assert.equal(within(group).queryAllByRole("status").length, 0);
});

test("a group folds from its header button and unfolds again, on its own: the header and its counts stay, the rows go, the other groups are left as they were", async () => {
  serve({ instances: [train1, train2], clusters: cloud });
  const { user } = mount({ entries: [ackEntry] });
  const cluster = await groupOf("ack-gpu");
  const instances = await groupOf("Standalone instances");
  await screen.findByText("train-01");
  const fold = await foldButton(instances);
  assert.deepEqual([fold.getAttribute("aria-label"), fold.getAttribute("aria-expanded")], ["Collapse Standalone instances", "true"]);

  await user.click(fold);
  assert.deepEqual([fold.getAttribute("aria-label"), fold.getAttribute("aria-expanded")], ["Expand Standalone instances", "false"]);
  assert.equal(screen.queryAllByText("train-01").length, 0, "the instances are folded away");
  assert.equal((await groupRows(instances)).length, 0);
  assert.deepEqual(countIn(await groupHeader(instances)), ["2"], "the header still says how many there are");
  assert.deepEqual(await groupNames(), ["Standalone instances", "ack-gpu", "ack-lab"], "a folded group is still a group");
  assert.equal((await foldButton(cluster)).getAttribute("aria-expanded"), "true");
  assert.ok(within(cluster).getByText("l20 · NVIDIA L20 ×1 · 2 nodes"), "the cluster's pools are still shown");

  // A cluster's group folds the same way, by its own name, and leaves the instances folded.
  const foldCluster = await foldButton(cluster);
  assert.equal(foldCluster.getAttribute("aria-label"), "Collapse ack-gpu");
  await user.click(foldCluster);
  assert.deepEqual([foldCluster.getAttribute("aria-label"), foldCluster.getAttribute("aria-expanded")], ["Expand ack-gpu", "false"]);
  assert.equal(within(cluster).queryAllByText("l20 · NVIDIA L20 ×1 · 2 nodes").length, 0, "its facts are folded away");
  // Where the entry stands is in the header, so it is still told.
  assert.ok(within(cluster).getByText("Active"));
  assert.equal(fold.getAttribute("aria-expanded"), "false");

  await user.click(fold);
  assert.equal(fold.getAttribute("aria-expanded"), "true");
  assert.ok(screen.getByText("train-01"));
  assert.equal(foldCluster.getAttribute("aria-expanded"), "false", "unfolding one group does not unfold another");
});

test("folding works from the keyboard: the fold button is a tab stop, Enter and Space toggle it, and the focus stays on it", async () => {
  serve({ instances: [train1], clusters: cloud });
  const { user } = mount();
  await groupOf("ack-gpu");
  await screen.findByText("train-01");
  const fold = await foldButton(await groupOf("Standalone instances"));
  assert.equal(fold.tagName, "BUTTON");
  assert.equal(fold.tabIndex, 0);
  // Tab from the Sync button above the table lands on the first group's fold button.
  screen.getByRole("button", { name: "Sync" }).focus();
  await user.tab();
  assert.ok(document.activeElement === fold, "the first stop in the table");

  await user.keyboard("{Enter}");
  assert.equal(fold.getAttribute("aria-expanded"), "false");
  assert.equal(screen.queryAllByText("train-01").length, 0);
  assert.ok(document.activeElement === fold, "the focus is not lost with the rows");
  await user.keyboard(" ");
  assert.equal(fold.getAttribute("aria-expanded"), "true");
  assert.ok(screen.getByText("train-01"));
  assert.ok(document.activeElement === fold);
});

const plainCopy = {
  en: { synced: `Last synced: ${formatTime("2026-11-10T07:55:00Z")}`, sync: "Sync", titles: ["Instance", "GPU", "Status", "Region", "Connection", "Actions"] },
  zh: { synced: `上次同步时间：${formatTime("2026-11-10T07:55:00Z")}`, sync: "同步", titles: ["实例", "GPU", "状态", "区域", "连接方式", "操作"] },
};
for (const language of ["en", "zh"] as const) {
  test(`in ${language} an account whose kind has no managed Kubernetes has the same table without groups: its instances, when they were synced, nothing to fold and nothing asked of the cloud, even after a sync`, async () => {
    const copy = plainCopy[language];
    const { reads } = serve({ instances: [instance({ kind: "container" }), instance({ external_id: "pod-2", name: "pod-02", kind: "container" })] });
    let synced = 0;
    const { user, rerender } = mount({ provider: pods, language, onSync: () => { synced += 1; } });
    await screen.findByText("pod-02");

    assert.deepEqual(await columnTitles(), copy.titles);
    assert.deepEqual(await groupNames(), [], "no group is named");
    assert.deepEqual(await instanceNames(), ["train-01", "pod-02"]);
    assert.equal(within(table()).getAllByRole("row").length, 3, "the header row and one row per instance: no group header");
    assert.equal(within(table()).queryAllByRole("button").filter((button) => button.hasAttribute("aria-expanded") && !button.hasAttribute("aria-haspopup")).length, 0, "nothing to fold");
    assert.ok(screen.getByText(copy.synced));
    assert.deepEqual(reads, { instances: [1], clusters: [] });
    await user.click(screen.getByRole("button", { name: copy.sync }));
    assert.equal(synced, 1, "Sync is the page's: the account is synchronized");
    // The sync done, the page hands a new refresh key.
    rerender(view({ provider: pods, language, refreshKey: "2", onSync: () => { synced += 1; } }));
    await waitFor(() => assert.deepEqual(reads, { instances: [1, 1], clusters: [] }, "the instances are read again; there is no cluster to read"));
    assert.equal(screen.queryAllByText(/Kubernetes|Standalone instances|独立实例/).length, 0);
  });
}

// 25 instances, named so their order is plain.
const many = Array.from({ length: 25 }, (_, index) => instance({ external_id: `i-${index + 1}`, name: `gpu-${String(index + 1).padStart(2, "0")}`, node_id: index < 12 ? 100 + index : 0 }));
const shown = (count: number) => many.slice(0, count).map((each) => each.name);

for (const [what, provider] of [["an account with managed Kubernetes", ali], ["an account without", pods]] as const) {
  test(`more than ten instances of ${what} are shown ten at a time: the last row offers the next ten and says how many are left, until all are shown`, async () => {
    serve({ instances: many, clusters: cloud });
    const { user } = mount({ provider });
    await screen.findByText("gpu-01");
    if (provider === ali) await groupOf("ack-gpu");
    assert.deepEqual(await instanceNames(), shown(10));
    const more = screen.getByRole("button", { name: "Show 10 more (15 left)" });
    // The offer is the last row of the instances, before any cluster.
    const rows = provider === ali ? await groupRows(await groupOf("Standalone instances")) : within(table()).getAllByRole("row").slice(1);
    assert.equal(rows.length, 11);
    assert.ok(rows[10].contains(more));
    if (provider === ali) {
      // What is counted is what the account has, not what is on screen.
      const header = await groupHeader(await groupOf("Standalone instances"));
      assert.deepEqual(countIn(header), ["25"]);
      assert.ok(header.textContent?.includes("Nodes joined 12"), header.textContent ?? "");
    }

    await user.click(more);
    assert.deepEqual(await instanceNames(), shown(20));
    await user.click(screen.getByRole("button", { name: "Show 5 more (5 left)" }));
    assert.deepEqual(await instanceNames(), shown(25));
    assert.equal(screen.queryAllByRole("button", { name: /^Show \d+ more/ }).length, 0, "nothing left to show");
  });
}

test("exactly ten instances are all shown, with nothing more to offer", async () => {
  serve({ instances: many.slice(0, 10) });
  mount({ provider: pods });
  await screen.findByText("gpu-10");
  assert.deepEqual(await instanceNames(), shown(10));
  assert.equal(screen.queryAllByRole("button", { name: /^Show \d+ more/ }).length, 0);
});

test("in zh the offer of more instances reads 再显示 N 台（还有 M 台）", async () => {
  serve({ instances: many.slice(0, 13) });
  const { user } = mount({ provider: pods, language: "zh" });
  await screen.findByText("gpu-01");
  await user.click(screen.getByRole("button", { name: "再显示 3 台（还有 3 台）" }));
  assert.deepEqual(await instanceNames(), shown(13));
});

for (const [what, provider] of [["an account with managed Kubernetes", ali], ["an account without", pods]] as const) {
  test(`while the instances of ${what} are being read the table says so and is marked busy, not empty; an account without instances then says so and points to Sync`, async () => {
    let answer!: (instances: unknown) => void;
    serve({ clusters: [] });
    Object.assign(api, { providerInstances: () => new Promise((resolve) => { answer = resolve; }) });
    mount({ provider });
    const loading = await screen.findByText(p.detail.loadingInstances);
    assert.equal(loading.closest('[role="status"]')?.textContent, p.detail.loadingInstances);
    assert.equal(table().getAttribute("aria-busy"), "true");
    assert.equal(screen.queryAllByText(p.instances.empty).length, 0, "still reading is not empty");

    await act(async () => answer(list([])));
    assert.ok(await screen.findByText(p.instances.empty));
    assert.equal(screen.queryAllByText(p.detail.loadingInstances).length, 0);
    await waitFor(() => assert.equal(table().getAttribute("aria-busy"), "false"));
    assert.deepEqual(await instanceNames(), []);
  });

  test(`when the instances of ${what} cannot be read the detail says so with the server's reason, does not call the account empty, and Retry reads them again`, async () => {
    const { reads, state } = serve({ instances: new Error("provider instances: database is locked"), clusters: cloud });
    const { user } = mount({ provider });
    // An error, in the detail and beside what it is about: the table below has no instance to show.
    const failed = (await screen.findByText(p.instances.loadFailed)).closest('[role="alert"]') as HTMLElement;
    assert.ok(failed.textContent?.includes("provider instances: database is locked"));
    assert.equal(screen.queryAllByText(p.instances.empty).length, 0, "failing is not being empty");
    assert.deepEqual(await instanceNames(), []);
    if (provider === ali) assert.ok(await groupOf("ack-gpu"), "the clusters do not wait for the instances");

    // Still failing: the error stays.
    await user.click(within(failed).getByRole("button", { name: "Retry" }));
    await waitFor(() => assert.equal(reads.instances.length, 2));
    assert.ok(await screen.findByText(p.instances.loadFailed));

    state.instances = [train1];
    await user.click(within(screen.getByText(p.instances.loadFailed).closest('[role="alert"]') as HTMLElement).getByRole("button", { name: "Retry" }));
    assert.ok(await screen.findByText("train-01"));
    assert.equal(screen.queryAllByText(p.instances.loadFailed).length, 0, "the error goes with its cause");
    assert.deepEqual(reads.instances, [provider.id, provider.id, provider.id]);
  });
}

test("the instances and the clusters are read again when the account was synchronized, and only then", async () => {
  const { reads, state } = serve({ instances: [train1], clusters: cloud });
  const mounted = mount();
  await screen.findByText("train-01");
  await groupOf("ack-gpu");
  // The page renders again on every poll: the same sync, nothing to read.
  mounted.rerender(view());
  assert.deepEqual(reads, { instances: [3], clusters: [3] });

  state.instances = [train1, train2];
  state.clusters = [...cloud, cloudCluster("c-new", "ack-new")];
  mounted.rerender(view({ refreshKey: "2" }));
  assert.ok(await screen.findByText("train-02"));
  assert.ok(await groupOf("ack-new"), "the clusters are read from the cloud again with the sync");
  assert.deepEqual(reads, { instances: [3, 3], clusters: [3, 3] }, "once each");
  mounted.rerender(view({ refreshKey: "2" }));
  assert.deepEqual(reads, { instances: [3, 3], clusters: [3, 3] }, "the same sync again reads nothing");
});

test("Sync above the table asks the page to sync, from the pointer or the keyboard; it is busy while the account syncs; reading nothing by itself", async () => {
  const { reads } = serve({ instances: [train1], clusters: cloud });
  let synced = 0;
  const onSync = () => { synced += 1; };
  const { user, rerender } = mount({ onSync });
  await groupOf("ack-gpu");
  const sync = screen.getByRole("button", { name: "Sync" }) as HTMLButtonElement;
  const bar = sync.parentElement as HTMLElement;
  assert.ok(bar.firstElementChild === sync, "Sync leads the bar");
  assert.equal(bar.textContent, `Sync${`Last synced: ${formatTime("2026-11-10T07:55:00Z")}`}`);
  await user.click(sync);
  assert.equal(synced, 1);
  sync.focus();
  await user.keyboard("{Enter}");
  assert.equal(synced, 2);
  assert.deepEqual(reads, { instances: [3], clusters: [3] }, "pressing Sync reads nothing itself: the page's sync hands a new key");
  const busy = (button: HTMLButtonElement) => button.disabled || button.getAttribute("aria-busy") === "true" || button.hasAttribute("data-loading");
  assert.equal(busy(sync), false);
  rerender(view({ onSync, syncing: true }));
  await waitFor(() => assert.equal(busy(screen.getByRole("button", { name: "Sync" }) as HTMLButtonElement), true, "busy while the account syncs"));
  rerender(view({ onSync, syncing: false }));
  await waitFor(() => assert.equal(busy(screen.getByRole("button", { name: "Sync" }) as HTMLButtonElement), false));
});

test("an account of a kind that cannot list instances has neither Sync nor a sync time above its table", async () => {
  serve({ instances: [train1], clusters: cloud });
  mount({ onSync: null });
  await groupOf("ack-gpu");
  assert.equal(screen.queryAllByRole("button", { name: "Sync" }).length, 0);
  assert.equal(screen.queryAllByText(`Last synced: ${formatTime("2026-11-10T07:55:00Z")}`).length + screen.queryAllByText(/never synced|not synced yet/i).length, 0);
});
