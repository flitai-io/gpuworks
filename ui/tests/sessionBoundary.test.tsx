import assert from "node:assert/strict";
import fs from "node:fs";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { JSDOM } from "jsdom";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
for (const name of ["window", "document", "navigator", "HTMLElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
Object.defineProperty(globalThis, "ResizeObserver", { value: class { observe() {} unobserve() {} disconnect() {} }, configurable: true });
const auth = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/auth.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({ lng: "en", resources: { en: { auth } }, defaultNS: "auth", interpolation: { escapeValue: false } });
const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const { ConfigProvider } = await import("antd");
const { default: SessionBoundary } = await import("../src/components/SessionBoundary.tsx");
const { notifySessionExpired, notifySessionAuthenticated } = await import("../src/session.ts");
const originalFetch = globalThis.fetch;
afterEach(() => { cleanup(); globalThis.fetch = originalFetch; notifySessionAuthenticated(); });

function Editor({ logout }: { logout: () => Promise<void> }) {
  const [draft, setDraft] = useState("");
  return <><input aria-label="Deployment name" value={draft} onChange={(event) => setDraft(event.target.value)} /><button onClick={() => void logout()}>Log out</button></>;
}
function mount(onLogout?: () => void) {
  return render(<I18nextProvider i18n={i18n}><ConfigProvider theme={{ token: { motion: false } }}><SessionBoundary onLogout={onLogout}>{(logout) => <Editor logout={logout} />}</SessionBoundary></ConfigProvider></I18nextProvider>);
}

test("initial session loading waits for the server and an explicit authentication failure shows login", async () => {
  let finish: ((response: Response) => void) | undefined;
  globalThis.fetch = async () => new Promise((resolve) => { finish = resolve; });
  mount();
  assert.ok(screen.getByText(auth.checkingSession));
  assert.equal(screen.queryByRole("textbox", { name: "Username" }), null);
  finish!(Response.json({ code: "AUTHENTICATION_REQUIRED" }, { status: 401 }));
  await screen.findByRole("textbox", { name: "Username" });
  assert.equal(screen.queryByRole("textbox", { name: "Deployment name" }), null);
});

test("expired session preserves an edited form and supports keyboard reauthentication with loading and error", async () => {
  let completeLogin: ((response: Response) => void) | undefined;
  globalThis.fetch = async (url) => {
    if (String(url).endsWith("/login")) return new Promise((resolve) => { completeLogin = resolve; });
    return Response.json({ id: 1, username: "admin", role: "admin" });
  };
  mount();
  const user = userEvent.setup();
  await user.type(await screen.findByRole("textbox", { name: "Deployment name" }), "draft deployment");
  notifySessionExpired();
  notifySessionExpired();
  const dialog = await screen.findByRole("dialog");
  assert.equal(screen.getAllByRole("dialog").length, 1);
  await user.type(within(dialog).getByRole("textbox", { name: "Username" }), "admin");
  await user.type(within(dialog).getByLabelText("Password"), "bad-password");
  await user.keyboard("{Enter}");
  await waitFor(() => assert.ok(completeLogin));
  assert.ok(within(dialog).getByRole("button", { name: /Sign in/ }).className.includes("loading"));
  completeLogin!(new Response(null, { status: 401 }));
  await within(dialog).findByRole("alert");
  assert.ok(within(dialog).getByText(auth.invalidCredentials));
  await user.clear(within(dialog).getByLabelText("Password"));
  await user.type(within(dialog).getByLabelText("Password"), "password");
  completeLogin = undefined;
  await user.keyboard("{Enter}");
  await waitFor(() => assert.ok(completeLogin));
  completeLogin!(Response.json({ ok: true }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
  assert.equal((screen.getByRole("textbox", { name: "Deployment name" }) as HTMLInputElement).value, "draft deployment");
});

test("initial network and server failures offer retry instead of replacing the console with login", async () => {
  let attempts = 0;
  globalThis.fetch = async () => {
    attempts++;
    if (attempts === 1) throw new Error("offline");
    if (attempts === 2) return new Response(null, { status: 503 });
    return Response.json({ id: 1 });
  };
  mount();
  await screen.findByRole("alert");
  assert.equal(screen.queryByRole("textbox", { name: "Username" }), null);
  fireEvent.click(screen.getByRole("button", { name: auth.retry }));
  await waitFor(() => assert.equal(attempts, 2));
  await screen.findByRole("alert");
  fireEvent.click(screen.getByRole("button", { name: auth.retry }));
  await screen.findByRole("textbox", { name: "Deployment name" });
});

test("remote logout preserves drafts and verified remote login restores the editor", async () => {
  globalThis.fetch = async () => Response.json({ id: 1 });
  mount();
  const editor = await screen.findByRole("textbox", { name: "Deployment name" });
  fireEvent.change(editor, { target: { value: "draft" } });
  fireEvent(window, new dom.window.StorageEvent("storage", { key: "gpuworks.session", newValue: JSON.stringify({ type: "expired" }) }));
  await screen.findByRole("dialog");
  fireEvent(window, new dom.window.StorageEvent("storage", { key: "gpuworks.session", newValue: JSON.stringify({ type: "authenticated" }) }));
  await waitFor(() => assert.ok(!screen.queryByRole("dialog")));
  assert.equal((editor as HTMLInputElement).value, "draft");
  fireEvent.click(screen.getByRole("button", { name: "Log out" }));
  await screen.findByRole("textbox", { name: "Username" });
  assert.equal(screen.queryByRole("textbox", { name: "Deployment name" }), null);
});

test("logout preserves work on server failure and clears account state after successful revocation", async () => {
  let logoutStatus = 503;
  let clearedAccount = false;
  globalThis.fetch = async (url) => String(url).endsWith("/logout")
    ? new Response(null, { status: logoutStatus })
    : Response.json({ id: 1 });
  mount(() => { clearedAccount = true; });
  const editor = await screen.findByRole("textbox", { name: "Deployment name" });
  fireEvent.change(editor, { target: { value: "draft" } });
  fireEvent.click(screen.getByRole("button", { name: "Log out" }));
  await screen.findByText(auth.logoutFailed);
  assert.equal(clearedAccount, false);
  assert.equal((editor as HTMLInputElement).value, "draft");
  assert.equal(screen.queryByRole("textbox", { name: "Username" }), null);
  logoutStatus = 200;
  fireEvent.click(screen.getByRole("button", { name: "Log out" }));
  await screen.findByRole("textbox", { name: "Username" });
  assert.equal(clearedAccount, true);
  assert.equal(screen.queryByRole("textbox", { name: "Deployment name" }), null);
});

test("the user read while confirming the session reaches the shell without a second request", async () => {
  const { useSessionUser } = await import("../src/components/sessionUser.ts");
  const asked: string[] = [];
  globalThis.fetch = async (url) => {
    asked.push(String(url));
    return Response.json({ id: 1, username: "ops", role: "admin", password_policy: { min_length: 8, max_length: 128 } });
  };
  function Who() {
    const user = useSessionUser();
    return <p>{user ? `${user.username} ${user.password_policy?.min_length}` : "unknown"}</p>;
  }
  render(<I18nextProvider i18n={i18n}><SessionBoundary>{() => <Who />}</SessionBoundary></I18nextProvider>);
  assert.ok(await screen.findByText("ops 8"));
  assert.deepEqual(asked, ["/api/v1/auth/me"]);
});

test("an unreadable identity answer still opens the shell, without a user", async () => {
  const { useSessionUser } = await import("../src/components/sessionUser.ts");
  globalThis.fetch = async () => new Response("not json", { status: 200 });
  function Who() {
    return <p>{useSessionUser()?.username ?? "unknown"}</p>;
  }
  render(<I18nextProvider i18n={i18n}><SessionBoundary>{() => <Who />}</SessionBoundary></I18nextProvider>);
  assert.ok(await screen.findByText("unknown"));
});
