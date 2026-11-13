import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { api, confirm, localStorage, locale, modelFile, nodeStatus, renderDetail, restoreApi, rtl, s, stubDetail, detailOf } from "./helpers/storageDetailHarness.tsx";
import { nfsStorage } from "./helpers/storageFixtures.ts";
import { list } from "./helpers/listPage.ts";

const { cleanup, fireEvent, screen, waitFor, within } = rtl;
afterEach(() => { cleanup(); restoreApi(); });
const GIB = 1024 ** 3;
const models = locale.models;

const cacheStorage = nfsStorage({
  id: 3, name: "team-cache", type: "nfs_cache", cache_path: "/data/cache", cache_max_bytes: 200 * GIB, deployment_ids: [], model_count: 3,
  all_nodes: false, targets: [{ node_uuid: "n1", node_name: "gpu-01", cache_max_bytes: 100 * GIB }],
  nodes: {
    total: 3, ready: 1,
    problems: [nodeStatus("n3", "gpu-03", "failed", { message: "mount failed" })],
    all: [
      nodeStatus("n1", "gpu-01", "ready", { path: "/mnt/gpuworks/team-cache", cache_path: "/data/cache", total_bytes: 117 * GIB, available_bytes: 17 * GIB, cache_total_bytes: 1000 * GIB, cache_available_bytes: 900 * GIB, cache: { used_bytes: 100 * GIB, reclaimable_bytes: 0, protected_bytes: 100 * GIB, limit_bytes: 100 * GIB, state: "satisfied" }, updated_at: new Date(Date.now() - 120_000).toISOString() }),
      nodeStatus("n2", "gpu-02", "offline", { path: "/mnt/gpuworks/team-cache", updated_at: new Date(Date.now() - 7200_000).toISOString() }),
      nodeStatus("n3", "gpu-03", "failed", { message: "mount failed", reason: "storage_unavailable" }),
    ],
  },
});
const cacheFiles = [
  modelFile({ id: 1, model_id: 7, model_name: "Qwen3-8B", deployment_ids: [4] }),
  modelFile({ id: 2, model_id: 7, model_name: "Qwen3-8B", node_uuid: "n1", node_name: "gpu-01", cache: true, size_bytes: 30 * GIB, downloaded: 30 * GIB, last_used_at: "2026-10-01T08:30:00Z", deployment_ids: [4] }),
  modelFile({ id: 3, model_id: 8, model_name: "Llama-3-70B", source_id: "meta/Llama-3-70B", node_uuid: "n1", node_name: "gpu-01", cache: true, state: "syncing", size_bytes: 140 * GIB, downloaded: 70 * GIB, progress: 0.5, path: "meta/Llama-3-70B", acquisition: "peer" }),
  modelFile({ id: 4, model_id: 9, model_name: "Broken", source_id: "x/Broken", state: "failed", message: "disk full", size_bytes: 0, downloaded: 0, progress: 0, path: "x/Broken", writer_node: "gpu-02", acquisition: "download" }),
];

test("the nodes tab lists every node with its state, mount, writability, capacity, and last report", async () => {
  stubDetail({ storage: cacheStorage, files: cacheFiles });
  renderDetail("/storage/3?tab=nodes");
  const row = async (name: string) => (await screen.findByText(name, { selector: "tbody a" })).closest("tr") as HTMLElement;
  const gpu1 = await row("gpu-01");
  assert.ok(within(gpu1).getByText(locale.common.storageNodeState.ready));
  // An NFS cache's row keeps the node and both states; the mount and cache
  // directory, writability, capacity and since when sit in its detail.
  const gpu1Detail = detailOf(gpu1);
  assert.ok(within(gpu1Detail).getByText(s.nodes.yes));
  assert.ok(within(gpu1Detail).getByText("Used 100.0 GiB of 117.0 GiB, 17.0 GiB available"));
  assert.ok(within(gpu1Detail).getByText("/mnt/gpuworks/team-cache"));
  assert.ok(within(gpu1Detail).getByText(s.nodes.cacheAt.replace("{{path}}", "/data/cache"), { exact: false }));
  assert.ok(within(gpu1Detail).getByText("2 min ago"));

  const gpu3 = await row("gpu-03");
  assert.ok(within(gpu3).getByText(locale.common.storageNodeState.failed));
  const gpu3Detail = detailOf(gpu3);
  assert.ok(within(gpu3Detail).getByText(/mount failed/));
  assert.ok(within(gpu3Detail).getByText(s.nodes.no));
  assert.ok(within(gpu3Detail).queryByText(/Used .* of/) === null, "no capacity reported is a dash, not a made-up bar");
  assert.ok(within(gpu3Detail).getAllByText("—").length >= 1);
});

test("an offline node is marked and says its figures are the last ones it reported", async () => {
  stubDetail({ storage: cacheStorage, files: cacheFiles });
  renderDetail("/storage/3?tab=nodes");
  const gpu2 = (await screen.findByText("gpu-02", { selector: "tbody a" })).closest("tr") as HTMLElement;
  assert.ok(gpu2.className.includes("storage-node-offline"));
  assert.ok(within(gpu2).getByText(locale.common.storageNodeState.offline));
  const gpu2Detail = detailOf(gpu2);
  assert.ok(within(gpu2Detail).getByText(s.nodes.offlineHint));
  assert.ok(within(gpu2Detail).getByText("2 h ago"));
});

test("an NFS cache adds the cache disk and the cached models against the node's limit, its own override first", async () => {
  stubDetail({ storage: cacheStorage, files: cacheFiles });
  renderDetail("/storage/3?tab=nodes");
  // The cache disk and the cached models are in each node's detail.
  const gpu1 = (await screen.findByText("gpu-01", { selector: "tbody a" })).closest("tr") as HTMLElement;
  const gpu1Detail = detailOf(gpu1);
  assert.ok(within(gpu1Detail).getByText(s.nodes.column.cache));
  assert.ok(within(gpu1Detail).getByText("Used 100.0 GiB of 1000.0 GiB, 900.0 GiB available"), "the cache disk");
  // 30 GiB finished + 70 GiB of the one still syncing, against the 100 GiB override.
  assert.ok(within(gpu1Detail).getByText("Cached models 100.0 GiB of 100.0 GiB limit"));
  const gpu2 = (await screen.findByText("gpu-02", { selector: "tbody a" })).closest("tr") as HTMLElement;
  assert.ok(within(detailOf(gpu2)).getByText(s.cacheManage.unknown), "missing cache accounting is unknown, not zero");
});

test("a local storage has no cache column; searching narrows the nodes; a storage without nodes says so", async () => {
  stubDetail({ storage: localStorage({ nodes: { total: 2, ready: 2, problems: [], all: [nodeStatus("n1", "gpu-01", "ready", { path: "/data/models", total_bytes: 100 * GIB, available_bytes: 60 * GIB }), nodeStatus("n2", "gpu-02", "ready", { path: "/data/models" })] } }) });
  const { user } = renderDetail("/storage/1?tab=nodes");
  await screen.findByText("gpu-01", { selector: "tbody a" });
  assert.ok(screen.queryByRole("columnheader", { name: s.nodes.column.cache }) === null, "Expected the element to be absent");
  await user.type(screen.getByRole("searchbox", { name: s.nodes.search }), "gpu-02");
  await waitFor(() => assert.ok(screen.queryByText("gpu-01", { selector: "tbody a" }) === null, "Expected the element to be absent"));
  assert.ok(screen.getByText("gpu-02", { selector: "tbody a" }));
  await user.clear(screen.getByRole("searchbox", { name: s.nodes.search }));
  await user.type(screen.getByRole("searchbox", { name: s.nodes.search }), "zzz");
  assert.ok(await screen.findByText(s.nodes.empty));
});

test("a node that cannot use the local storage's directory is told to get one of its own; others are not", async () => {
  stubDetail({ storage: localStorage({ path: "/opt/gpuworks/models", nodes: { total: 3, ready: 1, problems: [], all: [
    nodeStatus("n1", "gpu-01", "ready", { path: "/opt/gpuworks/models", writable: true }),
    nodeStatus("n2", "rootless", "failed", { path: "/opt/gpuworks/models", reason: "permission_denied", message: "mkdir /opt/gpuworks/models: permission denied", needs_own_directory: true }),
    nodeStatus("n3", "read-only-disk", "ready", { path: "/opt/gpuworks/models", writable: false, needs_own_directory: true }),
  ] } }) });
  renderDetail("/storage/1?tab=nodes");
  const row = async (name: string) => (await screen.findByText(name, { selector: "tbody a" })).closest("tr") as HTMLElement;
  assert.ok(within(detailOf(await row("rootless"))).getByText(s.nodes.ownDirectoryHint));
  assert.ok(within(detailOf(await row("read-only-disk"))).getByText(s.nodes.ownDirectoryHint));
  assert.ok(within(detailOf(await row("gpu-01"))).queryByText(s.nodes.ownDirectoryHint) === null, "Expected the element to be absent");
});

test("the files tab shows the shared copy and node caches with model link, place, path, size, state, source, last use and deployments", async () => {
  stubDetail({ storage: cacheStorage, files: cacheFiles });
  renderDetail("/storage/3?tab=files");
  const shared = (await screen.findAllByRole("link", { name: "Qwen3-8B" }))[0].closest("tr") as HTMLElement;
  assert.equal(within(shared).getByRole("link", { name: "Qwen3-8B" }).getAttribute("href"), "/models?detail=7");
  assert.ok(within(shared).getByText(s.files.shared));
  assert.ok(within(shared).getByText("16.0 GiB"));
  assert.ok(within(shared).getByText(models.presence.present));
  // How it got there and who deploys it sit in the row's detail.
  const sharedDetail = detailOf(shared);
  assert.ok(within(sharedDetail).getByText("Qwen/Qwen3-8B"), "its path");
  assert.ok(within(sharedDetail).getByText(models.acquisition.registered));
  assert.equal(within(sharedDetail).getByRole("link", { name: "#4" }).getAttribute("href"), "/deployments?deployment=4");

  const cached = (await screen.findAllByRole("link", { name: "Qwen3-8B" }))[1].closest("tr") as HTMLElement;
  assert.ok(within(cached).getByText("gpu-01"));
  assert.ok(within(cached).getByText(models.files.cache));
  assert.ok(within(cached).getByText("30.0 GiB"));
  assert.ok(within(detailOf(cached)).getByText(/2026-10-01 /), "last used");

  const syncing = (await screen.findByRole("link", { name: "Llama-3-70B" })).closest("tr") as HTMLElement;
  assert.ok(within(syncing).getByText(models.presence.syncing));
  assert.equal(within(syncing).getByRole("progressbar").getAttribute("aria-valuenow"), "50");
  assert.ok(within(detailOf(syncing)).getByText(models.acquisition.peer));

  const failed = (await screen.findByRole("link", { name: "Broken" })).closest("tr") as HTMLElement;
  assert.ok(within(failed).getByText(models.presence.failed));
  assert.ok(within(failed).queryByRole("progressbar") === null, "Expected the element to be absent");
  assert.ok(within(failed).getAllByText("—").length >= 1, "an unknown size is a dash");
  const failedDetail = detailOf(failed);
  assert.ok(within(failedDetail).getByText(models.files.writerNode.replace("{{node}}", "gpu-02")));
  assert.ok(within(failedDetail).getAllByText("—").length >= 1, "no deployments is a dash");
});

test("a failure's message is in a tooltip on its state", async () => {
  stubDetail({ storage: cacheStorage, files: cacheFiles });
  const { user } = renderDetail("/storage/3?tab=files");
  const failed = (await screen.findByRole("link", { name: "Broken" })).closest("tr") as HTMLElement;
  await user.hover(within(failed).getByText(models.presence.failed));
  assert.ok(await screen.findByText("disk full"));
});

test("the place filter offers all, shared and each node holding files; the search matches model names", async () => {
  stubDetail({ storage: cacheStorage, files: cacheFiles });
  const { user } = renderDetail("/storage/3?tab=files");
  await screen.findByRole("link", { name: "Broken" });
  const pick = async (label: string) => {
    fireEvent.mouseDown(screen.getByRole("combobox", { name: s.files.filterPlace }));
    const option = await waitFor(() => {
      const found = [...document.querySelectorAll(".ant-select-item-option")].find((o) => o.textContent === label);
      assert.ok(found, label);
      return found as HTMLElement;
    });
    fireEvent.click(option);
  };
  await pick(s.files.shared);
  await waitFor(() => assert.ok(screen.queryByRole("link", { name: "Llama-3-70B" }) === null, "Expected the element to be absent"));
  assert.ok(screen.getByRole("link", { name: "Broken" }));
  assert.equal(screen.getAllByRole("link", { name: "Qwen3-8B" }).length, 1, "only the shared copy");

  await pick("gpu-01");
  await waitFor(() => assert.ok(screen.queryByRole("link", { name: "Llama-3-70B" })));
  assert.ok(screen.queryByRole("link", { name: "Broken" }) === null, "Expected the element to be absent");

  await pick(s.files.placeAll);
  await user.type(screen.getByRole("searchbox", { name: s.files.search }), "llama");
  await waitFor(() => assert.ok(screen.queryByRole("link", { name: "Broken" }) === null, "Expected the element to be absent"));
  assert.ok(screen.getByRole("link", { name: "Llama-3-70B" }));
  await user.clear(screen.getByRole("searchbox", { name: s.files.search }));
  await user.type(screen.getByRole("searchbox", { name: s.files.search }), "nothing");
  assert.ok(await screen.findByText(s.files.noMatch));
});

test("an admin deletes the shared copy through the confirmation dialog, then the list reloads", async () => {
  const calls = stubDetail({ storage: cacheStorage, files: cacheFiles });
  const { user } = renderDetail("/storage/3?tab=files");
  const shared = (await screen.findAllByRole("link", { name: "Qwen3-8B" }))[0].closest("tr") as HTMLElement;
  const before = calls.storageModelFiles.length;
  await user.click(within(shared).getByRole("button", { name: models.action.deleteModelFiles }));
  // The detail is a drawer, itself a dialog: the confirmation is the other one.
  const impact = models.storageDelete.impactCache.replace("{{storage}}", "team-cache").replace("{{count}}", "1");
  const dialog = await waitFor(() => {
    const found = screen.getAllByRole("dialog").find((d) => within(d).queryByText(impact));
    assert.ok(found, "names the one node cache it feeds");
    return found;
  });
  await user.click(within(dialog).getByRole("button", { name: models.storageDelete.confirm }));
  await waitFor(() => assert.deepEqual(calls.deleteModelInStorage, [[7, 3, false]]));
  await waitFor(() => assert.ok(calls.storageModelFiles.length > before));
});

test("a node's cache is removed from that node only, after a confirmation; a failure is reported", async () => {
  const calls = stubDetail({ storage: cacheStorage, files: cacheFiles });
  const { user } = renderDetail("/storage/3?tab=files");
  const cached = (await screen.findAllByRole("link", { name: "Qwen3-8B" }))[1].closest("tr") as HTMLElement;
  await user.click(within(cached).getByRole("button", { name: s.files.evictCache }));
  await confirm(user, models.action.delete);
  await waitFor(() => assert.deepEqual(calls.deleteModelOnNode, [[7, "n1", 3]]));

  api.deleteModelOnNode = (async () => { throw new Error("node busy"); }) as never;
  const again = (await screen.findAllByRole("link", { name: "Qwen3-8B" }))[1].closest("tr") as HTMLElement;
  fireEvent.click(within(again).getByRole("button", { name: s.files.evictCache }));
  await confirm(user, models.action.delete);
  assert.ok(await screen.findByText("node busy"));
});

test("a local storage lists each node's copy and deletes it from that node", async () => {
  const calls = stubDetail({ storage: localStorage(), files: [
    modelFile({ id: 1, node_uuid: "n1", node_name: "gpu-01" }),
    modelFile({ id: 2, node_uuid: "n2", node_name: "gpu-02" }),
  ] });
  const { user } = renderDetail("/storage/1?tab=files");
  const first = (await screen.findAllByRole("link", { name: "Qwen3-8B" }))[0].closest("tr") as HTMLElement;
  assert.ok(within(first).getByText("gpu-01"));
  assert.ok(within(first).queryByText(models.files.cache) === null, "Expected the element to be absent");
  assert.ok(screen.queryByRole("columnheader", { name: s.files.column.lastUsed }) === null, "last use only matters for caches");
  await user.click(within(first).getByRole("button", { name: models.files.deleteOnNode }));
  await confirm(user, models.action.delete);
  await waitFor(() => assert.deepEqual(calls.deleteModelOnNode, [[7, "n1", 1]]));
});

test("a disabled NFS cache still lets an admin remove a node's cache, while its library stays protected", async () => {
  stubDetail({ storage: { ...cacheStorage, enabled: false, writable: false }, files: cacheFiles });
  renderDetail("/storage/3?tab=files");
  const rows = await screen.findAllByRole("link", { name: "Qwen3-8B" });
  const library = rows[0].closest("tr") as HTMLElement;
  const cached = rows[1].closest("tr") as HTMLElement;
  assert.equal((within(cached).getByRole("button", { name: s.files.evictCache }) as HTMLButtonElement).disabled, false);
  assert.equal((within(library).getByRole("button", { name: models.action.deleteModelFiles }) as HTMLButtonElement).disabled, true);
});

test("a read-only storage's delete buttons are disabled; a viewer has no actions column", async () => {
  const roStorage = nfsStorage({ enabled: false, writable: false, nodes: { total: 1, ready: 1, problems: [] } });
  stubDetail({ storage: roStorage, files: [modelFile()] });
  const first = renderDetail("/storage/2?tab=files");
  const button = await screen.findByRole("button", { name: models.action.deleteModelFiles }) as HTMLButtonElement;
  assert.equal(button.disabled, true);
  first.view.unmount();

  stubDetail({ storage: nfsStorage({ nodes: { total: 1, ready: 1, problems: [] } }), files: [modelFile()], admin: false });
  renderDetail("/storage/2?tab=files");
  await screen.findByRole("link", { name: "Qwen3-8B" });
  await waitFor(() => assert.ok(screen.queryByRole("columnheader", { name: s.files.column.actions }) === null, "Expected the element to be absent"));
  assert.ok(screen.queryByRole("button", { name: models.action.deleteModelFiles }) === null, "Expected the element to be absent");
});

test("an empty shared storage invites scanning (admin only); an empty local one explains where files come from", async () => {
  stubDetail({ storage: nfsStorage({ nodes: { total: 1, ready: 1, problems: [] } }), files: [] });
  const first = renderDetail("/storage/2?tab=files");
  assert.ok(await screen.findByText(s.files.emptyShared));
  await first.user.click(screen.getAllByRole("button", { name: s.actions.scan }).at(-1)!);
  assert.ok(await screen.findByText(s.scan.title.replace("{{name}}", "model-archive")));
  first.view.unmount();

  stubDetail({ storage: nfsStorage({ nodes: { total: 1, ready: 1, problems: [] } }), files: [], admin: false });
  const viewer = renderDetail("/storage/2?tab=files");
  assert.ok(await screen.findByText(s.files.emptyShared));
  assert.ok(screen.queryByRole("button", { name: s.actions.scan }) === null, "Expected the element to be absent");
  viewer.view.unmount();

  stubDetail({ storage: localStorage(), files: [] });
  renderDetail("/storage/1?tab=files");
  assert.ok(await screen.findByText(s.files.emptyLocal));
});

test("while the files load there is a spinner; a failed load says so and offers retry", async () => {
  const calls = stubDetail({ storage: nfsStorage({ nodes: { total: 1, ready: 1, problems: [] } }), files: new Error("files down") });
  const first = renderDetail("/storage/2?tab=files");
  assert.ok(await screen.findByText("files down"));
  const before = calls.storageModelFiles.length;
  await first.user.click(screen.getByRole("button", { name: locale.common.actions.retry }));
  await waitFor(() => assert.ok(calls.storageModelFiles.length > before));
  first.view.unmount();

  let release: (rows: unknown) => void = () => {};
  stubDetail({ storage: nfsStorage({ nodes: { total: 1, ready: 1, problems: [] } }) });
  api.storageModelFiles = (() => new Promise((resolve) => { release = resolve as never; })) as never;
  renderDetail("/storage/2?tab=files");
  await screen.findByRole("tab", { name: /Model files/ });
  assert.ok(screen.getByRole("table").getAttribute("aria-busy") === "true");
  release(list([modelFile()]));
  assert.ok(await screen.findByRole("link", { name: "Qwen3-8B" }));
});
