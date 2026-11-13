// The console frame: top bar, side menu (ui/src/app/AppSidebar.tsx) and the
// content panel of ui/src/App.tsx, mounted whole behind a signed-in session.
//
// The server is a fetch stub that answers only the endpoints the frame itself
// reads; every other request gets a 404 and is recorded, so a page reaching for
// data shows its own error state instead of pretending to succeed.
import assert from "node:assert/strict";
import fs from "node:fs";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { dom, createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

// App.tsx imports page stylesheets; under node they load as empty modules.
register(pathToFileURL(new URL("./deploySleepableCssHook.mjs", import.meta.url).pathname));
const closeChannels = trackMessageChannels();
// Zeron hooks pass an AbortSignal to window.addEventListener; jsdom accepts only its own.
for (const name of ["KeyboardEvent", "PointerEvent", "MouseEvent", "FocusEvent", "Document", "HTMLAnchorElement", "HTMLDivElement", "HTMLSpanElement", "HTMLImageElement", "DocumentFragment", "Text", "Event", "CustomEvent", "AbortController", "AbortSignal"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
// jsdom has no Web Animations API; Base UI's ScrollArea asks which animations are running.
if (!("getAnimations" in dom.window.Element.prototype)) {
  Object.defineProperty(dom.window.Element.prototype, "getAnimations", { configurable: true, value: () => [] });
}
// The server event stream (/api/v1/events) never delivers here: pages show what they loaded.
class SilentEventSource { readyState = 0; addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: SilentEventSource, configurable: true });
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(globalThis, "sessionStorage", { value: dom.window.sessionStorage, configurable: true });

// Zeron's Sidebar reads "(max-width: 1279px)" through window.matchMedia and
// follows its change events. This one answers width queries from a settable
// window width and, like a browser, fires "change" only on lists whose answer flips.
let viewportWidth = 1440;
type MediaListener = (event: { matches: boolean; media: string }) => void;
const mediaLists = new Set<{ media: string; last: boolean; listeners: Set<MediaListener> }>();
function matchesQuery(query: string) {
  const max = /max-width:\s*(\d+(?:\.\d+)?)px/.exec(query);
  const min = /min-width:\s*(\d+(?:\.\d+)?)px/.exec(query);
  if (!max && !min) return false;
  return (!max || viewportWidth <= Number(max[1])) && (!min || viewportWidth >= Number(min[1]));
}
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: (media: string) => {
    const list = { media, last: matchesQuery(media), listeners: new Set<MediaListener>() };
    mediaLists.add(list);
    return {
      get matches() { return matchesQuery(media); },
      media,
      onchange: null,
      addListener: (listener: MediaListener) => list.listeners.add(listener),
      removeListener: (listener: MediaListener) => list.listeners.delete(listener),
      addEventListener: (_type: string, listener: MediaListener) => list.listeners.add(listener),
      removeEventListener: (_type: string, listener: MediaListener) => list.listeners.delete(listener),
      dispatchEvent: () => false,
    };
  },
});
function resizeTo(width: number) {
  viewportWidth = width;
  Object.defineProperty(dom.window, "innerWidth", { configurable: true, value: width });
  for (const list of mediaLists) {
    const matches = matchesQuery(list.media);
    if (matches === list.last) continue;
    list.last = matches;
    for (const listener of [...list.listeners]) listener({ matches, media: list.media });
  }
}

const namespaces = fs.readdirSync(new URL("../src/i18n/locales/en/", import.meta.url)).map((file) => file.replace(/\.json$/, ""));
const { i18n, locale } = await createI18n(["common", ...namespaces.filter((ns) => ns !== "common")]);
const common = locale.common;

const { act, cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter, RouterProvider, createMemoryRouter } = await import("react-router-dom");
const { ConfigProvider } = await import("antd");
const { default: App } = await import("../src/App.tsx");
const { navGroups } = await import("../src/navigation.tsx");
const { resetBrandingForTest } = await import("../src/branding.tsx");
// helpers/dom.ts drops a finished test's pending animation frames; Base UI's
// process-wide frame scheduler would then wait forever for the dropped one, and
// the next test's menus never open. Base UI ships this reset for test runners.
const { resetAnimationFrameScheduler } = await import("@base-ui/utils/useAnimationFrame");

const originalFetch = globalThis.fetch;
type Route = () => Response;
let unexpected: string[] = [];
// The frame's own reads, answered the way the server does for a signed-in administrator.
const frameRoutes: Record<string, Route> = {
  "/api/v1/auth/me": () => Response.json({ id: 1, username: "admin", role: "admin" }),
  "/api/v1/branding": () => Response.json({ product_name: "", console_title: "", login_description: "", logo: "" }),
  "/api/v1/notices": () => Response.json({ notices: [], unread: 0 }),
  // No update check result: the server could not reach the channel.
  "/api/v1/updates": () => Response.json({ code: "UPDATE_CHECK_FAILED", message: "channel unreachable" }, { status: 502 }),
};
function serve(overrides: Record<string, Route> = {}) {
  unexpected = [];
  const routes = { ...frameRoutes, ...overrides };
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    const path = new URL(String(input instanceof Request ? input.url : input), "http://localhost/").pathname;
    const route = routes[path];
    if (route) return route();
    unexpected.push(path);
    return Response.json({ code: "NOT_FOUND", message: `no stub for ${path}` }, { status: 404 });
  }) as typeof fetch;
}

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  resizeTo(1440);
  resetBrandingForTest();
  serve();
});
afterEach(() => { cleanup(); resetAnimationFrameScheduler(); globalThis.fetch = originalFetch; });
after(() => closeChannels());

async function mountApp(path: string) {
  const view = render(
    <I18nextProvider i18n={i18n}>
      <ConfigProvider theme={{ token: { motion: false } }}>
        <MemoryRouter initialEntries={[path]}><App /></MemoryRouter>
      </ConfigProvider>
    </I18nextProvider>,
  );
  await screen.findByRole("complementary", { name: common.header.navigation });
  return view;
}
const menu = () => screen.getByRole("complementary", { name: common.header.navigation });
const sidebarState = () => menu().closest("[data-slot=sidebar]")?.getAttribute("data-state");
const toggleButton = () => within(menu()).getByRole("button", { name: /^(Collapse menu|Expand menu)$/ });

test("the menu lists every group and entry from navigation.tsx and marks the current route's entry", async () => {
  // A path below a menu entry with no page of its own still files under that entry.
  await mountApp("/nodes/does-not-exist");
  for (const group of navGroups) {
    const label = group.labelKey ? i18n.t(group.labelKey) : common.navigation.dashboard;
    const region = within(menu()).getByRole("region", { name: label });
    if (group.labelKey) assert.ok(within(region).getByText(label), `group heading ${label}`);
    const links = within(region).getAllByRole("link");
    assert.deepEqual(links.map((link) => link.textContent), group.items.map((item) => i18n.t(item.labelKey)));
    assert.deepEqual(links.map((link) => link.getAttribute("href")), group.items.map((item) => item.key));
  }
  const current = within(menu()).getAllByRole("link").filter((link) => link.getAttribute("aria-current") === "page");
  assert.deepEqual(current.map((link) => link.textContent), [common.navigation.nodes]);
});

test("following a menu link moves the current mark with the route", async () => {
  const user = userEvent.setup();
  await mountApp("/nodes/does-not-exist");
  await user.click(within(menu()).getByRole("link", { name: common.navigation.apiKeys }));
  await waitFor(() => assert.equal(within(menu()).getByRole("link", { name: common.navigation.apiKeys }).getAttribute("aria-current"), "page"));
  assert.equal(within(menu()).getByRole("link", { name: common.navigation.nodes }).getAttribute("aria-current"), null);
});

const navCollapsedKey = "gpuworks.navCollapsed";

test("the collapse button works from the keyboard, renames itself and the choice survives a reload", async () => {
  const user = userEvent.setup();
  await mountApp("/nodes/does-not-exist");
  assert.equal(sidebarState(), "expanded");
  const collapse = within(menu()).getByRole("button", { name: common.header.collapseNavigation });
  assert.equal(collapse.getAttribute("aria-expanded"), "true");
  collapse.focus();
  await user.keyboard("{Enter}");
  const expand = await within(menu()).findByRole("button", { name: common.header.expandNavigation });
  assert.equal(expand.getAttribute("aria-expanded"), "false");
  assert.equal(sidebarState(), "collapsed");
  assert.equal(localStorage.getItem(navCollapsedKey), "1");
  // Folded to icons, every entry keeps its name for assistive technology.
  assert.ok(within(menu()).getByRole("link", { name: common.navigation.nodes }));

  cleanup();
  await mountApp("/nodes/does-not-exist");
  assert.equal(sidebarState(), "collapsed", "the folded menu is remembered");
  toggleButton().focus();
  await user.keyboard(" ");
  await within(menu()).findByRole("button", { name: common.header.collapseNavigation });
  assert.equal(sidebarState(), "expanded");
  assert.equal(localStorage.getItem(navCollapsedKey), "0");
});

test("a folded menu names each entry in a tooltip", async () => {
  localStorage.setItem(navCollapsedKey, "1");
  const user = userEvent.setup();
  await mountApp("/nodes/does-not-exist");
  await user.hover(within(menu()).getByRole("link", { name: common.navigation.gpus }));
  const tip = await screen.findByRole("tooltip");
  assert.equal(tip.textContent, common.navigation.gpus);
});

test("below 1280px the menu folds to icons on its own and opening it there is not saved", async () => {
  resizeTo(1024);
  const user = userEvent.setup();
  await mountApp("/nodes/does-not-exist");
  assert.equal(sidebarState(), "collapsed");
  assert.equal(localStorage.getItem(navCollapsedKey), null, "folding for the window is not a preference");
  await user.click(within(menu()).getByRole("button", { name: common.header.expandNavigation }));
  await within(menu()).findByRole("button", { name: common.header.collapseNavigation });
  assert.equal(sidebarState(), "expanded");
  assert.equal(localStorage.getItem(navCollapsedKey), null, "a temporary opening is not saved");
  await user.click(toggleButton());
  await waitFor(() => assert.equal(sidebarState(), "collapsed"));
  assert.equal(localStorage.getItem(navCollapsedKey), null);
});

test("the menu follows the window across 1280px and keeps the user's choice above it", async () => {
  await mountApp("/nodes/does-not-exist");
  assert.equal(sidebarState(), "expanded");
  act(() => resizeTo(1100));
  await waitFor(() => assert.equal(sidebarState(), "collapsed"));
  act(() => resizeTo(1440));
  await waitFor(() => assert.equal(sidebarState(), "expanded"));
  assert.equal(localStorage.getItem(navCollapsedKey), null);
});

test("a browser that refuses storage still gets a working menu toggle", async () => {
  const blocked = {
    getItem() { throw new dom.window.DOMException("blocked", "SecurityError"); },
    setItem() { throw new dom.window.DOMException("blocked", "SecurityError"); },
    removeItem() {}, clear() {}, key: () => null, length: 0,
  };
  Object.defineProperty(globalThis, "localStorage", { value: blocked, configurable: true });
  try {
    const user = userEvent.setup();
    await mountApp("/nodes/does-not-exist");
    assert.equal(sidebarState(), "expanded", "unreadable storage means the default, expanded");
    await user.click(within(menu()).getByRole("button", { name: common.header.collapseNavigation }));
    await within(menu()).findByRole("button", { name: common.header.expandNavigation });
    assert.equal(sidebarState(), "collapsed");
  } finally {
    Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
  }
});

test("every top bar control has a name, and the language and notice menus open from the frame", async () => {
  const { computeAccessibleName } = await import("dom-accessibility-api");
  const user = userEvent.setup();
  await mountApp("/nodes/does-not-exist");
  const banner = screen.getByRole("banner");
  const account = await within(banner).findByRole("button", { name: i18n.t("header.account", { name: "admin" }) });
  const names = within(banner).getAllByRole("button").map((button) => computeAccessibleName(button));
  assert.ok(names.every((name) => name.trim() !== ""), `unnamed top bar button among ${JSON.stringify(names)}`);
  assert.deepEqual(names, [common.notices.title, locale.ai.title, common.language.switch, account.getAttribute("aria-label")]);
  assert.ok(within(banner).queryByRole("button", { name: common.header.updateAvailable }) === null, "a failed update check says nothing");

  const language = within(banner).getByRole("button", { name: common.language.switch });
  await user.click(language);
  assert.ok(await screen.findByRole("menuitemradio", { name: "English" }));
  await user.keyboard("{Escape}");
  await waitFor(() => assert.ok(screen.queryByRole("menuitemradio", { name: "English" }) === null, "Escape closes the language menu"));
  assert.equal(language.getAttribute("aria-expanded"), "false");

  await user.click(within(banner).getByRole("button", { name: common.notices.title }));
  const panel = await screen.findByRole("dialog");
  assert.ok(await within(panel).findByText(common.notices.empty));

  await user.keyboard("{Escape}");
  await user.click(account);
  assert.ok(await screen.findByRole("menuitem", { name: common.header.logout }));
});

test("the assistant button opens and closes the resident panel and says so", async () => {
  const user = userEvent.setup();
  await mountApp("/nodes/does-not-exist");
  const banner = screen.getByRole("banner");
  const button = within(banner).getByRole("button", { name: locale.ai.title });
  assert.equal(button.getAttribute("aria-pressed"), "false");
  await user.click(button);
  await waitFor(() => assert.equal(button.getAttribute("aria-pressed"), "true"));
  await user.click(button);
  await waitFor(() => assert.equal(button.getAttribute("aria-pressed"), "false"));
});

test("a configured console title follows the logo in the top bar", async () => {
  serve({ "/api/v1/branding": () => Response.json({ product_name: "Acme AI", console_title: "Lab Console", login_description: "", logo: "" }) });
  await mountApp("/nodes/does-not-exist");
  const banner = screen.getByRole("banner");
  assert.ok(await within(banner).findByText("Lab Console"));
});

test("a menu page has no breadcrumb; a page below one names its section and parent", async () => {
  await mountApp("/apikeys");
  assert.ok(await screen.findByRole("heading", { level: 2, name: locale.apikeys.title }));
  assert.ok(screen.queryByRole("navigation", { name: "breadcrumb" }) === null);
  // The page inside the frame asked for its own data and shows the server's refusal.
  assert.ok(unexpected.includes("/api/v1/apikeys"), `requests: ${unexpected.join(", ")}`);
  await waitFor(() => assert.ok(screen.getAllByRole("alert").some((alert) => alert.textContent?.includes("no stub for /api/v1/apikeys"))));
  cleanup();
  await mountApp("/evaluations/compare");
  const trail = await screen.findByRole("navigation", { name: "breadcrumb" });
  assert.deepEqual(within(trail).getAllByRole("listitem").map((item) => item.textContent), [common.navigation.groups.inference, locale.evaluations.list.breadcrumb.evaluations]);
  assert.equal(within(trail).getByRole("link", { name: locale.evaluations.list.breadcrumb.evaluations }).getAttribute("href"), "/evaluations");
  assert.equal(within(menu()).getByRole("link", { name: common.navigation.evaluations }).getAttribute("aria-current"), "page");
});

test("the old models address lands on the models entry with its query", async () => {
  await mountApp("/ai-store/ai-model?detail=7");
  await waitFor(() => assert.equal(within(menu()).getByRole("link", { name: common.navigation.models }).getAttribute("aria-current"), "page"));
});

// The private cloud page became the external platforms list (ui/src/pages/CloudProvidersPage.tsx) opened on private clouds.
const platforms = {
  "/api/v1/providers": () => Response.json({ items: [
    { id: 1, name: "Pods", kind: "runpod", category: "public_cloud", state: "valid", instance_count: 3, created_at: "", updated_at: "" },
    { id: 2, name: "Hosts", kind: "zstack", category: "private_cloud", state: "valid", instance_count: 2, created_at: "", updated_at: "" },
  ], total: 2 }),
  "/api/v1/providers/kinds": () => Response.json({ items: [], total: 0 }),
};

test("the old private cloud address opens the external platforms page on the private clouds", async () => {
  serve(platforms);
  await mountApp("/zstack");
  assert.ok(await screen.findByRole("heading", { level: 2, name: locale.providers.platforms.title }));
  assert.ok(await screen.findByText("Hosts"));
  // What the old page listed: private clouds, without the public ones.
  assert.equal(screen.queryAllByText("Pods").length, 0);
  assert.ok(within(screen.getByRole("group", { name: "Filtered" })).getByRole("button", { name: /^Clear filter: Platform type Private cloud/ }));
  assert.equal(within(menu()).getByRole("link", { name: common.navigation.platforms }).getAttribute("aria-current"), "page");
});

test("going back from the old private cloud address returns to the page before it, not to the old address", async () => {
  serve(platforms);
  const router = createMemoryRouter([{ path: "*", element: <App /> }], { initialEntries: ["/apikeys", "/zstack"], initialIndex: 1 });
  render(<I18nextProvider i18n={i18n}><ConfigProvider theme={{ token: { motion: false } }}><RouterProvider router={router} /></ConfigProvider></I18nextProvider>);
  await screen.findByRole("heading", { level: 2, name: locale.providers.platforms.title });
  assert.equal(router.state.location.pathname + router.state.location.search, "/providers?category=private_cloud");
  await act(() => router.navigate(-1));
  assert.equal(router.state.location.pathname, "/apikeys");
  assert.ok(await screen.findByRole("heading", { level: 2, name: locale.apikeys.title }));
});

test("without a session the frame is not shown, only the sign-in form", async () => {
  serve({ "/api/v1/auth/me": () => Response.json({ code: "AUTHENTICATION_REQUIRED", message: "sign in" }, { status: 401 }) });
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/nodes"]}><App /></MemoryRouter></I18nextProvider>);
  assert.ok(await screen.findByRole("textbox", { name: locale.auth.username }));
  assert.ok(screen.queryByRole("complementary", { name: common.header.navigation }) === null);
  assert.ok(screen.queryByRole("banner") === null);
});
