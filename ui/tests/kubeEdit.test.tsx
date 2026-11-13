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
const { vendorsText } = await import("../src/kubeClusters.ts");
import type { KubeCluster, KubeClusterEdit, KubeClusterUpdate, KubeDefaults } from "../src/api";

const k = locale.kubernetes;
const originals = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originals); });

const DEFAULTS: KubeDefaults = {
  namespace: "gpuworks-system", state_dir: "/opt/gpuworks", engine_ports: "40000-42767", idle_return_seconds: 60, idle_return_keep: -1,
  agent_image: "gpuworks/gpuworks-agent:r1", stalled_after_seconds: 300, agent_image_source: "dockerhub",
  agent_image_sources: [{ id: "dockerhub", image: "gpuworks/gpuworks-agent:r1" }, { id: "swr", image: "swr.example/gpuworks-agent:r1" }],
  priority: 10000, priority_max: 1000000000, allow_preemption: false,
};
const SOURCE = { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-gpu" };
const REMOVE = ["kubectl delete daemonset gpuworks-agent -n gpuworks-system", "kubectl delete pods -n gpuworks-system -l app.kubernetes.io/part-of=gpuworks"];

function cluster(overrides: Partial<KubeCluster> = {}): KubeCluster {
  return {
    id: 1, uuid: "c-1", name: "prod-gpu", namespace: "gpuworks-system", server_url: "https://gpuworks.example",
    vendors: [{ vendor: "nvidia", nodes: 3 }, { vendor: "ascend", nodes: 1 }], state_dir: "/opt/gpuworks", agent_image: "", agent_image_source: "dockerhub",
    effective_agent_image: "gpuworks/gpuworks-agent:r1", node_selector: "gpu=a100", runtime_class: "", image_pull_secret: "", containerd_socket: "", api_server: "",
    engine_ports: "40000-42767", idle_return_seconds: 600, priority: 10000, allow_preemption: false, cluster_uid: "uid-1", revoked: false, node_count: 4,
    created_at: "2026-10-19T08:30:00Z", apply_command: "kubectl apply -f 'https://gpuworks.example/m.yaml'", remove_commands: REMOVE,
    stalled: false, pool_ids: [], namespace_shared_with: [], can_apply: false, can_remove: false, operation_blocked: "no_source", ...overrides,
  };
}
const cloud = (overrides: Partial<KubeCluster> = {}) => cluster({
  id: 2, uuid: "c-2", name: "ack-gpu", node_selector: "", source: SOURCE, pool_ids: ["np-a"], can_apply: true, can_remove: true, operation_blocked: undefined, remove_commands: undefined, ...overrides,
});

type Calls = Record<string, unknown[][]>;
function stubApi(clusters: KubeCluster[], options: { admin?: boolean; edit?: (uuid: string, body: KubeClusterUpdate) => KubeClusterEdit | Error } = {}) {
  const calls: Calls = {};
  const note = (name: string, args: unknown[]) => { (calls[name] ??= []).push(args); };
  api.me = (async () => ({ role: options.admin === false ? "viewer" : "admin" })) as never;
  api.kubeClusters = (async () => list(clusters)) as never;
  api.kubeDefaults = (async () => DEFAULTS) as never;
  api.updateKubeCluster = (async (uuid: string, body: KubeClusterUpdate) => {
    note("updateKubeCluster", [uuid, body]);
    const result = options.edit ? options.edit(uuid, body) : { cluster: clusters.find((c) => c.uuid === uuid)!, changed: Object.keys(body).sort(), reapply: false, commands: [] };
    if (result instanceof Error) throw result;
    return result;
  }) as never;
  api.kubeClusterOperation = (async (uuid: string, op: string, dryRun: boolean) => {
    note("kubeClusterOperation", [uuid, op, dryRun]);
    return {
      dry_run: dryRun,
      results: [
        { kind: "PriorityClass", name: "gpuworks-inference-gpuworks-system", action: "replace" },
        { kind: "DaemonSet", name: "gpuworks-agent", namespace: "gpuworks-system", action: "update" },
      ],
    };
  }) as never;
  api.deleteKubeCluster = (async (...args: unknown[]) => { note("deleteKubeCluster", args); }) as never;
  return calls;
}

function renderPage() {
  render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter initialEntries={["/kubernetes"]}><KubernetesPage /></MemoryRouter>
    </I18nextProvider>,
  );
  return userEvent.setup({ document: dom.window.document });
}
const rowOf = async (name: string) => (await screen.findByText(name)).closest("tr") as HTMLElement;
const dialog = () => within(screen.getByRole("dialog"));
const field = (text: string) => new RegExp(`^${text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}( \\*)?$`);
const escape = () => fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape", code: "Escape", keyCode: 27 });
const noDialog = () => waitFor(() => assert.ok(!screen.queryByRole("dialog"), "the dialog closed"));
async function openEdit(user: ReturnType<typeof userEvent.setup>, name: string) {
  await user.click(within(await rowOf(name)).getByRole("button", { name: k.actions.edit }));
  const form = dialog();
  await form.findByDisplayValue(name);
  return form;
}
// The confirmation is a popover beside the row's button.
const popover = () => within([...document.querySelectorAll('[data-slot="popover-content"]')].at(-1) as HTMLElement);

test("the vendors of a cluster's nodes read most first with their counts; no accelerator and no node have their own words", () => {
  const t = i18n.getFixedT("en", "kubernetes");
  const tCommon = i18n.getFixedT("en", "common") as (key: string) => string;
  assert.equal(vendorsText(t, tCommon, [{ vendor: "nvidia", nodes: 3 }, { vendor: "ascend", nodes: 1 }, { vendor: "", nodes: 2 }]), `${locale.common.vendor.nvidia} ×3 · ${locale.common.vendor.ascend} ×1 · ${k.vendors.none} ×2`);
  assert.equal(vendorsText(t, tCommon, [{ vendor: "somenew", nodes: 1 }]), "somenew ×1", "a vendor this UI does not know keeps the server's name");
  assert.equal(vendorsText(t, tCommon, []), "—");
  assert.equal(vendorsText(t, tCommon, undefined), "—");
});

test("the list shows the vendors column from the server's counts", async () => {
  stubApi([cluster(), cluster({ id: 3, uuid: "c-3", name: "empty", node_count: 0, vendors: [] })]);
  renderPage();
  assert.ok(within(await rowOf("prod-gpu")).getByText(`${locale.common.vendor.nvidia} ×3 · ${locale.common.vendor.ascend} ×1`));
  assert.ok(within(await rowOf("empty")).getByText("—"));
});

test("editing opens the entry's values; the namespace, server address and state directory are fixed", async () => {
  stubApi([cluster({ runtime_class: "nvidia", image_pull_secret: "regcred" })]);
  const user = renderPage();
  const form = await openEdit(user, "prod-gpu");
  assert.ok(screen.getByText(k.drawer.editTitle));
  const server = form.getByLabelText(field(k.drawer.serverUrl)) as HTMLInputElement;
  assert.deepEqual([server.value, server.disabled], ["https://gpuworks.example", true]);
  assert.equal((form.getByLabelText(field(k.drawer.nodeSelector)) as HTMLInputElement).value, "gpu=a100");
  assert.equal((form.getByLabelText(field(k.drawer.nodeSelector)) as HTMLInputElement).disabled, false, "an entry made by hand selects by node selector");
  await user.click(form.getByText(k.drawer.advanced));
  const namespace = form.getByLabelText(field(k.drawer.namespace)) as HTMLInputElement;
  const stateDir = form.getByLabelText(field(k.drawer.stateDir)) as HTMLInputElement;
  assert.deepEqual([namespace.value, namespace.disabled, stateDir.value, stateDir.disabled], ["gpuworks-system", true, "/opt/gpuworks", true]);
  assert.ok(form.getAllByText(k.drawer.fixedAfterCreate).length >= 3);
  assert.equal((form.getByLabelText(field(k.drawer.runtimeClass)) as HTMLInputElement).value, "nvidia");
  assert.equal((form.getByLabelText(field(k.drawer.imagePullSecret)) as HTMLInputElement).value, "regcred");
  assert.equal((form.getByLabelText(field(k.drawer.idleReturnSeconds)) as HTMLInputElement).value, "600");
  assert.ok(form.getByRole("button", { name: k.drawer.save }));
  assert.equal(form.queryByRole("button", { name: k.drawer.submit }), null);
});

test("saving sends only the changed fields; when the cluster needs nothing applied the drawer simply closes", async () => {
  const calls = stubApi([cluster()]);
  const user = renderPage();
  const form = await openEdit(user, "prod-gpu");
  await user.clear(form.getByLabelText(field(k.drawer.name)));
  await user.type(form.getByLabelText(field(k.drawer.name)), "prod-gpu-2");
  await user.clear(form.getByLabelText(field(k.drawer.nodeSelector)));
  await user.type(form.getByLabelText(field(k.drawer.nodeSelector)), "gpu=h100");
  await user.click(form.getByText(k.drawer.advanced));
  await user.type(form.getByLabelText(field(k.drawer.runtimeClass)), " nvidia ");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  await waitFor(() => assert.equal(calls.updateKubeCluster?.length, 1));
  assert.deepEqual(calls.updateKubeCluster[0], ["c-1", { name: "prod-gpu-2", node_selector: "gpu=h100", runtime_class: "nvidia" }],
    "nothing else is sent, least of all the namespace, server address or state directory");
  await noDialog();
  assert.ok(screen.queryByText(k.edit.savedTitle.replace("{{name}}", "prod-gpu")) === null, "no apply step when nothing needs applying");
});

test("a cleared field is sent empty so the server clears it, a retyped number travels as a number, and an untouched priority is not sent", async () => {
  const calls = stubApi([cluster({ image_pull_secret: "regcred", containerd_socket: "/run/k3s/containerd/containerd.sock", priority: 500 })]);
  const user = renderPage();
  const form = await openEdit(user, "prod-gpu");
  await user.click(form.getByText(k.drawer.advanced));
  await user.clear(form.getByLabelText(field(k.drawer.imagePullSecret)));
  await user.clear(form.getByLabelText(field(k.drawer.containerdSocket)));
  await user.clear(form.getByLabelText(field(k.drawer.idleReturnSeconds)));
  await user.type(form.getByLabelText(field(k.drawer.idleReturnSeconds)), "120");
  const priority = form.getByLabelText(field(k.drawer.priority)) as HTMLInputElement;
  assert.equal(priority.value, "500");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  await waitFor(() => assert.equal(calls.updateKubeCluster?.length, 1));
  assert.deepEqual(calls.updateKubeCluster[0], ["c-1", { image_pull_secret: "", containerd_socket: "", idle_return_seconds: 120 }]);
});

test("saving without a change sends nothing and closes", async () => {
  const calls = stubApi([cluster()]);
  const user = renderPage();
  const form = await openEdit(user, "prod-gpu");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  await noDialog();
  assert.equal(calls.updateKubeCluster, undefined);
});

test("an edit that needs applying on a cluster gpuworks cannot reach shows the commands in order, each copyable", async () => {
  const commands = ["kubectl delete priorityclass gpuworks-inference-gpuworks-system", "kubectl apply -f 'https://gpuworks.example/m.yaml'"];
  const entry = cluster();
  stubApi([entry], { edit: () => ({ cluster: { ...entry, priority: 20000 }, changed: ["priority"], reapply: true, commands }) });
  const user = renderPage();
  const form = await openEdit(user, "prod-gpu");
  await user.click(form.getByText(k.drawer.advanced));
  await user.clear(form.getByLabelText(field(k.drawer.priority)));
  await user.type(form.getByLabelText(field(k.drawer.priority)), "20000");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  const saved = dialog();
  assert.ok(await saved.findByText(k.edit.savedTitle.replace("{{name}}", "prod-gpu")));
  assert.ok(saved.getByText(k.edit.savedHelp));
  assert.ok(saved.getByText(k.edit.commandsTitle));
  assert.equal(saved.getByLabelText("Command 1").textContent, commands[0]);
  assert.equal(saved.getByLabelText("Command 2").textContent, commands[1]);
  assert.equal(saved.queryByRole("button", { name: k.edit.applyNow }), null, "the server cannot apply it");
  await user.click(saved.getByRole("button", { name: "Copy command 1" }));
  assert.equal(await navigator.clipboard.readText(), commands[0]);
  await user.click(saved.getByRole("button", { name: k.commands.copyAll }));
  assert.equal(await navigator.clipboard.readText(), commands.join("\n"));
  await user.click(saved.getAllByRole("button", { name: k.drawer.close }).at(-1) as HTMLElement);
  await noDialog();
});

test("an edit that needs applying on a cluster gpuworks can reach offers the checked apply flow", async () => {
  const entry = cloud();
  const calls = stubApi([entry], { edit: () => ({ cluster: entry, changed: ["engine_ports"], reapply: true, commands: [] }) });
  const user = renderPage();
  const form = await openEdit(user, "ack-gpu");
  await user.click(form.getByText(k.drawer.advanced));
  await user.clear(form.getByLabelText(field(k.drawer.enginePorts)));
  await user.type(form.getByLabelText(field(k.drawer.enginePorts)), "41000-41999");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  const apply = await screen.findByRole("button", { name: k.edit.applyNow });
  assert.equal(calls.kubeClusterOperation, undefined, "saving applies nothing");
  assert.equal(screen.queryByText(k.edit.commandsTitle), null);
  await user.click(apply);
  assert.ok(await screen.findByText(k.operation.checkedApply));
  assert.deepEqual(calls.kubeClusterOperation, [["c-2", "apply", true]], "the dry run comes first");
  // A PriorityClass that had to be deleted and created again is told apart from a plain update.
  assert.ok(screen.getByText(k.operation.plan.replace));
  assert.ok(screen.getByText(k.operation.plan.update));
});

test("the plan and the result name a replaced PriorityClass", async () => {
  stubApi([cloud()]);
  const user = renderPage();
  await user.click(within(await rowOf("ack-gpu")).getByRole("button", { name: k.actions.apply }));
  assert.ok(await screen.findByText(k.operation.plan.replace));
  await user.click(screen.getByRole("button", { name: k.operation.confirmApply }));
  assert.ok(await screen.findByText(k.operation.done.replace));
});

test("a cloud entry's node selector is not editable and points to the node pools; it is never sent", async () => {
  const calls = stubApi([cloud()]);
  const user = renderPage();
  const form = await openEdit(user, "ack-gpu");
  const selector = form.getByLabelText(field(k.drawer.nodeSelector)) as HTMLInputElement;
  assert.equal(selector.disabled, true);
  assert.ok(form.getByText(k.drawer.nodeSelectorCloud));
  await user.clear(form.getByLabelText(field(k.drawer.name)));
  await user.type(form.getByLabelText(field(k.drawer.name)), "ack-renamed");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  await waitFor(() => assert.equal(calls.updateKubeCluster?.length, 1));
  assert.deepEqual(calls.updateKubeCluster[0], ["c-2", { name: "ack-renamed" }]);
});

test("a name another entry has, or a field the server refuses, is shown at the field and the drawer stays", async () => {
  const entry = cluster();
  let refusal: Error = new APIError("KUBE_CLUSTER_NAME_TAKEN", { name: "lab" }, "taken", 409);
  stubApi([entry], { edit: () => refusal });
  const user = renderPage();
  const form = await openEdit(user, "prod-gpu");
  await user.clear(form.getByLabelText(field(k.drawer.name)));
  await user.type(form.getByLabelText(field(k.drawer.name)), "lab");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  assert.ok(await form.findByText(locale.common.apiError.KUBE_CLUSTER_NAME_TAKEN.replace("{{name}}", "lab")));
  refusal = new APIError("KUBE_CLUSTER_INVALID", { field: "engine_ports", reason: "overlaps the NodePort range" }, "invalid", 400);
  await user.clear(form.getByLabelText(field(k.drawer.name)));
  await user.type(form.getByLabelText(field(k.drawer.name)), "lab2");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  assert.ok(await form.findByText(locale.common.apiError.KUBE_CLUSTER_INVALID.replace("{{field}}", "engine_ports").replace("{{reason}}", "overlaps the NodePort range")));
  assert.ok(screen.getByRole("dialog"), "still open");
  escape();
  await noDialog();
});

test("a viewer has no edit or uninstall action", async () => {
  stubApi([cluster()], { admin: false });
  renderPage();
  await rowOf("prod-gpu");
  assert.equal(screen.queryByRole("button", { name: k.actions.edit }), null);
  assert.equal(screen.queryByRole("button", { name: k.actions.uninstall }), null);
});

test("uninstall lists the commands that remove the agents and the pods they started, and what follows", async () => {
  const calls = stubApi([cluster(), cloud()]);
  const user = renderPage();
  assert.equal(within(await rowOf("ack-gpu")).queryByRole("button", { name: k.actions.uninstall }), null, "an entry the server removes itself keeps the remove flow");
  await user.click(within(await rowOf("prod-gpu")).getByRole("button", { name: k.actions.uninstall }));
  const modal = dialog();
  assert.ok(await modal.findByText(k.uninstall.title.replace("{{name}}", "prod-gpu")));
  assert.ok(modal.getByText(k.uninstall.intro));
  assert.ok(modal.getByText(k.uninstall.next));
  assert.equal(modal.getByLabelText("Command 1").textContent, REMOVE[0]);
  assert.equal(modal.getByLabelText("Command 2").textContent, REMOVE[1]);
  await user.click(modal.getByRole("button", { name: "Copy command 2" }));
  assert.equal(await navigator.clipboard.readText(), REMOVE[1]);
  assert.equal(calls.kubeClusterOperation, undefined, "nothing is sent from the modal");
  await user.click(modal.getAllByRole("button", { name: k.uninstall.close }).at(-1) as HTMLElement);
  await noDialog();
});

test("an entry without remove commands (or whose removal the server does) shows no uninstall", async () => {
  stubApi([cluster({ remove_commands: undefined })]);
  renderPage();
  assert.equal(within(await rowOf("prod-gpu")).queryByRole("button", { name: k.actions.uninstall }), null);
});

test("a revoked entry can still be uninstalled by hand", async () => {
  stubApi([cluster({ revoked: true })]);
  const user = renderPage();
  const button = within(await rowOf("prod-gpu")).getByRole("button", { name: k.actions.uninstall }) as HTMLButtonElement;
  assert.equal(button.disabled, false);
  await user.click(button);
  assert.ok(await dialog().findByLabelText("Command 1"));
});

test("deleting an entry that has nodes says to uninstall first and offers it; one without nodes just confirms", async () => {
  stubApi([cluster(), cluster({ id: 3, uuid: "c-3", name: "empty", node_count: 0, vendors: [], remove_commands: undefined })]);
  const user = renderPage();
  await user.click(within(await rowOf("prod-gpu")).getByRole("button", { name: k.actions.delete }));
  const confirm = await waitFor(() => { const p = [...document.querySelectorAll('[data-slot="popover-content"]')].at(-1); assert.ok(p); return within(p as HTMLElement); });
  assert.ok(await confirm.findByText(k.actions.deleteHasNodes_other.replace("{{count}}", "4")));
  await user.click(confirm.getByRole("button", { name: k.actions.uninstall }));
  assert.ok(await screen.findByLabelText("Command 1"), "the uninstall commands open");
  await user.click(dialog().getAllByRole("button", { name: k.uninstall.close }).at(-1) as HTMLElement);
  await noDialog();

  await user.click(within(await rowOf("empty")).getByRole("button", { name: k.actions.delete }));
  await waitFor(() => assert.ok(popover().getByText(k.actions.deleteHelp)));
  assert.equal(popover().queryByText(/cannot be deleted yet/), null);
});

test("deleting an entry with nodes that the server can remove offers its remove flow", async () => {
  stubApi([cloud({ node_count: 1, vendors: [{ vendor: "nvidia", nodes: 1 }] })]);
  const user = renderPage();
  await user.click(within(await rowOf("ack-gpu")).getByRole("button", { name: k.actions.delete }));
  await waitFor(() => assert.ok(popover().getByText(k.actions.deleteHasNodes_one.replace("{{count}}", "1"))));
  await user.click(popover().getByRole("button", { name: k.actions.remove }));
  assert.ok(await screen.findByText(k.operation.removeTitle.replace("{{name}}", "ack-gpu")));
});

// A node selector that leaves out nodes still running instances is refused by the server until confirmed.
const DROPS = {
  nodes: [
    { node_uuid: "n-1", node_name: "gpu-a1", kube_node: "ip-10-0-0-11", deployments: ["chat", "embed"] },
    { node_uuid: "n-2", node_name: "gpu-a2", kube_node: "gpu-a2", deployments: [] },
  ],
  verified: true,
};
const refusedUnlessConfirmed = (drops: unknown) => (_uuid: string, body: KubeClusterUpdate): KubeClusterEdit | Error => {
  if (body.confirm_drop_instances) return { cluster: cluster({ node_selector: body.node_selector }), changed: ["node_selector"], reapply: false, commands: [] };
  return new APIError("KUBE_SELECTION_DROPS_INSTANCES", drops as Record<string, unknown>, "drops instances", 409);
};
const confirmDialog = () => within(screen.getByText(k.selectionDrops.title).closest('[role="dialog"]') as HTMLElement);

test("a node selector that drops nodes still running instances lists them and their deployments; cancelling sends nothing more, confirming resends with the confirmation", async () => {
  const calls = stubApi([cluster()], { edit: refusedUnlessConfirmed(DROPS) });
  const user = renderPage();
  const form = await openEdit(user, "prod-gpu");
  await user.clear(form.getByLabelText(field(k.drawer.nodeSelector)));
  await user.type(form.getByLabelText(field(k.drawer.nodeSelector)), "gpu=h100");
  await user.click(form.getByRole("button", { name: k.drawer.save }));

  await screen.findByText(k.selectionDrops.title);
  const ask = confirmDialog();
  assert.ok(ask.getByText("gpu-a1"));
  assert.ok(ask.getByText("(ip-10-0-0-11)"), "the Kubernetes name is shown when it differs");
  assert.ok(ask.getByText(k.selectionDrops.deployments.replace("{{names}}", "chat, embed")));
  assert.ok(ask.getByText("gpu-a2"));
  assert.ok(ask.getByText(k.selectionDrops.noDeployments));
  assert.ok(ask.getByText(k.selectionDrops.later));
  assert.equal(ask.queryByText(k.selectionDrops.unverified), null, "a verified answer carries no caveat");
  assert.deepEqual(calls.updateKubeCluster, [["c-1", { node_selector: "gpu=h100" }]], "the first request carries no confirmation");

  await user.click(ask.getByRole("button", { name: k.selectionDrops.cancel }));
  await waitFor(() => assert.equal(screen.queryByText(k.selectionDrops.title), null));
  assert.equal(calls.updateKubeCluster.length, 1, "cancelling sends nothing");
  assert.ok(form.getByRole("button", { name: k.drawer.save }), "the form stays for another try");
  assert.equal(screen.queryByText(locale.common.apiError.KUBE_SELECTION_DROPS_INSTANCES), null, "the dialog is the message, not a banner");

  await user.click(form.getByRole("button", { name: k.drawer.save }));
  await screen.findByText(k.selectionDrops.title);
  await user.click(confirmDialog().getByRole("button", { name: k.selectionDrops.confirm }));
  await waitFor(() => assert.equal(calls.updateKubeCluster.length, 3));
  assert.deepEqual(calls.updateKubeCluster[2], ["c-1", { node_selector: "gpu=h100", confirm_drop_instances: true }]);
  await noDialog();
});

test("when no agent could be asked the dialog says the nodes may be affected", async () => {
  stubApi([cluster()], { edit: refusedUnlessConfirmed({ ...DROPS, verified: false }) });
  const user = renderPage();
  const form = await openEdit(user, "prod-gpu");
  await user.clear(form.getByLabelText(field(k.drawer.nodeSelector)));
  await user.type(form.getByLabelText(field(k.drawer.nodeSelector)), "gpu=h100");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  assert.ok(await screen.findByText(k.selectionDrops.unverified));
  assert.ok(confirmDialog().getByText("gpu-a1"));
});

test("another refusal of an edit is still a message in the form, not the confirmation", async () => {
  stubApi([cluster()], { edit: () => new APIError("KUBE_CLUSTER_NAME_TAKEN", { name: "taken" }, "taken", 409) });
  const user = renderPage();
  const form = await openEdit(user, "prod-gpu");
  await user.type(form.getByLabelText(field(k.drawer.name)), "x");
  await user.click(form.getByRole("button", { name: k.drawer.save }));
  assert.ok(await form.findByText(locale.common.apiError.KUBE_CLUSTER_NAME_TAKEN.replace("{{name}}", "taken")));
  assert.equal(screen.queryByText(k.selectionDrops.title), null);
});
