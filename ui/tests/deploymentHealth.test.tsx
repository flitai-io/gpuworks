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
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const common = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/common.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { common, deployments: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deployments.json", import.meta.url), "utf8")) } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const { HealthTag } = await import("../src/components/DeploymentHealth.tsx");
const { default: HealthCheckFields } = await import("../src/pages/deploy/HealthCheckFields.tsx");
const { HealthCheckEditor } = await import("../src/pages/DeploymentHealthEditor.tsx");
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
import type { FormInstance } from "antd";
import type { Deployment, DeploymentHealth } from "../src/api";

const original = api.setDeploymentHealthCheck;
afterEach(() => {
  cleanup();
  api.setDeploymentHealthCheck = original;
});
after(() => {
  closeChannels();
  dom.window.close();
});

function health(over: Partial<DeploymentHealth> = {}): DeploymentHealth {
  return { status: "ok", threshold_sec: 60, last: null, recent_mean_ms: null, recent_ok: 0, recent: [], ...over };
}

// The form shows the server's health_checkable of the evaluation and decides
// nothing itself: no operation list reaches the component.
test("the deploy form shows the check on when the server says the draft is checkable and sends only what the operator states", async () => {
  let form: FormInstance | undefined;
  function Host({ checkable }: { checkable: boolean | undefined }) {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance} initialValues={{ engine: "vllm" }}>
        <HealthCheckFields healthCheckable={checkable} />
      </Form>
    );
  }
  const view = render(<I18nextProvider i18n={i18n}><Host checkable={true} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  const box = screen.getByRole("checkbox", { name: "Health check" }) as HTMLInputElement;
  const threshold = screen.getByRole("spinbutton", { name: "Threshold" }) as HTMLInputElement;
  assert.equal(box.checked, true, "unstated, a checkable draft is checked, as the server will do");
  assert.equal(threshold.placeholder, "60");
  assert.equal("health_check" in buildPayload(form!.getFieldsValue(true)).payload, false, "the default is the server's");

  await user.click(box);
  assert.equal(box.checked, false);
  assert.equal(buildPayload(form!.getFieldsValue(true)).payload.health_check, false);
  assert.equal(threshold.disabled, true, "no threshold for a check that is off");

  await user.click(box);
  await user.type(threshold, "15");
  await user.tab();
  const payload = buildPayload(form!.getFieldsValue(true)).payload;
  assert.equal(payload.health_check, true);
  assert.equal(payload.health_check_threshold_sec, 15);

  // Until the next evaluation answers the value is unknown: the box offers
  // nothing, says nothing is wrong and keeps the operator's choice.
  view.rerender(<I18nextProvider i18n={i18n}><Host checkable={undefined} /></I18nextProvider>);
  assert.equal((screen.getByRole("checkbox", { name: "Health check" }) as HTMLInputElement).disabled, true);
  assert.equal(screen.queryByText(common.healthCheck.unsupported), null, "unknown is not unsupported");
  assert.equal(form!.getFieldValue("health_check"), true, "an unknown answer keeps the choice");
  view.rerender(<I18nextProvider i18n={i18n}><Host checkable={true} /></I18nextProvider>);
  assert.equal((screen.getByRole("checkbox", { name: "Health check" }) as HTMLInputElement).checked, true);

  // The server says the draft cannot be checked: the box says so and the
  // choice made while it could be is dropped.
  view.rerender(<I18nextProvider i18n={i18n}><Host checkable={false} /></I18nextProvider>);
  await waitFor(() => assert.equal((screen.getByRole("checkbox", { name: "Health check" }) as HTMLInputElement).checked, false));
  assert.equal((screen.getByRole("checkbox", { name: "Health check" }) as HTMLInputElement).disabled, true);
  screen.getByText(common.healthCheck.unsupported);
  await waitFor(() => assert.equal("health_check" in buildPayload(form!.getFieldsValue(true)).payload, false));
});

test("the health tag names each status, with the last time when it answered", async () => {
  const at = Date.now() - 120_000;
  const cases: Array<[DeploymentHealth, string]> = [
    [health({ status: "ok", last: { at_ms: at, status: "ok", latency_ms: 180 } }), "Healthy · 180 ms"],
    [health({ status: "timeout" }), "Timed out"],
    [health({ status: "failed" }), "Failed"],
    [health({ status: "off" }), "Off"],
    [health({ status: "no_replica" }), "No replica"],
    [health({ status: "pending" }), "Checking"],
  ];
  for (const [h, text] of cases) {
    render(<I18nextProvider i18n={i18n}><HealthTag health={h} /></I18nextProvider>);
    screen.getByText(text);
    cleanup();
  }

  render(
    <I18nextProvider i18n={i18n}>
      <HealthTag health={health({ status: "failed", last: { at_ms: at, status: "failed", latency_ms: 30, http_status: 503, instance_uuid: "abcdef0123456789", message: "engine overloaded" }, recent: [{ at_ms: at, status: "failed", latency_ms: 30 }] })} />
    </I18nextProvider>,
  );
  fireEvent.mouseEnter(screen.getByText("Failed"));
  await waitFor(() => screen.getByText("Reason: engine overloaded"));
  screen.getByText("Last check: Failed · 30 ms · 2 min ago");
  screen.getByText("Answered by abcdef01");
  screen.getByText("HTTP 503");
  screen.getByText("None of the last 1 answered in time");
});

test("the detail turns the check off and saves a new threshold, and says why a change was refused", async () => {
  const calls: Array<[number, boolean, number]> = [];
  let refuse = false;
  api.setDeploymentHealthCheck = async (id, enabled, threshold = 0) => {
    calls.push([id, enabled, threshold]);
    if (refuse) throw new Error("the model serves neither chat nor embeddings");
    return {} as Deployment;
  };
  let changed = 0;
  const dep = { id: 5, health_checkable: true, health: health({ status: "ok", threshold_sec: 60 }) } as unknown as Deployment;
  const view = render(<I18nextProvider i18n={i18n}><HealthCheckEditor dep={dep} onChanged={() => changed++} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });

  assert.equal((screen.getByRole("spinbutton", { name: "Threshold" }) as HTMLInputElement).value, "60");
  fireEvent.change(screen.getByRole("spinbutton"), { target: { value: "30" } });
  await user.click(screen.getByRole("button", { name: "Cancel" }));
  assert.deepEqual(calls, []);
  assert.equal((screen.getByRole("spinbutton") as HTMLInputElement).value, "60");
  const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
  assert.equal(save.disabled, true, "nothing to save yet");
  const threshold = screen.getByRole("spinbutton", { name: "Threshold" });
  await user.clear(threshold);
  await user.type(threshold, "20");
  await user.click(save);
  await waitFor(() => assert.deepEqual(calls, [[5, true, 20]]));
  await waitFor(() => assert.equal(changed, 1));
  assert.ok(screen.getByRole("spinbutton", { name: "Threshold" }));
  view.rerender(<I18nextProvider i18n={i18n}><HealthCheckEditor dep={{ ...dep, health: health({ threshold_sec: 20 }) }} onChanged={() => changed++} /></I18nextProvider>);
  fireEvent.change(screen.getByRole("spinbutton"), { target: { value: "40" } });

  await user.click(screen.getByRole("switch", { name: "Health check" }));
  await waitFor(() => assert.deepEqual(calls[1], [5, false, 20]));

  refuse = true;
  await user.click(screen.getByRole("switch", { name: "Health check" }));
  await waitFor(() => assert.ok(dom.window.document.body.textContent?.includes("the model serves neither chat nor embeddings")));
  assert.equal(changed, 2);
});

test("a deployment that cannot be checked can still turn an old check off, but not on", () => {
  const off = { id: 6, health_checkable: false, health: health({ status: "off" }) } as unknown as Deployment;
  render(<I18nextProvider i18n={i18n}><HealthCheckEditor dep={off} onChanged={() => {}} /></I18nextProvider>);
  const toggle = screen.getByRole("switch", { name: "Health check" });
  assert.ok(screen.getByText("Off").compareDocumentPosition(toggle) & Node.DOCUMENT_POSITION_FOLLOWING);
  assert.equal(toggle.getAttribute("aria-disabled"), "true");
  assert.equal(screen.queryByRole("spinbutton", { name: "Threshold" }), null);
  cleanup();
  const on = { id: 7, health_checkable: false, health: health({ status: "failed" }) } as unknown as Deployment;
  render(<I18nextProvider i18n={i18n}><HealthCheckEditor dep={on} onChanged={() => {}} /></I18nextProvider>);
  assert.equal(screen.getByRole("switch", { name: "Health check" }).getAttribute("aria-disabled"), null);
});

test("a deployment the server marks checkable can turn the check on even when its operations list is empty", async () => {
  // The model's operations may still be empty on the wire; the switch follows
  // health_checkable only.
  const calls: Array<[number, boolean, number]> = [];
  api.setDeploymentHealthCheck = async (id, enabled, threshold = 0) => {
    calls.push([id, enabled, threshold]);
    return {} as Deployment;
  };
  const off = { id: 8, operations: [], health_checkable: true, health: health({ status: "off" }) } as unknown as Deployment;
  render(<I18nextProvider i18n={i18n}><HealthCheckEditor dep={off} onChanged={() => {}} /></I18nextProvider>);
  const toggle = screen.getByRole("switch", { name: "Health check" });
  assert.equal(toggle.getAttribute("aria-disabled"), null);
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(toggle);
  await waitFor(() => assert.deepEqual(calls, [[8, true, 60]]));
});
