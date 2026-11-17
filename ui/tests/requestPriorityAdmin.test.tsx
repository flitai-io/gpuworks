import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
dom.window.HTMLElement.prototype.setPointerCapture = () => {};
dom.window.HTMLElement.prototype.hasPointerCapture = () => false;
dom.window.HTMLElement.prototype.scrollIntoView = () => {};
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot", "KeyboardEvent", "PointerEvent", "MouseEvent"] as const) {
  if (dom.window[name]) Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const read = (lang: string, ns: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${lang}/${ns}.json`, import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { common: read("en", "common"), settings: read("en", "settings"), apikeys: read("en", "apikeys"), analytics: read("en", "analytics") } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { default: PrioritySettingsCard } = await import("../src/pages/settings/PrioritySettingsCard.tsx");
const { KeyPriorityCell } = await import("../src/pages/apikeys/KeyPriority.tsx");
const { default: QualityClassTable } = await import("../src/pages/analytics/QualityClassTable.tsx");
const { FeedbackHost } = await import("../src/ui-kit/feedback/index.tsx");
import type { ApiKey, PriorityClasses } from "../src/api";

const original = { ...api };
afterEach(() => {
  cleanup();
  Object.assign(api, original);
});
after(() => {
  closeChannels();
  dom.window.close();
});

const classes = (): PriorityClasses => ({
  min_value: -100, max_value: 100, policies: ["within_ceiling", "fixed", "down_only"], default_policy: "within_ceiling", ceiling_policies: ["within_ceiling"],
  items: [
    { name: "critical", display_name: "", description: "", value: 40, builtin: true, default: false, deletable: false, created_at: "" },
    { name: "gold", display_name: "Gold", description: "paying tenants", value: 30, builtin: false, default: false, deletable: true, created_at: "" },
    { name: "normal", display_name: "", description: "", value: 0, builtin: true, default: true, deletable: false, created_at: "" },
    // Named by a live key: the server says it cannot be deleted.
    { name: "bound", display_name: "", description: "", value: 10, builtin: false, default: false, deletable: false, created_at: "" },
  ],
});
const wrap = (node: React.ReactNode) => render(<I18nextProvider i18n={i18n}>{node}<FeedbackHost /></I18nextProvider>);
const asRole = (role: string) => { api.me = async () => ({ role }) as never; };

test("the settings card lists the classes, and an administrator adds one, makes one the default and deletes one", async () => {
  asRole("admin");
  let listed = classes();
  const calls: string[] = [];
  api.priorityClasses = async () => listed;
  api.createPriorityClass = async (body) => { calls.push(`create ${body.name} ${body.value} ${body.display_name}`); listed = { ...listed, items: [...listed.items, { ...body, builtin: false, default: false, deletable: true, created_at: "" }] }; return listed.items.at(-1)!; };
  api.setDefaultPriorityClass = async (name) => { calls.push(`default ${name}`); return listed; };
  api.deletePriorityClass = async (name) => { calls.push(`delete ${name}`); };
  wrap(<PrioritySettingsCard />);
  const user = userEvent.setup({ document: dom.window.document });
  const row = await screen.findByTestId("priority-class-gold");
  within(row).getByText("Gold");
  within(row).getByText("gold");
  within(row).getByText("paying tenants");
  const normal = screen.getByTestId("priority-class-normal");
  within(normal).getByText("Default");
  within(normal).getByText("Built-in");
  // The default and built-in classes offer no delete; the default no "make default".
  assert.equal(within(normal).queryByRole("button", { name: "Delete" }), null);
  assert.equal(within(normal).queryByRole("button", { name: "Make default" }), null);
  assert.equal(within(screen.getByTestId("priority-class-critical")).queryByRole("button", { name: "Delete" }), null);
  // Nor one the server says is in use.
  const bound = screen.getByTestId("priority-class-bound");
  assert.equal(within(bound).queryByRole("button", { name: "Delete" }), null);
  within(bound).getByRole("button", { name: "Make default" });

  await user.click(await screen.findByRole("button", { name: "New class" }));
  await user.type(screen.getByLabelText("Name"), "silver");
  await user.type(screen.getByLabelText("Display name"), "Silver");
  const value = screen.getByLabelText("Value");
  await user.clear(value);
  await user.type(value, "10");
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => assert.deepEqual(calls, ["create silver 10 Silver"]));

  await user.click(within(screen.getByTestId("priority-class-gold")).getByRole("button", { name: "Make default" }));
  await waitFor(() => assert.ok(calls.includes("default gold")));
  await user.click(within(screen.getByTestId("priority-class-gold")).getByRole("button", { name: "Delete" }));
  await screen.findByText("Delete priority class Gold?");
  // The confirmation's own Delete is the last one on the page.
  await user.click(screen.getAllByRole("button", { name: "Delete" }).at(-1)!);
  await waitFor(() => assert.ok(calls.includes("delete gold")));
});

test("a user sees the classes but changes none", async () => {
  asRole("user");
  api.priorityClasses = async () => classes();
  wrap(<PrioritySettingsCard />);
  await screen.findByTestId("priority-class-gold");
  screen.getByText("Only administrators can change priority classes.");
  assert.equal(screen.queryByRole("button", { name: "New class" }), null);
  assert.equal(screen.queryByRole("button", { name: "Edit" }), null);
});

test("a load failure says why and retries", async () => {
  asRole("admin");
  let fail = true;
  api.priorityClasses = async () => { if (fail) throw new Error("server down"); return classes(); };
  wrap(<PrioritySettingsCard />);
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText("server down");
  fail = false;
  await user.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByTestId("priority-class-gold");
});

const key = (priority: ApiKey["priority"]): ApiKey => ({ name: "svc", prefix: "gw-svc", created_at: "", all_deployments: true, deployments: [], priority });

test("a key's cell names its class and policy, and an administrator rebinds it", async () => {
  asRole("admin");
  const sent: unknown[] = [];
  api.setApiKeyPriority = async (prefix, priority) => { sent.push([prefix, priority]); return key(priority); };
  let changed = 0;
  wrap(<KeyPriorityCell apiKey={key({ class: "gold", ceiling: "", policy: "fixed" })} classes={classes()} onChanged={() => changed++} />);
  screen.getByText("Gold");
  screen.getByText("Fixed");
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(await screen.findByRole("button", { name: "Edit the request priority of svc" }));
  await screen.findByText("Request priority of svc");
  // A fixed binding has no ceiling to choose.
  assert.equal(screen.queryByLabelText("Ceiling"), null);
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => assert.deepEqual(sent, [["gw-svc", { class: "gold", ceiling: "", policy: "fixed" }]]));
  await waitFor(() => assert.equal(changed, 1));
});

test("an unbound key reads as the default, and a user cannot edit it", async () => {
  asRole("user");
  wrap(<KeyPriorityCell apiKey={key({ class: "", ceiling: "", policy: "" })} classes={classes()} onChanged={() => {}} />);
  screen.getByText("Default");
  screen.getByText("Within ceiling");
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(screen.queryByRole("button", { name: /Edit the request priority/ }), null);
});

test("a refused binding says why in the dialog", async () => {
  asRole("admin");
  const { APIError } = await import("../src/apiError.ts");
  api.setApiKeyPriority = async () => { throw new APIError("PRIORITY_CLASS_INVALID", {}, "no class", 400); };
  wrap(<KeyPriorityCell apiKey={key({ class: "gold", ceiling: "", policy: "fixed" })} classes={classes()} onChanged={() => {}} />);
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(await screen.findByRole("button", { name: "Edit the request priority of svc" }));
  await user.click(await screen.findByRole("button", { name: "Save" }));
  await screen.findByText(/Invalid priority class/);
});

test("the quality table splits by class and names requests from before classes", () => {
  const figures = { requests: 10, completed: 9, length: 0, client_closed: 0, interrupted: 0, error: 1, ttft_p50_ms: 300, ttft_p95_ms: 9000, e2e_p50_ms: 1000, e2e_p95_ms: 12000, output_tokens_per_sec: null };
  wrap(<QualityClassTable rows={[{ ...figures, priority_class: "low" }, { ...figures, priority_class: "" }]} />);
  const table = screen.getByRole("table", { name: "By request priority" });
  within(table).getByText("low");
  within(table).getByText("Unrecorded");
  cleanup();
  const { container } = wrap(<QualityClassTable rows={[]} />);
  assert.equal(container.querySelector("[data-testid=quality-by-class]"), null);
});
