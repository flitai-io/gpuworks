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

const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEventModule = await import("@testing-library/user-event");
const userEvent = userEventModule.default ?? userEventModule;
const apiModule = await import("../src/api.ts");
const LoginPage = (await import("../src/pages/LoginPage.tsx")).default;
const { DEFAULT_LOGO, resetBrandingForTest } = await import("../src/branding.tsx");

const originalPrefetch = apiModule.api.prefetchUpdates;
const originalBranding = apiModule.api.branding;
const originalSignInOptions = apiModule.api.signInOptions;
const passwordOnly = async () => ({ local_password: "all" as const, entries: [] });
apiModule.api.signInOptions = passwordOnly;
const originalFetch = globalThis.fetch;
const defaultBranding = async () => ({ product_name: "", console_title: "", login_description: "", logo: "" });
apiModule.api.branding = defaultBranding;
afterEach(() => {
  cleanup();
  apiModule.api.prefetchUpdates = originalPrefetch;
  apiModule.api.branding = defaultBranding;
  apiModule.api.signInOptions = passwordOnly;
  globalThis.fetch = originalFetch;
  window.history.replaceState(null, "", "/");
  resetBrandingForTest();
});
after(() => { apiModule.api.branding = originalBranding; apiModule.api.signInOptions = originalSignInOptions; });
after(() => closeChannels());

function page(onLogin: () => void = () => {}) {
  return (
    <I18nextProvider i18n={i18n}>
      <LoginPage onLogin={onLogin} />
    </I18nextProvider>
  );
}

test("opening the sign-in page starts the server's update check once", async () => {
  let prefetches = 0;
  apiModule.api.prefetchUpdates = async () => { prefetches++; };
  const view = render(page());
  await screen.findByRole("button", { name: "Sign in" });
  assert.equal(prefetches, 1);
  view.rerender(page());
  assert.equal(prefetches, 1, "a re-render must not ask again");
});

test("the sign-in page works when the update check cannot be started", async () => {
  apiModule.api.prefetchUpdates = () => Promise.reject(new Error("server unreachable"));
  const requests: string[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    requests.push(String(input));
    return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  let signedIn = 0;
  render(page(() => { signedIn++; }));
  const user = userEvent.setup();
  await user.type(await screen.findByLabelText("Username"), "admin");
  await user.type(screen.getByLabelText("Password"), "password{Enter}");
  await waitFor(() => assert.equal(signedIn, 1));
  assert.deepEqual(requests, ["/api/v1/auth/login"]);
  assert.equal(screen.queryByRole("alert"), null);
});

test("a throttled sign-in shows the retry delay and permits a later successful attempt", async () => {
  apiModule.api.prefetchUpdates = async () => {};
  let attempts = 0;
  let signedIn = 0;
  globalThis.fetch = (async () => {
    attempts++;
    return attempts === 1
      ? new Response('{"code":"LOGIN_THROTTLED"}', { status: 429, headers: { "Retry-After": "900" } })
      : new Response("{}", { status: 200 });
  }) as typeof fetch;
  render(page(() => { signedIn++; }));
  const user = userEvent.setup();
  await user.type(await screen.findByLabelText("Username"), "admin");
  await user.type(screen.getByLabelText("Password"), "password{Enter}");
  const alert = await screen.findByRole("alert");
  assert.equal(alert.textContent?.includes("Too many sign-in attempts. Try again in 900 seconds."), true);
  assert.equal(signedIn, 0);
  await user.click(screen.getByRole("button", { name: "Sign in" }));
  await waitFor(() => assert.equal(signedIn, 1));
  assert.equal(screen.queryByRole("alert"), null);
});

for (const retryAfter of [null, "invalid", "-1", "9007199254740992"]) {
  test(`a throttled sign-in with Retry-After ${retryAfter} shows a safe fallback`, async () => {
    apiModule.api.prefetchUpdates = async () => {};
    globalThis.fetch = (async () => new Response('{"code":"LOGIN_THROTTLED"}', {
      status: 429,
      headers: retryAfter === null ? {} : { "Retry-After": retryAfter },
    })) as typeof fetch;
    render(page());
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "password{Enter}");
    assert.ok((await screen.findByRole("alert")).textContent?.includes(locale("common").apiError.LOGIN_THROTTLED));
  });
}

test("the update check asks the server without a session and reads no answer", async () => {
  const calls: { url: string; init?: RequestInit }[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: String(input), init });
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  await apiModule.api.prefetchUpdates();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "/api/v1/updates/prefetch");
  assert.equal(calls[0].init?.method, "POST");
  assert.equal(new Headers(calls[0].init?.headers).get("Authorization"), null);
});

test("the sign-in page shows the built-in branding and no account hint by default", async () => {
  apiModule.api.prefetchUpdates = async () => {};
  render(page());
  const panel = await screen.findByRole("region", { name: "GPUWorks product information" });
  // The built-in wordmark spells the name, so it is not repeated as text.
  const mark = within(panel).getByRole("img", { name: "GPUWorks" });
  assert.equal(mark.getAttribute("src"), DEFAULT_LOGO);
  assert.equal(panel.querySelector(".auth-brand-name"), null);
  assert.ok(panel.textContent?.includes(locale("common").branding.defaultLoginDescription));
  assert.equal(screen.queryByText(/administrator account/i), null);
  await waitFor(() => assert.equal(document.title, "GPUWorks"));
});

test("the sign-in page shows the configured product name, logo and text", async () => {
  apiModule.api.prefetchUpdates = async () => {};
  const logo = "data:image/png;base64,AA==";
  apiModule.api.branding = async () => ({ product_name: "Acme AI", console_title: "", login_description: "Line one\nLine two", logo });
  render(page());
  const panel = await screen.findByRole("region", { name: "Acme AI product information" });
  assert.equal(panel.querySelector(".auth-brand-name")?.textContent, "Acme AI");
  assert.equal(panel.querySelector(".auth-brand-copy")?.textContent, "Line one\nLine two");
  assert.equal(panel.querySelector("img")?.getAttribute("src"), logo);
  await waitFor(() => assert.equal(document.title, "Acme AI"));
});

test("the sign-in page keeps the built-in branding when it cannot be loaded", async () => {
  apiModule.api.prefetchUpdates = async () => {};
  let asked = 0;
  apiModule.api.branding = async () => { asked++; throw new Error("server unreachable"); };
  render(page());
  await waitFor(() => assert.equal(asked, 1));
  assert.ok(await screen.findByRole("region", { name: "GPUWorks product information" }));
  assert.ok(screen.getByRole("button", { name: "Sign in" }));
});

for (const failure of [new TypeError("Failed to fetch"), new TypeError("NetworkError when attempting to fetch resource."), new Error("connection closed")]) {
  test(`unreachable sign-in service shows a retry hint instead of ${failure.message}`, async () => {
    apiModule.api.prefetchUpdates = async () => {};
    globalThis.fetch = async () => { throw failure; };
    let signedIn = 0;
    render(page(() => { signedIn++; }));
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "password{Enter}");
    const alert = await screen.findByRole("alert");
    assert.ok(alert.textContent?.includes("Sign-in failed. Try again later."));
    assert.ok(!alert.textContent?.includes(failure.message), "browser transport text must not become the user-facing message");
    assert.equal(signedIn, 0);
  });
}

test("invalid credentials retain their specific sign-in guidance", async () => {
  apiModule.api.prefetchUpdates = async () => {};
  globalThis.fetch = async () => new Response(JSON.stringify({ code: "AUTH_INVALID_CREDENTIALS" }), { status: 401 });
  render(page());
  const user = userEvent.setup();
  await user.type(await screen.findByLabelText("Username"), "admin");
  await user.type(screen.getByLabelText("Password"), "password{Enter}");
  assert.ok((await screen.findByRole("alert")).textContent?.includes("Incorrect username or password. Check your credentials and try again."));
});

const acme = { source: "acme", label: "Acme ID", url: "/api/v1/auth/sign-in/start?source=acme" };

test("while the sign-in options load, and when they fail, the password form is all there is", async () => {
  apiModule.api.prefetchUpdates = async () => {};
  let resolve!: (v: Awaited<ReturnType<typeof passwordOnly>>) => void;
  apiModule.api.signInOptions = () => new Promise((r) => { resolve = r; });
  const view = render(page());
  assert.ok(await screen.findByRole("button", { name: "Sign in" }));
  assert.equal(screen.queryByRole("navigation"), null);
  resolve({ local_password: "all", entries: [] });
  await waitFor(() => assert.ok(screen.getByLabelText("Password")));
  assert.equal(screen.queryByRole("navigation"), null, "no entries, no list");
  view.unmount();

  apiModule.api.signInOptions = async () => { throw new Error("server unreachable"); };
  render(page());
  assert.ok(await screen.findByLabelText("Password"));
  assert.equal(screen.queryByRole("navigation"), null);
  assert.equal(screen.queryByRole("alert"), null, "a failed options read is not an error the user can act on");
});

test("the identity sources the server offers are links under the password form", async () => {
  apiModule.api.prefetchUpdates = async () => {};
  apiModule.api.signInOptions = async () => ({ local_password: "all", entries: [acme] });
  render(page());
  const nav = await screen.findByRole("navigation", { name: "Or sign in with" });
  const link = within(nav).getByRole("link", { name: "Acme ID" });
  assert.equal(link.getAttribute("href"), "/api/v1/auth/sign-in/start?source=acme");
  assert.ok(screen.getByLabelText("Password"), "the password form stays for everyone");
  // Keyboard: the entry is the next stop after the password form's last
  // control, and Enter follows it.
  const user = userEvent.setup();
  screen.getByRole("button", { name: "Forgot password?" }).focus();
  await user.tab();
  assert.ok(document.activeElement === link, "one Tab from the password form reaches the entry");
  await user.tab({ shift: true });
  assert.ok(document.activeElement === screen.getByRole("button", { name: "Forgot password?" }), "Shift+Tab goes back to the form");
  link.focus();
  let followed = "";
  link.addEventListener("click", (e) => { followed = link.getAttribute("href") ?? ""; e.preventDefault(); });
  await user.keyboard("{Enter}");
  assert.equal(followed, "/api/v1/auth/sign-in/start?source=acme");
});

test("when only local administrators use a password, the form is one click away", async () => {
  apiModule.api.prefetchUpdates = async () => {};
  apiModule.api.signInOptions = async () => ({ local_password: "local_admin", entries: [acme] });
  render(page());
  assert.ok(await screen.findByRole("link", { name: "Acme ID" }));
  assert.equal(screen.queryByLabelText("Password"), null);
  const user = userEvent.setup();
  const reveal = screen.getByRole("button", { name: "Sign in with a password as a local administrator" });
  reveal.focus();
  await user.keyboard("{Enter}");
  assert.ok(await screen.findByLabelText("Password"));
});

test("local administrators keep the password form when no identity source is offered", async () => {
  apiModule.api.prefetchUpdates = async () => {};
  apiModule.api.signInOptions = async () => ({ local_password: "local_admin", entries: [] });
  render(page());
  await waitFor(() => assert.ok(screen.getByLabelText("Password")));
  assert.equal(screen.queryByRole("button", { name: "Sign in with a password as a local administrator" }), null);
});

test("a refused sign-in comes back with its code, and only known codes are shown", async () => {
  apiModule.api.prefetchUpdates = async () => {};
  window.history.replaceState(null, "", "/login?code=IDENTITY_TICKET_INVALID");
  const view = render(page());
  const alert = await screen.findByRole("alert");
  assert.ok(alert.textContent?.includes(locale("common").apiError.IDENTITY_TICKET_INVALID));
  view.unmount();

  window.history.replaceState(null, "", "/login?code=SERVICE_UNAVAILABLE");
  const fault = render(page());
  assert.ok((await screen.findByRole("alert")).textContent?.includes(locale("common").apiError.SERVICE_UNAVAILABLE));
  fault.unmount();

  window.history.replaceState(null, "", "/login?code=SOMETHING_ELSE");
  render(page());
  assert.ok(await screen.findByLabelText("Password"));
  assert.equal(screen.queryByRole("alert"), null);
});
