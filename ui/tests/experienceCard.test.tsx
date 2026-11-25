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
const { default: ExperienceCard } = await import("../src/pages/settings/ExperienceCard.tsx");
const m = locale.settings.experience as Record<string, string>;
const errors = locale.common.apiError as Record<string, string>;

type Status = import("../src/api.ts").ExperienceStatus;
const original = {
  experience: apiModule.api.experience,
  setExperienceEnabled: apiModule.api.setExperienceEnabled,
  resetExperience: apiModule.api.resetExperience,
  rotateExperiencePassword: apiModule.api.rotateExperiencePassword,
};
const off: Status = { enabled: false, ready: false, port: 8810, user: "admin" };
const ready: Status = { enabled: true, ready: true, port: 8810, user: "admin", password: "5d706bcd0e1b2eaa" };

// The stub behaves like the console: `load` is what it reports until an action
// succeeds, and from then on every status read (the card re-polls on a timer
// and on focus) returns what that action left behind. A stub that always
// answered with the original status would overwrite the result of a reset or
// password change with a stale one whenever a poll landed after it.
function stub(load: () => Promise<Status>) {
  const calls: string[] = [];
  let fail: Error | null = null;
  let current: Status | null = null;
  const answer = (name: string, s: Status) => async () => {
    calls.push(name);
    if (fail) throw fail;
    current = s;
    return s;
  };
  apiModule.api.experience = async () => current ?? load();
  apiModule.api.setExperienceEnabled = async (on: boolean) => answer(on ? "on" : "off", on ? ready : off)();
  apiModule.api.resetExperience = answer("reset", ready);
  apiModule.api.rotateExperiencePassword = answer("password", { ...ready, password: "rotated00000000" });
  return { calls, failWith: (e: Error) => { fail = e; } };
}

function renderCard() {
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><ExperienceCard /></I18nextProvider>);
  return user;
}
const toggle = () => screen.getByRole("switch", { name: m.title });
const confirm = () => screen.getByRole("button", { name: /^(OK|确 ?定)$/ });

afterEach(() => {
  cleanup();
  Object.assign(apiModule.api, original);
});
after(() => closeChannels());

test("a server without the environment shows no card", async () => {
  let loaded = false;
  stub(async () => { loaded = true; throw new APIError("EXPERIENCE_UNAVAILABLE", undefined, "none", 404); });
  renderCard();
  await waitFor(() => assert.ok(loaded));
  assert.equal(screen.queryByText(m.title), null);
  assert.equal(screen.queryByRole("switch"), null);
});

test("off, the card is its title and switch; turning it on starts the environment", async () => {
  const api = stub(async () => off);
  const user = renderCard();
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "false"));
  assert.ok(screen.getByText(m.title));
  assert.equal(screen.queryByText(m.address), null);
  assert.equal(screen.queryByRole("button", { name: m.reset }), null);
  await user.click(toggle());
  await waitFor(() => assert.deepEqual(api.calls, ["on"]));
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  const link = screen.getByRole("link");
  assert.equal(link.getAttribute("href"), `${window.location.protocol}//${window.location.hostname}:8810/`);
  assert.equal(link.getAttribute("target"), "_blank");
  assert.ok(screen.getByText("admin"));
  assert.ok(screen.getByText("5d706bcd0e1b2eaa"));
  assert.ok(screen.getByText(m.portHint.replace("{{port}}", "8810")));
});

test("while the environment starts the card says so and shows no account yet", async () => {
  stub(async () => ({ ...off, enabled: true, password: "5d706bcd0e1b2eaa" }));
  renderCard();
  assert.ok(await screen.findByText(m.starting));
  assert.ok(toggle().classList.contains("ant-switch-loading"));
  assert.equal(screen.queryByText("5d706bcd0e1b2eaa"), null);
});

test("an environment that stopped by itself says why", async () => {
  stub(async () => ({ ...off, error: "exit status 1" }));
  renderCard();
  assert.ok(await screen.findByText(m.exited.replace("{{error}}", "exit status 1")));
});

test("reset and a new password each ask first, then call the console", async () => {
  const api = stub(async () => ready);
  const user = renderCard();
  await user.click(await screen.findByRole("button", { name: m.reset }));
  assert.ok(await screen.findByText(m.resetConfirm));
  await user.click(confirm());
  await waitFor(() => assert.deepEqual(api.calls, ["reset"]));
  await user.click(screen.getByRole("button", { name: m.changePassword }));
  assert.ok(await screen.findByText(m.passwordConfirm));
  const buttons = screen.getAllByRole("button", { name: /^(OK|确 ?定)$/ });
  await user.click(buttons[buttons.length - 1]);
  await waitFor(() => assert.deepEqual(api.calls, ["reset", "password"]));
  assert.ok(await screen.findByText("rotated00000000"));
});

test("a refused switch keeps the state and reports the reason", async () => {
  const api = stub(async () => off);
  api.failWith(new APIError("EXPERIENCE_FAILED", { reason: "port 8810 is not available" }, "failed", 409));
  const user = renderCard();
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "false"));
  await user.click(toggle());
  const text = errors.EXPERIENCE_FAILED.replace("{{reason}}", "port 8810 is not available");
  assert.ok(await screen.findByText(text));
  assert.equal(toggle().getAttribute("aria-checked"), "false");
});
