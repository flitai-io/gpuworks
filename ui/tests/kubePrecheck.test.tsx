// The pre-check of a Kubernetes cluster form (ui/src/pages/kubernetes/KubePrecheckModal.tsx,
// opened from the form in ui/src/pages/kubernetes/useKubeClusterForm.tsx) and the
// check after an apply (ui/src/pages/kubernetes/KubeVerifyCommand.tsx), shown once
// the cluster exists and in the troubleshooting of a stalled entry
// (ui/src/pages/kubernetes/KubeEntryStatus.tsx). The pre-check is recommended,
// never required: Connect works without it.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import { field, footerButtons, freshPopupFramesPerTest } from "./helpers/platformDialog.ts";
import { KUBE_DEFAULTS, KUBERNETES_CATEGORY, VERIFY_COMMAND, kubeEntry } from "./helpers/platformFixtures.ts";

const { i18n, locale } = await createI18n(["kubernetes", "providers"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { default: KubeClusterDrawer } = await import("../src/pages/kubernetes/KubeClusterDrawer.tsx");
const { default: KubeEntryStatus } = await import("../src/pages/kubernetes/KubeEntryStatus.tsx");
import type { KubePrecheck, KubePrecheckRequest } from "../src/api";
const originals = { ...api };
await freshPopupFramesPerTest();
afterEach(() => { cleanup(); Object.assign(api, originals); });

const k = locale.kubernetes;

// What POST /api/v1/kube/precheck answers for a form left at its defaults.
const PRECHECK: KubePrecheck = {
  script: "#!/usr/bin/env bash\n# GPUWorks check for a Kubernetes cluster\nMODE='precheck'\n",
  file_name: "gpuworks-precheck.sh",
  image: "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1",
  server: "gpuworks.example:8800",
};

function stubApi(precheck: KubePrecheck | Error = PRECHECK) {
  const calls: { precheck: KubePrecheckRequest[]; created: number } = { precheck: [], created: 0 };
  api.kubeDefaults = async () => KUBE_DEFAULTS;
  api.kubePrecheck = async (body: KubePrecheckRequest) => {
    calls.precheck.push(body);
    if (precheck instanceof Error) throw precheck;
    return precheck;
  };
  api.createKubeCluster = async (body) => {
    calls.created += 1;
    return kubeEntry({ id: 9, uuid: "c-9", name: body.name, cluster_uid: "", node_count: 0 });
  };
  return calls;
}

function Harness() {
  const [open, setOpen] = React.useState(true);
  return (
    <I18nextProvider i18n={i18n}>
      <ProviderConfigModal open={open} defaultKind="kubernetes" kindInfos={[]} categories={[KUBERNETES_CATEGORY]} providerLabel="Platform" canConnectCluster
        onClose={() => setOpen(false)} onSaved={() => {}} onClusterCreated={() => {}} />
    </I18nextProvider>
  );
}

function openForm() {
  render(<Harness />);
  return userEvent.setup({ document: dom.window.document });
}

/** The pre-check's own dialog, the one titled for it. */
const precheckDialog = () => within(screen.getByRole("dialog", { name: k.precheck.title }));

test("the form offers the pre-check beside Connect", async () => {
  stubApi();
  openForm();
  await within(screen.getByRole("dialog")).findByLabelText(field(k.drawer.serverUrl));
  assert.deepEqual(footerButtons(screen.getByRole("dialog")), [k.drawer.cancel, k.precheck.open, k.drawer.submit]);
});

test("the pre-check is rendered for what the form holds and explains how to run it and every code", async () => {
  const calls = stubApi();
  const user = openForm();
  const form = within(screen.getByRole("dialog"));
  const address = await form.findByLabelText(field(k.drawer.serverUrl));
  await user.clear(address);
  await user.type(address, "https://gpuworks.example:8800/");
  await user.type(form.getByLabelText(field(k.drawer.nodeSelector)), "gpu=yes");
  await user.click(form.getByText(k.drawer.advanced));
  await user.type(form.getByLabelText(field(k.drawer.imagePullSecret)), " regcred ");
  await user.click(form.getByRole("button", { name: k.precheck.open }));

  const modal = precheckDialog();
  assert.ok(await modal.findByText(PRECHECK.image));
  // Only what was typed is sent, trimmed; the server fills in the rest as for a new entry.
  assert.deepEqual(calls.precheck, [{ server_url: "https://gpuworks.example:8800/", node_selector: "gpu=yes", image_pull_secret: "regcred" }]);
  assert.ok(modal.getByText(PRECHECK.server));
  assert.equal(modal.getByLabelText(k.precheck.run).textContent, "bash gpuworks-precheck.sh");
  assert.ok(modal.getByText(k.precheck.intro));
  // Every code the server lists, in its order, with what it means.
  const codes = within(modal.getByLabelText(k.check.codesTitle));
  assert.deepEqual(codes.getAllByRole("term").map((term) => term.textContent), KUBE_DEFAULTS.check_codes.precheck);
  assert.ok(codes.getByText(k.check.codes.IMAGE_PULL_FAILED.text));
  assert.ok(codes.getByText(k.check.codes.SERVER_UNREACHABLE.text));
  // What to do sits behind each code's HelpTip, named for the code.
  assert.ok(codes.getByRole("button", { name: i18n.t("check.fixLabel", { ns: "kubernetes", code: "IMAGE_PULL_FAILED" }) }));
  // The script itself: copied whole, and readable on request.
  await user.click(modal.getByRole("button", { name: k.precheck.copy }));
  assert.equal(await navigator.clipboard.readText(), PRECHECK.script);
  await user.click(modal.getByText(k.precheck.showScript));
  assert.equal(modal.getByLabelText(k.precheck.script).textContent, PRECHECK.script);
  assert.ok(modal.getByRole("button", { name: k.precheck.download }));

  // The footer's Close, not the mark in the corner (named by its label).
  await user.click(modal.getAllByRole("button", { name: k.precheck.close }).find((button) => !button.hasAttribute("aria-label")) as HTMLElement);
  await waitFor(() => assert.equal(screen.queryAllByRole("dialog", { name: k.precheck.title }).length, 0));
  assert.equal(calls.created, 0, "a pre-check connects nothing");
  assert.ok(screen.getByRole("dialog"), "the form is still open, with what was typed");
});

test("the pre-check needs a server address it can test, and asks nothing of the server without one", async () => {
  const calls = stubApi();
  const user = openForm();
  const form = within(screen.getByRole("dialog"));
  const address = await form.findByLabelText(field(k.drawer.serverUrl));
  await user.clear(address);
  await user.type(address, "gpuworks.example");
  await user.click(form.getByRole("button", { name: k.precheck.open }));
  assert.ok(await form.findByText(k.drawer.serverUrlInvalid));
  assert.deepEqual(calls.precheck, []);
  assert.equal(screen.queryAllByRole("dialog", { name: k.precheck.title }).length, 0);
});

test("a pre-check the server refuses says why in its dialog", async () => {
  stubApi(new APIError("KUBE_CLUSTER_INVALID", { field: "node_selector", reason: "want key=value label pairs" }, "node_selector: want key=value label pairs", 400));
  const user = openForm();
  const form = within(screen.getByRole("dialog"));
  await form.findByLabelText(field(k.drawer.serverUrl));
  await user.click(form.getByRole("button", { name: k.precheck.open }));
  const modal = precheckDialog();
  assert.ok(await modal.findByText(k.precheck.failed));
  assert.ok(modal.getByText(/want key=value label pairs/), "the server's reason, in the reader's words");
  assert.equal(modal.queryAllByRole("button", { name: k.precheck.copy }).length, 0, "nothing to copy");
  assert.equal(modal.queryAllByRole("button", { name: k.precheck.download }).length, 0);
});

test("connecting needs no pre-check, and the command to check every node follows the apply command", async () => {
  const calls = stubApi();
  const user = openForm();
  const form = within(screen.getByRole("dialog"));
  await user.type(await form.findByLabelText(field(k.drawer.name)), "edge");
  await user.click(form.getByRole("button", { name: k.drawer.submit }));
  const created = within(screen.getByRole("dialog"));
  assert.equal((await created.findByLabelText(k.check.verifyTitle)).textContent, VERIFY_COMMAND);
  assert.deepEqual(calls.precheck, []);
  assert.equal(calls.created, 1);
  assert.ok(created.getByText(k.check.verifyHint));
  await user.click(created.getByRole("button", { name: i18n.t("check.copyCommand", { ns: "kubernetes", title: k.check.verifyTitle }) }));
  assert.equal(await navigator.clipboard.readText(), VERIFY_COMMAND);
  // Its button is named for this command, not the connect command above it.
  assert.equal(created.getByRole("button", { name: i18n.t("check.copyCommand", { ns: "kubernetes", title: k.check.verifyTitle }) }).textContent, k.check.copy);
  // The codes are folded until asked for, then the server's list for the check after an apply.
  assert.equal(created.queryAllByRole("term").length, 0);
  await user.click(created.getByRole("button", { name: k.check.codesTitle }));
  await waitFor(() => assert.deepEqual(created.getAllByRole("term").map((term) => term.textContent), KUBE_DEFAULTS.check_codes.verify));
});

test("a cluster of a cloud account and an entry being edited offer no pre-check", async () => {
  stubApi();
  const pools = [{ id: "np-1", name: "gpu", vendor: "nvidia" as const, nodes: 2, instance_types: ["ecs.gn7i"], gpu_models: ["A10"] }];
  const { unmount } = render(
    <I18nextProvider i18n={i18n}>
      <KubeClusterDrawer open onClose={() => {}} onCreated={() => {}}
        prefill={{ name: "ack", server_url: "https://gpuworks.example", provider_id: 3, provider_cluster_id: "c-1", source_label: "ali / c-1", pools, pool_ids: ["np-1"] }} />
    </I18nextProvider>,
  );
  await screen.findByRole("button", { name: k.drawer.submit });
  assert.equal(screen.queryAllByRole("button", { name: k.precheck.open }).length, 0);
  unmount();
  render(
    <I18nextProvider i18n={i18n}>
      <KubeClusterDrawer open onClose={() => {}} onCreated={() => {}} edit={kubeEntry()} />
    </I18nextProvider>,
  );
  await screen.findByRole("button", { name: k.drawer.save });
  assert.equal(screen.queryAllByRole("button", { name: k.precheck.open }).length, 0);
});

test("a stalled entry's troubleshooting starts with the check of every node", async () => {
  api.kubeDefaults = async () => KUBE_DEFAULTS;
  const user = userEvent.setup({ document: dom.window.document });
  render(<I18nextProvider i18n={i18n}><KubeEntryStatus cluster={kubeEntry({ stalled: true, state: "stalled" })} /></I18nextProvider>);
  await user.click(screen.getByRole("button", { name: k.clusterUid.troubleshoot }));
  const popover = within(await screen.findByRole("dialog"));
  assert.equal(popover.getByLabelText(k.check.verifyTitle).textContent, VERIFY_COMMAND);
  assert.ok(popover.getByText(k.stalled.introMore));
  // An entry the reader gets no commands for (not an administrator) keeps the plain advice.
  cleanup();
  render(<I18nextProvider i18n={i18n}><KubeEntryStatus cluster={kubeEntry({ stalled: true, state: "stalled", apply_command: undefined, verify_command: undefined })} /></I18nextProvider>);
  await user.click(screen.getByRole("button", { name: k.clusterUid.troubleshoot }));
  const plain = within(await screen.findByRole("dialog"));
  assert.ok(plain.getByText(k.stalled.intro));
  assert.equal(plain.queryAllByLabelText(k.check.verifyTitle).length, 0);
});
