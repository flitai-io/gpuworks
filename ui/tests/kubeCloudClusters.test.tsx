import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";
import { account, aliyunKind, cloudCluster, cloudEntry, connectedCluster, instance, KUBE_DEFAULTS, l20Pool } from "./helpers/platformFixtures.ts";
import { clusterHeader, factValue, facts, groupNames, groupOf } from "./helpers/platformDetail.ts";
import { freshPopupFramesPerTest } from "./helpers/platformDialog.ts";
import { poolPicker } from "./helpers/poolPicker.ts";

// A cloud account's managed Kubernetes clusters, read live from the cloud,
// each a group of the account's table (ui/src/pages/platforms/ClusterGroup.tsx
// in ui/src/pages/platforms/ProviderResources.tsx, data from
// ui/src/pages/platforms/useAccountClusters.ts): what the group says of the
// cluster as "label value" pairs (ui/src/pages/platforms/FactList.tsx), with
// nothing to tick in it; connecting it (the drawer of
// ui/src/pages/kubernetes/KubeClusterDrawer.tsx, prefilled, where its pools are
// chosen with ui/src/pages/kubernetes/KubePoolPicker.tsx); changing the pools an entry holds (KubePoolsDrawer, from the pools'
// Change link); the bar above the table (Refresh, last sync); and the listing
// while it loads, fails or is empty. The entries made from the clusters and
// what is done to them are in tests/providerClusterEntries.test.tsx.

const { i18n, locale } = await createI18n(["kubernetes", "providers"]);
const { MemoryRouter } = await import("react-router-dom");
const { act, cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { formatTime } = await import("../src/i18n/datetime.ts");
const { default: ProviderResources } = await import("../src/pages/platforms/ProviderResources.tsx");
import type { CloudProvider, KubeCluster, KubeClusterOperation, ProviderCluster } from "../src/api";

const k = locale.kubernetes;
const p = locale.providers;
const originals = { ...api };
await freshPopupFramesPerTest();
afterEach(() => { cleanup(); Object.assign(api, originals); });
// The drawer asks the server for its defaults when it opens; the account has no instance.
beforeEach(() => { Object.assign(api, { kubeDefaults: async () => KUBE_DEFAULTS, providerInstances: async () => list([]) }); });

const gpuCluster = cloudCluster("c-gpu", "ack-gpu");
const privateCluster = cloudCluster("c-private", "ack-private", { public_api: false, gpu_pools: [] });
const doneCluster = connectedCluster("c-done", "ack-done", "e-1");
const newCluster = cloudCluster("c-new", "ack-new", { running: false, state: "initial", gpu_pools: [] });

/** The entry the server makes from a cluster of account 3. */
function entry(overrides: Partial<KubeCluster> = {}): KubeCluster {
  return cloudEntry({ server_url: "http://localhost", apply_command: "kubectl apply -f 'http://localhost/kube/manifest.yaml?token=t'", source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-gpu" }, ...overrides });
}

const plan: KubeClusterOperation = {
  dry_run: true,
  results: [
    { kind: "Namespace", name: "gpuworks-system", action: "create" },
    { kind: "DaemonSet", name: "gpuworks-agent", namespace: "gpuworks-system", action: "dependent" },
    { kind: "PriorityClass", name: "gpuworks-inference", action: "update" },
  ],
};

/** Serves the account's clusters; replace `state.clusters` (or make it an Error) to change what the next read answers. */
function serveClusters(clusters: ProviderCluster[] | Error) {
  const state = { clusters, reads: 0 };
  api.providerClusters = (async () => { state.reads += 1; if (state.clusters instanceof Error) throw state.clusters; return list(state.clusters); }) as never;
  return state;
}
interface Mounted { entries?: KubeCluster[]; onChanged?: () => void; admin?: boolean; provider?: CloudProvider; refreshKey?: string; onSync?: () => void; syncing?: boolean }
function view({ entries = [], onChanged = () => {}, admin = true, provider = account(3, "ali-prod", "aliyun"), refreshKey = "1", onSync, syncing = false }: Mounted) {
  return (
    <I18nextProvider i18n={i18n}>
      <MemoryRouter>
        <ProviderResources provider={provider} kindInfo={aliyunKind} refreshKey={refreshKey}
          entries={Object.fromEntries(entries.map((each) => [each.uuid, each]))} onEntriesChanged={onChanged} admin={admin} onSync={onSync} syncing={syncing} />
      </MemoryRouter>
    </I18nextProvider>
  );
}
let rerenderView: (options: Mounted) => void = () => {};
function mount(options: Mounted = {}) {
  const { rerender } = render(view(options));
  rerenderView = (next) => rerender(view(next));
  return userEvent.setup({ document: dom.window.document });
}
// The operation dialog opens over the drawer: the last dialog is the one on top.
const modal = () => within(screen.getAllByRole("dialog").at(-1) as HTMLElement);
// A label's required mark ("Name *") is part of its text.
const field = (text: string) => new RegExp(`^${text}( \\*)?$`);
/** The pool choice of the drawer on top (the connect drawer or the pools drawer), whose list is named by `label`. */
const pickerIn = (label: string) => poolPicker(() => screen.getAllByRole("dialog").at(-1) as HTMLElement, {
  label, addLabel: k.pools.add, removeName: (name) => k.pools.removeOne.replace("{{name}}", name), inEntry: p.clusters.inEntry,
});
const connectPools = pickerIn(k.drawer.pools);
const editPools = pickerIn(k.pools.group);
const L20_2 = "l20 · NVIDIA L20 ×1 · 2 nodes";
const POOLS = p.clusters.column.gpuPools;

test("each cluster of the cloud is a group of its own, read live and in the cloud's order: its header names it and where it stands, under it what it is as label and value, with nothing to tick", async () => {
  const state = serveClusters([gpuCluster, privateCluster, doneCluster, newCluster]);
  mount();
  const group = await groupOf("ack-gpu");
  assert.equal(state.reads, 1);
  assert.deepEqual(await groupNames(), ["Standalone instances", "ack-gpu", "ack-private", "ack-done", "ack-new"]);

  // The header: the name and the vendor's product it is; no entry yet, so it can be connected.
  assert.deepEqual(await clusterHeader(group), { title: ["ack-gpu", "ACK managed"], standing: p.table.state.pending });
  assert.deepEqual(await facts(group), {
    [p.clusters.field.id]: "c-gpu", [p.clusters.field.region]: "cn-shanghai", [p.clusters.field.version]: "1.35.7-aliyun.1",
    [p.clusters.field.api]: p.clusters.publicAPI, [POOLS]: L20_2,
  }, "no node count, namespace or creation time before it is connected");
  assert.ok(within(group).getByRole("button", { name: p.clusters.connect }));
  assert.equal(within(group).queryAllByRole("link").length, 0, "no node is listed or linked: the cluster's nodes are compute nodes once joined");

  const priv = await groupOf("ack-private");
  assert.equal((await facts(priv))[p.clusters.field.api], p.clusters.privateAPI);
  assert.equal((await facts(priv))[POOLS], p.clusters.noGPUPools);
  assert.equal(within(priv).queryAllByRole("button", { name: p.clusters.connect }).length, 0, "no pool, nothing to connect");

  // Without the page's entry (none was passed) a connected cluster says only that it is.
  const done = await groupOf("ack-done");
  assert.equal((await clusterHeader(done)).standing, p.clusters.connected);
  assert.equal(within(done).queryAllByRole("button", { name: p.clusters.connect }).length, 0, "a cluster has one entry: no second connect");

  // Not ready: the cloud's word for its state, and nothing to connect.
  const fresh = await groupOf("ack-new");
  assert.equal((await clusterHeader(fresh)).standing, "initial");
  assert.equal((await facts(fresh))[POOLS], p.clusters.notRunning);
  assert.equal(within(fresh).queryAllByRole("button", { name: p.clusters.connect }).length, 0);

  assert.equal(screen.queryAllByRole("checkbox").length, 0, "the groups only show: pools are ticked in the connect form");
});

test("a type the server has no name for shows as sent, a cluster the driver cannot type shows none, and a cluster without a region or version leaves those out; the whole cloud id is shown", async () => {
  const longID = "c8a1f0e2b7d94c06a3f5e9d1b2c4a6f80";
  serveClusters([
    cloudCluster("c-new", "ack-future", { type: "ack_future", type_label: undefined }),
    cloudCluster(longID, "ack-untyped", { type: undefined, type_label: undefined, region: "", version: "" }),
  ]);
  mount();
  assert.deepEqual((await clusterHeader(await groupOf("ack-future"))).title, ["ack-future", "ack_future"]);
  const untyped = await groupOf("ack-untyped");
  assert.deepEqual((await clusterHeader(untyped)).title, ["ack-untyped"]);
  assert.deepEqual(Object.keys(await facts(untyped)), [p.clusters.field.id, p.clusters.field.api, POOLS]);
  assert.equal((await facts(untyped))[p.clusters.field.id], longID);
});

test("a cluster whose API server is private says why that matters when its badge is pointed at or reached with Tab", async () => {
  serveClusters([privateCluster]);
  const user = mount();
  // The badge's label sits inside it; the badge itself is what takes focus.
  const badge = within(await groupOf("ack-private")).getByText(p.clusters.privateAPI).closest("[tabindex]") as HTMLElement;
  await user.hover(badge);
  assert.equal((await screen.findByRole("tooltip")).textContent, p.clusters.privateAPIHint);
  await user.unhover(badge);
  await waitFor(() => assert.equal(screen.queryAllByRole("tooltip").length, 0));
  // Tab reaches the badge (after Refresh and the groups' buttons), and focus opens the same hint.
  for (let i = 0; i < 20 && document.activeElement !== badge; i += 1) await user.tab();
  assert.ok(document.activeElement === badge, "the badge is a tab stop");
  assert.equal((await screen.findByRole("tooltip")).textContent, p.clusters.privateAPIHint);
});

test("while the cloud is being read the clusters say so in place of their groups and the table is busy; then the groups come", async () => {
  let answer!: (clusters: unknown) => void;
  api.providerClusters = (() => new Promise((resolve) => { answer = resolve; })) as never;
  mount();
  const reading = await screen.findByText(p.detail.loadingClusters);
  assert.equal(reading.closest('[role="status"]')?.textContent, p.detail.loadingClusters);
  await waitFor(() => assert.equal(screen.getByRole("table").getAttribute("aria-busy"), "true"));
  assert.equal(screen.queryAllByText(p.clusters.empty).length, 0, "still reading is not empty");
  assert.deepEqual(await groupNames(), ["Standalone instances"]);

  await act(async () => answer(list([gpuCluster])));
  await groupOf("ack-gpu");
  assert.equal(screen.queryAllByText(p.detail.loadingClusters).length, 0);
  await waitFor(() => assert.equal(screen.getByRole("table").getAttribute("aria-busy"), "false"));
});

test("a failed listing is an error with the cloud's reason and a retry; it is not called empty, and retrying reads the cloud again", async () => {
  const state = serveClusters(new Error("aliyun: Forbidden.RAM cs:DescribeClustersV1"));
  const user = mount();
  const failed = (await screen.findByText(p.clusters.loadFailed)).closest('[role="alert"]') as HTMLElement;
  assert.ok(failed.textContent?.includes("aliyun: Forbidden.RAM cs:DescribeClustersV1"));
  assert.equal(screen.queryAllByText(p.clusters.empty).length, 0, "the cloud was not heard: nothing says the account has no cluster");
  assert.deepEqual(await groupNames(), ["Standalone instances"]);

  state.clusters = [gpuCluster];
  await user.click(within(failed).getByRole("button", { name: p.instances.retry }));
  await groupOf("ack-gpu");
  assert.equal(state.reads, 2);
  assert.equal(screen.queryAllByText(p.clusters.loadFailed).length, 0);
});

/** The bar above the account's table: Sync (when the kind can list instances), then when the account last synced. */
const topBar = () => {
  const sync = screen.getByRole("button", { name: p.table.sync });
  return { sync, bar: sync.parentElement as HTMLElement };
};

test("the bar above the table leads with Sync, then when the account was last synced; Sync asks the page to sync, from the pointer or the keyboard, and once it has the instances and the clusters are both read again", async () => {
  const state = serveClusters([]);
  let instanceReads = 0;
  let instances = [] as ReturnType<typeof instance>[];
  api.providerInstances = (async () => { instanceReads += 1; return list(instances); }) as never;
  let synced = 0;
  const onSync = () => { synced += 1; };
  const provider = account(3, "ali-prod", "aliyun", { synced_at: "2026-11-10T07:55:00Z" });
  const user = mount({ provider, onSync });
  assert.ok(await screen.findByText(p.clusters.empty));
  const { sync, bar } = topBar();
  // Sync is the first control of the account's detail: nothing stands before it.
  assert.ok(screen.getAllByRole("button")[0] === sync, "Sync is the first button");
  assert.ok(bar.firstElementChild === sync, "and leads its bar");
  assert.equal(bar.textContent, `${p.table.sync}${p.detail.syncedAt.replace("{{time}}", formatTime("2026-11-10T07:55:00Z"))}`);
  assert.equal(screen.queryAllByRole("button", { name: "Refresh" }).length, 0, "no separate refresh");
  assert.deepEqual([instanceReads, state.reads], [1, 1]);

  await user.click(sync);
  assert.equal(synced, 1);
  assert.deepEqual([instanceReads, state.reads], [1, 1], "pressing Sync reads nothing by itself");
  // The page's sync done, it hands a new refresh key: both halves are read again, once each.
  state.clusters = [gpuCluster];
  instances = [instance()];
  rerenderView({ provider, onSync, refreshKey: "2" });
  await groupOf("ack-gpu");
  assert.ok(await screen.findByText("train-01"));
  assert.deepEqual([instanceReads, state.reads], [2, 2]);
  assert.equal(screen.queryAllByText(p.clusters.empty).length, 0);

  // From the keyboard: the button takes focus and Enter presses it.
  topBar().sync.focus();
  await user.keyboard("{Enter}");
  assert.equal(synced, 2);
});

test("an account never synced says so where the sync time would be; Sync is busy while the account syncs; a kind that cannot list instances has no Sync", async () => {
  serveClusters([]);
  const provider = account(3, "ali-prod", "aliyun", { synced_at: undefined });
  const onSync = () => {};
  mount({ provider, onSync, syncing: true });
  await screen.findByText(p.clusters.empty);
  const { sync, bar } = topBar();
  assert.equal(bar.textContent?.endsWith(p.detail.neverSynced), true, bar.textContent ?? "");
  assert.equal(screen.queryAllByText(/Last synced/).length, 0);
  const busy = (button: HTMLElement) => button.getAttribute("aria-busy") === "true" || button.hasAttribute("disabled") || button.getAttribute("data-loading") !== null;
  assert.equal(busy(sync), true, "busy while the account syncs");
  rerenderView({ provider, onSync, syncing: false });
  await waitFor(() => assert.equal(busy(topBar().sync), false));
  rerenderView({ provider });
  await waitFor(() => assert.equal(screen.queryAllByRole("button", { name: p.table.sync }).length, 0));
  assert.equal(screen.queryAllByText(p.detail.neverSynced).length, 0, "a kind that is never synced says nothing about syncing");
});

test("connecting opens the form filled from the cluster, with its pools chosen there, records the source, then applies only after a checked plan is confirmed", async () => {
  const state = serveClusters([gpuCluster]);
  const created: unknown[] = [];
  api.createKubeCluster = (async (body: unknown) => { created.push(body); return entry(); }) as never;
  const ops: [string, string, boolean][] = [];
  api.kubeClusterOperation = (async (uuid: string, op: string, dryRun: boolean) => {
    ops.push([uuid, op, dryRun]);
    return dryRun ? plan : { dry_run: false, results: plan.results.map((r) => ({ ...r, action: "create" })) };
  }) as never;
  let changed = 0;
  const user = mount({ onChanged: () => { changed += 1; } });
  await user.click(within(await groupOf("ack-gpu")).getByRole("button", { name: p.clusters.connect }));
  const drawer = within(await screen.findByRole("dialog", { name: k.drawer.title }));
  assert.ok(await drawer.findByText(k.drawer.fromSource.replace("{{source}}", "ali-prod · ack-gpu")), "the source is the account and the cluster, without its cloud id");
  assert.equal((drawer.getByLabelText(field(k.drawer.name)) as HTMLInputElement).value, "ack-gpu");
  assert.equal(drawer.queryAllByPlaceholderText(k.drawer.nodeSelectorPlaceholder).length, 0, "an entry from a cloud cluster selects by node pool, not by a selector");
  assert.deepEqual(await connectPools.chosen(), [L20_2], "its one pool is chosen");
  assert.deepEqual(await connectPools.removeButtons(), [], "and cannot be taken out: an entry holds at least one");
  assert.equal(await connectPools.adder(), null, "no other pool to add");
  await user.click(drawer.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(created.length, 1));
  assert.deepEqual(created[0], { name: "ack-gpu", server_url: "http://localhost", provider_id: 3, provider_cluster_id: "c-gpu", provider_pool_ids: ["np-l20"] });
  assert.ok(await drawer.findByText(k.created.byGpuworks));
  assert.ok(drawer.getByText(k.created.byKubectl), "kubectl stays an option");
  assert.equal(ops.length, 0, "nothing touches the cluster before the operator asks");
  assert.equal(changed, 1, "the page is told an entry was made, so its entry list follows");
  await waitFor(() => assert.equal(state.reads, 2, "and the cloud is read again, so the cluster shows its entry"));

  await user.click(drawer.getByRole("button", { name: k.created.applyNow }));
  assert.ok(await modal().findByText(k.operation.checkedApply));
  assert.deepEqual(ops, [["e-5", "apply", true]], "a dry run first");
  assert.ok(modal().getByText("gpuworks-system/gpuworks-agent"));
  assert.ok(modal().getByText(k.operation.plan.create));
  assert.ok(modal().getByText(k.operation.plan.dependent));
  assert.ok(modal().getByText(k.operation.applyIntro.replace("{{account}}", "ali-prod").replace("{{cluster}}", "c-gpu")));
  await user.click(modal().getByRole("button", { name: k.operation.confirmApply }));
  assert.ok(await modal().findByText(k.operation.applied));
  assert.deepEqual(ops, [["e-5", "apply", true], ["e-5", "apply", false]]);
  assert.equal(modal().getAllByText(k.operation.done.create).length, 3);
  assert.equal(changed, 2, "and again once the manifest is applied");
});

test("Escape closes the connect drawer without making anything, and focus goes back to Connect", async () => {
  serveClusters([gpuCluster]);
  let created = 0;
  api.createKubeCluster = (async () => { created += 1; return entry(); }) as never;
  const user = mount();
  const connect = within(await groupOf("ack-gpu")).getByRole("button", { name: p.clusters.connect });
  await user.click(connect);
  await screen.findByRole("dialog", { name: k.drawer.title });
  await user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0));
  assert.equal(created, 0);
  await waitFor(() => assert.ok(document.activeElement === connect, "focus is back on Connect"));
});

const multiCluster = cloudCluster("c-multi", "ack-multi", {
  gpu_pools: [
    { ...l20Pool, id: "np-a", name: "a10", gpu_model: "NVIDIA A10", nodes: 2 },
    { ...l20Pool, id: "np-l", name: "l20", nodes: 1 },
    { ...l20Pool, id: "np-p", name: "ppu", gpu_model: "PPU-ZW810E", nodes: 1, vendor: "ppu" },
  ],
});
const A10 = "a10 · NVIDIA A10 ×1 · 2 nodes";
const L20 = "l20 · NVIDIA L20 ×1 · 1 nodes";
const PPU = "ppu · PPU-ZW810E ×1 · 1 nodes";

test("a cluster not yet connected lists all its GPU pools; the connect form opens with all of them chosen, of any vendors, under a field label and what joining means; pools taken out are offered to add back, and Connect sends the pools left", async () => {
  serveClusters([multiCluster]);
  const created: unknown[] = [];
  api.createKubeCluster = (async (body: unknown) => { created.push(body); return entry({ name: "ack-multi", pool_ids: (body as { provider_pool_ids: string[] }).provider_pool_ids }); }) as never;
  const user = mount();
  const group = await groupOf("ack-multi");
  const pools = await factValue(group, POOLS);
  assert.deepEqual([...pools.firstElementChild!.children].map((line) => line.textContent), [A10, L20, PPU], "every pool, one per line");
  assert.equal(within(group).queryAllByRole("button", { name: p.clusters.editPools }).length, 0, "nothing to change before an entry exists");

  await user.click(within(group).getByRole("button", { name: p.clusters.connect }));
  const drawer = within(await screen.findByRole("dialog", { name: k.drawer.title }));
  assert.deepEqual(drawer.getAllByText(k.drawer.pools).map((label) => label.textContent), [`${k.drawer.pools} *`], "labelled like the other fields, as required");
  assert.ok(drawer.getByText(k.pools.autoJoin), "and says once what choosing a pool does");
  assert.deepEqual(await connectPools.chosen(), [A10, L20, PPU], "all chosen by default, the PPU pool beside the NVIDIA ones");
  assert.deepEqual(await connectPools.tagged(), [], "none is in an entry yet");
  assert.equal(await connectPools.adder(), null, "nothing left to add");
  assert.equal(drawer.queryAllByText(/^Pools taken out:/).length, 0, "a cluster not yet connected has no pool to lose");
  const submit = drawer.getByRole("button", { name: k.drawer.submit }) as HTMLButtonElement;
  await connectPools.remove(user, "a10");
  await connectPools.remove(user, "l20");
  assert.deepEqual(await connectPools.chosen(), [PPU]);
  assert.deepEqual(await connectPools.removeButtons(), [], "the last pool stays");
  assert.equal(submit.disabled, false);
  assert.deepEqual(await connectPools.offered(user), [A10, L20], "the pools taken out are offered again");
  await connectPools.add(user, A10);
  assert.deepEqual(await connectPools.chosen(), [A10, PPU]);
  await user.click(submit);
  await waitFor(() => assert.equal(created.length, 1));
  assert.deepEqual([...(created[0] as { provider_pool_ids: string[] }).provider_pool_ids].sort(), ["np-a", "np-p"], "only the pools left go, of different vendors together");
  assert.ok(await drawer.findByText(k.created.byGpuworks));
  assert.equal(drawer.queryAllByRole("alert").length, 0, "no vendor refusal");
});

const heldMulti: ProviderCluster = { ...multiCluster, entry: { uuid: "e-5", name: "ack-multi" }, gpu_pools: multiCluster.gpu_pools.map((q) => ({ ...q, in_entry: q.id === "np-a" })) };
const heldEntry = entry({ uuid: "e-5", name: "ack-multi", pool_ids: ["np-a"], source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-multi" } });

test("a connected cluster lists only the pools its entry holds; an administrator's Change opens the pools drawer, where saving sends the set and offers to apply again", async () => {
  const state = serveClusters([heldMulti]);
  const saves: [string, string[]][] = [];
  api.setKubeClusterPools = (async (uuid: string, ids: string[]) => { saves.push([uuid, ids]); return { ...heldEntry, pool_ids: ids }; }) as never;
  const ops: [string, string, boolean][] = [];
  api.kubeClusterOperation = (async (uuid: string, op: string, dryRun: boolean) => { ops.push([uuid, op, dryRun]); return dryRun ? plan : { dry_run: false, results: plan.results }; }) as never;
  let changed = 0;
  const user = mount({ entries: [heldEntry], onChanged: () => { changed += 1; } });
  const group = await groupOf("ack-multi");
  const pools = await factValue(group, POOLS);
  assert.equal(within(pools).getByText(A10).textContent, A10);
  assert.equal(within(pools).queryAllByText(L20).length + within(pools).queryAllByText(PPU).length, 0, "the pools the entry does not hold are not its");
  assert.equal(within(group).queryAllByRole("button", { name: p.clusters.connect }).length, 0);

  await user.click(within(pools).getByRole("button", { name: p.clusters.editPools }));
  const drawer = within(await screen.findByRole("dialog", { name: k.pools.title.replace("{{name}}", "ack-multi") }));
  await waitFor(async () => assert.deepEqual(await editPools.chosen(), [A10], "the held pool is chosen"));
  assert.deepEqual(await editPools.tagged(), [A10], "and tagged as in the entry");
  assert.ok(drawer.getByText(k.pools.group), "the field is labelled");
  assert.ok(drawer.getByText(k.pools.autoJoin));
  const save = drawer.getByRole("button", { name: k.pools.save }) as HTMLButtonElement;
  assert.equal(save.disabled, true, "nothing changed yet");
  assert.deepEqual(await editPools.offered(user), [L20, PPU]);
  await editPools.add(user, L20);
  assert.equal(save.disabled, false);
  await user.click(save);
  await waitFor(() => assert.equal(saves.length, 1));
  assert.deepEqual(saves[0][0], "e-5");
  assert.deepEqual([...saves[0][1]].sort(), ["np-a", "np-l"]);
  // Read once for the table, once by the drawer for the pools to choose from, and again once saved.
  await waitFor(() => assert.deepEqual([state.reads, changed], [3, 1], "the cloud is read again and the page told"));
  assert.equal(ops.length, 0, "saving applies nothing");
  const saved = within(await screen.findByRole("dialog", { name: k.pools.title.replace("{{name}}", "ack-multi") }));
  assert.ok(await saved.findByText(k.pools.saved.replace("{{name}}", "ack-multi")), "the drawer stays on what was saved");
  await user.click(saved.getByRole("button", { name: k.pools.applyNow }));
  assert.ok(await modal().findByText(k.operation.checkedApply));
  assert.deepEqual(ops, [["e-5", "apply", true]], "a checked plan comes first");
});

test("a refused pool edit shows the server's reason in the drawer and nothing is offered to apply", async () => {
  serveClusters([heldMulti]);
  api.setKubeClusterPools = (async () => { throw new APIError("KUBE_CLUSTER_SOURCE_TAKEN", { cluster: "c-multi" }, "taken", 409); }) as never;
  const user = mount({ entries: [heldEntry] });
  await user.click(within(await groupOf("ack-multi")).getByRole("button", { name: p.clusters.editPools }));
  const drawer = within(await screen.findByRole("dialog", { name: k.pools.title.replace("{{name}}", "ack-multi") }));
  await waitFor(async () => assert.deepEqual(await editPools.chosen(), [A10]));
  await editPools.add(user, PPU);
  await user.click(drawer.getByRole("button", { name: k.pools.save }));
  assert.ok(await drawer.findByText(locale.common.apiError.KUBE_CLUSTER_SOURCE_TAKEN.replace("{{cluster}}", "c-multi")));
  assert.equal(screen.queryAllByRole("button", { name: k.pools.applyNow }).length, 0);
});

test("a viewer sees the pools a connected cluster's entry holds, but no Change", async () => {
  serveClusters([heldMulti]);
  mount({ entries: [heldEntry], admin: false });
  const group = await groupOf("ack-multi");
  assert.equal((await facts(group))[POOLS], A10);
  assert.equal(within(group).queryAllByRole("button", { name: p.clusters.editPools }).length, 0);
});

test("an entry from before node pools says under its pools that it selects by a node selector; its pools drawer says so too and starts with none chosen, every pool to add", async () => {
  const selector = "alibabacloud.com/nodepool-id=np-a,gpu=a10";
  // The cloud lists the cluster with its entry, which holds none of the pools.
  const legacy: ProviderCluster = { ...multiCluster, entry: { uuid: "e-5", name: "ack-multi" } };
  serveClusters([legacy, connectedCluster("c-done", "ack-done", "e-6")]);
  const user = mount({ entries: [
    { ...heldEntry, pool_ids: [], node_selector: selector },
    entry({ id: 6, uuid: "e-6", name: "ack-done", source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-done" } }),
  ] });
  const group = await groupOf("ack-multi");
  assert.ok(within(await factValue(group, POOLS)).getByText(k.pools.legacyHelp.replace("{{selector}}", selector)));
  assert.equal(within(await groupOf("ack-done")).queryAllByText(/node selector|selects nodes with/).length, 0, "an entry that holds pools has no selector to speak of");
  await user.click(within(group).getByRole("button", { name: p.clusters.editPools }));
  const drawer = within(await screen.findByRole("dialog", { name: k.pools.title.replace("{{name}}", "ack-multi") }));
  assert.ok(await drawer.findByText(k.pools.legacy));
  await waitFor(async () => assert.deepEqual(await editPools.chosen(), []));
  const save = drawer.getByRole("button", { name: k.pools.save }) as HTMLButtonElement;
  assert.equal(save.disabled, true, "nothing chosen yet");
  assert.deepEqual(await editPools.offered(user), [A10, L20, PPU]);
  await editPools.add(user, L20);
  assert.equal(save.disabled, false, "one pool chosen replaces the selector");
});

const twoHeld: ProviderCluster = { ...multiCluster, entry: { uuid: "e-5", name: "ack-multi" }, gpu_pools: multiCluster.gpu_pools.map((q) => ({ ...q, in_entry: q.id !== "np-p" })) };
const twoHeldEntry = { ...heldEntry, pool_ids: ["np-a", "np-l"] };

test("in the pools drawer, taking out a pool the entry holds warns that its nodes leave; adding it back takes the warning away; Save sends the set chosen", async () => {
  serveClusters([twoHeld]);
  const saves: string[][] = [];
  api.setKubeClusterPools = (async (_uuid: string, ids: string[]) => { saves.push(ids); return { ...twoHeldEntry, pool_ids: ids }; }) as never;
  const user = mount({ entries: [twoHeldEntry] });
  await user.click(within(await groupOf("ack-multi")).getByRole("button", { name: p.clusters.editPools }));
  const drawer = within(await screen.findByRole("dialog", { name: k.pools.title.replace("{{name}}", "ack-multi") }));
  await waitFor(async () => assert.deepEqual(await editPools.chosen(), [A10, L20]));
  assert.deepEqual(await editPools.tagged(), [A10, L20]);
  const save = drawer.getByRole("button", { name: k.pools.save }) as HTMLButtonElement;
  const taken = (names: string) => k.pools.removing.replace("{{names}}", names);

  await editPools.remove(user, "a10");
  assert.ok(await drawer.findByText(taken("a10")), "the held pool taken out is named");
  assert.ok(drawer.getByText(k.pools.removingHelp), "with what follows once applied");
  assert.equal(save.disabled, false);

  await editPools.add(user, A10);
  await waitFor(() => assert.equal(drawer.queryAllByText(k.pools.removingHelp).length, 0, "back as it was: no warning"));
  assert.equal(save.disabled, true, "and nothing to save");

  // Adding a pool the entry does not hold warns of nothing; taking out a held one does.
  await editPools.add(user, PPU);
  assert.equal(drawer.queryAllByText(k.pools.removingHelp).length, 0);
  await editPools.remove(user, "l20");
  assert.ok(await drawer.findByText(taken("l20")));
  await user.click(save);
  await waitFor(() => assert.equal(saves.length, 1));
  assert.deepEqual([...saves[0]].sort(), ["np-a", "np-p"], "the pools left, the one added among them");
  assert.ok(await drawer.findByText(k.pools.saved.replace("{{name}}", "ack-multi")));
});

test("in the pools drawer, while the cloud's pools are read it says so and Save waits; a failed read says why and offers no choice", async () => {
  let answer!: (clusters: unknown) => void;
  let reads = 0;
  api.providerClusters = (() => { reads += 1; return reads === 1 ? Promise.resolve(list([twoHeld])) : new Promise((resolve) => { answer = resolve; }); }) as never;
  const user = mount({ entries: [twoHeldEntry] });
  await user.click(within(await groupOf("ack-multi")).getByRole("button", { name: p.clusters.editPools }));
  const drawer = within(await screen.findByRole("dialog", { name: k.pools.title.replace("{{name}}", "ack-multi") }));
  assert.ok(await drawer.findByText(k.pools.loading));
  assert.equal(drawer.queryAllByRole("list", { name: k.pools.group }).length, 0);
  assert.equal((drawer.getByRole("button", { name: k.pools.save }) as HTMLButtonElement).disabled, true);
  await act(async () => answer(list([])));
  assert.ok(await drawer.findByText(k.pools.clusterGone), "the cluster is gone from the account");
  assert.equal(drawer.queryAllByRole("list", { name: k.pools.group }).length, 0);
  assert.equal((drawer.getByRole("button", { name: k.pools.save }) as HTMLButtonElement).disabled, true);
});

// Taking pools away from nodes that still run instances is refused by the server until confirmed.
const POOL_DROPS = { nodes: [{ node_uuid: "n-1", node_name: "gpu-a1", kube_node: "gpu-a1", deployments: ["chat"] }], verified: true };
const refusedPools = (calls: [string, string[], boolean | undefined][], held: KubeCluster, drops: unknown = POOL_DROPS) =>
  (async (uuid: string, ids: string[], confirm?: boolean) => {
    calls.push([uuid, ids, confirm]);
    if (!confirm) throw new APIError("KUBE_SELECTION_DROPS_INSTANCES", drops as Record<string, unknown>, "drops instances", 409);
    return { ...held, pool_ids: ids, stopped_instances: [{ instance_uuid: "i-1", deployment: "chat", node_uuid: "n-1" }] };
  }) as never;
const askDialog = () => within(screen.getByText(k.selectionDrops.title).closest('[role="dialog"]') as HTMLElement);

// Taking pools away from nodes that still run instances (master's confirmation, KUBE_SELECTION_DROPS_INSTANCES)
// is asked about in the drawer of an account's cluster: the server names the nodes and their deployments.
for (const verified of [true, false]) {
  test(`the pools drawer asks before taking pools away from nodes that still run instances${verified ? "" : ", with the unverified caveat when no agent answered"}: it lists them, cancel sends nothing, confirm resends with the confirmation`, async () => {
    const held = { ...heldMulti, gpu_pools: heldMulti.gpu_pools.map((q) => ({ ...q, in_entry: q.id !== "np-p" })) };
    const holding = { ...heldEntry, pool_ids: ["np-a", "np-l"] };
    serveClusters([held]);
    const calls: [string, string[], boolean | undefined][] = [];
    api.setKubeClusterPools = refusedPools(calls, holding, verified ? POOL_DROPS : { ...POOL_DROPS, verified: false });
    const user = mount({ entries: [holding] });
    await user.click(within(await factValue(await groupOf("ack-multi"), POOLS)).getByRole("button", { name: p.clusters.editPools }));
    const drawer = within(await screen.findByRole("dialog", { name: k.pools.title.replace("{{name}}", "ack-multi") }));
    await waitFor(async () => assert.deepEqual(await editPools.chosen(), [A10, L20]));
    await editPools.remove(user, "l20");
    await user.click(drawer.getByRole("button", { name: k.pools.save }));

    await screen.findByText(k.selectionDrops.title);
    assert.ok(askDialog().getByText("gpu-a1"));
    assert.ok(askDialog().getByText(k.selectionDrops.deployments.replace("{{names}}", "chat")));
    assert.ok(askDialog().getByText(k.selectionDrops.intro), "confirming stops those instances first, then saves");
    assert.equal(askDialog().queryAllByText(k.selectionDrops.unverified).length, verified ? 0 : 1);
    assert.deepEqual(calls, [["e-5", ["np-a"], undefined]], "the first request carries no confirmation");

    await user.click(askDialog().getByRole("button", { name: k.selectionDrops.cancel }));
    await waitFor(() => assert.equal(screen.queryByText(k.selectionDrops.title), null));
    assert.equal(calls.length, 1, "cancelling sends nothing");
    assert.equal(drawer.queryByText(k.pools.saved.replace("{{name}}", "ack-multi")), null, "and saves nothing");

    await user.click(drawer.getByRole("button", { name: k.pools.save }));
    await screen.findByText(k.selectionDrops.title);
    await user.click(askDialog().getByRole("button", { name: k.selectionDrops.confirm }));
    await waitFor(() => assert.equal(calls.length, 3));
    assert.deepEqual(calls[2], ["e-5", ["np-a"], true]);
    assert.ok(await screen.findByText(k.pools.saved.replace("{{name}}", "ack-multi")));
    // The instances the server asked to stop are listed beside the saved notice.
    assert.ok(screen.getByText(i18n.getFixedT("en", "kubernetes")("stopped.title", { count: 1 })));
    assert.ok(screen.getByText(k.stopped.item.replace("{{deployment}}", "chat").replace("{{instance}}", "i-1")));
  });
}
