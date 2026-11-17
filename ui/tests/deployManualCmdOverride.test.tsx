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
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLInputElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const read = (name: string) => JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${name}.json`, import.meta.url), "utf8"));
const locale = read("deploy");
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en", fallbackLng: "en", defaultNS: "common", interpolation: { escapeValue: false },
  resources: { en: { deploy: locale, images: read("images"), common: read("common") } },
});
const { cleanup, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployImageFields = (await import("../src/pages/deploy/DeployImageFields.tsx")).default;
const DeployRuntimeCommand = (await import("../src/pages/deploy/DeployRuntimeCommand.tsx")).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");

afterEach(cleanup);
after(() => { closeChannels(); dom.window.close(); });

const profile = { name: "vllm", sleep: { supported: false }, quick_form_keys: [], param_specs: [] } as never;
const script = 'set -eu\ncase "${GPUWORKS_NODE_RANK}" in\n  0) exec vllm serve /model ;;\n  *) exec vllm serve /model --headless ;;\nesac';

function open(commandMode: string, command?: string) {
  let form: FormInstance | undefined;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return (
      <Form form={instance} initialValues={{ engine: "vllm", image_mode: "auto", command_mode: commandMode, container_cmd: command }}>
        <DeployImageFields
          engines={{ data: [profile], loading: false }}
          imageMode="auto"
          inferenceSeries={[]}
          imageSeriesLoading={false}
          catalogVersions={[]}
          autoResolutionText="automatic"
          compatibilityWarnings={[]}
        />
        <DeployRuntimeCommand result={null} loading={false} error={null} />
      </Form>
    );
  }
  render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
  return () => form!;
}

test("the single-line Cmd override cannot edit a manual command", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const form = open("manual", script);
  await user.click(screen.getByText(locale.catalog.launchOverrides));
  const override = screen.getByRole("textbox", { name: locale.image.cmdOverride }) as HTMLInputElement;
  assert.equal(override.disabled, true);
  assert.equal(override.value, "", "the script is not mirrored into one line");
  assert.ok(screen.getByText(locale.image.cmdOverrideManualHelp));
  await user.type(override, "x");
  assert.equal(form().getFieldValue("container_cmd"), script);

  const editor = document.querySelector("textarea.deploy-command-editor") as HTMLTextAreaElement;
  assert.equal(editor.value, script);
  await user.type(editor, "\n# tail");
  assert.equal(form().getFieldValue("container_cmd"), `${script}\n# tail`);
  assert.equal(buildPayload(form().getFieldsValue(true)).payload.container_overrides?.cmd_display, `${script}\n# tail`);
});

test("outside manual mode the Cmd override stays an editable argv field", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const form = open("engine");
  await user.click(screen.getByText(locale.catalog.launchOverrides));
  const override = screen.getByRole("textbox", { name: locale.image.cmdOverride }) as HTMLInputElement;
  assert.equal(override.disabled, false);
  await user.type(override, "serve /model");
  assert.equal(form().getFieldValue("container_cmd"), "serve /model");
  assert.equal(screen.queryByText(locale.image.cmdOverrideManualHelp), null);
});
