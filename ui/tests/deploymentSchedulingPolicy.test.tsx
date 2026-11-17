import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import * as shapes from "./helpers/deploymentCapabilities.ts";

// A deployment's scheduling policy section lists the current
// policies (rescheduling on node loss, schedule, yield policy), and
// the dialog that edits them. The editors' own validation is in
// deploymentPolicyEditors, the yield policy editor's in deploymentYieldPolicy.

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["deployments", "deploy"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { yieldPolicyApi } = await import("../src/yieldPolicyApi.ts");
const { deploymentScheduleApi } = await import("../src/deploymentScheduleApi.ts");
const { APIError } = await import("../src/apiError.ts");
const { formatTime } = await import("../src/i18n/datetime.ts");
const { default: DeploymentSchedulingPolicy } = await import("../src/pages/DeploymentSchedulingPolicy.tsx");
type Deployment = import("../src/api.ts").Deployment;
type Schedule = import("../src/deploymentScheduleApi.ts").LifecycleSchedule;

const p = locale.deployments.schedulingPolicy;
const a = locale.deployments.automation;
const n = locale.deployments.nodeReplacement;
const original = { ...api };
const originalYieldPolicyApi = { ...yieldPolicyApi };
const originalDeploymentScheduleApi = { ...deploymentScheduleApi };
// The dictionary the server answers GET /scheduling/yield-policies with, literally.
const dictionary = JSON.parse(fs.readFileSync(new URL("./fixtures/yield-policies.json", import.meta.url), "utf8")) as import("../src/yieldPolicyApi.ts").YieldPolicyDictionary;

function deployment(overrides: Record<string, unknown> = {}): Deployment {
  return {
    id: 1, name: "qwen", model_id: 1, model_name: "m", served_name: "m", engine: "vllm", node_uuid: "n1",
    desired_state: "active", runtime_state: "running", sleepable: true, replicas: 2, max_replicas: 32, nnodes: 1,
    health: { status: "off", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [] },
    template_deviated: false, tokenize_enabled: false, created_at: "2026-10-01T00:00:00Z",
    node_replacement: { after_sec: 600 },
    ...shapes.sleepableSingleNode,
    ...overrides,
  } as unknown as Deployment;
}

const nightly: Schedule = { id: 5, action: "sleep", weekdays: [1, 2, 3, 4, 5], time: "22:00", timezone: "", sleep_level: 2, enabled: true, next_at: "2026-10-30T14:00:00Z" };
const morning: Schedule = { id: 6, action: "wake", weekdays: [1, 2, 3, 4, 5], time: "08:30", timezone: "", enabled: true, next_at: "2026-10-31T00:30:00Z" };
const paused: Schedule = { id: 7, action: "wake", weekdays: [0], time: "01:00", timezone: "", enabled: false, next_at: "2026-10-29T01:00:00Z" };

// What the server holds: the stubs write here and the host reloads from it, as
// the deployment page does after onChanged.
let current: Deployment;
let schedules: Schedule[];
let role = "admin";
let changed = 0;

function stub() {
  schedules = [];
  role = "admin";
  changed = 0;
  api.me = async () => ({ id: 1, username: "u", role }) as Awaited<ReturnType<typeof api.me>>;
  deploymentScheduleApi.list = async () => ({ items: schedules, total: schedules.length });
  deploymentScheduleApi.create = async (_id, body) => {
    const row = { ...body, id: 20 + schedules.length, enabled: body.enabled ?? true, next_at: "2026-10-30T15:00:00Z" } as Schedule;
    schedules = [...schedules, row];
    return row;
  };
  yieldPolicyApi.set = async (_id, body) => {
    // For these tests the policy the server holds is what was sent, whole; the
    // editor's own save path has its tests in deploymentYieldPolicySave.
    current = { ...current, yield_policy: body };
    return body;
  };
  yieldPolicyApi.dictionary = async () => structuredClone(dictionary);
  api.setNodeReplacement = async (_id, body) => { current = { ...current, node_replacement: body }; return body; };
}

function Host() {
  const [dep, setDep] = useState(current);
  return <DeploymentSchedulingPolicy dep={dep} onChanged={() => { changed += 1; setDep(current); }} />;
}

function mount(dep = deployment()) {
  current = dep;
  render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
  return userEvent.setup();
}

const y = locale.deployments.yieldPolicy;
const failover = (value: string) => [n.title, value];
const schedule = (value: string) => [a.schedule.title, value];
const level = (value: string) => [y.summaryLabel, value];

async function expectPolicyRows(...expected: string[][]) {
  await waitFor(() => {
    const region = screen.getByLabelText(p.title, { selector: "section" });
    assert.equal(within(region).getByRole("rowheader").textContent, p.title);
    const summary = expected.map(([label, value]) => i18n.t("deployments:schedulingPolicy.item", { label, value })).join(p.separator);
    assert.equal(within(region).getByRole("cell").textContent?.trim().replace(/\s*Edit$/, ""), summary);
  });
}
const labelled = (scope: ReturnType<typeof within>, text: string) => scope.getByText(text).closest("button") as HTMLButtonElement;
const dialogs = () => screen.getAllByRole("dialog");

afterEach(() => { cleanup(); Object.assign(api, original); Object.assign(yieldPolicyApi, originalYieldPolicyApi); Object.assign(deploymentScheduleApi, originalDeploymentScheduleApi); });
after(() => closeChannels());

test("the detail shows each applicable policy and its current value before editing", async () => {
  stub();
  mount();
  await expectPolicyRows(failover("10 min"), schedule(p.none), level(y.policy.name.never));
  assert.ok(screen.getByRole("region", { name: p.title }));
});

test("the schedule part counts the orders and names the next enabled one, with what it does", async () => {
  stub();
  schedules = [paused, morning, nightly];
  mount();
  const count = i18n.t("deployments:schedulingPolicy.scheduleCount", { count: 3 });
  const next = i18n.t("deployments:schedulingPolicy.scheduleNext", {
    time: formatTime(nightly.next_at),
    order: i18n.t("deployments:automation.schedule.actionText.sleep", { level: 2 }),
  });
  await expectPolicyRows(failover("10 min"), schedule(count + next), level(y.policy.name.never));
  cleanup();

  schedules = [paused];
  mount();
  await expectPolicyRows(failover("10 min"), schedule(i18n.t("deployments:schedulingPolicy.scheduleCount", { count: 1 }) + p.scheduleAllOff), level(y.policy.name.never));
});

test("rescheduling on node loss reads Off at 0, and long waits read in hours and days", async () => {
  stub();
  mount(deployment({ node_replacement: { after_sec: 0 } }));
  await expectPolicyRows(failover(p.off), schedule(p.none), level(y.policy.name.never));
  cleanup();
  mount(deployment({ node_replacement: { after_sec: 7200 } }));
  await expectPolicyRows(failover("2 h"), schedule(p.none), level(y.policy.name.never));
  cleanup();
  // A deployment with no saved setting waits the default 10 minutes.
  mount(deployment({ node_replacement: undefined }));
  await expectPolicyRows(failover("10 min"), schedule(p.none), level(y.policy.name.never));
});

test("the yield policy is summed up by its name and the parameters that matter; a deployment that cannot sleep has none to show", async () => {
  stub();
  mount(deployment({ yield_policy: { policy: "idle", idle_sec: 60, sleep_level: "auto", hold_max_sec: 300 } }));
  await expectPolicyRows(failover("10 min"), schedule(p.none), level("Yield when idle · yields after 1 min idle"));
  cleanup();
  mount(deployment({ yield_policy: { policy: "timeslice", idle_sec: 30, sleep_level: "l1", max_serve_sec: 120, unfinished: "abort", hold_max_sec: 600 } }));
  await expectPolicyRows(failover("10 min"), schedule(p.none), level("Yield on a turn · idle 30 s / up to 2 min per turn"));
  cleanup();
  mount(deployment({ yield_policy: { policy: "anytime", idle_sec: 30, sleep_level: "auto", unfinished: "abort", hold_max_sec: 3600 } }));
  await expectPolicyRows(failover("10 min"), schedule(p.none), level("Yield anytime · idle 30 s"));
  cleanup();
  mount(deployment({ sleepable: false, ...shapes.notSleepableSingleNode }));
  await expectPolicyRows(failover("10 min"), schedule(p.none));
  assert.equal(screen.queryByText(new RegExp(y.summaryLabel)), null);
});

test("the schedule part says it is loading, and that it failed when the list cannot be read", async () => {
  stub();
  deploymentScheduleApi.list = () => new Promise(() => {});
  mount();
  await expectPolicyRows(failover("10 min"), schedule(p.loading), level(y.policy.name.never));
  cleanup();
  deploymentScheduleApi.list = async () => { throw new APIError("DEPLOYMENT_NOT_FOUND", {}, "no such deployment", 404); };
  mount();
  await expectPolicyRows(failover("10 min"), schedule(p.loadFailed), level(y.policy.name.never));
});

test("a read-only user sees the summary and no edit action; so does the card of a deployment being deleted", async () => {
  stub();
  role = "viewer";
  mount();
  await expectPolicyRows(failover("10 min"), schedule(p.none), level(y.policy.name.never));
  await waitFor(() => assert.equal(screen.queryByRole("button", { name: p.edit }), null));
  cleanup();
  role = "admin";
  mount(deployment({ desired_state: "deleting" }));
  assert.equal(screen.queryByRole("region", { name: p.title }), null);
  assert.equal(screen.queryByText(p.title), null);
});

test("the edit action opens the dialog from the keyboard, with a tab for each policy", async () => {
  stub();
  const user = mount();
  const edit = await screen.findByRole("button", { name: p.edit });
  edit.focus();
  await user.keyboard("{Enter}");
  const dialog = within(await screen.findByRole("dialog"));
  const tabs = dialog.getAllByRole("tab").map((tab) => tab.textContent);
  assert.deepEqual(tabs, [n.title, a.schedule.title, y.title]);
  // The long explanations are not on the page any more.
  assert.equal(screen.queryByText(/回来|returns before the moved replica/), null);
  fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape", keyCode: 27 });
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  edit.focus();
  await user.keyboard(" ");
  await screen.findByRole("dialog");
  await user.click(screen.getByRole("button", { name: p.close }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  cleanup();

  const user2 = mount(deployment({ sleepable: false, ...shapes.notSleepableSingleNode }));
  await user2.click(await screen.findByRole("button", { name: p.edit }));
  const plain = within(await screen.findByRole("dialog"));
  assert.deepEqual(plain.getAllByRole("tab").map((tab) => tab.textContent), [n.title, a.schedule.title]);
});

test("saving the wait on the first tab calls the node replacement API and updates the summary", async () => {
  stub();
  const saved: unknown[] = [];
  const set = api.setNodeReplacement;
  api.setNodeReplacement = async (id, body) => { saved.push(body); return set(id, body); };
  const user = mount();
  await user.click(await screen.findByRole("button", { name: p.edit }));
  const dialog = within(await screen.findByRole("dialog"));
  fireEvent.change(dialog.getByLabelText(n.minutes), { target: { value: "30" } });
  await user.click(labelled(dialog, n.save));
  await waitFor(() => assert.deepEqual(saved, [{ after_sec: 1800 }]));
  await screen.findByText(n.saved);
  assert.equal(changed, 1);
  await expectPolicyRows(failover("30 min"), schedule(p.none), level(y.policy.name.never));
  // Turning it off.
  fireEvent.change(dialog.getByLabelText(n.minutes), { target: { value: "0" } });
  await user.click(labelled(dialog, n.save));
  await expectPolicyRows(failover(p.off), schedule(p.none), level(y.policy.name.never));
});

test("saving the yield policy on its tab calls the yield policy API and updates the summary", async () => {
  stub();
  const saved: unknown[] = [];
  const set = yieldPolicyApi.set;
  yieldPolicyApi.set = async (id, body) => { saved.push(body); return set(id, body); };
  const user = mount();
  await user.click(await screen.findByRole("button", { name: p.edit }));
  await user.click(within(await screen.findByRole("dialog")).getByRole("tab", { name: y.title }));
  const section = within(await screen.findByRole("region", { name: y.title }));
  await user.click(await section.findByRole("combobox", { name: y.policyLabel }));
  await user.click(await screen.findByText(`${y.policy.name.idle} - ${y.policy.description.idle}`));
  await user.click(labelled(section, a.save));
  await waitFor(() => assert.deepEqual(saved, [{ policy: "idle", idle_sec: 60, sleep_level: "auto", hold_max_sec: 300 }]));
  await expectPolicyRows(failover("10 min"), schedule(p.none), level("Yield when idle · yields after 1 min idle"));
  assert.equal(changed, 1);
});

test("adding an order on the schedule tab reloads the list and the summary", async () => {
  stub();
  const user = mount();
  await user.click(await screen.findByRole("button", { name: p.edit }));
  await user.click(within(await screen.findByRole("dialog")).getByRole("tab", { name: a.schedule.title }));
  const section = within(await screen.findByRole("region", { name: a.schedule.title }));
  await section.findByText(a.schedule.empty);
  await user.click(section.getByRole("button", { name: a.schedule.add }));
  const form = within(dialogs().at(-1) as HTMLElement);
  fireEvent.change(form.getByLabelText(a.schedule.field.time), { target: { value: "23:30" } });
  await user.click(form.getByRole("button", { name: a.schedule.save }));
  await section.findByText(`23:30 ${a.schedule.serverZone}`);
  const next = i18n.t("deployments:schedulingPolicy.scheduleNext", {
    time: formatTime("2026-10-30T15:00:00Z"),
    order: i18n.t("deployments:automation.schedule.actionText.sleep", { level: 2 }),
  });
  await expectPolicyRows(failover("10 min"), schedule(i18n.t("deployments:schedulingPolicy.scheduleCount", { count: 1 }) + next), level(y.policy.name.never));
});

test("a refused save is shown and leaves the summary as it was", async () => {
  stub();
  api.setNodeReplacement = async () => { throw new APIError("NODE_REPLACEMENT_INVALID", { minimum: 60, maximum: 604800 }, "out of range", 400); };
  yieldPolicyApi.set = async () => { throw new APIError("SLEEP_NOT_ENABLED", {}, "sleep mode was not enabled", 400); };
  const user = mount();
  await user.click(await screen.findByRole("button", { name: p.edit }));
  const dialog = within(await screen.findByRole("dialog"));
  fireEvent.change(dialog.getByLabelText(n.minutes), { target: { value: "0.5" } });
  fireEvent.change(dialog.getByLabelText(n.minutes), { target: { value: "1" } });
  await user.click(labelled(dialog, n.save));
  await screen.findByText(i18n.t("deployments:nodeReplacement.saveFailed", { detail: i18n.t("common:apiError.NODE_REPLACEMENT_INVALID", { minimum: 60, maximum: 604800 }) }));
  await user.click(dialog.getByRole("tab", { name: y.title }));
  const section = within(await screen.findByRole("region", { name: y.title }));
  await user.click(await section.findByRole("combobox", { name: y.policyLabel }));
  await user.click(await screen.findByText(`${y.policy.name.anytime} - ${y.policy.description.anytime}`));
  await user.click(labelled(section, a.save));
  await screen.findByText(i18n.t("deployments:yieldPolicy.saveFailed", { detail: i18n.t("common:apiError.SLEEP_NOT_ENABLED") }));
  assert.equal(changed, 0);
  await expectPolicyRows(failover("10 min"), schedule(p.none), level(y.policy.name.never));
});
