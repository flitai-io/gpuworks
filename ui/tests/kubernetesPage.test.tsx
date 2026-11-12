import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import { list } from "./helpers/listPage.ts";

const { i18n, locale } = await createI18n(["kubernetes", "providers"]);
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: KubernetesPage } = await import("../src/pages/KubernetesPage.tsx");
const { cardReturnText } = await import("../src/kubeClusters.ts");
import type { KubeCluster, KubeDefaults } from "../src/api";

// What GET /api/v1/kube/defaults answers: the drawer shows these as hints and sends only what was typed.
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


const k = locale.kubernetes;
const originals = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originals); });

const APPLY = "kubectl apply -f 'https://gpuworks.example/kube/manifest.yaml?token=secret-token'";
function cluster(overrides: Partial<KubeCluster> = {}): KubeCluster {
  return {
    id: 1, uuid: "c-1", name: "prod-gpu", namespace: "gpuworks-system", server_url: "https://gpuworks.example",
    vendors: [{ vendor: "nvidia", nodes: 3 }], state_dir: "/opt/gpuworks", agent_image: "", agent_image_source: "dockerhub", effective_agent_image: "gpuworks/gpuworks-agent:r1",
    node_selector: "nvidia.com/gpu.present=true", runtime_class: "", image_pull_secret: "",
    containerd_socket: "", api_server: "", engine_ports: "40000-42767", idle_return_seconds: 600, priority: 10000, allow_preemption: false,
    cluster_uid: "7f3c2a10-aaaa-bbbb-cccc-0123456789ab", revoked: false, node_count: 3,
    created_at: "2026-10-19T08:30:00Z", apply_command: APPLY, stalled: false, pool_ids: [], namespace_shared_with: [], can_apply: false, can_remove: false, operation_blocked: "no_source", ...overrides,
  };
}
const prod = cluster();
const lab = cluster({ id: 2, uuid: "c-2", name: "lab", namespace: "gpu-lab", cluster_uid: "", node_count: 0, vendors: [], created_at: new Date(Date.now() - 60_000).toISOString(), idle_return_seconds: -1, apply_command: "kubectl apply -f 'https://gpuworks.example/kube/manifest.yaml?token=lab-token'" });
const old = cluster({ id: 3, uuid: "c-3", name: "retired", revoked: true, idle_return_seconds: 7200, vendors: [{ vendor: "ascend", nodes: 2 }, { vendor: "", nodes: 1 }], runtime_class: "" });

type Calls = Record<string, unknown[][]>;
function stubApi(options: { clusters?: KubeCluster[] | Error; admin?: boolean; defaults?: KubeDefaults | Error } = {}) {
  const calls: Calls = {};
  const record = (name: string, result: unknown) => async (...args: unknown[]) => {
    (calls[name] ??= []).push(args);
    if (result instanceof Error) throw result;
    return typeof result === "function" ? (result as (...a: unknown[]) => unknown)(...args) : result;
  };
  const clusters = options.clusters ?? [prod, lab, old];
  api.me = record("me", { role: options.admin === false ? "viewer" : "admin" }) as never;
  api.kubeClusters = record("kubeClusters", clusters instanceof Error ? clusters : list(clusters.map((c) => options.admin === false ? { ...c, apply_command: undefined } : c))) as never;
  api.kubeDefaults = record("kubeDefaults", options.defaults ?? DEFAULTS) as never;
  api.kubeManifest = record("kubeManifest", "apiVersion: apps/v1\nkind: DaemonSet\n") as never;
  api.revokeKubeCluster = record("revokeKubeCluster", (uuid: string) => ({ ...prod, uuid, revoked: true })) as never;
  api.deleteKubeCluster = record("deleteKubeCluster", undefined) as never;
  api.createKubeCluster = record("createKubeCluster", (body: { name: string }) => cluster({ id: 9, uuid: "c-9", name: body.name, cluster_uid: "", node_count: 0 })) as never;
  return calls;
}

function renderPage() {
  const view = render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/kubernetes"]}><KubernetesPage /></MemoryRouter>
    </I18nextProvider>,
  );
  return { view, user: userEvent.setup({ document: dom.window.document }) };
}
const rowOf = async (name: string) => (await screen.findByText(name)).closest("tr") as HTMLElement;
// The confirmation is a popover beside the row's button; its buttons are found inside the popup.
const confirm = async (user: ReturnType<typeof userEvent.setup>, label: string) => {
  const ok = await waitFor(() => {
    const found = [...document.querySelectorAll('[data-slot="popover-content"]')]
      .flatMap((pop) => within(pop as HTMLElement).queryAllByRole("button", { name: new RegExp(`${label}$`) }));
    assert.ok(found.length > 0);
    return found.at(-1) as HTMLElement;
  });
  await user.click(ok);
};
const add = k.page.add;
const dialog = () => within(screen.getByRole("dialog"));
// A label's required mark ("Name *") is part of its text.
const field = (text: string) => new RegExp(`^${text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}( \\*)?$`);
// user-event does not set keyCode, which the dialogs read; the focused element receives the key.
const escape = () => fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape", code: "Escape", keyCode: 27 });
const noDialog = () => waitFor(() => assert.ok(!screen.queryByRole("dialog"), "the dialog closed"));

test("while loading it shows a spinner; an empty system invites connecting the first cluster; a failed load offers retry", async () => {
  let release: (rows: unknown) => void = () => {};
  stubApi();
  api.kubeClusters = (() => new Promise((resolve) => { release = resolve as never; })) as never;
  const first = renderPage();
  assert.ok(screen.getByRole("status"), "a spinner while the first load runs");
  release(list([]));
  assert.ok(await screen.findByText(k.page.empty));
  assert.ok(screen.getAllByRole("button", { name: new RegExp(`${add}$`) }).length >= 2, "header and empty state both offer connecting");
  first.view.unmount();

  const calls = stubApi({ clusters: new Error("boom") });
  const second = renderPage();
  assert.ok(await screen.findByText("boom"));
  const before = calls.kubeClusters.length;
  await second.user.click(screen.getByRole("button", { name: locale.common.actions.retry }));
  await waitFor(() => assert.ok(calls.kubeClusters.length > before));
});

test("the header explains how a cluster joins", async () => {
  stubApi();
  renderPage();
  assert.ok(await screen.findByText(k.page.description));
});

test("one row per cluster: node count, the vendors of its nodes, card return policy and status; namespace, cluster UID and creation time are off by default", async () => {
  stubApi();
  renderPage();
  const row = await rowOf("prod-gpu");
  assert.equal(screen.getAllByRole("row").length, 1 + 3, "header plus a row per cluster");
  assert.ok(within(row).getByText(`${locale.common.vendor.nvidia} ×3`), "the vendors of the cluster's nodes");
  assert.ok(within(await rowOf("retired")).getByText(`${locale.common.vendor.ascend} ×2 · ${k.vendors.none} ×1`), "nodes that reported no cards are counted too");
  assert.ok(within(await rowOf("lab")).getByText("—"), "no node, no vendor");
  assert.ok(within(row).getByText("3"));
  assert.ok(within(row).getByText("Returned after 10 min idle"));
  assert.ok(within(row).getByText(k.status.active));
  // The columns that rarely matter are a column-settings choice away, so the
  // rest fits the page without a sideways scroll.
  assert.ok(!within(row).queryByText("gpuworks-system"), "namespace is hidden by default");
  assert.ok(!within(row).queryByText("7f3c2a10-aaaa-bbbb-cccc-0123456789ab"), "cluster UID is hidden by default");
  assert.ok(!within(row).queryByText(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/), "creation time is hidden by default");

  const fresh = await rowOf("lab");
  assert.ok(within(fresh).getByText(k.clusterUid.notEnrolled), "a cluster no node joined yet says so in its status");
  assert.ok(within(fresh).getByText(k.returnPolicy.keep));
  assert.ok(within(await rowOf("retired")).getByText(k.status.revoked));
  assert.ok(within(await rowOf("retired")).getByText("Returned after 2 h idle"));
});

test("an entry the server says shares its namespace with another of maybe the same cluster carries a warning", async () => {
  stubApi({ clusters: [cluster({ namespace_shared_with: ["lab", "west"] }), lab] });
  renderPage();
  const row = await rowOf("prod-gpu");
  assert.ok(within(row).getByText(k.namespaceShared.badge));
  assert.ok(!within(await rowOf("lab")).queryByText(k.namespaceShared.badge), "only where the server names a clash");
});

test("the return policy reads in seconds, minutes or hours, whichever divides evenly", () => {
  const t = i18n.getFixedT("en", "kubernetes");
  assert.equal(cardReturnText(t, 45), "Returned after 45 s idle");
  assert.equal(cardReturnText(t, 90), "Returned after 90 s idle");
  assert.equal(cardReturnText(t, 600), "Returned after 10 min idle");
  assert.equal(cardReturnText(t, 3600), "Returned after 1 h idle");
  assert.equal(cardReturnText(t, 0), "Returned after 0 s idle");
  assert.equal(cardReturnText(t, -1), "Kept (never returned)");
});

test("a viewer sees the clusters without the connect button, the actions column or the token", async () => {
  stubApi({ admin: false });
  renderPage();
  await rowOf("prod-gpu");
  await waitFor(() => assert.ok(screen.queryByRole("button", { name: new RegExp(`${add}$`) }) === null));
  assert.ok(screen.queryByRole("button", { name: k.actions.copyCommand }) === null);
  assert.ok(screen.queryByRole("button", { name: k.actions.delete }) === null);
  assert.equal(document.body.textContent?.includes("secret-token"), false);
});

test("an admin copies a cluster's connect command from its row; a revoked cluster cannot", async () => {
  stubApi();
  const { user } = renderPage();
  const row = await rowOf("prod-gpu");
  await user.click(within(row).getByRole("button", { name: k.actions.copyCommand }));
  assert.equal(await navigator.clipboard.readText(), APPLY);
  const revoked = within(await rowOf("retired")).getByRole("button", { name: k.actions.copyCommand }) as HTMLButtonElement;
  assert.ok(revoked.disabled, "a revoked token connects nothing");
});

test("the manifest opens in a dialog with a copy button; a failed load says so", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click(within(await rowOf("prod-gpu")).getByRole("button", { name: k.actions.manifest }));
  const modal = within(await screen.findByRole("dialog"));
  assert.ok(await modal.findByText(/kind: DaemonSet/));
  assert.deepEqual(calls.kubeManifest, [["c-1"]]);
  await user.click(modal.getByRole("button", { name: k.manifest.copy }));
  assert.equal(await navigator.clipboard.readText(), "apiVersion: apps/v1\nkind: DaemonSet\n");
  await user.click(modal.getAllByRole("button", { name: k.manifest.close }).at(-1) as HTMLElement);
  await noDialog();

  api.kubeManifest = (async () => { throw new Error("manifest 404"); }) as never;
  await user.click(within(await rowOf("lab")).getByRole("button", { name: k.actions.manifest }));
  const failed = within(await screen.findByRole("dialog"));
  assert.ok(await failed.findByText(k.manifest.failed));
  assert.ok(failed.getByText("manifest 404"));
});

test("revoking asks first, calls the API for that cluster only on confirm and reloads the list", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  const row = await rowOf("prod-gpu");
  await user.click(within(row).getByRole("button", { name: k.actions.revoke }));
  await screen.findByText(k.actions.revokeConfirm.replace("{{name}}", "prod-gpu"));
  assert.ok(screen.getByText(k.actions.revokeHelp));
  assert.equal(calls.revokeKubeCluster, undefined, "asking is not revoking");
  const before = calls.kubeClusters.length;
  await confirm(user, k.actions.revoke);
  await waitFor(() => assert.deepEqual(calls.revokeKubeCluster, [["c-1"]]));
  await waitFor(() => assert.ok(calls.kubeClusters.length > before));
  const revokedRow = within(await rowOf("retired")).getByRole("button", { name: k.actions.revoke }) as HTMLButtonElement;
  assert.ok(revokedRow.disabled, "an already revoked cluster offers no second revoke");
});

test("cancelling the revoke confirmation calls nothing", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click(within(await rowOf("prod-gpu")).getByRole("button", { name: k.actions.revoke }));
  await confirm(user, k.actions.cancel);
  assert.equal(calls.revokeKubeCluster, undefined);
});

test("deleting asks first, calls the API on confirm and reloads", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click(within(await rowOf("lab")).getByRole("button", { name: k.actions.delete }));
  await screen.findByText(k.actions.deleteConfirm.replace("{{name}}", "lab"));
  assert.equal(calls.deleteKubeCluster, undefined);
  const before = calls.kubeClusters.length;
  await confirm(user, k.actions.delete);
  await waitFor(() => assert.deepEqual(calls.deleteKubeCluster, [["c-2"]]));
  await waitFor(() => assert.ok(calls.kubeClusters.length > before));
  assert.ok(screen.queryByText(k.actions.failed) === null);
});

test("a cluster that still has nodes cannot be deleted: the page shows the product error and the row stays", async () => {
  const calls = stubApi();
  api.deleteKubeCluster = (async (...args: unknown[]) => {
    (calls.deleteKubeCluster ??= []).push(args);
    throw new APIError("KUBE_CLUSTER_HAS_NODES", undefined, "cluster has nodes", 409);
  }) as never;
  const { user } = renderPage();
  await user.click(within(await rowOf("prod-gpu")).getByRole("button", { name: k.actions.delete }));
  await confirm(user, k.actions.delete);
  const alert = await screen.findByRole("alert");
  assert.ok(alert.textContent?.includes(k.actions.failed));
  assert.ok(alert.textContent?.includes(locale.common.apiError.KUBE_CLUSTER_HAS_NODES), alert.textContent ?? "");
  assert.ok(screen.getByText("prod-gpu"), "the cluster is still listed");
});

test("connecting opens a form with the server address prefilled and the server's defaults as hints in the advanced section, every field left empty", async () => {
  stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  assert.equal((await form.findByLabelText(field(k.drawer.serverUrl)) as HTMLInputElement).value, "http://localhost", "the origin of this page, stated explicitly");
  assert.ok(form.getByPlaceholderText("nvidia.com/gpu.present=true"));
  await user.click(form.getByText(k.drawer.advanced));
  const namespace = form.getByLabelText(field(k.drawer.namespace)) as HTMLInputElement;
  await waitFor(() => assert.equal(namespace.placeholder, "gpuworks-system", "the server's default, as a hint"));
  assert.equal(namespace.value, "", "left empty so the server applies its default");
  const runtimeClass = form.getByLabelText(field(k.drawer.runtimeClass)) as HTMLInputElement;
  assert.equal(runtimeClass.value, "", "empty: each node decides");
  assert.equal(runtimeClass.placeholder, k.drawer.runtimeClassPlaceholder);
  assert.equal((form.getByLabelText(field(k.drawer.imagePullSecret)) as HTMLInputElement).value, "");
  assert.ok(form.getByText(k.drawer.imagePullSecretHelp));
  const socket = form.getByLabelText(field(k.drawer.containerdSocket)) as HTMLInputElement;
  assert.equal(socket.value, "", "empty means the agents detect the socket themselves");
  assert.equal(socket.placeholder, k.drawer.containerdSocketPlaceholder);
  assert.equal((form.getByLabelText(field(k.drawer.stateDir)) as HTMLInputElement).placeholder, "/opt/gpuworks");
  assert.ok(form.getByText(k.drawer.stateDirHelp));
  assert.equal(form.queryByRole("combobox", { name: /vendor|厂商/i }), null, "no vendor is asked: each agent detects its own node's cards");
  const ports = form.getByLabelText(field(k.drawer.enginePorts)) as HTMLInputElement;
  assert.deepEqual([ports.value, ports.placeholder], ["", "40000-42767"]);
  const idle = form.getByLabelText(field(k.drawer.idleReturnSeconds)) as HTMLInputElement;
  assert.deepEqual([idle.value, idle.placeholder], ["", "60"]);
  assert.equal(form.getByRole("combobox", { name: field(k.drawer.agentImageSource) }).textContent, k.drawer.agentImageSources.dockerhub, "the server's default registry");
  assert.equal((form.getByLabelText(field(k.drawer.agentImage)) as HTMLInputElement).placeholder, "gpuworks/gpuworks-agent:r1", "the default registry's image");
  assert.ok(form.getByText(k.drawer.containerdSocketHelp), "the auto-detect hint");
  assert.ok(form.getByText(k.drawer.enginePortsHelp), "the NodePort hint");
});

test("a form without a name or with a server address that is no URL is refused before any request", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.click(await form.findByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(k.drawer.nameRequired));
  await user.type(form.getByLabelText(field(k.drawer.name)), "edge");
  await user.clear(form.getByLabelText(field(k.drawer.serverUrl)));
  await user.type(form.getByLabelText(field(k.drawer.serverUrl)), "gpuworks.example");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(k.drawer.serverUrlInvalid));
  assert.equal(calls.createKubeCluster, undefined);
});

test("submitting sends exactly what was typed, then presents the command to run in the cluster and reloads the list", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "edge");
  await user.type(form.getByPlaceholderText("nvidia.com/gpu.present=true"), "gpu=a100,zone=b");
  const before = calls.kubeClusters.length;
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "edge", server_url: "http://localhost", node_selector: "gpu=a100,zone=b" }], "what was not typed is left to the server");
  assert.ok(await dialog().findByText(k.created.note));
  assert.ok(dialog().getByText(k.created.title.replace("{{name}}", "edge")));
  await waitFor(() => assert.ok(calls.kubeClusters.length > before));
  await user.click(dialog().getByRole("button", { name: k.created.copyCommand }));
  assert.equal(await navigator.clipboard.readText(), APPLY);
  assert.ok(dialog().getByLabelText(k.created.command).textContent === APPLY, "the command is shown for reading too");
});

test("advanced choices travel: a named RuntimeClass, an image pull secret, a resident cluster is -1", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "k3s");
  await user.click(form.getByText(k.drawer.advanced));
  await user.type(form.getByLabelText(field(k.drawer.runtimeClass)), "nvidia");
  await user.type(form.getByLabelText(field(k.drawer.imagePullSecret)), " regcred ");
  await user.clear(form.getByLabelText(field(k.drawer.containerdSocket)));
  await user.type(form.getByLabelText(field(k.drawer.containerdSocket)), "/run/k3s/containerd/containerd.sock");
  await user.type(form.getByLabelText(field(k.drawer.apiServer)), "https://10.0.0.5:6443");
  const keep = form.getByRole("switch", { name: k.drawer.keepCards });
  keep.focus();
  await user.keyboard(" ");
  await waitFor(() => assert.equal(keep.getAttribute("aria-checked"), "true"));
  assert.ok((form.getByLabelText(field(k.drawer.idleReturnSeconds)) as HTMLInputElement).disabled, "no seconds to enter once cards are kept");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  const body = calls.createKubeCluster[0][0] as Record<string, unknown>;
  assert.equal(body.runtime_class, "nvidia");
  assert.equal(body.image_pull_secret, "regcred");
  assert.equal(body.idle_return_seconds, -1);
  assert.equal(body.containerd_socket, "/run/k3s/containerd/containerd.sock");
  assert.equal(body.api_server, "https://10.0.0.5:6443");
  assert.equal("node_selector" in body, false, "an empty selector is left to the server");
  assert.equal("agent_image" in body, false, "an empty image takes the default");
});

test("a refused cluster shows the reason at the field; a taken name shows at the name", async () => {
  stubApi();
  api.createKubeCluster = (async (body: { name: string }) => {
    if (body.name === "prod-gpu") throw new APIError("KUBE_CLUSTER_NAME_TAKEN", { name: "prod-gpu" }, "name taken", 409);
    throw new APIError("KUBE_CLUSTER_INVALID", { field: "engine_ports", reason: "overlaps the NodePort range" }, "invalid", 400);
  }) as never;
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "prod-gpu");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(locale.common.apiError.KUBE_CLUSTER_NAME_TAKEN.replace("{{name}}", "prod-gpu")));
  assert.ok(form.queryByText(k.created.note) === null, "nothing was created");

  await user.clear(form.getByLabelText(field(k.drawer.name)));
  await user.type(form.getByLabelText(field(k.drawer.name)), "other");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(locale.common.apiError.KUBE_CLUSTER_INVALID.replace("{{field}}", "engine_ports").replace("{{reason}}", "overlaps the NodePort range")));
});

test("the drawer works from the keyboard: the name is focused on open, Enter submits, Escape closes", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  const name = await form.findByLabelText(field(k.drawer.name));
  await waitFor(() => assert.ok(document.activeElement === name, "focus starts in the first field"));
  await user.keyboard("kbd{Enter}");
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.equal((calls.createKubeCluster[0][0] as { name: string }).name, "kbd");
  await dialog().findByText(k.created.note);
  escape();
  await noDialog();
});

test("Escape closes the form without sending anything", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  await dialog().findByLabelText(field(k.drawer.name));
  escape();
  await noDialog();
  assert.equal(calls.createKubeCluster, undefined);
});

test("the agent image registry offers what the server publishes to; choosing SWR shows its image and sends the choice", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "cn");
  await user.click(form.getByText(k.drawer.advanced));
  const registry = form.getByRole("combobox", { name: field(k.drawer.agentImageSource) });
  registry.focus();
  await user.keyboard("{Enter}");
  assert.deepEqual((await screen.findAllByRole("option")).map((option) => option.textContent),
    [k.drawer.agentImageSources.dockerhub, k.drawer.agentImageSources.swr, k.drawer.agentImageSources.quay]);
  await user.click(screen.getByRole("option", { name: k.drawer.agentImageSources.swr }));
  await waitFor(() => assert.equal((form.getByLabelText(field(k.drawer.agentImage)) as HTMLInputElement).placeholder,
    "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1"));
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "cn", server_url: "http://localhost", agent_image_source: "swr" }]);
});

test("priority and preemption show the server's defaults and are not sent untouched", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "pri");
  await user.click(form.getByText(k.drawer.advanced));
  const priority = form.getByLabelText(field(k.drawer.priority)) as HTMLInputElement;
  await waitFor(() => assert.equal(priority.placeholder, "10000", "the server's default, as a hint"));
  const preempt = form.getByRole("switch", { name: k.drawer.allowPreemptionSwitch });
  assert.equal(preempt.getAttribute("aria-checked"), "false", "off by default");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "pri", server_url: "http://localhost" }], "untouched fields take the server's defaults");
});

test("a priority and preemption the operator sets are sent", async () => {
  const calls = stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "pri2");
  await user.click(form.getByText(k.drawer.advanced));
  await user.type(form.getByLabelText(field(k.drawer.priority)), "0");
  const preempt = form.getByRole("switch", { name: k.drawer.allowPreemptionSwitch });
  await waitFor(() => assert.equal(preempt.hasAttribute("disabled"), false));
  await user.click(preempt);
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "pri2", server_url: "http://localhost", priority: 0, allow_preemption: true }]);
});

test("the RuntimeClass field is always offered and says it only affects NVIDIA nodes' instance pods", async () => {
  stubApi();
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await form.findByLabelText(field(k.drawer.name));
  await user.click(form.getByText(k.drawer.advanced));
  assert.ok(form.getByLabelText(field(k.drawer.runtimeClass)), "offered whatever the nodes' vendors");
  assert.ok(form.getByText(k.drawer.runtimeClassHelp));
  assert.ok(k.drawer.runtimeClassHelp.includes("NVIDIA"));
});

test("a cluster the server calls stalled offers the commands and causes to look at; one it does not, does not, whatever its age", async () => {
  const stalled = cluster({ id: 4, uuid: "c-4", name: "doks", namespace: "gpuworks-system", cluster_uid: "", node_count: 0, stalled: true });
  stubApi({ clusters: [stalled, lab] });
  const { user } = renderPage();
  assert.equal(within(await rowOf("lab")).queryByRole("button", { name: k.clusterUid.troubleshoot }), null, "the server did not call it stalled");
  await user.click(within(await rowOf("doks")).getByRole("button", { name: k.clusterUid.troubleshoot }));
  assert.ok(await screen.findByText(k.stalled.title));
  assert.ok(screen.getByText(k.stalled.intro));
  for (const key of ["pull", "runtimeClass", "selector", "connect"] as const) assert.ok(screen.getByText(k.stalled[key]));
  const commands = screen.getByLabelText(k.stalled.copy, { selector: "pre" }).textContent ?? "";
  assert.ok(commands.includes("kubectl -n gpuworks-system describe daemonset gpuworks-agent"), commands);
  await user.click(screen.getByRole("button", { name: k.stalled.copy }));
  assert.equal(await navigator.clipboard.readText(), commands);
});

test("the idle-return switch sends the server's own keep value", async () => {
  const calls = stubApi({ defaults: { ...DEFAULTS, idle_return_keep: -7 } });
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "keeper");
  await user.click(form.getByText(k.drawer.advanced));
  const keep = form.getByRole("switch", { name: k.drawer.keepCards });
  // The switch is enabled once the dictionary, which owns the keep value, has arrived.
  await waitFor(() => assert.equal((form.getByLabelText(field(k.drawer.namespace)) as HTMLInputElement).placeholder, "gpuworks-system"));
  keep.focus();
  await user.keyboard(" ");
  await waitFor(() => assert.equal(keep.getAttribute("aria-checked"), "true"));
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.equal((calls.createKubeCluster[0][0] as { idle_return_seconds: number }).idle_return_seconds, -7);
});

test("when the defaults cannot be loaded the form says so and still sends only what was typed", async () => {
  const calls = stubApi({ defaults: new Error("defaults down") });
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  assert.ok(await form.findByText(k.drawer.defaultsFailed));
  await user.type(form.getByLabelText(field(k.drawer.name)), "plain");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "plain", server_url: "http://localhost" }]);
});

test("applying and removing follow the server's can_apply and can_remove, with its reason", async () => {
  const source = { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-1" };
  const cloud = (overrides: Partial<KubeCluster>) => cluster({ source, can_apply: true, can_remove: true, operation_blocked: undefined, ...overrides });
  stubApi({
    clusters: [
      cloud({ id: 11, uuid: "u-ok", name: "ok" }),
      cloud({ id: 12, uuid: "u-private", name: "private", can_apply: false, can_remove: false, operation_blocked: "private_api" }),
      cloud({ id: 13, uuid: "u-revoked", name: "revoked-cloud", revoked: true, can_apply: false, can_remove: true, operation_blocked: "revoked" }),
      cluster({ id: 14, uuid: "u-hand", name: "by-hand" }),
    ],
  });
  const { user } = renderPage();
  const state = async (name: string, action: string) => (within(await rowOf(name)).queryByRole("button", { name: action }) as HTMLButtonElement | null)?.disabled;
  assert.equal(await state("ok", k.actions.apply), false);
  assert.equal(await state("ok", k.actions.remove), false);
  assert.equal(await state("private", k.actions.apply), true);
  assert.equal(await state("private", k.actions.remove), true);
  assert.equal(await state("revoked-cloud", k.actions.apply), true);
  assert.equal(await state("revoked-cloud", k.actions.remove), false);
  assert.equal(await state("by-hand", k.actions.apply), undefined, "no apply for an entry made by hand");

  await user.hover(within(await rowOf("private")).getByRole("button", { name: k.actions.apply }).parentElement as HTMLElement);
  assert.ok(await screen.findByText(k.blocked.private_api), "the reason is the server's code, written in this language");
});

test("a created cloud entry the server cannot apply to says why instead of offering to", async () => {
  stubApi();
  const source = { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-1" };
  api.createKubeCluster = (async (body: { name: string }) => cluster({ id: 9, uuid: "c-9", name: body.name, cluster_uid: "", node_count: 0, source, can_apply: false, can_remove: false, operation_blocked: "private_api" })) as never;
  const { user } = renderPage();
  await user.click((await screen.findAllByRole("button", { name: new RegExp(`${add}$`) }))[0]);
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "priv");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await dialog().findByText(k.created.applyBlocked));
  assert.ok(dialog().getByText(k.blocked.private_api));
  assert.equal(dialog().queryByRole("button", { name: k.created.applyNow }), null);
  assert.ok(dialog().getByLabelText(k.created.command), "the kubectl command is still there");
});
