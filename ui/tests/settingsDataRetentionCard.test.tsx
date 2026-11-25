import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["settings"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { default: DataRetentionCard, shortened } = await import("../src/pages/settings/DataRetentionCard.tsx");
type Retention = import("../src/api.ts").DataRetention;
type View = import("../src/api.ts").DataRetentionView;
type Status = import("../src/api.ts").DataRetentionStatus;

const d = locale.settings.dataRetention;
const original = { dataRetention: api.dataRetention, putDataRetention: api.putDataRetention, dataRetentionStatus: api.dataRetentionStatus };
afterEach(() => { cleanup(); Object.assign(api, original); });
after(() => closeChannels());

const value: Retention = {
  metrics_hours: 72, logs_hours: 72, request_log_days: 7, request_log_max_mb: 2048,
  usage_hourly_days: 31, usage_daily_days: 400, stats_hourly_days: 31, stats_daily_days: 400,
  quality: true, gpu: true, gpu_time: true, time_zone: "Asia/Shanghai",
};
const limits = {
  metrics_hours: { min: 24, max: 720 }, logs_hours: { min: 24, max: 720 }, request_log_days: { min: 1, max: 90 },
  request_log_max_mb: { min: 512, max: 51200 }, usage_hourly_days: { min: 7, max: 93 }, usage_daily_days: { min: 31, max: 1825 },
  stats_hourly_days: { min: 7, max: 93 }, stats_daily_days: { min: 31, max: 1825 },
};
const status: Status = {
  stores: [
    { kind: "metrics", file: "metrics.db", file_bytes: 3 * 1024 ** 3, hourly_rows: null, daily_rows: null, earliest_ms: Date.UTC(2026, 9, 19) },
    { kind: "usage", file: "main", file_bytes: null, hourly_rows: 1234, daily_rows: 56, earliest_ms: null },
    { kind: "gpu", file: "stats.db", file_bytes: 40 * 1024 ** 2, hourly_rows: 99, daily_rows: 3, earliest_ms: null },
    { kind: "scheduling", file: "stats.db", file_bytes: 40 * 1024 ** 2, hourly_rows: 4321, daily_rows: 7, earliest_ms: null },
  ],
  live_since_ms: 1, backfill: { usage_done: true, usage_through_ms: 0, quality_done: true, quality_through_ms: 0 },
  retain: { at_ms: Date.UTC(2026, 9, 22, 12) }, rollup: { at_ms: 0 }, usage_flush: { at_ms: 0, rows: 0, ms: 0 },
};

function setup(load: View | Error = { value, limits, server_time_zone: "Asia/Shanghai" }) {
  const puts: Retention[] = [];
  api.dataRetention = async () => { if (load instanceof Error) throw load; return structuredClone(load); };
  api.dataRetentionStatus = async () => status;
  api.putDataRetention = async (v) => { puts.push(v); return { value: v, limits, server_time_zone: "Asia/Shanghai" }; };
  render(<I18nextProvider i18n={i18n}><DataRetentionCard /></I18nextProvider>);
  return { puts, user: userEvent.setup() };
}

test("shortened lists only the values a draft lowers", () => {
  assert.deepEqual(shortened(value, { ...value, logs_hours: 48, usage_daily_days: 800, gpu: false }), ["logs_hours"]);
  assert.deepEqual(shortened(value, value), []);
});

test("a load error is shown instead of the form", async () => {
  setup(new Error("server unreachable"));
  assert.ok(await screen.findByText(/server unreachable/));
  assert.equal(screen.queryByRole("spinbutton"), null);
});

test("what each store holds is shown beside its retention, with the bounds the server sent", async () => {
  setup();
  assert.ok(await screen.findByText(d.groups.usage.title));
  assert.ok(await screen.findByText(i18n.t("settings:dataRetention.rows", { hourly: (1234).toLocaleString(), daily: "56" }), { exact: false }));
  assert.equal(screen.getAllByText(i18n.t("settings:dataRetention.range.hours", { min: 24, max: 720 })).length, 2);
  assert.ok(screen.getByText(d.billing));
  // The scheduling activity is always counted: its rows show, with no switch.
  assert.ok(screen.getByText(d.alwaysOn.scheduling));
  assert.ok(screen.getByText(i18n.t("settings:dataRetention.rows", { hourly: (4321).toLocaleString(), daily: "7" }), { exact: false }));
  assert.equal(screen.queryByRole("switch", { name: d.alwaysOn.scheduling }), null);
  assert.equal((screen.getByLabelText(d.fields.usage_daily_days) as HTMLInputElement).value, "400");
  // Nothing changed: nothing to save.
  assert.equal((screen.getByRole("button", { name: locale.settings.common.save }) as HTMLButtonElement).disabled, true);
});

test("lengthening saves the whole setting at once", async () => {
  const { puts, user } = setup();
  const input = await screen.findByLabelText(d.fields.usage_daily_days);
  await user.clear(input);
  await user.type(input, "730");
  await user.click(screen.getByRole("switch", { name: d.switches.gpu }));
  await user.click(screen.getByRole("button", { name: locale.settings.common.save }));
  await waitFor(() => assert.equal(puts.length, 1));
  assert.deepEqual(puts[0], { ...value, usage_daily_days: 730, gpu: false });
  assert.ok(await screen.findByText(d.saved));
});

test("an emptied value keeps save disabled", async () => {
  const { puts, user } = setup();
  const input = await screen.findByLabelText(d.fields.metrics_hours);
  await user.clear(input);
  await user.tab();
  const save = screen.getByRole("button", { name: locale.settings.common.save }) as HTMLButtonElement;
  await waitFor(() => assert.equal(save.disabled, true));
  assert.equal(puts.length, 0);
});

test("shortening asks first, names what shrinks, and cancelling saves nothing", async () => {
  const { puts, user } = setup();
  const input = await screen.findByLabelText(d.fields.logs_hours);
  await user.clear(input);
  await user.type(input, "48");
  await user.click(screen.getByRole("button", { name: locale.settings.common.save }));
  const dialog = await screen.findByRole("dialog");
  assert.ok(within(dialog).getAllByText(d.shrinkTitle).length > 0);
  assert.ok(within(dialog).getByText(new RegExp(d.fields.logs_hours)));
  await user.click(within(dialog).getAllByRole("button", { name: locale.settings.common.cancel }).at(-1)!);
  assert.equal(puts.length, 0);
  await user.click(screen.getByRole("button", { name: locale.settings.common.save }));
  // antd keeps a closing dialog in the page through its exit animation.
  await waitFor(() => assert.ok(screen.getAllByRole("button", { name: d.shrinkOk }).length > 0));
  await user.click(screen.getAllByRole("button", { name: d.shrinkOk }).at(-1)!);
  await waitFor(() => assert.equal(puts.length, 1));
  assert.equal(puts[0].logs_hours, 48);
});

test("reset puts the saved values back", async () => {
  const { user } = setup();
  const input = await screen.findByLabelText(d.fields.request_log_days);
  await user.clear(input);
  await user.type(input, "30");
  await user.click(screen.getByRole("button", { name: d.reset }));
  assert.equal((screen.getByLabelText(d.fields.request_log_days) as HTMLInputElement).value, "7");
});
