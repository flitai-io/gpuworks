import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["settings", "common"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: DebugSessionCard } = await import("../src/pages/settings/DebugSessionCard.tsx");
const { default: DiagnosticsBundleCard } = await import("../src/pages/settings/DiagnosticsBundleCard.tsx");
type DebugSessionState = import("../src/diagnosticsApi.ts").DebugSessionState;
type DebugSessionStart = import("../src/diagnosticsApi.ts").DebugSessionStart;
type DiagnosticsBundles = import("../src/diagnosticsApi.ts").DiagnosticsBundles;
type DiagnosticsBundleStart = import("../src/diagnosticsApi.ts").DiagnosticsBundleStart;

const d = locale.settings.diagnostics as {
  adminOnly: string;
  debug: Record<string, string> & { nodeState: Record<string, string>; nodeReason: Record<string, string>; ended: Record<string, string> };
  bundle: Record<string, string> & { bundleState: Record<string, string>; itemState: Record<string, string>; itemReason: Record<string, string> };
};
const original = { ...apiModule.api };

const limits = {
  default_duration_seconds: 600, min_duration_seconds: 60, max_duration_seconds: 3600, default_max_bytes: 10 * 2 ** 30,
  max_max_bytes: 50 * 2 ** 30, node_max_bytes: 200 * 2 ** 20, request_max_bytes: 16 * 2 ** 20, keep_days: 7,
};
const bundleLimits = { default_window_seconds: 3600, max_window_seconds: 604800, max_nodes: 64, max_instances: 64, keep_days: 7 };

function stub({ role = "admin", debug, bundles }: { role?: string; debug?: () => DebugSessionState | Error; bundles?: () => DiagnosticsBundles | Error }) {
  const starts: DebugSessionStart[] = [];
  const bundleStarts: DiagnosticsBundleStart[] = [];
  let stops = 0;
  apiModule.api.me = (async () => ({ id: 1, username: role, role })) as typeof apiModule.api.me;
  apiModule.api.deployments = (async () => ({ items: [{ id: 7, name: "qwen-chat" }], total: 1 })) as unknown as typeof apiModule.api.deployments;
  apiModule.api.nodes = (async () => ({ items: [{ uuid: "n1", name: "gpu-01", hostname: "gpu-01" }], total: 1 })) as unknown as typeof apiModule.api.nodes;
  apiModule.api.debugSession = async () => {
    const v = debug ? debug() : { session: null, limits };
    if (v instanceof Error) throw v;
    return v;
  };
  apiModule.api.startDebugSession = async (body) => {
    starts.push(body);
    return { session: null, limits };
  };
  apiModule.api.stopDebugSession = async () => {
    stops++;
    return { session: null, limits };
  };
  apiModule.api.diagnosticsBundles = async () => {
    const v = bundles ? bundles() : { bundles: [], limits: bundleLimits };
    if (v instanceof Error) throw v;
    return v;
  };
  apiModule.api.startDiagnosticsBundle = async (body) => {
    bundleStarts.push(body);
    return { id: "b1", requested_by: "admin", created_at: "", from: "", to: "", deployment_ids: [], instance_uuids: [], node_uuids: [], doctor: true, state: "running", size_bytes: 0, items: [] };
  };
  return { starts, bundleStarts, stops: () => stops };
}

function renderWith(node: React.ReactNode) {
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}>{node}</I18nextProvider>);
  return user;
}

async function confirm(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole("button", { name: "OK" }));
}

afterEach(() => {
  cleanup();
  Object.assign(apiModule.api, original);
});
after(() => closeChannels());

test("an admin starts a debug collection with the server's defaults and the chosen deployment", async () => {
  const api = stub({});
  const user = renderWith(<DebugSessionCard />);
  const duration = (await screen.findByRole("spinbutton", { name: d.debug.duration })) as HTMLInputElement;
  await waitFor(() => assert.equal(duration.value, "10"));
  assert.equal((screen.getByRole("spinbutton", { name: d.debug.size }) as HTMLInputElement).value, "10");
  assert.ok(screen.getByText(/prompts and replies included/));
  await user.click(screen.getByRole("combobox", { name: d.debug.deployments }));
  await user.click(await screen.findByText("qwen-chat"));
  await user.click(screen.getByRole("button", { name: d.debug.start }));
  await confirm(user);
  await waitFor(() => assert.equal(api.starts.length, 1));
  assert.deepEqual(api.starts[0], { duration_seconds: 600, max_bytes: 10 * 2 ** 30, deployment_ids: [7], node_uuids: [] });
  assert.ok(await screen.findByText(d.debug.started));
});

test("an open collection shows its nodes and stops", async () => {
  const session = {
    id: "s1", started_by: "admin", started_at: "2026-11-02T10:00:00Z", until: "2026-11-02T10:10:00Z", max_bytes: 2 ** 30,
    deployment_ids: [7], open: true, server_log_bytes: 1024, request_bytes: 1024, requests: 3,
    nodes: [{ uuid: "n1", name: "gpu-01", state: "collecting" }, { uuid: "n2", name: "gpu-02", state: "skipped", reason: "unsupported" }],
  };
  const api = stub({ debug: () => ({ session, limits }) });
  const user = renderWith(<DebugSessionCard />);
  assert.ok(await screen.findByText("gpu-02"));
  assert.ok(screen.getByText(d.debug.nodeReason.unsupported));
  assert.equal(screen.queryByRole("button", { name: d.debug.start }), null);
  await user.click(screen.getByRole("button", { name: d.debug.stop }));
  await confirm(user);
  await waitFor(() => assert.equal(api.stops(), 1));
});

test("a member sees the collection but cannot start one; a load failure is said", async () => {
  stub({ role: "member" });
  renderWith(<DebugSessionCard />);
  assert.ok(await screen.findByText(d.adminOnly));
  await waitFor(() => assert.equal((screen.getByRole("button", { name: d.debug.start }) as HTMLButtonElement).disabled, true));
  cleanup();
  stub({ debug: () => new APIError("DIAGNOSTICS_UNAVAILABLE", undefined, "no", 404) });
  renderWith(<DebugSessionCard />);
  assert.ok(await screen.findByText(/Diagnostics are not enabled on this server/));
});

test("collecting diagnostics sends the window and lists the archive to download", async () => {
  const ready = {
    id: "20261102-1", requested_by: "admin", created_at: "2026-11-02T10:00:00Z", finished_at: "2026-11-02T10:01:00Z",
    from: "2026-11-02T09:00:00Z", to: "2026-11-02T10:00:00Z", deployment_ids: [], instance_uuids: [], node_uuids: ["n1"], doctor: true,
    state: "ready", size_bytes: 2048,
    items: [{ name: "nodes/n2", state: "skipped", reason: "offline", bytes: 0 }, { name: "server/gpuworks-server.log", state: "collected", bytes: 100 }],
  };
  let list: DiagnosticsBundles = { bundles: [], limits: bundleLimits };
  const api = stub({ bundles: () => list });
  const user = renderWith(<DiagnosticsBundleCard />);
  assert.ok(await screen.findByText(d.bundle.empty));
  await user.click(screen.getByRole("switch", { name: d.bundle.doctor }));
  await user.click(screen.getByRole("button", { name: d.bundle.start }));
  await confirm(user);
  await waitFor(() => assert.equal(api.bundleStarts.length, 1));
  const sent = api.bundleStarts[0];
  assert.equal(sent.doctor, false);
  assert.deepEqual([sent.deployment_ids, sent.node_uuids], [[], []]);
  assert.equal(Date.parse(sent.to!) - Date.parse(sent.from!), 3600 * 1000, "the default window is the last hour");

  list = { bundles: [ready], limits: bundleLimits };
  cleanup();
  renderWith(<DiagnosticsBundleCard />);
  const download = await screen.findByRole("link", { name: new RegExp(d.bundle.download) });
  assert.equal(download.getAttribute("href"), "/api/v1/diagnostics/bundles/20261102-1/download");
  assert.ok(screen.getByText(d.bundle.bundleState.ready));
  await user.click(screen.getByRole("button", { name: /expand row/i }));
  const items = await screen.findByText("nodes/n2");
  assert.ok(within(items.closest("tr")!).getByText(d.bundle.itemReason.offline));
});

test("a running collection blocks another; a failed one says why", async () => {
  const failed = {
    id: "b2", requested_by: "admin", created_at: "2026-11-02T10:00:00Z", from: "2026-11-02T09:00:00Z", to: "2026-11-02T10:00:00Z",
    deployment_ids: [], instance_uuids: [], node_uuids: [], doctor: false, state: "failed", error: "disk full", size_bytes: 0, items: [],
  };
  stub({ bundles: () => ({ bundles: [{ ...failed, id: "b3", state: "running", error: undefined }, failed], limits: bundleLimits }) });
  renderWith(<DiagnosticsBundleCard />);
  assert.ok(await screen.findByText("disk full"));
  const busy = screen.getByRole("button", { name: new RegExp(d.bundle.running) }) as HTMLButtonElement;
  assert.ok(busy.className.includes("loading"));
});

test("each card states what it does in one line and keeps the details behind the help mark", async () => {
  stub({});
  for (const [card, description, help] of [
    [<DebugSessionCard />, d.debug.description, d.debug.descriptionHelp],
    [<DiagnosticsBundleCard />, d.bundle.description.replace("{{days}}", "7"), d.bundle.descriptionHelp],
  ] as const) {
    const user = renderWith(card);
    assert.ok(await screen.findByText(description));
    assert.equal(screen.queryByText(help), null);
    await user.hover(screen.getByRole("button", { name: locale.common.helpTip.label }));
    assert.equal((await screen.findByRole("tooltip")).textContent, help);
    cleanup();
  }
});
