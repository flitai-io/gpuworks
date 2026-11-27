// How a cloud entry's agents stand, node by node (ui/src/pages/kubernetes/KubeRolloutStatus.tsx):
// shown once the server applied the manifest (ui/src/pages/kubernetes/KubeClusterOperationModal.tsx)
// and in a stalled cloud entry's troubleshooting (ui/src/pages/kubernetes/KubeEntryStatus.tsx).
// The server reads the cluster and judges; the page shows its rows and asks
// again after the delay the server gives, until the server says done.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n, dom } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";
import { freshPopupFramesPerTest } from "./helpers/platformDialog.ts";
import { kubeEntry } from "./helpers/platformFixtures.ts";

const { i18n, locale } = await createI18n(["kubernetes", "providers"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { api } = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: KubeClusterOperationModal } = await import("../src/pages/kubernetes/KubeClusterOperationModal.tsx");
const { default: KubeEntryStatus } = await import("../src/pages/kubernetes/KubeEntryStatus.tsx");
import type { KubeRollout } from "../src/api";
const originals = { ...api };
await freshPopupFramesPerTest();
afterEach(() => { cleanup(); Object.assign(api, originals); });

const k = locale.kubernetes;
const SOURCE = { provider_id: 3, provider_name: "ali", provider_kind: "aliyun", cluster_id: "c-gpu" };
const cloudEntry = (overrides = {}) => kubeEntry({ source: SOURCE, can_apply: true, can_remove: true, operation_blocked: undefined, ...overrides });

// What GET /api/v1/kube/clusters/{uuid}/rollout answers, in turn.
const WAITING: KubeRollout = {
  done: false, poll_after_seconds: 1, desired: 2,
  rows: [
    { node: "cn-shanghai.10.0.0.5", pod: "gpuworks-agent-a", code: "OK" },
    { node: "cn-shanghai.10.0.0.6", pod: "gpuworks-agent-b", code: "STILL_STARTING", detail: "still pulling the image or starting" },
  ],
};
const DONE: KubeRollout = {
  done: true, desired: 2,
  rows: [
    { node: "cn-shanghai.10.0.0.5", pod: "gpuworks-agent-a", code: "OK" },
    { node: "cn-shanghai.10.0.0.6", pod: "gpuworks-agent-b", code: "IMAGE_PULL_FAILED", detail: "Failed to pull image \"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-agent:r1\": i/o timeout" },
  ],
};

function stubRollout(answers: (KubeRollout | Error)[]) {
  const asked: string[] = [];
  api.kubeRollout = async (uuid: string) => {
    asked.push(uuid);
    const next = answers[Math.min(asked.length - 1, answers.length - 1)];
    if (next instanceof Error) throw next;
    return next;
  };
  return asked;
}

const rowTexts = (region: HTMLElement) => within(region).getAllByRole("row").slice(1).map((row) => row.textContent);

test("after the server applied the manifest, the dialog follows the agents node by node until the server says done", async () => {
  api.kubeClusterOperation = (async (_uuid: string, _op: string, dryRun: boolean) => ({ dry_run: dryRun, results: [{ kind: "DaemonSet", name: "gpuworks-agent", namespace: "gpuworks-system", action: "create" }] })) as never;
  const asked = stubRollout([WAITING, DONE]);
  const user = userEvent.setup({ document: dom.window.document });
  render(<I18nextProvider i18n={i18n}><KubeClusterOperationModal cluster={cloudEntry()} op="apply" onClose={() => {}} /></I18nextProvider>);
  const dialog = within(screen.getByRole("dialog"));
  assert.equal(dialog.queryAllByRole("region", { name: k.rollout.title }).length, 0, "nothing to follow before applying");
  await user.click(await dialog.findByRole("button", { name: k.operation.confirmApply }));
  const region = await dialog.findByRole("region", { name: k.rollout.title });
  await waitFor(() => assert.equal(rowTexts(region).length, 2));
  assert.ok(within(region).getByText(k.check.codes.STILL_STARTING.text));
  assert.ok(within(region).getByText(i18n.t("rollout.waiting", { ns: "kubernetes", seconds: 1 })));
  // Asked again after the server's delay, then settled: no more asking.
  await waitFor(() => assert.ok(within(region).getByText(k.check.codes.IMAGE_PULL_FAILED.text)), { timeout: 3000 });
  assert.ok(within(region).getByText(/i\/o timeout/));
  assert.ok(within(region).getByRole("button", { name: i18n.t("check.fixLabel", { ns: "kubernetes", code: "IMAGE_PULL_FAILED" }) }));
  assert.equal(within(region).queryAllByText(i18n.t("rollout.waiting", { ns: "kubernetes", seconds: 1 })).length, 0);
  const settled = asked.length;
  await new Promise((resolve) => setTimeout(resolve, 1200));
  assert.equal(asked.length, settled, "a settled answer is not asked for again");
  assert.deepEqual(asked, Array(settled).fill("c-1"));
});

test("a read the cluster refuses says so, and can be tried again", async () => {
  api.kubeClusterOperation = (async (_uuid: string, _op: string, dryRun: boolean) => ({ dry_run: dryRun, results: [] })) as never;
  const asked = stubRollout([new APIError("KUBE_CLUSTER_OPERATION_FAILED", { step: "api_server", reason: "pods is forbidden" }, "Kubernetes API server: pods is forbidden (HTTP 403)", 424), DONE]);
  const user = userEvent.setup({ document: dom.window.document });
  render(<I18nextProvider i18n={i18n}><KubeClusterOperationModal cluster={cloudEntry()} op="apply" onClose={() => {}} /></I18nextProvider>);
  const dialog = within(screen.getByRole("dialog"));
  await user.click(await dialog.findByRole("button", { name: k.operation.confirmApply }));
  const region = await dialog.findByRole("region", { name: k.rollout.title });
  assert.ok(await within(region).findByText(k.rollout.failed));
  assert.ok(within(region).getByText(/pods is forbidden/));
  await user.click(within(region).getByRole("button", { name: k.rollout.retry }));
  await waitFor(() => assert.equal(rowTexts(region).length, 2));
  assert.equal(asked.length, 2);
});

test("removing follows no agents", async () => {
  api.kubeClusterOperation = (async (_uuid: string, _op: string, dryRun: boolean) => ({ dry_run: dryRun, results: [] })) as never;
  const asked = stubRollout([DONE]);
  const user = userEvent.setup({ document: dom.window.document });
  render(<I18nextProvider i18n={i18n}><KubeClusterOperationModal cluster={cloudEntry()} op="remove" onClose={() => {}} /></I18nextProvider>);
  const dialog = within(screen.getByRole("dialog"));
  await user.click(await dialog.findByRole("button", { name: k.operation.confirmRemove }));
  await waitFor(() => assert.ok(dialog.getAllByRole("button", { name: k.operation.close }).some((b) => !b.hasAttribute("aria-label")), "the footer's Close: the removal is done"));
  assert.equal(dialog.queryAllByRole("region", { name: k.rollout.title }).length, 0);
  assert.equal(asked.length, 0);
});

test("a stalled cloud entry's troubleshooting shows its agents as the server reads them, not a command", async () => {
  const asked = stubRollout([DONE]);
  const user = userEvent.setup({ document: dom.window.document });
  render(<I18nextProvider i18n={i18n}><KubeEntryStatus cluster={cloudEntry({ stalled: true, state: "stalled" })} /></I18nextProvider>);
  await user.click(screen.getByRole("button", { name: k.clusterUid.troubleshoot }));
  const popover = within(await screen.findByRole("dialog"));
  const region = await popover.findByRole("region", { name: k.rollout.title });
  await waitFor(() => assert.equal(rowTexts(region).length, 2));
  assert.equal(popover.queryAllByLabelText(k.check.verifyTitle).length, 0);
  assert.ok(popover.getByText(k.stalled.introMore));
  assert.deepEqual(asked, ["c-1"]);
});
