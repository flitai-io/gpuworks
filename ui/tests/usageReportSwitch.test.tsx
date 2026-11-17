import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["settings"]);
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const apiModule = await import("../src/api.ts");
const { APIError } = await import("../src/apiError.ts");
const UsageReportSwitch = (await import("../src/pages/settings/UsageReportSwitch.tsx")).default;
type UsageReportStatus = import("../src/api.ts").UsageReportStatus;

const u = locale.settings.usageReport as Record<string, string>;
const original = { usageReport: apiModule.api.usageReport, setUsageReport: apiModule.api.setUsageReport };
afterEach(() => { cleanup(); Object.assign(apiModule.api, original); });
after(() => closeChannels());

function stub(status: UsageReportStatus | Error) {
  const saves: boolean[] = [];
  let current = status;
  let saveError: Error | null = null;
  let loads = 0;
  apiModule.api.usageReport = async () => { loads++; if (current instanceof Error) throw current; return current; };
  apiModule.api.setUsageReport = async (enabled: boolean) => {
    saves.push(enabled);
    if (saveError) throw saveError;
    current = { ...(current as UsageReportStatus), enabled };
    return current;
  };
  return { saves, loads: () => loads, failSaveWith: (error: Error) => { saveError = error; } };
}

function renderSwitch() {
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><UsageReportSwitch /></I18nextProvider>);
  return user;
}

const toggle = () => screen.getByRole("switch", { name: u.title }) as HTMLButtonElement;

test("a switch with its name and the hint on the icon, and nothing else", async () => {
  stub({ enabled: true, editable: true, last_day: "2026-10-21" });
  renderSwitch();
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  await userEvent.hover(screen.getByRole("button"));
  assert.equal((await screen.findByRole("tooltip")).textContent, u.hint);
  assert.equal(screen.queryByText(/2026-10-21/), null);
});

test("an administrator switches the program off from the keyboard and back on", async () => {
  const api = stub({ enabled: true, editable: true });
  const user = renderSwitch();
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  toggle().focus();
  await user.keyboard(" ");
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "false"));
  await user.click(toggle());
  await waitFor(() => assert.deepEqual(api.saves, [false, true]));
});

test("a failed save leaves the switch where it was", async () => {
  const api = stub({ enabled: true, editable: true });
  api.failSaveWith(new APIError("USAGE_REPORT_READ_ONLY", undefined, "the server was started without server.yaml", 409));
  const user = renderSwitch();
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  await user.click(toggle());
  await waitFor(() => assert.deepEqual(api.saves, [false]));
  assert.equal(toggle().getAttribute("aria-checked"), "true");
});

test("without server.yaml the switch cannot be changed and the hint says where", async () => {
  stub({ enabled: true, editable: false });
  renderSwitch();
  await waitFor(() => assert.ok(toggle().hasAttribute("data-disabled")));
  await userEvent.hover(screen.getByRole("button"));
  assert.equal((await screen.findByRole("tooltip")).textContent, `${u.hint} ${u.readOnly}`);
});

test("a server that cannot say shows no switch", async () => {
  const api = stub(new Error("network down"));
  renderSwitch();
  await waitFor(() => assert.equal(api.loads(), 1));
  assert.equal(screen.queryByRole("switch"), null);
});
