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
  resources: { en: { common } },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
const { PrioritySchedulingLabel, PrioritySchedulingSwitch, withFixArgs } = await import("../src/pages/deploy/PrioritySchedulingField.tsx");
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
import type { FormInstance } from "antd";
import type { PrioritySchedulingEligibility } from "../src/api";

afterEach(() => cleanup());
after(() => {
  closeChannels();
  dom.window.close();
});

let form: FormInstance | undefined;
function Host({ eligibility, initial }: { eligibility?: PrioritySchedulingEligibility; initial?: Record<string, unknown> }) {
  const [instance] = Form.useForm();
  form = instance;
  return (
    <Form form={instance} initialValues={initial}>
      <Form.Item name="extra_args" noStyle><input type="hidden" /></Form.Item>
      <Form.Item name="priority_scheduling" label={<PrioritySchedulingLabel />}>
        <PrioritySchedulingSwitch
          eligibility={eligibility}
          onApplyFix={(fixArgs) => instance.setFieldsValue({ extra_args: withFixArgs(instance.getFieldValue("extra_args"), fixArgs), priority_scheduling: undefined })}
        />
      </Form.Item>
    </Form>
  );
}
const view = (eligibility?: PrioritySchedulingEligibility, initial?: Record<string, unknown>) =>
  render(<I18nextProvider i18n={i18n}><Host eligibility={eligibility} initial={initial} /></I18nextProvider>);
const box = () => screen.getByRole("checkbox", { name: "Schedule by request priority" });
const payload = () => buildPayload(form!.getFieldsValue(true)).payload as Record<string, unknown>;

test("an allowed draft shows priority scheduling on, leaves the default to the server, and sends what the operator states", async () => {
  view({ ok: true });
  const user = userEvent.setup({ document: dom.window.document });
  assert.equal(box().getAttribute("aria-checked"), "true", "unstated, it is on, as the server will make it");
  assert.equal("priority_scheduling" in payload(), false, "the default is the server's");
  await user.click(box());
  assert.equal(box().getAttribute("aria-checked"), "false");
  assert.equal(payload().priority_scheduling, false);
  box().focus();
  await user.keyboard(" ");
  assert.equal(box().getAttribute("aria-checked"), "true");
  assert.equal(payload().priority_scheduling, true);
  assert.equal(screen.queryByTestId("priority-scheduling-refusal"), null);
});

test("before the evaluation answers, a deployment's own choice stays", () => {
  view(undefined, { priority_scheduling: false });
  assert.equal(box().getAttribute("aria-checked"), "false");
  assert.equal(payload().priority_scheduling, false);
});

test("a refused draft says why, offers nothing to tick, and drops a choice that no longer applies", async () => {
  view({ ok: false, refusal: { code: "PRIORITY_SCHEDULING_PD_UNSUPPORTED" } }, { priority_scheduling: true });
  assert.equal(box().getAttribute("aria-disabled") === "true" || box().hasAttribute("data-disabled"), true);
  assert.equal(box().getAttribute("aria-checked"), "false");
  screen.getByText(/PD disaggregated deployments do not support request priority scheduling yet/);
  await waitFor(() => assert.equal("priority_scheduling" in payload(), false));
  assert.equal(screen.queryByRole("button", { name: /Use/ }), null, "no fix to offer");
});

test("a template policy conflict offers its fix in one click, added to the custom arguments", async () => {
  view(
    { ok: false, refusal: { code: "PRIORITY_SCHEDULING_POLICY_CONFLICT", params: { flag: "--schedule-policy", value: "lpm" } }, fix_args: ["--schedule-policy", "fcfs"] },
    { extra_args: "--trust-remote-code" },
  );
  const user = userEvent.setup({ document: dom.window.document });
  screen.getByText("The launch argument --schedule-policy lpm conflicts with request priority scheduling; this deployment does not distinguish priorities.");
  await user.click(screen.getByRole("button", { name: "Use --schedule-policy fcfs" }));
  assert.equal(form!.getFieldValue("extra_args"), "--trust-remote-code --schedule-policy fcfs");
  assert.equal(withFixArgs("", ["--schedule-policy", "fcfs"]), "--schedule-policy fcfs");
});
