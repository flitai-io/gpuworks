import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["settings", "common"]);
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const { default: TelemetryCard } = await import("../src/pages/settings/TelemetryCard.tsx");
type TelemetryExport = import("../src/telemetryApi.ts").TelemetryExport;
type TelemetryExportUpdate = import("../src/telemetryApi.ts").TelemetryExportUpdate;
type TelemetryExportTest = import("../src/telemetryApi.ts").TelemetryExportTest;

const tl = locale.settings.telemetry as Record<string, string> & { state: Record<string, string> };
const original = {
  telemetryExport: apiModule.api.telemetryExport,
  updateTelemetryExport: apiModule.api.updateTelemetryExport,
  testTelemetryExport: apiModule.api.testTelemetryExport,
  me: apiModule.api.me,
};

const idle = { active: false, traces_url: "", exported: 0, dropped: 0, failed: 0, queued: 0, last_error: "", last_error_at: null, last_export_at: null, setting_error: "" };

function exported(over: Partial<TelemetryExport> = {}): TelemetryExport {
  return { enabled: false, endpoint: "", headers: [], sample_ratio: 1, trace_link: "", status: idle, ...over };
}

function stub({ role = "admin", load }: { role?: string; load: () => Promise<TelemetryExport> }) {
  const updates: TelemetryExportUpdate[] = [];
  const tests: TelemetryExportUpdate[] = [];
  let saveError: Error | null = null;
  let testAnswer: TelemetryExportTest | Error = { ok: true, trace_id: "0199e5fa257b00000000000000000001", traces_url: "http://c:4318/v1/traces", error: "" };
  let loads = 0;
  apiModule.api.me = (async () => ({ id: 1, username: role, role })) as typeof apiModule.api.me;
  apiModule.api.telemetryExport = async () => {
    loads++;
    return load();
  };
  apiModule.api.updateTelemetryExport = async (body) => {
    updates.push(body);
    if (saveError) throw saveError;
    return exported({
      enabled: body.enabled, endpoint: body.endpoint, sample_ratio: body.sample_ratio, trace_link: body.trace_link,
      headers: body.headers.map((h) => ({ name: h.name })),
      status: { ...idle, active: body.enabled, traces_url: body.enabled ? `${body.endpoint}/v1/traces` : "" },
    });
  };
  apiModule.api.testTelemetryExport = async (body) => {
    tests.push(body);
    if (testAnswer instanceof Error) throw testAnswer;
    return testAnswer;
  };
  return {
    updates, tests, loads: () => loads,
    failSaveWith: (e: Error) => { saveError = e; },
    answerTest: (a: TelemetryExportTest | Error) => { testAnswer = a; },
  };
}

function renderCard() {
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><TelemetryCard /></I18nextProvider>);
  return user;
}
const saveButton = () => screen.getByRole("button", { name: locale.settings.common.save });
const endpoint = () => screen.getByRole("textbox", { name: tl.endpoint }) as HTMLInputElement;

afterEach(() => {
  cleanup();
  Object.assign(apiModule.api, original);
});
after(() => closeChannels());

test("an admin turns export on with a collector and a token, and saves it", async () => {
  const api = stub({ load: async () => exported() });
  const user = renderCard();
  await waitFor(() => assert.equal(endpoint().disabled, false));
  assert.ok(screen.getByText(tl.state.off));
  await user.click(screen.getByRole("switch", { name: tl.enabled }));
  await user.type(endpoint(), "http://otel-collector:4318");
  await user.click(screen.getByRole("button", { name: new RegExp(tl.addHeader) }));
  await user.type(screen.getByRole("textbox", { name: "Name of header 1" }), "Authorization");
  await user.type(screen.getByLabelText("Value of header 1"), "Bearer t0k");
  const ratio = screen.getByRole("spinbutton", { name: tl.sampleRatio });
  await user.clear(ratio);
  await user.type(ratio, "25");
  await user.type(screen.getByRole("textbox", { name: tl.traceLink }), "https://grafana/t/{{trace_id}");
  assert.equal(api.updates.length, 0, "editing alone does not save");
  await user.click(saveButton());
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.equal(api.updates.length, 1));
  assert.deepEqual(api.updates[0], {
    enabled: true, endpoint: "http://otel-collector:4318", sample_ratio: 0.25, trace_link: "https://grafana/t/{trace_id}",
    headers: [{ name: "Authorization", value: "Bearer t0k" }],
  });
  assert.ok(await screen.findByText(tl.saved));
  assert.ok(await screen.findByText(tl.state.on));
  assert.ok(screen.getByText("http://otel-collector:4318/v1/traces"));
  // The saved header comes back by name only; its value field is empty and says it is kept.
  await waitFor(() => assert.equal((screen.getByLabelText("Value of header 1") as HTMLInputElement).value, ""));
  assert.equal((screen.getByLabelText("Value of header 1") as HTMLInputElement).placeholder, tl.headerKept);
});

test("a stored header left empty is kept, a removed one is dropped", async () => {
  const api = stub({ load: async () => exported({ enabled: true, endpoint: "http://c:4318", headers: [{ name: "Authorization" }, { name: "X-Scope-OrgID" }] }) });
  const user = renderCard();
  await waitFor(() => assert.equal(endpoint().value, "http://c:4318"));
  await user.click(screen.getByRole("button", { name: "Remove header 2" }));
  await user.click(saveButton());
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.equal(api.updates.length, 1));
  assert.deepEqual(api.updates[0].headers, [{ name: "Authorization", value: null }]);
});

test("a test sends the draft without saving and shows what the collector said", async () => {
  const api = stub({ load: async () => exported({ endpoint: "http://c:4318", headers: [{ name: "Authorization" }] }) });
  const user = renderCard();
  await waitFor(() => assert.equal(endpoint().value, "http://c:4318"));
  await user.click(screen.getByRole("button", { name: new RegExp(tl.test) }));
  assert.ok(await screen.findByText(tl.testOK));
  assert.ok(screen.getByText("0199e5fa257b00000000000000000001"));
  assert.deepEqual(api.tests[0], { enabled: false, endpoint: "http://c:4318", headers: [{ name: "Authorization", value: null }], sample_ratio: 1, trace_link: "" });
  assert.equal(api.updates.length, 0);

  api.answerTest({ ok: false, trace_id: "x", traces_url: "http://c:4318/v1/traces", error: "collector answered 401: bad token" });
  await user.click(screen.getByRole("button", { name: new RegExp(tl.test) }));
  assert.ok(await screen.findByText(tl.testFailed));
  assert.ok(screen.getByText("collector answered 401: bad token"));

  api.answerTest(new APIError("TELEMETRY_ENDPOINT_INVALID", undefined, "endpoint must be an http(s) URL", 400));
  await user.click(screen.getByRole("button", { name: new RegExp(tl.test) }));
  assert.ok(await screen.findByText(/Invalid collector address/));
});

test("the test is off without an endpoint", async () => {
  stub({ load: async () => exported() });
  renderCard();
  await waitFor(() => assert.equal(endpoint().disabled, false));
  assert.equal((screen.getByRole("button", { name: new RegExp(tl.test) }) as HTMLButtonElement).disabled, true);
});

test("a refused save names the field from the server's code", async () => {
  const api = stub({ load: async () => exported() });
  api.failSaveWith(new APIError("TELEMETRY_HEADER_VALUE_REQUIRED", { name: "X-Token" }, "header X-Token needs a value", 400));
  const user = renderCard();
  await waitFor(() => assert.equal(endpoint().disabled, false));
  await user.click(saveButton());
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  assert.ok(await screen.findByText(/Header X-Token has no value yet/));
});

test("the status shows the counts, the last error and an unusable stored setting", async () => {
  const at = Date.UTC(2026, 9, 15, 8, 0, 0);
  stub({ load: async () => exported({
    enabled: true, endpoint: "http://c:4318",
    status: { ...idle, active: true, traces_url: "http://c:4318/v1/traces", exported: 120, dropped: 3, failed: 7, queued: 2,
      last_error: "collector answered 503: busy", last_error_at: at + 1000, last_export_at: at, setting_error: "" },
  }) });
  renderCard();
  assert.ok(await screen.findByText("exported 120 · dropped 3 · failed 7 · queued 2"));
  assert.ok(screen.getByText(/collector answered 503: busy/));
  assert.ok(screen.getByText(tl.state.failing), "an error after the last success reads as failing");
  cleanup();

  stub({ load: async () => exported({ enabled: true, status: { ...idle, setting_error: "telemetry export header \"A\": decrypt failed" } }) });
  renderCard();
  assert.ok(await screen.findByText(tl.settingError));
  assert.ok(screen.getByText(tl.state.broken));
});

test("a failed load is reported and can be retried", async () => {
  let fail = true;
  const api = stub({ load: async () => {
    if (fail) throw new Error("settings offline");
    return exported({ endpoint: "http://c" });
  } });
  const user = renderCard();
  assert.ok(await screen.findByText(/settings offline/));
  assert.equal((saveButton() as HTMLButtonElement).disabled, true);
  fail = false;
  await user.click(screen.getByRole("button", { name: locale.common.actions.retry }));
  await waitFor(() => assert.equal(endpoint().value, "http://c"));
  assert.equal(api.loads(), 2);
});

test("a non-admin sees the setting but cannot change or test it", async () => {
  stub({ role: "user", load: async () => exported({ endpoint: "http://c" }) });
  renderCard();
  assert.ok(await screen.findByText(tl.adminOnly));
  await waitFor(() => assert.equal(endpoint().value, "http://c"));
  assert.equal(endpoint().disabled, true);
  assert.equal((saveButton() as HTMLButtonElement).disabled, true);
  assert.equal((screen.getByRole("button", { name: new RegExp(tl.test) }) as HTMLButtonElement).disabled, true);
});

test("the switch and the save work from the keyboard", async () => {
  const api = stub({ load: async () => exported({ endpoint: "http://c" }) });
  const user = renderCard();
  await waitFor(() => assert.equal(endpoint().disabled, false));
  screen.getByRole("switch", { name: tl.enabled }).focus();
  await user.keyboard(" ");
  saveButton().focus();
  await user.keyboard("{Enter}");
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.equal(api.updates.length, 1));
  assert.equal(api.updates[0].enabled, true);
});
