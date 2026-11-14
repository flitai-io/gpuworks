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
const { default: SecurityCard } = await import("../src/pages/settings/SecurityCard.tsx");
const s = locale.settings.security as Record<string, string>;

type Settings = import("../src/api.ts").SecuritySettings;
const original = { securitySettings: apiModule.api.securitySettings, updateSecuritySettings: apiModule.api.updateSecuritySettings, me: apiModule.api.me };
const off = { playground_preview_network: false, inference_open_access: false };

// The GET returns the whole policy and the partial PATCH returns the policy as
// it now stands, so the stub keeps the server state between saves.
function stub({ role = "admin", load }: { role?: string; load: () => Promise<Settings> }) {
  const updates: Partial<Settings>[] = [];
  let current: Settings = off;
  let saveError: Error | null = null;
  let loads = 0;
  apiModule.api.me = (async () => ({ id: 1, username: role, role })) as typeof apiModule.api.me;
  apiModule.api.securitySettings = async () => { loads++; current = await load(); return current; };
  apiModule.api.updateSecuritySettings = async (body) => {
    updates.push(body);
    if (saveError) throw saveError;
    current = { ...current, ...body };
    return current;
  };
  return { updates, loads: () => loads, failSaveWith: (error: Error) => { saveError = error; } };
}

function renderCard() {
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><SecurityCard /></I18nextProvider>);
  return user;
}
const toggle = () => screen.getByRole("switch", { name: s.previewNetwork });
const openAccessToggle = () => screen.getByRole("switch", { name: s.inferenceOpenAccess });
const saveButton = () => screen.getByRole("button", { name: locale.settings.common.save });

// antd's static message lives outside the rendered tree: cleanup() does not remove
// it, message.destroy() leaves it stuck in its leave animation (jsdom never fires
// the transition end), and an earlier test's "saved" toast would otherwise still
// be in the DOM, for a time that depends on how long that test took. A save is
// proven by one more toast than before the click, whatever is left over.
const savedToasts = () => screen.queryAllByText(s.saved).length;
async function saveAndExpectNewSavedToast(user: ReturnType<typeof userEvent.setup>) {
  const before = savedToasts();
  await user.click(saveButton());
  await waitFor(() => assert.equal(savedToasts(), before + 1));
}

afterEach(() => {
  cleanup();
  Object.assign(apiModule.api, original);
});
after(() => closeChannels());

test("the switches are off and busy until the policy loads, then show it", async () => {
  let resolve!: (value: Settings) => void;
  stub({ load: () => new Promise<Settings>((r) => { resolve = r; }) });
  renderCard();
  assert.equal(toggle().getAttribute("aria-checked"), "false");
  assert.ok(toggle().classList.contains("ant-switch-loading"), "loading while the request is in flight");
  assert.equal(openAccessToggle().getAttribute("aria-checked"), "false");
  assert.ok(openAccessToggle().classList.contains("ant-switch-loading"), "the keyless-access switch loads too");
  assert.equal((saveButton() as HTMLButtonElement).disabled, true, "nothing to save before the policy is known");
  resolve({ playground_preview_network: true, inference_open_access: true });
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  await waitFor(() => assert.equal(openAccessToggle().getAttribute("aria-checked"), "true"));
  assert.ok(!toggle().classList.contains("ant-switch-loading"));
  assert.ok(!openAccessToggle().classList.contains("ant-switch-loading"));
  assert.ok(screen.getByText(s.previewNetworkHelp));
  assert.ok(screen.getByText(s.inferenceOpenAccessHelp));
  assert.equal((saveButton() as HTMLButtonElement).disabled, true, "a freshly loaded policy has nothing to save");
});

test("an admin turns network access on and saves it", async () => {
  const api = stub({ load: async () => ({ ...off }) });
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  await user.click(toggle());
  assert.equal(toggle().getAttribute("aria-checked"), "true");
  assert.equal(api.updates.length, 0, "toggling alone does not save");
  await saveAndExpectNewSavedToast(user);
  await waitFor(() => assert.deepEqual(api.updates, [{ playground_preview_network: true }]));
});

test("turning keyless /v1 access on patches only that field", async () => {
  const api = stub({ load: async () => ({ ...off }) });
  const user = renderCard();
  await waitFor(() => assert.equal((openAccessToggle() as HTMLButtonElement).disabled, false));
  assert.equal(openAccessToggle().getAttribute("aria-checked"), "false");
  await user.click(openAccessToggle());
  assert.equal(openAccessToggle().getAttribute("aria-checked"), "true");
  assert.equal(api.updates.length, 0, "toggling alone does not save");
  await saveAndExpectNewSavedToast(user);
  // A PATCH that carried the unchanged preview policy would clobber a
  // concurrent change to it; only the field the admin moved goes out.
  await waitFor(() => assert.deepEqual(api.updates, [{ inference_open_access: true }]));
  assert.equal(openAccessToggle().getAttribute("aria-checked"), "true");
});

test("changing both switches patches both fields in one request", async () => {
  const api = stub({ load: async () => ({ ...off }) });
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  await user.click(toggle());
  await user.click(openAccessToggle());
  await user.click(saveButton());
  await waitFor(() => assert.deepEqual(api.updates, [{ playground_preview_network: true, inference_open_access: true }]));
});

test("a switch moved back to its stored value leaves nothing to save", async () => {
  const api = stub({ load: async () => ({ playground_preview_network: true, inference_open_access: false }) });
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  await user.click(toggle());
  assert.equal((saveButton() as HTMLButtonElement).disabled, false, "a real change can be saved");
  await user.click(toggle());
  assert.equal((saveButton() as HTMLButtonElement).disabled, true, "no change means nothing to save");
  assert.equal(api.updates.length, 0);
});

test("the switch toggles from the keyboard", async () => {
  stub({ load: async () => ({ ...off }) });
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  toggle().focus();
  await user.keyboard(" ");
  assert.equal(toggle().getAttribute("aria-checked"), "true");
  await user.keyboard("{Enter}");
  assert.equal(toggle().getAttribute("aria-checked"), "false");
});

test("the keyless-access switch toggles from the keyboard", async () => {
  stub({ load: async () => ({ ...off }) });
  const user = renderCard();
  await waitFor(() => assert.equal((openAccessToggle() as HTMLButtonElement).disabled, false));
  openAccessToggle().focus();
  await user.keyboard(" ");
  assert.equal(openAccessToggle().getAttribute("aria-checked"), "true");
  await user.keyboard("{Enter}");
  assert.equal(openAccessToggle().getAttribute("aria-checked"), "false");
});

test("a refused save reports the server's reason", async () => {
  const api = stub({ load: async () => ({ ...off }) });
  api.failSaveWith(new APIError("ADMINISTRATOR_REQUIRED", undefined, "administrator role required", 403));
  const user = renderCard();
  await waitFor(() => assert.equal((toggle() as HTMLButtonElement).disabled, false));
  await user.click(toggle());
  await user.click(saveButton());
  await waitFor(() => assert.equal(api.updates.length, 1));
  assert.ok(await screen.findByText(/Failed to save/));
});

test("a failed save of the keyless switch keeps the change pending", async () => {
  const api = stub({ load: async () => ({ ...off }) });
  api.failSaveWith(new Error("storage unavailable"));
  const user = renderCard();
  await waitFor(() => assert.equal((openAccessToggle() as HTMLButtonElement).disabled, false));
  await user.click(openAccessToggle());
  await user.click(saveButton());
  await waitFor(() => assert.equal(api.updates.length, 1));
  assert.ok(await screen.findByText(/storage unavailable/));
  assert.equal(openAccessToggle().getAttribute("aria-checked"), "true", "the unconfirmed change stays visible");
  // antd keeps the loading icon's label on the button while it animates out, so
  // match the Save button loosely and let the retry prove it still works.
  await user.click(screen.getByRole("button", { name: /Save/ }));
  await waitFor(() => assert.deepEqual(api.updates, [{ inference_open_access: true }, { inference_open_access: true }],
    "the pending change can be saved again"));
});

test("a failed load is reported and can be retried", async () => {
  let fail = true;
  const api = stub({ load: async () => {
    if (fail) throw new Error("settings offline");
    return { playground_preview_network: true, inference_open_access: true };
  } });
  const user = renderCard();
  assert.ok(await screen.findByText(/settings offline/));
  assert.equal((toggle() as HTMLButtonElement).disabled, true);
  assert.equal((openAccessToggle() as HTMLButtonElement).disabled, true);
  fail = false;
  await user.click(screen.getByRole("button", { name: locale.common.actions.retry }));
  await waitFor(() => assert.equal(toggle().getAttribute("aria-checked"), "true"));
  await waitFor(() => assert.equal(openAccessToggle().getAttribute("aria-checked"), "true"));
  assert.equal(api.loads(), 2);
  assert.ok(screen.queryByText(/settings offline/) === null, "the error clears after a successful retry");
});

test("a non-admin sees the policy but cannot change it", async () => {
  stub({ role: "user", load: async () => ({ ...off }) });
  renderCard();
  assert.ok(await screen.findByText(s.adminOnly));
  await waitFor(() => assert.ok(!toggle().classList.contains("ant-switch-loading")));
  assert.equal((toggle() as HTMLButtonElement).disabled, true);
  assert.equal((openAccessToggle() as HTMLButtonElement).disabled, true);
  assert.equal((saveButton() as HTMLButtonElement).disabled, true);
});

test("a #security deep link keeps the card in view while the page above it grows, until the user scrolls", async () => {
  stub({ load: async () => ({ ...off }) });
  const observers: { callback: () => void; disconnected: boolean }[] = [];
  class TrackingResizeObserver {
    entry: { callback: () => void; disconnected: boolean };
    constructor(callback: () => void) { this.entry = { callback, disconnected: false }; observers.push(this.entry); }
    observe() {}
    unobserve() {}
    disconnect() { this.entry.disconnected = true; }
  }
  const originalObserver = globalThis.ResizeObserver;
  const proto = window.HTMLElement.prototype as unknown as { scrollIntoView?: () => void };
  const originalScroll = proto.scrollIntoView;
  let jumps = 0;
  Object.defineProperty(globalThis, "ResizeObserver", { value: TrackingResizeObserver, configurable: true });
  proto.scrollIntoView = function () { if ((this as HTMLElement).id === "security") jumps++; };
  window.location.hash = "#security";
  try {
    renderCard();
    assert.equal(jumps, 1, "jumps once on mount");
    const observer = observers.at(-1)!;
    observer.callback();
    observer.callback();
    assert.equal(jumps, 3, "follows each layout change");
    window.dispatchEvent(new window.WheelEvent("wheel", { deltaY: -40 }));
    assert.equal(observer.disconnected, true, "the user's own scroll ends it");
  } finally {
    window.location.hash = "";
    Object.defineProperty(globalThis, "ResizeObserver", { value: originalObserver, configurable: true });
    proto.scrollIntoView = originalScroll;
  }
});

test("without the fragment the card does not move the page", () => {
  stub({ load: async () => ({ ...off }) });
  const proto = window.HTMLElement.prototype as unknown as { scrollIntoView?: () => void };
  const originalScroll = proto.scrollIntoView;
  let jumps = 0;
  proto.scrollIntoView = () => { jumps++; };
  try {
    renderCard();
    assert.equal(jumps, 0);
  } finally {
    proto.scrollIntoView = originalScroll;
  }
});