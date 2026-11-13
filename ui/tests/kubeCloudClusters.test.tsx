import assert from "node:assert/strict";
import { afterEach, beforeEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const { i18n, locale } = await createI18n(["kubernetes", "providers"]);
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: ProviderClustersTable } = await import("../src/components/ProviderClustersTable.tsx");
const { default: ProviderInstancesTable } = await import("../src/components/ProviderInstancesTable.tsx");
const { default: KubernetesPage } = await import("../src/pages/KubernetesPage.tsx");
import type { KubeCluster, KubeClusterOperation, KubeDefaults, ProviderCluster, ProviderInstance } from "../src/api";

const k = locale.kubernetes;
const p = locale.providers;
const originals = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originals); });
// The drawer asks the server for its defaults when it opens.
beforeEach(() => { api.kubeDefaults = (async () => DEFAULTS) as never; });
const DEFAULTS: KubeDefaults = {
  namespace: "gpuworks-system", state_dir: "/opt/gpuworks", engine_ports: "40000-42767", idle_return_seconds: 60, idle_return_keep: -1,
  agent_image: "gpuworks/gpuworks-agent:r1", stalled_after_seconds: 300,
  agent_image_source: "dockerhub",
  agent_image_sources: [
    { id: "dockerhub", image: "gpuworks/gpuworks-agent:r1" },
    { id: "swr", image: "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1" },
    { id: "quay", image: "quay.io/gpuworks/gpuworks-agent:r1" },
  ],
  priority: 10000, priority_max: 1000000000, allow_preemption: false,
};

const gpuCluster: ProviderCluster = {
  external_id: "c-gpu", name: "ack-gpu", region: "cn-shanghai", version: "1.35.7-aliyun.1", state: "running", running: true, public_api: true, agent_image_source: "swr",
  gpu_pools: [{ id: "np-l20", name: "l20", gpu_model: "NVIDIA L20", gpu_count: 1, nodes: 2, vendor: "nvidia", in_entry: false }],
};
const privateCluster: ProviderCluster = { ...gpuCluster, external_id: "c-private", name: "ack-private", public_api: false, gpu_pools: [] };
const connectedCluster: ProviderCluster = { ...gpuCluster, external_id: "c-done", name: "ack-done", entry: { uuid: "e-1", name: "ack-done" }, gpu_pools: gpuCluster.gpu_pools.map((p) => ({ ...p, in_entry: true })) };
const newCluster: ProviderCluster = { ...gpuCluster, external_id: "c-new", name: "ack-new", running: false, state: "initial", gpu_pools: [] };

function entry(overrides: Partial<KubeCluster> = {}): KubeCluster {
  return {
    id: 5, uuid: "e-5", name: "ack-gpu", namespace: "gpuworks-system", server_url: "http://localhost", agent_image: "", node_selector: "",
    agent_image_source: "swr", effective_agent_image: "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1",
    vendors: [], state_dir: "/opt/gpuworks", runtime_class: "", image_pull_secret: "", containerd_socket: "", api_server: "", engine_ports: "40000-42767",
    idle_return_seconds: 60, priority: 10000, allow_preemption: false, cluster_uid: "", revoked: false, node_count: 0, created_at: new Date().toISOString(), pool_ids: ["np-l20"], namespace_shared_with: [],
    apply_command: "kubectl apply -f 'http://localhost/kube/manifest.yaml?token=t'", stalled: false, can_apply: true, can_remove: true,
    source: { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-gpu" }, ...overrides,
  };
}

const plan: KubeClusterOperation = {
  dry_run: true,
  results: [
    { kind: "Namespace", name: "gpuworks-system", action: "create" },
    { kind: "DaemonSet", name: "gpuworks-agent", namespace: "gpuworks-system", action: "dependent" },
    { kind: "PriorityClass", name: "gpuworks-inference-gpuworks-system", action: "update" },
  ],
};

function wrap(node: React.ReactNode) {
  return render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter>{node}</MemoryRouter>
    </I18nextProvider>,
  );
}
const user = () => userEvent.setup({ document: dom.window.document });
// The operation dialog opens over the drawer: the last dialog is the one on top.
const modal = () => within(screen.getAllByRole("dialog").at(-1) as HTMLElement);
// A label's required mark ("Name *") is part of its text.
const field = (text: string) => new RegExp(`^${text}( \\*)?$`);

test("a cloud account lists its clusters: GPU pools to connect, ones already connected, private APIs and clusters not ready", async () => {
  const calls: number[] = [];
  api.providerClusters = (async (id: number) => { calls.push(id); return list([gpuCluster, privateCluster, connectedCluster, newCluster]); }) as never;
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  const row = (await screen.findByText("ack-gpu")).closest("tr") as HTMLElement;
  assert.deepEqual(calls, [3]);
  assert.ok(within(row).getByText("l20 · NVIDIA L20 ×1 · 2 nodes"));
  assert.ok(within(row).getByText(p.clusters.publicAPI));
  assert.ok(within(row).getByRole("button", { name: p.clusters.connect }));
  assert.equal((within(row).getByRole("checkbox", { name: "l20 · NVIDIA L20 ×1 · 2 nodes" }) as HTMLElement).getAttribute("aria-checked"), "true", "a cluster with one pool offers it ticked");
  const priv = screen.getByText("ack-private").closest("tr") as HTMLElement;
  assert.ok(within(priv).getByText(p.clusters.privateAPI));
  assert.ok(within(priv).getByText(p.clusters.noGPUPools));
  const done = screen.getAllByText("ack-done")[0].closest("tr") as HTMLElement;
  assert.ok(within(done).getByText(p.clusters.connected));
  assert.ok(within(done).getByText(p.clusters.inEntry), "the pool the entry holds says so");
  assert.equal(within(done).queryByRole("button", { name: p.clusters.connect }), null, "a cluster has one entry: no second connect");
  assert.ok(within(done).getByRole("button", { name: p.clusters.savePools }), "its pools are edited instead");
  assert.ok(within(screen.getByText("ack-new").closest("tr") as HTMLElement).getByText(p.clusters.notRunning));
});

test("a failed listing says why and offers a refresh", async () => {
  let n = 0;
  api.providerClusters = (async () => { n++; if (n === 1) throw new Error("aliyun: Forbidden.RAM cs:DescribeClustersV1"); return list([]); }) as never;
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  assert.ok(await screen.findByText("aliyun: Forbidden.RAM cs:DescribeClustersV1"));
  await user().click(screen.getByRole("button", { name: p.clusters.refresh }));
  assert.ok(await screen.findByText(p.clusters.empty));
});

test("connecting a pool opens the form filled from the cluster, records the source, then applies only after a checked plan is confirmed", async () => {
  api.providerClusters = (async () => list([gpuCluster])) as never;
  const created: unknown[] = [];
  api.createKubeCluster = (async (body: unknown) => { created.push(body); return entry(); }) as never;
  const ops: [string, string, boolean][] = [];
  api.kubeClusterOperation = (async (uuid: string, op: string, dryRun: boolean) => {
    ops.push([uuid, op, dryRun]);
    return dryRun ? plan : { dry_run: false, results: plan.results.map((r) => ({ ...r, action: "create" })) };
  }) as never;
  const u = user();
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  await u.click(await screen.findByRole("button", { name: p.clusters.connect }));
  const drawer = within(await screen.findByRole("dialog"));
  assert.ok(await drawer.findByText(k.drawer.fromSource.replace("{{source}}", "ali-prod · ack-gpu (c-gpu)")));
  assert.equal((drawer.getByLabelText(field(k.drawer.name)) as HTMLInputElement).value, "ack-gpu");
  assert.equal(drawer.queryByPlaceholderText("nvidia.com/gpu.present=true"), null, "an entry from a cloud cluster selects by node pool, not by a selector");
  assert.equal(drawer.queryByRole("combobox", { name: /vendor|厂商/i }), null, "no vendor is asked either: each agent detects its own node's cards");
  assert.equal((drawer.getByRole("checkbox", { name: "l20 · NVIDIA L20 ×1 · 2 nodes" }) as HTMLElement).getAttribute("aria-checked"), "true");
  await u.click(drawer.getByText(k.drawer.advanced));
  assert.equal(drawer.getByRole("combobox", { name: field(k.drawer.agentImageSource) }).textContent, k.drawer.agentImageSources.swr,
    "the registry the server suggests for the cluster's region");
  assert.equal((drawer.getByLabelText(field(k.drawer.agentImage)) as HTMLInputElement).placeholder, "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1");
  await u.click(drawer.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(created.length, 1));
  assert.deepEqual(created[0], {
    name: "ack-gpu", server_url: "http://localhost", provider_id: 3, provider_cluster_id: "c-gpu", provider_pool_ids: ["np-l20"],
  });
  assert.ok(await drawer.findByText(k.created.byGpuworks));
  assert.ok(drawer.getByText(k.created.byKubectl), "kubectl stays an option");
  assert.equal(ops.length, 0, "nothing touches the cluster before the operator asks");

  await u.click(drawer.getByRole("button", { name: k.created.applyNow }));
  assert.ok(await modal().findByText(k.operation.checkedApply));
  assert.deepEqual(ops, [["e-5", "apply", true]], "a dry run first");
  assert.ok(modal().getByText("gpuworks-system/gpuworks-agent"));
  assert.ok(modal().getByText(k.operation.plan.create));
  assert.ok(modal().getByText(k.operation.plan.dependent));
  assert.ok(modal().getByText(k.operation.applyIntro.replace("{{account}}", "ali-prod").replace("{{cluster}}", "c-gpu")));
  await u.click(modal().getByRole("button", { name: k.operation.confirmApply }));
  assert.ok(await modal().findByText(k.operation.applied));
  assert.deepEqual(ops, [["e-5", "apply", true], ["e-5", "apply", false]]);
  assert.equal(modal().getAllByText(k.operation.done.create).length, 3);
});

test("a refused check names the step and the cluster's reason, and nothing can be confirmed", async () => {
  api.kubeClusters = (async () => list([entry()])) as never;
  api.me = (async () => ({ role: "admin" })) as never;
  const ops: boolean[] = [];
  api.kubeClusterOperation = (async (_u: string, _o: string, dryRun: boolean) => {
    ops.push(dryRun);
    return {
      dry_run: true, code: "KUBE_CLUSTER_OPERATION_FAILED", error: "Kubernetes API server: forbidden",
      params: { step: "api_server", reason: "clusterroles is forbidden: User \"ram-user\" cannot patch" },
      results: [{ kind: "Namespace", name: "gpuworks-system", action: "create" }, { kind: "ClusterRole", name: "gpuworks-agent-gpuworks-system", action: "failed", error: "forbidden" }],
    };
  }) as never;
  const u = user();
  wrap(<KubernetesPage />);
  const row = (await screen.findByText("ack-gpu")).closest("tr") as HTMLElement;
  assert.ok(within(row).getByText("ali-prod"), "the source account");
  assert.ok(within(row).getByText("c-gpu"));
  await u.click(within(row).getByRole("button", { name: k.actions.apply }));
  assert.ok(await modal().findByText(k.operation.failed.api_server));
  assert.ok(modal().getByText("clusterroles is forbidden: User \"ram-user\" cannot patch"));
  assert.equal(modal().queryByRole("button", { name: k.operation.confirmApply }), null);
  await u.click(modal().getByRole("button", { name: k.operation.retry }));
  await waitFor(() => assert.equal(ops.length, 2));
  assert.deepEqual(ops, [true, true], "only dry runs");
});

test("removing from the cluster checks first, then deletes on confirmation; an entry made by hand offers neither", async () => {
  api.kubeClusters = (async () => list([entry(), entry({ id: 6, uuid: "e-6", name: "by-hand", source: undefined, can_apply: false, can_remove: false, operation_blocked: "no_source" })])) as never;
  api.me = (async () => ({ role: "admin" })) as never;
  const ops: [string, string, boolean][] = [];
  api.kubeClusterOperation = (async (uuid: string, op: string, dryRun: boolean) => {
    ops.push([uuid, op, dryRun]);
    return { dry_run: dryRun, results: [{ kind: "DaemonSet", name: "gpuworks-agent", namespace: "gpuworks-system", action: "delete" },
      { kind: "PriorityClass", name: "gpuworks-inference-gpuworks-system", action: "delete" }] };
  }) as never;
  const u = user();
  wrap(<KubernetesPage />);
  const hand = (await screen.findByText("by-hand")).closest("tr") as HTMLElement;
  assert.ok(within(hand).getByText(k.source.byHand));
  assert.equal(within(hand).queryByRole("button", { name: k.actions.remove }), null);
  assert.equal(within(hand).queryByRole("button", { name: k.actions.apply }), null);
  const row = screen.getByText("ack-gpu").closest("tr") as HTMLElement;
  await u.click(within(row).getByRole("button", { name: k.actions.remove }));
  assert.ok(await modal().findByText(k.operation.checkedRemove));
  assert.ok(modal().getByText("gpuworks-inference-gpuworks-system"));
  await u.click(modal().getByRole("button", { name: k.operation.confirmRemove }));
  assert.ok(await modal().findByText(k.operation.removed));
  assert.deepEqual(ops, [["e-5", "remove", true], ["e-5", "remove", false]]);
});

test("a product error before any object (no source any more) is shown as such", async () => {
  api.kubeClusters = (async () => list([entry()])) as never;
  api.me = (async () => ({ role: "admin" })) as never;
  api.kubeClusterOperation = (async () => { throw new APIError("KUBE_CLUSTER_NO_SOURCE", undefined, "no source", 409); }) as never;
  const u = user();
  wrap(<KubernetesPage />);
  const row = (await screen.findByText("ack-gpu")).closest("tr") as HTMLElement;
  await u.click(within(row).getByRole("button", { name: k.actions.apply }));
  assert.ok(await modal().findByText(locale.common.apiError.KUBE_CLUSTER_NO_SOURCE));
  assert.ok(modal().getByText(k.operation.failed.other));
});

test("a node of a managed cluster is marked as such and offers no install, power or release of its own; the server's actions decide", async () => {
  const base: ProviderInstance = { external_id: "i-1", name: "plain", gpu_model: "NVIDIA A10", gpu_count: 1, power: "running", price_per_hr: 0, region: "cn-shanghai",
    public_ip: "203.0.113.5", ssh_port: 22, ssh_user: "root", ssh_connections: [], kind: "vm", node_id: 0, actions: ["power_off", "install", "delete"] };
  const stopped: ProviderInstance = { ...base, external_id: "i-3", name: "idle", power: "stopped", actions: ["power_on", "install", "delete"] };
  const clusterNode: ProviderInstance = { ...base, external_id: "i-2", name: "ack-node", managed_cluster: "c-gpu", actions: [] };
  api.providerInstances = (async () => list([base, clusterNode, stopped])) as never;
  wrap(<ProviderInstancesTable providerId={3} refreshKey="1" />);
  const node = (await screen.findByText("ack-node")).closest("tr") as HTMLElement;
  assert.ok(within(node).getByText(p.instances.clusterNode));
  assert.equal(within(node).queryByRole("button", { name: p.instances.installAgent }), null);
  assert.equal(within(node).queryByRole("button", { name: p.instances.powerOff }), null);
  assert.equal(within(node).queryByRole("button", { name: p.instances.powerOn }), null);
  const plain = screen.getByText("plain").closest("tr") as HTMLElement;
  assert.ok(within(plain).getByRole("button", { name: p.instances.installAgent }));
  assert.ok(within(plain).getByRole("button", { name: p.instances.powerOff }));
  assert.equal(within(plain).queryByRole("button", { name: p.instances.powerOn }), null);
  const idle = screen.getByText("idle").closest("tr") as HTMLElement;
  assert.ok(within(idle).getByRole("button", { name: p.instances.powerOn }));
});

test("an instance the server offers no action on shows none, even where the kind controls power", async () => {
  const bare: ProviderInstance = { external_id: "i-9", name: "bare", gpu_model: "", gpu_count: 0, power: "running", price_per_hr: 0, region: "",
    public_ip: "", ssh_port: 0, ssh_user: "", ssh_connections: [], kind: "vm", node_id: 4, actions: [] };
  api.providerInstances = (async () => list([bare])) as never;
  const kind = { kind: "aliyun", label: "Aliyun", capabilities: { can_list_instances: true, can_power_control: true, can_install_agent: true, can_create: false, can_delete: true, can_inject_user_data: false, has_pricing: false }, credential_fields: [], server_dial_supported: false, can_list_clusters: false };
  wrap(<ProviderInstancesTable providerId={3} kindInfo={kind as never} refreshKey="1" />);
  const row = (await screen.findByText("bare")).closest("tr") as HTMLElement;
  assert.equal(within(row).queryByRole("button", { name: p.instances.powerOff }), null);
  assert.ok(within(row).getByText(p.instances.onboarded));
});

test("an account whose cloud lists clusters shows Instances and Clusters tabs; the clusters are read only when their tab opens", async () => {
  const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
  const calls: number[] = [];
  const instance: ProviderInstance = { external_id: "i-1", name: "plain-host", gpu_model: "NVIDIA A10", gpu_count: 1, power: "running", price_per_hr: 0, region: "cn-shanghai",
    public_ip: "203.0.113.5", ssh_port: 22, ssh_user: "root", ssh_connections: [], kind: "vm", node_id: 0, actions: ["install"] };
  api.providers = (async () => list([{ id: 3, name: "ali-prod", kind: "aliyun", state: "valid", instance_count: 1, created_at: "", updated_at: "" }])) as never;
  api.providerKinds = (async () => list([{ kind: "aliyun", label: "Aliyun", capabilities: { can_list_instances: true, can_power_control: false, can_install_agent: true, can_create: false, can_delete: false, can_inject_user_data: false, has_pricing: false }, credential_fields: [], server_dial_supported: false, can_list_clusters: true }])) as never;
  api.providerInstances = (async () => list([instance])) as never;
  api.providerClusters = (async (id: number) => { calls.push(id); return list([gpuCluster]); }) as never;
  const u = user();
  wrap(<CloudProvidersPage scope="public" />);
  await screen.findByText("ali-prod");
  await u.click(screen.getByRole("button", { name: /expand row/i }));
  assert.ok(await screen.findByText("plain-host"), "the instances tab is the first");
  const tabs = screen.getAllByRole("tab");
  assert.deepEqual(tabs.map((tab) => tab.textContent), [p.tabs.instances, p.tabs.clusters]);
  assert.equal(tabs[0].getAttribute("aria-selected"), "true");
  assert.deepEqual(calls, [], "nothing is asked of the cloud before the tab opens");
  await u.click(screen.getByRole("tab", { name: p.tabs.clusters }));
  assert.ok(await screen.findByText("ack-gpu"));
  assert.deepEqual(calls, [3]);
  await waitFor(() => assert.ok(!screen.queryByText("plain-host"), "the inactive tab is unmounted"));
  await u.click(screen.getByRole("tab", { name: p.tabs.instances }));
  assert.ok(await screen.findByText("plain-host"));
  await waitFor(() => assert.ok(!screen.queryByText("ack-gpu"), "the clusters tab is unmounted again"));
});

test("a cloud account's clusters show a loading status until the cloud answers", async () => {
  let release: (rows: unknown) => void = () => {};
  api.providerClusters = (() => new Promise((resolve) => { release = resolve as never; })) as never;
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  assert.ok(screen.getByRole("status"), "a spinner while the first listing runs");
  release(list([gpuCluster]));
  assert.ok(await screen.findByText("ack-gpu"));
  await waitFor(() => assert.ok(!screen.queryByRole("status")));
});

const multiCluster: ProviderCluster = {
  ...gpuCluster, external_id: "c-multi", name: "ack-multi",
  gpu_pools: [
    { id: "np-a", name: "a10", gpu_model: "NVIDIA A10", gpu_count: 1, nodes: 2, vendor: "nvidia", in_entry: false },
    { id: "np-l", name: "l20", gpu_model: "NVIDIA L20", gpu_count: 1, nodes: 1, vendor: "nvidia", in_entry: false },
    { id: "np-p", name: "ppu", gpu_model: "PPU-ZW810E", gpu_count: 1, nodes: 1, vendor: "ppu", in_entry: false },
  ],
};
const poolBox = (scope: HTMLElement | ReturnType<typeof within>, name: string) =>
  (scope instanceof dom.window.HTMLElement ? within(scope) : scope).getByRole("checkbox", { name }) as HTMLElement;
const checked = (box: HTMLElement) => box.getAttribute("aria-checked") === "true";
const A10 = "a10 · NVIDIA A10 ×1 · 2 nodes";
const L20 = "l20 · NVIDIA L20 ×1 · 1 nodes";
const PPU = "ppu · PPU-ZW810E ×1 · 1 nodes";

test("several pools of one cluster are ticked and connected as one entry; connecting waits for a pool; pools of different vendors go together", async () => {
  api.providerClusters = (async () => list([multiCluster])) as never;
  const created: unknown[] = [];
  api.createKubeCluster = (async (body: unknown) => {
    created.push(body);
    return entry({ name: "ack-multi", pool_ids: (body as { provider_pool_ids: string[] }).provider_pool_ids });
  }) as never;
  const u = user();
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  const row = (await screen.findByText("c-multi")).closest("tr") as HTMLElement;
  assert.equal(checked(poolBox(row, A10)) || checked(poolBox(row, L20)) || checked(poolBox(row, PPU)), false, "with several pools none is ticked for you");
  const connect = within(row).getByRole("button", { name: p.clusters.connect }) as HTMLButtonElement;
  assert.equal(connect.disabled, true, "nothing to connect yet");
  await u.click(poolBox(row, A10));
  await u.click(poolBox(row, L20));
  assert.equal(connect.disabled, false);
  await u.click(connect);
  const drawer = within(await screen.findByRole("dialog"));
  assert.equal(checked(poolBox(drawer, A10)) && checked(poolBox(drawer, L20)), true, "the drawer opens with the pools ticked in the row");
  assert.equal(checked(poolBox(drawer, PPU)), false);
  await u.click(drawer.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(created.length, 1));
  assert.deepEqual(created[0], { name: "ack-multi", server_url: "http://localhost", provider_id: 3, provider_cluster_id: "c-multi", provider_pool_ids: ["np-a", "np-l"] });
  assert.ok(await drawer.findByText(k.created.byGpuworks));
  await u.click(drawer.getAllByRole("button", { name: k.drawer.close }).at(-1) as HTMLElement);

  // Changing the choice in the drawer is allowed, and pools of another vendor can join: the entry is not of one vendor.
  api.providerClusters = (async () => list([multiCluster])) as never;
  await u.click(within(document.body).getByRole("button", { name: p.clusters.refresh }));
  const again = (await screen.findByText("c-multi")).closest("tr") as HTMLElement;
  await u.click(poolBox(again, A10));
  await u.click(within(again).getByRole("button", { name: p.clusters.connect }));
  const second = within(await screen.findByRole("dialog"));
  await u.click(poolBox(second, PPU));
  await u.click(second.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await second.findByText(k.created.byGpuworks));
  assert.deepEqual((created[1] as { provider_pool_ids: string[] }).provider_pool_ids.sort(), ["np-a", "np-p"]);
  assert.equal(second.queryByRole("alert"), null, "no vendor refusal");
});

test("an entry's pools are edited in the cloud row: the held ones are ticked and marked, saving sends the set, and applying again is offered", async () => {
  const held: ProviderCluster = {
    ...multiCluster, entry: { uuid: "e-5", name: "ack-multi" },
    gpu_pools: multiCluster.gpu_pools.map((q) => ({ ...q, in_entry: q.id === "np-a" })),
  };
  api.providerClusters = (async () => list([held])) as never;
  const saves: [string, string[]][] = [];
  api.setKubeClusterPools = (async (uuid: string, ids: string[]) => { saves.push([uuid, ids]); return entry({ uuid, name: "ack-multi", pool_ids: ids }); }) as never;
  const ops: [string, string, boolean][] = [];
  api.kubeClusterOperation = (async (uuid: string, op: string, dryRun: boolean) => { ops.push([uuid, op, dryRun]); return dryRun ? plan : { dry_run: false, results: plan.results }; }) as never;
  const u = user();
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  const row = (await screen.findByText("c-multi")).closest("tr") as HTMLElement;
  assert.deepEqual([A10, L20, PPU].map((n) => checked(poolBox(row, n))), [true, false, false], "the entry's pool is ticked");
  assert.equal(within(row).getAllByText(p.clusters.inEntry).length, 1, "and the only one marked");
  assert.equal(within(row).queryByRole("button", { name: p.clusters.connect }), null);
  const save = within(row).getByRole("button", { name: p.clusters.savePools }) as HTMLButtonElement;
  assert.equal(save.disabled, true, "nothing changed yet");
  await u.click(poolBox(row, A10));
  assert.equal(save.disabled, true, "an entry cannot be left without a pool");
  await u.click(poolBox(row, L20));
  await u.click(poolBox(row, A10));
  assert.equal(save.disabled, false);
  await u.click(save);
  await waitFor(() => assert.equal(saves.length, 1));
  assert.deepEqual(saves[0][0], "e-5");
  assert.deepEqual([...saves[0][1]].sort(), ["np-a", "np-l"]);
  assert.ok(await screen.findByText(k.pools.saved.replace("{{name}}", "ack-multi")));
  assert.ok(screen.getByText(new RegExp(k.pools.savedHelp.slice(0, 30))));
  assert.equal(ops.length, 0, "saving applies nothing");
  await u.click(screen.getByRole("button", { name: k.pools.applyNow }));
  assert.ok(await modal().findByText(k.operation.checkedApply));
  assert.deepEqual(ops, [["e-5", "apply", true]], "a checked plan comes first");
});

test("a refused pool edit shows the server's reason and nothing is offered to apply", async () => {
  const held: ProviderCluster = { ...multiCluster, entry: { uuid: "e-5", name: "ack-multi" }, gpu_pools: multiCluster.gpu_pools.map((q) => ({ ...q, in_entry: q.id === "np-a" })) };
  api.providerClusters = (async () => list([held])) as never;
  api.setKubeClusterPools = (async () => { throw new APIError("KUBE_CLUSTER_SOURCE_TAKEN", { cluster: "c-multi" }, "taken", 409); }) as never;
  const u = user();
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  const row = (await screen.findByText("c-multi")).closest("tr") as HTMLElement;
  await u.click(poolBox(row, PPU));
  await u.click(within(row).getByRole("button", { name: p.clusters.savePools }));
  assert.ok(await screen.findByText(locale.common.apiError.KUBE_CLUSTER_SOURCE_TAKEN.replace("{{cluster}}", "c-multi")));
  assert.equal(screen.queryByRole("button", { name: k.pools.applyNow }), null);
});

test("the Kubernetes page edits an entry's pools in a drawer: the cloud's pools are listed, the held ones ticked, and a saved set offers applying again", async () => {
  api.me = (async () => ({ role: "admin" })) as never;
  const held = entry({ pool_ids: ["np-a"] });
  api.kubeClusters = (async () => list([held, entry({ id: 6, uuid: "e-6", name: "by-hand", source: undefined, pool_ids: [], can_apply: false, can_remove: false, operation_blocked: "no_source" })])) as never;
  api.providerClusters = (async () => list([{ ...multiCluster, external_id: "c-gpu" }])) as never;
  const saves: [string, string[]][] = [];
  api.setKubeClusterPools = (async (uuid: string, ids: string[]) => { saves.push([uuid, ids]); return { ...held, pool_ids: ids }; }) as never;
  const u = user();
  wrap(<KubernetesPage />);
  const hand = (await screen.findByText("by-hand")).closest("tr") as HTMLElement;
  assert.equal(within(hand).queryByRole("button", { name: k.actions.pools }), null, "an entry made by hand has no pools to edit");
  const row = screen.getByText("ack-gpu").closest("tr") as HTMLElement;
  assert.ok(within(row).getByText("1 node pool"));
  await u.click(within(row).getByRole("button", { name: k.actions.pools }));
  const drawer = within(await screen.findByRole("dialog"));
  assert.ok(await drawer.findByText(k.pools.title.replace("{{name}}", "ack-gpu")));
  assert.deepEqual(await waitFor(() => [A10, L20, PPU].map((n) => checked(poolBox(drawer, n)))), [true, false, false]);
  const save = drawer.getByRole("button", { name: k.pools.save }) as HTMLButtonElement;
  assert.equal(save.disabled, true);
  await u.click(poolBox(drawer, L20));
  await u.click(save);
  await waitFor(() => assert.equal(saves.length, 1));
  assert.deepEqual(saves[0][0], "e-5");
  assert.deepEqual([...saves[0][1]].sort(), ["np-a", "np-l"]);
  assert.ok(await drawer.findByText(k.pools.saved.replace("{{name}}", "ack-gpu")));
  assert.ok(drawer.getByRole("button", { name: k.pools.applyNow }));
});

test("an entry from before node pools says it selects by a node selector, and saving pools replaces it", async () => {
  api.me = (async () => ({ role: "admin" })) as never;
  api.kubeClusters = (async () => list([entry({ pool_ids: [], node_selector: "alibabacloud.com/nodepool-id=np-a,gpu=a10" })])) as never;
  api.providerClusters = (async () => list([{ ...multiCluster, external_id: "c-gpu" }])) as never;
  const u = user();
  wrap(<KubernetesPage />);
  const row = (await screen.findByText("ack-gpu")).closest("tr") as HTMLElement;
  assert.equal(within(row).queryByText(/node pool/), null, "no pool count when it holds none");
  await u.click(within(row).getByRole("button", { name: k.actions.pools }));
  const drawer = within(await screen.findByRole("dialog"));
  assert.ok(await drawer.findByText(k.pools.legacy));
  assert.ok(drawer.getByText(k.pools.legacyHelp.replace("{{selector}}", "alibabacloud.com/nodepool-id=np-a,gpu=a10")));
  await waitFor(() => assert.equal(checked(poolBox(drawer, A10)), false));
  assert.equal((drawer.getByRole("button", { name: k.pools.save }) as HTMLButtonElement).disabled, true, "nothing chosen yet");
});

test("when the cluster is gone from the account, or the cloud cannot be read, the pools drawer says so", async () => {
  api.me = (async () => ({ role: "admin" })) as never;
  api.kubeClusters = (async () => list([entry({ pool_ids: ["np-a"] })])) as never;
  const u = user();
  api.providerClusters = (async () => list([])) as never;
  wrap(<KubernetesPage />);
  await u.click(within((await screen.findByText("ack-gpu")).closest("tr") as HTMLElement).getByRole("button", { name: k.actions.pools }));
  assert.ok(await screen.findByText(k.pools.clusterGone));
  assert.equal((screen.getByRole("button", { name: k.pools.save }) as HTMLButtonElement).disabled, true);
  cleanup();
  api.providerClusters = (async () => { throw new APIError("PROVIDER_FAILED", undefined, "aliyun: Forbidden.RAM", 424); }) as never;
  wrap(<KubernetesPage />);
  await u.click(within((await screen.findByText("ack-gpu")).closest("tr") as HTMLElement).getByRole("button", { name: k.actions.pools }));
  assert.ok(await screen.findByText(k.pools.loadFailed));
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

test("the pools drawer asks before taking pools away from nodes that still run instances: it lists them, cancel sends nothing, confirm resends with the confirmation", async () => {
  api.me = (async () => ({ role: "admin" })) as never;
  const held = entry({ pool_ids: ["np-a", "np-l"] });
  api.kubeClusters = (async () => list([held])) as never;
  api.providerClusters = (async () => list([{ ...multiCluster, external_id: "c-gpu" }])) as never;
  const calls: [string, string[], boolean | undefined][] = [];
  api.setKubeClusterPools = refusedPools(calls, held);
  const u = user();
  wrap(<KubernetesPage />);
  const row = (await screen.findByText("ack-gpu")).closest("tr") as HTMLElement;
  await u.click(within(row).getByRole("button", { name: k.actions.pools }));
  const drawer = within(await screen.findByRole("dialog"));
  await waitFor(() => assert.equal(checked(poolBox(drawer, L20)), true));
  await u.click(poolBox(drawer, L20));
  await u.click(drawer.getByRole("button", { name: k.pools.save }));

  await screen.findByText(k.selectionDrops.title);
  assert.ok(askDialog().getByText("gpu-a1"));
  assert.ok(askDialog().getByText(k.selectionDrops.deployments.replace("{{names}}", "chat")));
  assert.ok(askDialog().getByText(k.selectionDrops.intro), "confirming stops those instances first, then saves");
  assert.deepEqual(calls, [["e-5", ["np-a"], undefined]], "the first request carries no confirmation");

  await u.click(askDialog().getByRole("button", { name: k.selectionDrops.cancel }));
  await waitFor(() => assert.equal(screen.queryByText(k.selectionDrops.title), null));
  assert.equal(calls.length, 1, "cancelling sends nothing");
  assert.equal(drawer.queryByText(k.pools.saved.replace("{{name}}", "ack-gpu")), null, "and saves nothing");

  await u.click(drawer.getByRole("button", { name: k.pools.save }));
  await screen.findByText(k.selectionDrops.title);
  await u.click(askDialog().getByRole("button", { name: k.selectionDrops.confirm }));
  await waitFor(() => assert.equal(calls.length, 3));
  assert.deepEqual(calls[2], ["e-5", ["np-a"], true]);
  assert.ok(await drawer.findByText(k.pools.saved.replace("{{name}}", "ack-gpu")));
  // The instances the server asked to stop are listed beside the saved notice.
  assert.ok(drawer.getByText(i18n.getFixedT("en", "kubernetes")("stopped.title", { count: 1 })));
  assert.ok(drawer.getByText(k.stopped.item.replace("{{deployment}}", "chat").replace("{{instance}}", "i-1")));
});

test("pools of a cloud row taken away from nodes that still run instances are confirmed the same way, with the unverified caveat when no agent answered", async () => {
  const held: ProviderCluster = { ...multiCluster, entry: { uuid: "e-5", name: "ack-multi" }, gpu_pools: multiCluster.gpu_pools.map((q) => ({ ...q, in_entry: q.id !== "np-p" })) };
  api.providerClusters = (async () => list([held])) as never;
  const calls: [string, string[], boolean | undefined][] = [];
  api.setKubeClusterPools = refusedPools(calls, entry({ pool_ids: ["np-a"] }), { ...POOL_DROPS, verified: false });
  const u = user();
  wrap(<ProviderClustersTable providerId={3} providerName="ali-prod" />);
  const row = (await screen.findByText("c-multi")).closest("tr") as HTMLElement;
  await u.click(poolBox(row, L20));
  await u.click(within(row).getByRole("button", { name: p.clusters.savePools }));
  await screen.findByText(k.selectionDrops.title);
  assert.ok(askDialog().getByText(k.selectionDrops.unverified));
  assert.ok(askDialog().getByText("gpu-a1"));
  await u.click(askDialog().getByRole("button", { name: k.selectionDrops.cancel }));
  await waitFor(() => assert.equal(screen.queryByText(k.selectionDrops.title), null));
  assert.equal(calls.length, 1);
  await u.click(within(row).getByRole("button", { name: p.clusters.savePools }));
  await screen.findByText(k.selectionDrops.title);
  await u.click(askDialog().getByRole("button", { name: k.selectionDrops.confirm }));
  await waitFor(() => assert.equal(calls.length, 3));
  assert.deepEqual(calls[2], ["e-5", ["np-a"], true]);
  assert.ok(await screen.findByText(k.pools.saved.replace("{{name}}", "ack-gpu")));
  assert.ok(screen.getByText(k.stopped.item.replace("{{deployment}}", "chat").replace("{{instance}}", "i-1")));
});
