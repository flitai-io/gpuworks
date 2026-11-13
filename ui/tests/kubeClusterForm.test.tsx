// The form that connects a Kubernetes cluster (ui/src/pages/kubernetes/useKubeClusterForm.tsx,
// its rules in ui/src/pages/kubernetes/kubeClusterForm.ts),
// where the console shows it for a cluster connected on its own: in the Add
// External Platform dialog with Kubernetes chosen (ui/src/components/ProviderConfigModal.tsx).
// What it shows when it opens, what it refuses, what it sends, and the command
// it presents once the cluster exists. The dialog's platform choice is in
// tests/providerConfigModalConnects.test.tsx; a cloud account's cluster list
// opens the same form prefilled, in a drawer (tests/kubeCloudClusters.test.tsx);
// editing an entry in it is in tests/kubeEdit.test.tsx. A cluster has no vendor
// to choose: each node's agent reports the vendor of its own cards.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import { describedBy, field, footerButtons as buttonsIn, freshPopupFramesPerTest } from "./helpers/platformDialog.ts";
import { APPLY_COMMAND as APPLY, kubeEntry as cluster } from "./helpers/platformFixtures.ts";
import { poolPicker } from "./helpers/poolPicker.ts";

const { i18n, locale } = await createI18n(["kubernetes", "providers"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { default: KubeClusterDrawer } = await import("../src/pages/kubernetes/KubeClusterDrawer.tsx");
import type { KubeCluster, KubeDefaults } from "../src/api";
import type { KubeClusterPrefill } from "../src/pages/kubernetes/kubeClusterForm";

// What GET /api/v1/kube/defaults answers: the form shows these as hints and sends only what was typed.
const DEFAULTS: KubeDefaults = {
  namespace: "gpuworks-system", state_dir: "/opt/gpuworks", engine_ports: "40000-42767", idle_return_seconds: 60, idle_return_keep: -1,
  agent_image: "gpuworks/gpuworks-agent:v1", stalled_after_seconds: 300, agent_image_source: "dockerhub",
  agent_image_sources: [
    { id: "dockerhub", image: "gpuworks/gpuworks-agent:v1" },
    { id: "swr", image: "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:v1" },
    { id: "quay", image: "quay.io/gpuworks/gpuworks-agent:v1" },
  ],
  priority: 10000, priority_max: 1000000000, allow_preemption: false,
};

const k = locale.kubernetes;
const originals = { ...api };
await freshPopupFramesPerTest();
afterEach(() => { cleanup(); Object.assign(api, originals); });

type Calls = Record<string, unknown[][]>;
/** The two endpoints the form reads and writes; either can be made to fail. */
function stubApi(options: { defaults?: KubeDefaults | Error } = {}) {
  const calls: Calls = {};
  const record = (name: string, result: unknown) => async (...args: unknown[]) => {
    (calls[name] ??= []).push(args);
    if (result instanceof Error) throw result;
    return typeof result === "function" ? (result as (...a: unknown[]) => unknown)(...args) : result;
  };
  api.kubeDefaults = record("kubeDefaults", options.defaults ?? DEFAULTS) as never;
  // A cluster just made has no node yet.
  api.createKubeCluster = record("createKubeCluster", (body: { name: string }) => cluster({ id: 9, uuid: "c-9", name: body.name, cluster_uid: "", node_count: 0 })) as never;
  return calls;
}

/**
 * The dialog as the External Platforms page holds it for an administrator who
 * picked the Kubernetes card: open on Kubernetes until it asks to be closed.
 */
function Harness({ seen, language }: { seen: { closed: number; created: number; saved: number }; language: typeof i18n }) {
  const [open, setOpen] = React.useState(true);
  return (
    <I18nextProvider i18n={language}>
      <ProviderConfigModal open={open} defaultKind="kubernetes" kindInfos={[]} providerLabel={language.t("platforms.label", { ns: "providers" })} canConnectCluster
        onClose={() => { seen.closed += 1; setOpen(false); }} onSaved={() => { seen.saved += 1; }} onClusterCreated={() => { seen.created += 1; }} />
    </I18nextProvider>
  );
}
function openForm(language = i18n) {
  const seen = { closed: 0, created: 0, saved: 0 };
  render(<Harness seen={seen} language={language} />);
  return { seen, user: userEvent.setup({ document: dom.window.document }) };
}
const dialog = () => within(screen.getByRole("dialog"));
// The focused element receives the key, as from a keyboard.
const escape = () => fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape", code: "Escape", keyCode: 27 });
/** The names of the buttons in the dialog's footer, left to right. */
const footerButtons = () => buttonsIn(screen.getByRole("dialog"));
/** The mark in the dialog's corner and the footer's button are both named Close; the footer's is the one named by its text. */
const closeMark = () => dialog().getAllByRole("button", { name: k.drawer.close }).find((button) => button.hasAttribute("aria-label")) as HTMLButtonElement;
const closeButton = () => dialog().getAllByRole("button", { name: k.drawer.close }).find((button) => !button.hasAttribute("aria-label")) as HTMLButtonElement;
const noDialog = () => waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0, "the dialog closed"));

test("the form opens with the management node address prefilled and the server's defaults as hints in the advanced section, every field left empty", async () => {
  stubApi();
  const { user } = openForm();
  const form = dialog();
  // The address the agents connect to, named for what it is: this console's own origin, stated explicitly.
  const address = await form.findByLabelText(field("Management node address")) as HTMLInputElement;
  assert.equal(address.value, "http://localhost");
  assert.ok(describedBy(address).includes("This is not the Kubernetes API server's address."), "the address is not to be mistaken for the cluster's");
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
  assert.equal(form.queryAllByText(/^Vendor/).length, 0, "no vendor to choose: each node reports its own");
  const ports = form.getByLabelText(field(k.drawer.enginePorts)) as HTMLInputElement;
  assert.deepEqual([ports.value, ports.placeholder], ["", "40000-42767"]);
  const idle = form.getByLabelText(field(k.drawer.idleReturnSeconds)) as HTMLInputElement;
  assert.deepEqual([idle.value, idle.placeholder], ["", "60"]);
  assert.equal(form.getByRole("combobox", { name: field(k.drawer.agentImageSource) }).textContent, k.drawer.agentImageSources.dockerhub, "the server's default registry");
  assert.equal((form.getByLabelText(field(k.drawer.agentImage)) as HTMLInputElement).placeholder, "gpuworks/gpuworks-agent:v1");
  assert.ok(form.getByText(k.drawer.containerdSocketHelp), "the auto-detect hint");
  assert.ok(form.getByText(k.drawer.enginePortsHelp), "the NodePort hint");
});

test("a form without a name, without a management node address or with one that is no URL is refused before any request", async () => {
  const calls = stubApi();
  const { seen, user } = openForm();
  const form = dialog();
  await user.click(await form.findByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(k.drawer.nameRequired));
  await user.type(form.getByLabelText(field(k.drawer.name)), "edge");
  await user.clear(form.getByLabelText(field(k.drawer.serverUrl)));
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText("Enter the management node address"));
  await user.type(form.getByLabelText(field(k.drawer.serverUrl)), "gpuworks.example");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(k.drawer.serverUrlInvalid));
  assert.equal(calls.createKubeCluster, undefined);
  assert.equal(seen.created, 0, "nothing was created, so nothing is announced");
});

test("submitting sends exactly what was typed and tells its owner once; the dialog stays, showing only the command to run in the cluster, until it is closed", async () => {
  const calls = stubApi();
  const { seen, user } = openForm();
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "edge");
  await user.type(form.getByPlaceholderText("nvidia.com/gpu.present=true"), "gpu=a100,zone=b");
  assert.equal(seen.created, 0, "typing creates nothing");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "edge", server_url: "http://localhost", node_selector: "gpu=a100,zone=b" }], "what was not typed is left to the server");
  assert.ok(await dialog().findByText(k.created.note));
  assert.ok(dialog().getByText(k.created.title.replace("{{name}}", "edge")));
  assert.deepEqual(seen, { created: 1, closed: 0, saved: 0 }, "the owner reloads its cluster list as soon as the cluster exists; the dialog is neither closed nor an account saved");
  await user.click(dialog().getByRole("button", { name: k.created.copyCommand }));
  assert.equal(await navigator.clipboard.readText(), APPLY);
  assert.ok(dialog().getByLabelText(k.created.command).textContent === APPLY, "the command is shown for reading too");
  assert.ok(dialog().getByText(k.created.secret), "the command carries the token");
  // Only what to run next is left: no platform to choose, nothing it connects, no field, nothing more to submit.
  assert.equal(dialog().queryAllByRole("tablist").length + dialog().queryAllByRole("tab").length, 0);
  assert.equal(dialog().queryAllByRole("combobox").length, 0);
  assert.equal(dialog().queryAllByRole("list").length, 0);
  assert.equal(dialog().queryAllByRole("textbox").length, 0);
  assert.deepEqual(footerButtons(), [k.drawer.close]);
  await user.click(closeButton());
  await noDialog();
  assert.equal(seen.closed, 1);
});

test("while the cluster is being created the dialog cannot be dismissed: not with Escape, its close mark, Cancel or a click outside; once it is, it can", async () => {
  stubApi();
  let answer!: (cluster: KubeCluster) => void;
  api.createKubeCluster = ((body: { name: string }) => new Promise<KubeCluster>((resolve) => { answer = () => resolve(cluster({ id: 9, uuid: "c-9", name: body.name, cluster_uid: "", node_count: 0 })); })) as never;
  const { seen, user } = openForm();
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "slow");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  const cancel = form.getByRole("button", { name: k.drawer.cancel }) as HTMLButtonElement;
  await waitFor(() => assert.equal(cancel.disabled, true, "the request is on its way"));
  assert.equal(closeMark().disabled, true, "the close mark is out of use too");
  assert.equal((form.getByRole("button", { name: k.drawer.submit }) as HTMLButtonElement).disabled, true, "and a second Connect");
  // Nor can another platform be chosen meanwhile: the platform types are out of use (here the one there is).
  assert.deepEqual(form.getAllByRole("tab").map((tab) => [tab.textContent, tab.getAttribute("aria-selected"), tab.getAttribute("aria-disabled")]), [[locale.providers.config.kubernetesGroup, "true", "true"]]);

  escape();
  await user.keyboard("{Escape}");
  await user.click(closeMark());
  await user.click(cancel);
  await user.click(document.querySelector('[data-slot="dialog-backdrop"]') as HTMLElement);
  assert.equal(seen.closed, 0, "the answer carries the only copy of the command");
  assert.equal(screen.queryAllByRole("dialog").length, 1);

  answer(cluster());
  assert.ok(await dialog().findByText(k.created.note));
  // A click outside is never a way out of this dialog; the same close mark that was refused a moment ago is.
  await user.click(document.querySelector('[data-slot="dialog-backdrop"]') as HTMLElement);
  assert.equal(seen.closed, 0);
  assert.equal(closeMark().disabled, false);
  await user.click(closeMark());
  await noDialog();
  assert.deepEqual(seen, { created: 1, closed: 1, saved: 0 });
});

test("advanced choices travel: a named RuntimeClass, an image pull secret, a resident cluster is -1", async () => {
  const calls = stubApi();
  const { user } = openForm();
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "k3s");
  await user.click(form.getByText(k.drawer.advanced));
  await user.type(form.getByLabelText(field(k.drawer.runtimeClass)), "nvidia");
  await user.type(form.getByLabelText(field(k.drawer.imagePullSecret)), " regcred ");
  await user.clear(form.getByLabelText(field(k.drawer.containerdSocket)));
  await user.type(form.getByLabelText(field(k.drawer.containerdSocket)), "/run/k3s/containerd/containerd.sock");
  await user.type(form.getByLabelText(field(k.drawer.apiServer)), "https://10.0.0.5:6443");
  const keep = form.getByRole("switch", { name: k.drawer.keepCards });
  // The switch is enabled once the dictionary, which owns the keep value, has arrived.
  await waitFor(() => assert.equal((form.getByLabelText(field(k.drawer.namespace)) as HTMLInputElement).placeholder, "gpuworks-system"));
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
  const { seen, user } = openForm();
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "prod-gpu");
  const advanced = form.getByRole("button", { name: k.drawer.advanced });
  assert.equal(advanced.getAttribute("aria-expanded"), "false", "the advanced section starts collapsed");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  const taken = locale.common.apiError.KUBE_CLUSTER_NAME_TAKEN.replace("{{name}}", "prod-gpu");
  assert.ok(await form.findByText(taken));
  assert.ok(describedBy(form.getByLabelText(field(k.drawer.name))).includes(taken), "the refusal is said of the name field");
  assert.equal(form.queryAllByText(k.created.note).length, 0, "nothing was created");
  assert.equal(advanced.getAttribute("aria-expanded"), "false", "a refusal of the name opens nothing");

  await user.clear(form.getByLabelText(field(k.drawer.name)));
  await user.type(form.getByLabelText(field(k.drawer.name)), "other");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  const invalid = locale.common.apiError.KUBE_CLUSTER_INVALID.replace("{{field}}", "engine_ports").replace("{{reason}}", "overlaps the NodePort range");
  assert.ok(await form.findByText(invalid));
  // The refused field sits in the collapsed section: it is opened so the reason can be read where it applies.
  await waitFor(() => assert.equal(advanced.getAttribute("aria-expanded"), "true", "the advanced section opened on the refused field"));
  assert.ok(describedBy(form.getByLabelText(field(k.drawer.enginePorts))).includes(invalid), "the refused field carries the reason");
  assert.equal(seen.created, 0);
});

test("the form works from the keyboard: the name is focused on open, Enter submits, Escape closes", async () => {
  const calls = stubApi();
  const { seen, user } = openForm();
  const form = dialog();
  const name = await form.findByLabelText(field(k.drawer.name));
  await waitFor(() => assert.ok(document.activeElement === name, "focus starts in the first field"));
  await user.keyboard("kbd{Enter}");
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.equal((calls.createKubeCluster[0][0] as { name: string }).name, "kbd");
  await dialog().findByText(k.created.note);
  escape();
  await noDialog();
  assert.equal(seen.closed, 1);
});

test("once the cluster exists the focus is on the footer's Close, the one button left: Enter there closes the dialog", async () => {
  const calls = stubApi();
  const { seen, user } = openForm();
  const name = await dialog().findByLabelText(field(k.drawer.name));
  await waitFor(() => assert.ok(document.activeElement === name));
  await user.keyboard("kbd{Enter}");
  await dialog().findByText(k.created.note);
  assert.equal(calls.createKubeCluster?.length, 1);
  // The form that held the focus is gone with its fields; left on the page behind, the keyboard would have nothing to act on.
  await waitFor(() => assert.ok(document.activeElement === closeButton(), "the focus moved to the footer's Close, not the mark in the corner"));
  assert.equal(seen.closed, 0);
  await user.keyboard("{Enter}");
  await noDialog();
  assert.equal(seen.closed, 1);
});

test("Escape closes the form without sending anything", async () => {
  const calls = stubApi();
  const { seen } = openForm();
  await dialog().findByLabelText(field(k.drawer.name));
  escape();
  await noDialog();
  assert.equal(seen.closed, 1);
  assert.equal(calls.createKubeCluster, undefined);
  assert.equal(seen.created, 0);
});

test("the agent image registry offers what the server publishes to; choosing SWR shows its image and sends the choice", async () => {
  const calls = stubApi();
  const { user } = openForm();
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "cn");
  await user.click(form.getByText(k.drawer.advanced));
  const image = form.getByLabelText(field(k.drawer.agentImage)) as HTMLInputElement;
  await waitFor(() => assert.equal(image.placeholder, "gpuworks/gpuworks-agent:v1", "the server's default registry's image, as a hint"));
  const registry = form.getByRole("combobox", { name: field(k.drawer.agentImageSource) });
  registry.focus();
  await user.keyboard("{Enter}");
  assert.deepEqual((await screen.findAllByRole("option")).map((option) => option.textContent),
    [k.drawer.agentImageSources.dockerhub, k.drawer.agentImageSources.swr, k.drawer.agentImageSources.quay]);
  await user.click(screen.getByRole("option", { name: k.drawer.agentImageSources.swr }));
  await waitFor(() => assert.equal(image.placeholder, "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:v1"));
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "cn", server_url: "http://localhost", agent_image_source: "swr" }]);
});

test("priority and preemption show the server's defaults and are not sent untouched", async () => {
  const calls = stubApi();
  const { user } = openForm();
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "pri");
  await user.click(form.getByText(k.drawer.advanced));
  const priority = form.getByLabelText(field(k.drawer.priority)) as HTMLInputElement;
  await waitFor(() => assert.equal(priority.placeholder, "10000", "the server's default, as a hint"));
  assert.equal(priority.value, "");
  const preempt = form.getByRole("switch", { name: k.drawer.allowPreemptionSwitch });
  assert.equal(preempt.getAttribute("aria-checked"), "false", "the server's default: off");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "pri", server_url: "http://localhost" }], "untouched fields take the server's defaults");
});

test("a priority and preemption the operator sets are sent, the switch working from the keyboard", async () => {
  const calls = stubApi();
  const { user } = openForm();
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "pri2");
  await user.click(form.getByText(k.drawer.advanced));
  await user.type(form.getByLabelText(field(k.drawer.priority)), "0");
  const preempt = form.getByRole("switch", { name: k.drawer.allowPreemptionSwitch });
  await waitFor(() => assert.equal(preempt.hasAttribute("disabled"), false, "usable once the server's default is known"));
  preempt.focus();
  await user.keyboard(" ");
  await waitFor(() => assert.equal(preempt.getAttribute("aria-checked"), "true"));
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "pri2", server_url: "http://localhost", priority: 0, allow_preemption: true }], "a priority of 0 is a choice, not an empty field");
});

test("a server whose default allows preemption shows the switch on, and leaving it so sends nothing", async () => {
  const calls = stubApi({ defaults: { ...DEFAULTS, allow_preemption: true } });
  const { user } = openForm();
  const form = dialog();
  await user.type(await form.findByLabelText(field(k.drawer.name)), "pre");
  await user.click(form.getByText(k.drawer.advanced));
  await waitFor(() => assert.equal(form.getByRole("switch", { name: k.drawer.allowPreemptionSwitch }).getAttribute("aria-checked"), "true"));
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "pre", server_url: "http://localhost" }]);
});

test("the RuntimeClass field is always offered and says it only affects NVIDIA nodes' instance pods", async () => {
  stubApi();
  const { user } = openForm();
  const form = dialog();
  await form.findByLabelText(field(k.drawer.name));
  await user.click(form.getByText(k.drawer.advanced));
  const runtimeClass = form.getByLabelText(field(k.drawer.runtimeClass));
  assert.ok(describedBy(runtimeClass).includes(k.drawer.runtimeClassHelp), "offered whatever the nodes' vendors, with what it affects");
  assert.ok(k.drawer.runtimeClassHelp.includes("NVIDIA"));
});

test("a name of spaces only is refused at the field before any request, as an empty one is", async () => {
  const calls = stubApi();
  const { seen, user } = openForm();
  const form = dialog();
  const name = await form.findByLabelText(field(k.drawer.name));
  await user.type(name, "   ");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  assert.ok(await form.findByText(k.drawer.nameRequired));
  assert.ok(describedBy(form.getByLabelText(field(k.drawer.name))).includes(k.drawer.nameRequired), "said of the name field");
  assert.equal(calls.createKubeCluster, undefined);
  assert.equal(seen.created, 0);
  await user.type(form.getByLabelText(field(k.drawer.name)), "edge ");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.equal((calls.createKubeCluster[0][0] as { name: string }).name, "edge", "the name is sent trimmed");
});

test("the idle-return switch sends the server's own keep value", async () => {
  const calls = stubApi({ defaults: { ...DEFAULTS, idle_return_keep: -7 } });
  const { user } = openForm();
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
  const { seen, user } = openForm();
  const form = dialog();
  assert.ok(await form.findByText(k.drawer.defaultsFailed));
  assert.ok(form.getByText(k.drawer.defaultsFailedHelp));
  await user.type(form.getByLabelText(field(k.drawer.name)), "plain");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "plain", server_url: "http://localhost" }]);
  await waitFor(() => assert.equal(seen.created, 1));
});

test("a created cloud entry the server cannot apply to says why instead of offering to", async () => {
  stubApi();
  // Only an entry made from an account's cluster has a source: the same form, prefilled, in the drawer of the account's cluster list.
  const prefill: KubeClusterPrefill = {
    name: "priv", provider_id: 3, provider_cluster_id: "c-1", source_label: "ali-prod · priv", pool_ids: ["np-l20"],
    pools: [{ id: "np-l20", name: "l20", gpu_model: "NVIDIA L20", gpu_count: 1, nodes: 2, vendor: "nvidia", in_entry: false }],
  };
  const source = { provider_id: 3, provider_name: "ali-prod", provider_kind: "aliyun", cluster_id: "c-1" };
  api.createKubeCluster = (async (body: { name: string }) => cluster({ id: 9, uuid: "c-9", name: body.name, cluster_uid: "", node_count: 0, pool_ids: ["np-l20"], source, can_apply: false, can_remove: false, operation_blocked: "private_api" })) as never;
  render(<I18nextProvider i18n={i18n}><KubeClusterDrawer open prefill={prefill} onClose={() => {}} onCreated={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(await dialog().findByRole("button", { name: k.drawer.submit }));
  assert.ok(await dialog().findByText(k.created.applyBlocked));
  assert.ok(dialog().getByText(k.blocked.private_api));
  assert.equal(dialog().queryAllByRole("button", { name: k.created.applyNow }).length, 0);
  assert.ok(dialog().getByLabelText(k.created.command), "the kubectl command is still there");
});

const p = locale.providers;
const pools = poolPicker(() => screen.getByRole("dialog"), {
  label: k.drawer.pools, addLabel: k.pools.add, removeName: (name) => k.pools.removeOne.replace("{{name}}", name), inEntry: p.clusters.inEntry,
});
const A10 = "a10 · NVIDIA A10 ×1 · 2 nodes";
const PPU = "ppu · PPU-ZW810E ×1 · 1 nodes";
const cnPrefill: KubeClusterPrefill = {
  name: "ack-cn", provider_id: 3, provider_cluster_id: "c-cn", source_label: "ali-prod · ack-cn", pool_ids: ["np-a", "np-b"], agent_image_source: "swr",
  pools: [
    { id: "np-a", name: "a10", gpu_model: "NVIDIA A10", gpu_count: 1, nodes: 2, vendor: "nvidia", in_entry: false },
    { id: "np-b", name: "ppu", gpu_model: "PPU-ZW810E", gpu_count: 1, nodes: 1, vendor: "ppu", in_entry: false },
  ],
};

test("a cloud cluster's form says where it comes from, starts on the registry the server suggests for its region and its GPU node pools all chosen under a required field label; a pool taken out is not sent", async () => {
  const calls = stubApi();
  render(<I18nextProvider i18n={i18n}><KubeClusterDrawer open prefill={cnPrefill} onClose={() => {}} onCreated={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  const form = dialog();
  assert.ok(await form.findByText(k.drawer.fromSource.replace("{{source}}", "ali-prod · ack-cn")), "the account and the cluster, nothing more");
  assert.equal(form.getByText(/^Source:/).textContent, "Source: ali-prod · ack-cn");
  assert.deepEqual(form.getAllByText(k.drawer.pools).map((label) => label.textContent), [`${k.drawer.pools} *`], "labelled like the other fields, required");
  assert.ok(form.getByText(k.pools.autoJoin), "what choosing a pool does, said once under it");
  assert.equal(form.queryAllByRole("checkbox").length, 0, "nothing to tick: the chosen pools are a list");
  assert.deepEqual(await pools.chosen(), [A10, PPU], "every pool of the cluster is chosen, whatever its vendor");
  assert.equal(await pools.adder(), null, "nothing left to add");
  await user.click(form.getByText(k.drawer.advanced));
  await waitFor(() => assert.equal(form.getByRole("combobox", { name: field(k.drawer.agentImageSource) }).textContent, k.drawer.agentImageSources.swr, "the region's registry, not the server's default"));
  assert.equal((form.getByLabelText(field(k.drawer.agentImage)) as HTMLInputElement).placeholder, "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:v1");
  await pools.remove(user, "a10");
  assert.deepEqual(await pools.chosen(), [PPU]);
  assert.deepEqual(await pools.removeButtons(), [], "the last pool cannot be taken out");
  const connect = form.getByRole("button", { name: k.drawer.submit }) as HTMLButtonElement;
  assert.equal(connect.disabled, false);
  await user.click(connect);
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual(calls.createKubeCluster[0], [{ name: "ack-cn", server_url: "http://localhost", provider_id: 3, provider_cluster_id: "c-cn", provider_pool_ids: ["np-b"] }],
    "the suggested registry is the server's own, not sent back; only the pools left are");
});

test("a cloud cluster's form opened with no pool chosen offers them to add and will not connect until one is", async () => {
  const calls = stubApi();
  render(<I18nextProvider i18n={i18n}><KubeClusterDrawer open prefill={{ ...cnPrefill, pool_ids: [] }} onClose={() => {}} onCreated={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await dialog().findByText(k.drawer.fromSource.replace("{{source}}", "ali-prod · ack-cn"));
  assert.deepEqual(await pools.chosen(), []);
  const connect = dialog().getByRole("button", { name: k.drawer.submit }) as HTMLButtonElement;
  assert.equal(connect.disabled, true, "nothing to connect without a pool");
  await user.click(connect);
  assert.equal(calls.createKubeCluster, undefined);
  assert.deepEqual(await pools.offered(user), [A10, PPU]);
  await pools.add(user, PPU);
  assert.equal(connect.disabled, false);
  await user.click(connect);
  await waitFor(() => assert.equal(calls.createKubeCluster?.length, 1));
  assert.deepEqual((calls.createKubeCluster[0][0] as { provider_pool_ids: string[] }).provider_pool_ids, ["np-b"]);
});

test("in Chinese the form is labelled, validated and submitted in Chinese", async () => {
  const zh = await createI18n(["kubernetes", "providers"], "zh");
  const calls = stubApi();
  const { user } = openForm(zh.i18n);
  const form = dialog();
  const address = await form.findByLabelText(field("管理节点接入地址")) as HTMLInputElement;
  assert.ok(describedBy(address).includes("这里填的不是 Kubernetes API Server 的地址。"));
  await user.clear(address);
  await user.click(await form.findByRole("button", { name: "接入" }));
  assert.ok(await form.findByText("请输入集群名称"));
  assert.ok(form.getByText("请输入管理节点接入地址"));
  assert.equal(calls.createKubeCluster, undefined);
  await user.type(form.getByLabelText(field("名称")), "边缘");
  await user.type(address, "https://gpuworks.example");
  await user.click(form.getByRole("button", { name: "接入" }));
  assert.ok(await dialog().findByText("集群 边缘 已创建"));
  assert.ok(dialog().getByText("命令里含有接入令牌，请像密码一样保管。"));
  assert.deepEqual(footerButtons(), ["关闭"]);
  assert.equal(document.body.textContent?.includes("{{"), false, "no sentence is left with an unfilled placeholder");
});
