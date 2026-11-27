import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import type { AgentUpgradeCounts, AgentUpgradeCreate, AgentUpgradeItem, AgentUpgradePreview, AgentUpgradeRun } from "../src/agentUpgradesTypes";

const { i18n, locale } = await createI18n(["nodes"]);
const k = locale.nodes.batchUpgrade;
class Events {
  static listeners = new Map<string, Set<EventListener>>();
  addEventListener(topic: string, listener: EventListener) { const entries = Events.listeners.get(topic) ?? new Set<EventListener>(); entries.add(listener); Events.listeners.set(topic, entries); }
  removeEventListener(topic: string, listener: EventListener) { Events.listeners.get(topic)?.delete(listener); }
  close() {}
  static emit(topic: string) { for (const listener of this.listeners.get(topic) ?? []) listener(new dom.window.Event(topic) as unknown as Event); }
}
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { default: AgentUpgradeManager } = await import("../src/components/agentUpgrades/AgentUpgradeManager.tsx");
const originals = {
  agentUpgradePreview: api.agentUpgradePreview, createAgentUpgrade: api.createAgentUpgrade,
  agentUpgradeRuns: api.agentUpgradeRuns, agentUpgradeRun: api.agentUpgradeRun, agentUpgradeItems: api.agentUpgradeItems,
  stopAgentUpgrade: api.stopAgentUpgrade, continueAgentUpgradePreview: api.continueAgentUpgradePreview,
};
const originalFetch = globalThis.fetch;
afterEach(() => { cleanup(); Object.assign(api, originals); globalThis.fetch = originalFetch; Events.listeners.clear(); });
after(() => dom.window.close());

const counts = (over: Partial<AgentUpgradeCounts> = {}): AgentUpgradeCounts => ({ total: 2, processed: 0, succeeded: 0, failed: 0, skipped: 0, cancelled: 0, interrupted: 0, unconfirmed: 0, queued: 1, active: 1, ...over });
function run(over: Partial<AgentUpgradeRun> = {}): AgentUpgradeRun { return { id: 7, target_version: "dev-target", state: "running", counts: counts(), ...over }; }
function item(over: Partial<AgentUpgradeItem> = {}): AgentUpgradeItem { return { run_id: 7, node_uuid: "node-a", name: "node outside current page", from_version: "dev-old", target_version: "dev-target", current_version: "dev-old", phase: "waiting_registration", ...over }; }
function preview(): AgentUpgradePreview {
  const nodes = Array.from({ length: 25 }, (_, i) => ({ node_uuid: `node-${i}`, name: `fleet-${i}`, from_version: "dev-old", os: "linux", arch: "amd64", transport: "push", eligibility: "eligible" }));
  return { target_version: "dev-target", manifest_fingerprint: "sha256:sealed", node_uuids: nodes.map((node) => node.node_uuid), nodes, counts: { eligible: 25 } };
}
function backend(initial: AgentUpgradeRun[] = []) {
  const state = { runs: initial, items: [item()], previews: [] as unknown[], created: [] as AgentUpgradeCreate[], stopped: 0 };
  api.agentUpgradeRuns = async () => ({ items: state.runs, total: state.runs.length, page: 1, page_size: 20, blocked_by_item: state.runs.find((r) => r.blocked_by_item)?.blocked_by_item });
  api.agentUpgradeRun = async (id) => { const value = state.runs.find((r) => r.id === id); if (!value) throw new Error("batch missing"); return value; };
  api.agentUpgradeItems = async (_id, page = 1, pageSize = 20, phase = "") => {
    const matched = state.items.filter((entry) => !phase || entry.phase === phase);
    return { items: matched.slice((page - 1) * pageSize, page * pageSize), total: matched.length, page, page_size: pageSize };
  };
  api.agentUpgradePreview = async (body) => { state.previews.push(body); return preview(); };
  api.createAgentUpgrade = async (body) => { state.created.push(body); const value = run(); state.runs = [value]; return value; };
  api.stopAgentUpgrade = async () => { state.stopped++; const value = run({ state: "stopping", stop_requested_at: "2026-10-29T00:00:00Z", counts: counts({ queued: 0, cancelled: 1, processed: 1 }) }); state.runs = [value]; return value; };
  api.continueAgentUpgradePreview = async () => preview();
  return state;
}
function mount(isAdmin = true, requestedRunID?: number, selectedNodeUUIDs = preview().node_uuids, onStarted?: () => void) {
  return render(<I18nextProvider i18n={i18n}><AgentUpgradeManager isAdmin={isAdmin} requestedRunID={requestedRunID} selectedNodeUUIDs={selectedNodeUUIDs} onStarted={onStarted} /></I18nextProvider>);
}

test("the selected node preview requires consent and submits every snapshot UUID exactly once", async () => {
  const state = backend();
  let finish!: () => void;
  api.createAgentUpgrade = async (body) => {
    state.created.push(body); await new Promise<void>((resolve) => { finish = resolve; });
    const value = run({ counts: counts({ total: body.node_uuids.length, queued: body.node_uuids.length - 1 }) });
    state.items = body.node_uuids.map((uuid, index) => item({ node_uuid: uuid, name: `fleet-${index}`, phase: index ? "queued" : "waiting_registration" }));
    state.runs = [value]; return value;
  };
  const user = userEvent.setup(); mount();
  await user.click(screen.getByRole("button", { name: k.selected }));
  const modal = await screen.findByRole("dialog");
  await within(modal).findByText("fleet-0");
  assert.deepEqual(state.previews, [{ node_uuids: preview().node_uuids }]);
  const start = within(modal).getByRole("button", { name: k.start }) as HTMLButtonElement;
  assert.equal(start.disabled, true);
  await user.click(within(modal).getByRole("checkbox", { name: k.acknowledge }));
  start.focus(); await user.keyboard("{Enter}");
  fireEvent.click(start);
  await waitFor(() => assert.equal(state.created.length, 1));
  assert.equal(state.created[0].node_uuids.length, 25, "preview pagination must not truncate the fixed scope");
  assert.deepEqual(state.created[0].node_uuids, preview().node_uuids);
  assert.equal(state.created[0].expected_manifest_fingerprint, "sha256:sealed");
  assert.equal(state.created[0].expected_target_version, "dev-target");
  assert.ok(state.created[0].idempotency_key);
  await act(async () => finish());
  assert.ok(await screen.findByText(i18n.t("nodes:batchUpgrade.runTitle", { id: 7 })));
  const drawer = screen.getByRole("dialog", { name: i18n.t("nodes:batchUpgrade.runTitle", { id: 7 }) });
  assert.ok(await within(drawer).findByText("fleet-0"));
});

test("a lost create response can retry with the same idempotency key and a refresh resets consent", async () => {
  const state = backend(); const user = userEvent.setup();
  api.createAgentUpgrade = async (body) => { state.created.push(body); throw new Error("response lost"); };
  mount();
  await user.click(screen.getByRole("button", { name: k.selected }));
  const modal = await screen.findByRole("dialog"); await within(modal).findByText("fleet-0");
  await user.click(within(modal).getByRole("checkbox"));
  await user.click(within(modal).getByRole("button", { name: k.start }));
  await within(modal).findByText("response lost");
  await user.click(within(modal).getByRole("button", { name: k.start }));
  await waitFor(() => assert.equal(state.created.length, 2));
  assert.equal(state.created[0].idempotency_key, state.created[1].idempotency_key);
  await user.click(within(modal).getByRole("button", { name: k.refreshPreview }));
  await waitFor(() => assert.equal((within(modal).getByRole("checkbox") as HTMLInputElement).checked, false));
});

test("persisted partial results survive remount and read-only users can inspect without mutation actions", async () => {
  const blocker = { run_id: 7, node_uuid: "node-a", name: "unconfirmed node", reason: "unconfirmed" };
  backend([run({ state: "finished", blocked_by_item: blocker, counts: counts({ total: 2, processed: 2, succeeded: 1, unconfirmed: 1, queued: 0, active: 0 }) })]);
  const user = userEvent.setup(); const view = mount(false);
  await user.click(screen.getByRole("button", { name: k.history }));
  await screen.findByText(k.processingNotSuccess);
  assert.ok(!screen.queryByRole("button", { name: k.selected }));
  const details = screen.getAllByRole("button", { name: k.details }); await user.click(details[0]);
  await screen.findByText("node outside current page");
  assert.ok(!screen.queryByRole("button", { name: k.stop }));
  view.unmount(); mount(false);
  await user.click(screen.getByRole("button", { name: k.history }));
  assert.ok(await screen.findByText(k.processingNotSuccess));
});

test("stop cancels only queued nodes and preserves the active observation", async () => {
  const state = backend([run()]); const user = userEvent.setup(); mount(true, 7);
  await screen.findByText("node outside current page");
  await user.click(screen.getByRole("button", { name: k.stop }));
  await screen.findByText(k.stopConfirm);
  await user.click(screen.getAllByRole("button", { name: k.stop }).at(-1)!);
  await waitFor(() => assert.equal(state.stopped, 1));
  assert.equal(state.runs[0].counts.active, 1);
  assert.equal(state.runs[0].counts.cancelled, 1);
  assert.ok(await screen.findByText(k.phase.waiting_registration));
});

test("a node whose outcome is unknown asks nothing of the administrator, and the batch is shown going on", async () => {
  const blocker = { run_id: 7, node_uuid: "node-a", name: "silent node", reason: "unconfirmed" };
  const state = backend([run({ state: "running", blocked_by_item: blocker, counts: counts({ total: 3, processed: 1, unconfirmed: 1, active: 0, queued: 2 }) })]);
  state.items = [item({ name: "silent node", phase: "unconfirmed", reason: "confirmation_timeout" }), item({ node_uuid: "next", name: "next node", phase: "queued" })];
  mount(true, 7);
  const row = (await screen.findByText("silent node")).closest("tr")!;
  const drawer = screen.getByRole("dialog", { name: i18n.t("nodes:batchUpgrade.runTitle", { id: 7 }) });
  // What happened and what the platform does next, in one line each; nothing to verify, note or release.
  assert.ok(within(row).getByText(k.phase.unconfirmed));
  assert.ok(within(row).getByText(k.reason.confirmation_timeout));
  assert.ok(within(drawer).getByText(k.blocked.replace("{{name}}", "silent node")));
  assert.equal(within(row).queryAllByRole("button").length, 0, "an unknown outcome offers the administrator no action");
  assert.equal(within(drawer).queryAllByRole("textbox").length, 0);
  assert.ok(within(drawer).getByText(k.runState.running), "one silent node does not pause its batch");
  assert.ok(within(drawer).getByRole("button", { name: k.stop }), "stopping the rest stays one click away");
});

test("a batch paused after two unknown outcomes in a row says why and offers to continue with the unfinished nodes", async () => {
  const state = backend([run({ state: "paused", pause_reason: "repeated_unknown_outcome", counts: counts({ total: 3, processed: 2, unconfirmed: 2, active: 0, queued: 1 }) })]);
  state.items = [item({ name: "silent one", phase: "unconfirmed", reason: "confirmation_timeout" }), item({ node_uuid: "b", name: "silent two", phase: "unconfirmed", reason: "confirmation_timeout" }), item({ node_uuid: "c", name: "waiting", phase: "queued" })];
  let continued = 0;
  api.continueAgentUpgradePreview = async () => { continued++; return preview(); };
  const user = userEvent.setup(); mount(true, 7);
  await screen.findByText("silent one");
  const drawer = screen.getByRole("dialog", { name: i18n.t("nodes:batchUpgrade.runTitle", { id: 7 }) });
  assert.ok(within(drawer).getByText(k.pausedRepeatedUnknown), "the pause states its cause and the next step on the page");
  await user.click(within(drawer).getByRole("button", { name: k.continue }));
  await waitFor(() => assert.equal(continued, 1));
});

test("query and preview failures expose retry, and SSE refreshes the persisted results", async () => {
  const state = backend(); let fail = true;
  const list = api.agentUpgradeRuns;
  api.agentUpgradeRuns = async (...args) => { if (fail) throw new Error("result query unavailable"); return list(...args); };
  const user = userEvent.setup(); mount(); await user.click(screen.getByRole("button", { name: k.history })); await screen.findByText("result query unavailable");
  fail = false; await user.click(screen.getByRole("button", { name: k.refresh })); await screen.findByText(k.empty);
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Close" }));
  api.agentUpgradePreview = async () => { throw new Error("artifact metadata unavailable"); };
  await user.click(screen.getByRole("button", { name: k.selected }));
  const modal = await screen.findByRole("dialog", { name: k.previewTitle }); await within(modal).findByText("artifact metadata unavailable");
  assert.equal((within(modal).getByRole("button", { name: k.start }) as HTMLButtonElement).disabled, true);
  await user.click(within(modal).getAllByRole("button", { name: k.close }).at(-1)!);
  state.runs = [run({ state: "finished", counts: counts({ processed: 2, succeeded: 2, queued: 0, active: 0 }) })];
  await act(async () => Events.emit("agent_upgrade"));
  await user.click(screen.getByRole("button", { name: k.history }));
  assert.ok(await screen.findByText(i18n.t("nodes:batchUpgrade.processed", { processed: 2, total: 2 })));
});

test("a paused run can preview an atomic handoff after all uncertain attempts are resolved", async () => {
  const state = backend([run({ state: "paused", counts: counts({ active: 0, queued: 2 }), blocked_by_item: { run_id: 7, node_uuid: "node-a", reason: "active_run" } })]);
  state.items = [item({ phase: "queued" })];
  let continued = 0;
  api.continueAgentUpgradePreview = async (id) => { assert.equal(id, 7); continued++; return preview(); };
  const user = userEvent.setup(); mount(true, 7);
  await screen.findByText("node outside current page");
  const button = screen.getByRole("button", { name: k.continue }) as HTMLButtonElement;
  assert.equal(button.disabled, false, "the source run's own queued reservation must not deadlock handoff");
  await user.click(button);
  const modal = screen.getAllByRole("dialog").at(-1)!;
  await within(modal).findByText("fleet-0");
  await user.click(within(modal).getByRole("checkbox"));
  await user.click(within(modal).getByRole("button", { name: k.start }));
  await waitFor(() => assert.equal(state.created.length, 1));
  assert.equal(continued, 1);
  assert.equal(state.created[0].source_run_id, 7);
});

test("an interrupted run can confirm its own queued handoff but another run remains blocked", async () => {
  const state = backend([run({ state: "interrupted", counts: counts({ active: 0, queued: 2 }), blocked_by_item: { run_id: 7, node_uuid: "node-a", reason: "active_run" } })]);
  state.items = [item({ phase: "queued" })];
  mount(true, 7);
  await screen.findByText("node outside current page");
  assert.equal((screen.getByRole("button", { name: k.continue }) as HTMLButtonElement).disabled, false);
  state.runs = [run({ state: "interrupted", counts: counts({ active: 0, queued: 2 }), blocked_by_item: { run_id: 8, node_uuid: "other", reason: "active_run" } })];
  await act(async () => Events.emit("agent_upgrade"));
  await waitFor(() => assert.equal((screen.getByRole("button", { name: k.continue }) as HTMLButtonElement).disabled, true));
});

test("an all-skipped preview cannot create an upgrade even after consent", async () => {
  const state = backend();
  api.agentUpgradePreview = async () => ({ ...preview(), counts: { eligible: 0, already_current: 25 }, nodes: preview().nodes.map((node) => ({ ...node, eligibility: "already_current" })) });
  const user = userEvent.setup(); mount();
  await user.click(screen.getByRole("button", { name: k.selected }));
  const modal = await screen.findByRole("dialog"); await within(modal).findByText(k.noEligible);
  await user.click(within(modal).getByRole("checkbox"));
  assert.equal((within(modal).getByRole("button", { name: k.start }) as HTMLButtonElement).disabled, true);
  assert.equal(state.created.length, 0);
});

test("active batches poll without SSE and stop polling after a persisted terminal result", async () => {
  const state = backend([run()]);
  const originalSet = globalThis.setInterval;
  const originalClear = globalThis.clearInterval;
  const hidden = Object.getOwnPropertyDescriptor(document, "hidden");
  const intervals = new Map<ReturnType<typeof setInterval>, () => void>();
  Object.defineProperty(document, "hidden", { configurable: true, value: false });
  globalThis.setInterval = ((callback: () => void, delay: number) => {
    const timer = originalSet(() => {}, 60000);
    if (delay === 5000) intervals.set(timer, callback);
    return timer;
  }) as typeof setInterval;
  globalThis.clearInterval = ((timer: ReturnType<typeof setInterval>) => { intervals.delete(timer); originalClear(timer); }) as typeof clearInterval;
  try {
    mount();
    await userEvent.setup().click(screen.getByRole("button", { name: k.history }));
    await screen.findByText(i18n.t("nodes:batchUpgrade.processed", { processed: 0, total: 2 }));
    await waitFor(() => assert.ok(intervals.size > 0, "active batches need a polling fallback"));
    state.runs = [run({ state: "finished", counts: counts({ processed: 2, succeeded: 2, queued: 0, active: 0 }) })];
    await act(async () => { for (const callback of intervals.values()) callback(); });
    await screen.findByText(i18n.t("nodes:batchUpgrade.processed", { processed: 2, total: 2 }));
    await waitFor(() => assert.equal(intervals.size, 0, "terminal results stop the timer"));
  } finally {
    cleanup(); globalThis.setInterval = originalSet; globalThis.clearInterval = originalClear;
    if (hidden) Object.defineProperty(document, "hidden", hidden); else delete (document as unknown as Record<string, unknown>).hidden;
  }
});

test("upgrade API sends explicit wire JSON and encoded item filters with session credentials", async () => {
  const calls: Array<{ path: string; init?: RequestInit }> = [];
  globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    calls.push({ path: String(input), init });
    const result = String(input).includes("/items?") ? { items: [], total: 0, page: 2, page_size: 20 } : preview();
    return new Response(JSON.stringify(result), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  await originals.agentUpgradePreview({ node_uuids: ["node-a", "node-b"] });
  assert.equal(calls[0].path, "/api/v1/agent-upgrades/preview");
  assert.equal(calls[0].init?.credentials, "include");
  assert.equal(new Headers(calls[0].init?.headers).get("Content-Type"), "application/json");
  assert.deepEqual(JSON.parse(String(calls[0].init?.body)), { node_uuids: ["node-a", "node-b"] });
  await originals.agentUpgradeItems(7, 2, 20, "unconfirmed");
  assert.equal(calls[1].path, "/api/v1/agent-upgrades/7/items?page=2&page_size=20&state=unconfirmed");
});


test("empty selection cannot open a preview and history remains available without a standalone card", async () => {
  const state = backend(); mount(true, undefined, []);
  const button = screen.getByRole("button", { name: k.selected }) as HTMLButtonElement;
  assert.equal(button.disabled, true);
  assert.equal(document.querySelector(".ant-card"), null);
  assert.equal(screen.queryByText(k.empty), null);
  fireEvent.click(button);
  assert.equal(state.previews.length, 0);
  await userEvent.setup().click(screen.getByRole("button", { name: k.history }));
  assert.ok(await screen.findByText(k.empty));
});

test("refresh keeps the reviewed selection even when the list selection changes", async () => {
  const state = backend(); const user = userEvent.setup();
  const selected = ["node-0", "node-7"];
  api.agentUpgradePreview = async (body) => {
    state.previews.push(body);
    const nodes = preview().nodes.filter((n) => selected.includes(n.node_uuid));
    return { ...preview(), node_uuids: selected, nodes, counts: { eligible: 2 } };
  };
  let started = 0;
  const view = mount(true, undefined, selected, () => started++);
  await user.click(screen.getByRole("button", { name: k.selected }));
  const modal = await screen.findByRole("dialog"); await within(modal).findByText("fleet-7");
  assert.equal(within(modal).queryByText("fleet-1"), null);
  view.rerender(<I18nextProvider i18n={i18n}><AgentUpgradeManager isAdmin selectedNodeUUIDs={["node-2"]} onStarted={() => started++} /></I18nextProvider>);
  await user.click(within(modal).getByRole("button", { name: k.refreshPreview }));
  await waitFor(() => assert.equal(state.previews.length, 2));
  assert.deepEqual(state.previews, [{ node_uuids: selected }, { node_uuids: selected }]);
  await user.click(within(modal).getByRole("checkbox", { name: k.acknowledge }));
  await user.click(within(modal).getByRole("button", { name: k.start }));
  await waitFor(() => assert.equal(started, 1));
  assert.deepEqual(state.created[0].node_uuids, selected);
});

test("finished result rows distinguish full start and finish times and omit an unused action column", async () => {
  const { formatTimeSec } = await import("../src/i18n/datetime.ts");
  const started = "2026-10-30T03:20:01Z";
  const finished = "2026-10-30T03:20:59Z";
  const state = backend([run({ state: "finished", counts: counts({ total: 2, processed: 2, succeeded: 1, skipped: 1, queued: 0, active: 0 }) })]);
  state.items = [
    item({ node_uuid: "done", name: "completed node", phase: "succeeded", started_at: started, finished_at: finished }),
    item({ node_uuid: "offline", name: "not started node", phase: "skipped", reason: "offline" }),
  ];
  mount(true, 7);
  const name = await screen.findByText("completed node");
  const drawer = screen.getByRole("dialog", { name: i18n.t("nodes:batchUpgrade.runTitle", { id: 7 }) });
  const timeIndex = within(drawer).getAllByRole("columnheader").findIndex((cell) => cell.textContent === k.columns.time);
  assert.ok(timeIndex >= 0);
  const completed = within(name.closest("tr")!).getAllByRole("cell")[timeIndex];
  assert.ok(completed.textContent?.includes(formatTimeSec(started)), "start must include time of day, not only the date");
  assert.ok(completed.textContent?.includes(formatTimeSec(finished)), "finish must preserve its distinct seconds");
  assert.ok(completed.textContent?.includes(i18n.t("nodes:batchUpgrade.startedAt", { at: formatTimeSec(started) })));
  assert.ok(completed.textContent?.includes(i18n.t("nodes:batchUpgrade.finishedAt", { at: formatTimeSec(finished) })));
  const skipped = within(screen.getByText("not started node").closest("tr")!).getAllByRole("cell")[timeIndex];
  assert.equal((skipped.textContent?.match(/—/g) ?? []).length, 2, "missing start and finish each have an explicit placeholder");
});

test("an attempt the system settled shows why in the reader's language, and only an operator's release shows a note", async () => {
  const state = backend([run({ state: "finished", counts: counts({ total: 3, processed: 3, unconfirmed: 3, active: 0, queued: 0 }) })]);
  state.items = [
    item({ node_uuid: "node-a", name: "removed node", phase: "unconfirmed", reason: "confirmation_timeout", verified_at: "2026-10-29T01:00:00Z", verified_by: "system", verification_code: "node_removed", verification_note: "" }),
    item({ node_uuid: "node-c", name: "failed node", phase: "unconfirmed", reason: "confirmation_timeout", verified_at: "2026-10-29T01:00:00Z", verified_by: "system", verification_code: "agent_failed", verification_note: "disk full while unpacking" }),
    item({ node_uuid: "node-b", name: "checked node", phase: "unconfirmed", reason: "confirmation_timeout", verified_at: "2026-10-29T01:00:00Z", verified_by: "admin", verification_code: "operator_verified", verification_note: "process gone, logs clean" }),
  ];
  mount(true, 7);
  await screen.findByText("removed node");
  assert.ok(screen.getByText(/The node was removed from the platform, so no upgrade can still be running\./));
  assert.ok(screen.getByText("Note: process gone, logs clean"));
  assert.ok(screen.getByText("Note: disk full while unpacking"), "an agent's failure detail stays visible");
  // Once settled a row says how it ended, not that its outcome is unknown, and no longer promises a later settlement.
  const removed = screen.getByText("removed node").closest("tr")!;
  assert.ok(within(removed).getByText(k.settled.node_removed));
  assert.ok(within(screen.getByText("failed node").closest("tr")!).getByText(k.settled.agent_failed));
  assert.ok(within(screen.getByText("checked node").closest("tr")!).getByText(k.settled.operator_verified));
  assert.ok(!screen.queryByText(k.phase.unconfirmed));
  assert.ok(!screen.queryByText(k.reason.confirmation_timeout));
});

test("a whole-fleet request previews every node, fixes the range to what came back, and is ignored for a non-admin", async () => {
  const state = backend();
  const all = (request: number, isAdmin = true) => <I18nextProvider i18n={i18n}><AgentUpgradeManager isAdmin={isAdmin} selectedNodeUUIDs={[]} upgradeAllRequest={request} /></I18nextProvider>;
  const view = render(all(0));
  await act(async () => { await Promise.resolve(); });
  assert.deepEqual(state.previews, [], "nothing is previewed until the page asks");
  view.rerender(all(1));
  const dialog = await screen.findByRole("dialog", { name: k.previewTitle });
  assert.deepEqual(state.previews, [{ scope: "all" }]);
  // Previewing again asks about the nodes the first answer listed, as the dialog says.
  await userEvent.click(within(dialog).getByRole("button", { name: k.refreshPreview }));
  await waitFor(() => assert.deepEqual(state.previews.at(-1), { node_uuids: preview().node_uuids }));
  view.unmount();
  state.previews.length = 0;
  const member = render(all(0, false));
  member.rerender(all(1, false));
  await act(async () => { await Promise.resolve(); });
  assert.deepEqual(state.previews, []);
});

test("a whole-fleet request while a batch blocks the fleet shows what blocks it and previews nothing", async () => {
  const blocker = { run_id: 7, node_uuid: "node-a", name: "stuck-node", reason: "unconfirmed" };
  const state = backend([run({ state: "finished", blocked_by_item: blocker as never })]);
  const view = render(<I18nextProvider i18n={i18n}><AgentUpgradeManager isAdmin selectedNodeUUIDs={[]} upgradeAllRequest={0} /></I18nextProvider>);
  // A batch in progress, or a node whose outcome is not known yet, is not an alarm.
  await waitFor(() => assert.ok(!screen.getByRole("button", { name: k.history }).className.includes("dangerous")));
  view.rerender(<I18nextProvider i18n={i18n}><AgentUpgradeManager isAdmin selectedNodeUUIDs={[]} upgradeAllRequest={1} /></I18nextProvider>);
  assert.ok(await screen.findByText(k.blocked.replace("{{name}}", "stuck-node")));
  assert.deepEqual(state.previews, []);
});
