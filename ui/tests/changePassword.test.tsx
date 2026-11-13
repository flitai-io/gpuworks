import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "CustomEvent", "Event"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });
const closeChannels = trackMessageChannels();

const locale = (ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en", fallbackLng: "en", resources: { en: { common: locale("common"), auth: locale("auth") } },
  defaultNS: "common", interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const UserMenu = (await import("../src/components/UserMenu.tsx")).default;
const { SessionUserContext } = await import("../src/components/sessionUser.ts");
type Me = Awaited<ReturnType<typeof apiModule.api.me>>;

/** UserMenu as the shell renders it: under the user SessionBoundary read. */
function menu(user: Me | undefined, onLogout: () => void) {
  return <I18nextProvider i18n={i18n}><SessionUserContext.Provider value={user}><UserMenu onLogout={onLogout} /></SessionUserContext.Provider></I18nextProvider>;
}
const { LoginForm } = await import("../src/pages/LoginPage.tsx");

const originalMe = apiModule.api.me;
// The menu must not ask for the user itself; SessionBoundary already did.
apiModule.api.me = async () => { throw new Error("UserMenu requested /auth/me"); };
const originalFetch = globalThis.fetch;
afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
});
after(() => { apiModule.api.me = originalMe; closeChannels(); });

const auth = locale("auth").changePassword;

/** The field errors on screen, in form order. */
const fieldErrors = () => [...document.querySelectorAll('[data-slot="field-error"]')].map((e) => e.textContent);

type Sent = { url: string; body: unknown };
function answer(status: number, body: unknown, sent: Sent[]) {
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    sent.push({ url: String(input), body: init?.body ? JSON.parse(String(init.body)) : undefined });
    return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
}

async function openForm(onLogout: () => void = () => {}) {
  render(menu({ id: 1, username: "admin", role: "admin", password_policy: { min_length: 8, max_length: 128 } }, onLogout));
  const user = userEvent.setup({ delay: null });
  await user.click(await screen.findByRole("button", { name: "Account: admin" }));
  await user.click(await screen.findByText("Change password"));
  await screen.findByText(auth.description);
  return user;
}

async function fill(user: ReturnType<typeof userEvent.setup>, current: string, next: string, confirm = next) {
  await user.type(screen.getByLabelText(auth.current), current);
  await user.type(screen.getByLabelText(auth.next), next);
  await user.type(screen.getByLabelText(auth.confirm), confirm);
  await user.click(screen.getByRole("button", { name: auth.submit }));
}

test("the account menu names the user and signs out", async () => {
  let signedOut = 0;
  render(menu({ id: 1, username: "ops", role: "admin" }, () => { signedOut++; }));
  const user = userEvent.setup({ delay: null });
  await user.click(await screen.findByRole("button", { name: "Account: ops" }));
  assert.ok(await screen.findByText("Change password"));
  await user.click(screen.getByText("Log out"));
  assert.equal(signedOut, 1);
});

test("the account menu falls back to the role name when the user cannot be read", async () => {
  render(menu(undefined, () => {}));
  assert.ok(await screen.findByRole("button", { name: "Account: Administrator" }));
});

test("the form states the server's rule and signs out after a change", async () => {
  const sent: Sent[] = [];
  answer(200, { status: "password_changed" }, sent);
  let signedOut = 0;
  const user = await openForm(() => { signedOut++; });
  assert.ok(screen.getByText("8–128 characters. Avoid weak passwords to keep the account secure."));
  await fill(user, "old-password-1", "Tiger#Lake9");
  await waitFor(() => assert.equal(signedOut, 1));
  assert.deepEqual(sent, [{ url: "/api/v1/auth/password", body: { current_password: "old-password-1", new_password: "Tiger#Lake9" } }]);
});

test("mismatched new passwords are caught before the server is asked", async () => {
  const sent: Sent[] = [];
  answer(200, {}, sent);
  const user = await openForm();
  await fill(user, "old-password-1", "Tiger#Lake9", "Tiger#Lake8");
  await waitFor(() => assert.deepEqual(fieldErrors(), [auth.mismatch]));
  assert.equal(sent.length, 0);
});

test("server rejections land on the field they concern", async () => {
  const sent: Sent[] = [];
  answer(400, { code: "CURRENT_PASSWORD_INVALID", error: "the current password is wrong" }, sent);
  let signedOut = 0;
  const user = await openForm(() => { signedOut++; });
  await fill(user, "wrong-password", "Tiger#Lake9");
  await waitFor(() => assert.deepEqual(fieldErrors(), [locale("common").apiError.CURRENT_PASSWORD_INVALID]));
  answer(400, { code: "PASSWORD_LENGTH", error: "use 8 to 128 characters", params: { min: 8, max: 128 } }, sent);
  await user.clear(screen.getByLabelText(auth.current));
  await user.type(screen.getByLabelText(auth.current), "old-password-1");
  await user.click(screen.getByRole("button", { name: auth.submit }));
  await waitFor(() => assert.deepEqual(fieldErrors(), ["The password must have 8–128 characters. Choose another."]));
  assert.equal(signedOut, 0);
  assert.equal(document.querySelector('[data-slot="alert"]'), null, "a field error is not repeated above the form");
});

test("a refusal that is not about a field is shown above the form", async () => {
  answer(403, { code: "EXPERIENCE_UNSUPPORTED", error: "not supported" }, []);
  const user = await openForm();
  await fill(user, "old-password-1", "Tiger#Lake9");
  await waitFor(() => assert.ok(document.querySelector('[data-slot="alert"]')?.textContent?.includes(locale("common").apiError.EXPERIENCE_UNSUPPORTED)));
  assert.deepEqual(fieldErrors(), []);
});

test("the sign-in form tells how to reset a forgotten password", async () => {
  render(<I18nextProvider i18n={i18n}><LoginForm onLogin={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ delay: null });
  await user.click(screen.getByText(locale("auth").forgotPassword));
  assert.ok(await screen.findByText(locale("auth").forgotPasswordCommand));
});

test("a server rejection clears once the field is edited, and the next answer replaces it", async () => {
  const sent: Sent[] = [];
  answer(400, { code: "PASSWORD_GUESSABLE", error: "guessable" }, sent);
  const user = await openForm();
  await fill(user, "wrong-password", "Password123");
  await waitFor(() => assert.deepEqual(fieldErrors(), [locale("common").apiError.PASSWORD_GUESSABLE]));
  answer(400, { code: "CURRENT_PASSWORD_INVALID", error: "wrong" }, sent);
  for (const label of [auth.next, auth.confirm]) {
    await user.clear(screen.getByLabelText(label));
    await user.type(screen.getByLabelText(label), "Tiger#Lake9");
  }
  await waitFor(() => assert.deepEqual(fieldErrors(), []));
  await user.click(screen.getByRole("button", { name: auth.submit }));
  await waitFor(() => assert.deepEqual(fieldErrors(), [locale("common").apiError.CURRENT_PASSWORD_INVALID]));
  assert.equal(sent.length, 2);
});

test("a wrong current password and a weak new one are marked together", async () => {
  answer(400, { code: "CURRENT_PASSWORD_INVALID", error: "wrong", params: { new_password: "PASSWORD_GUESSABLE", min: 8, max: 128 } }, []);
  const user = await openForm();
  await fill(user, "wrong-password", "Password123");
  await waitFor(() => assert.deepEqual(fieldErrors(), [
    locale("common").apiError.CURRENT_PASSWORD_INVALID,
    locale("common").apiError.PASSWORD_GUESSABLE,
  ]));
});

test("a throttled change says so in the form's own words", async () => {
  answer(429, { code: "LOGIN_THROTTLED", error: "too many" }, []);
  const user = await openForm();
  await fill(user, "wrong-password", "Tiger#Lake9");
  await waitFor(() => assert.equal(document.querySelector('[data-slot="alert"]')?.textContent, auth.throttled));
});

test("the password toggle shows and hides what was typed", async () => {
  const user = await openForm();
  const input = screen.getByLabelText(auth.current) as HTMLInputElement;
  assert.equal(input.type, "password");
  const [show] = screen.getAllByRole("button", { name: locale("common").actions.showPassword });
  await user.click(show);
  assert.equal(input.type, "text");
  await user.click(screen.getByRole("button", { name: locale("common").actions.hidePassword }));
  assert.equal(input.type, "password");
});
