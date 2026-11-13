import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost/",
});
for (const name of [
  "window",
  "document",
  "navigator",
  "HTMLElement",
  "HTMLInputElement",
  "HTMLTextAreaElement",
  "SVGElement",
  "Element",
  "Node",
  "MutationObserver",
  "ShadowRoot",
] as const) {
  Object.defineProperty(globalThis, name, {
    value: dom.window[name],
    configurable: true,
  });
}
Object.defineProperty(globalThis, "getComputedStyle", {
  value: (element: Element) => dom.window.getComputedStyle(element),
  configurable: true,
});
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({
    matches: false,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent: () => false,
  }),
});
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
Object.defineProperty(globalThis, "ResizeObserver", {
  value: ResizeObserverStub,
  configurable: true,
});

const deployLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/deploy.json", import.meta.url),
    "utf8",
  ),
);
const imagesLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/images.json", import.meta.url),
    "utf8",
  ),
);
const commonLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/common.json", import.meta.url),
    "utf8",
  ),
);
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: {
    en: { deploy: deployLocale, images: imagesLocale, common: commonLocale },
  },
  defaultNS: "common",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor } =
  await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployQuickForm = (
  await import("../src/pages/deploy/DeployQuickForm.tsx")
).default;
const DeployImageFields = (
  await import("../src/pages/deploy/DeployImageFields.tsx")
).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");

afterEach(cleanup);
after(() => {
  closeChannels();
  dom.window.close();
});

const model = {
  id: 1,
  name: "org/model",
  source_id: "org/model",
  input_modalities: ["text"],
} as never;
const profile = {
  name: "vllm",
  sleep: { supported: false },
  quick_form_keys: [],
  param_specs: [],
} as never;

const gib = 2 ** 30;
const vllm = { ...(profile as object), shm_bytes: 16 * gib } as never;
const spec = (name: string, shm_bytes?: number) => ({
  name,
  source: "upstream",
  engine: "vllm",
  weight_sources: [],
  ...(shm_bytes === undefined ? {} : { shm_bytes }),
});
const template = {
  id: 7,
  model_id: "org/model",
  specs: [
    spec("large-shm", 400 * gib),
    spec("small-shm", 4 * gib),
    spec("no-shm"),
  ],
} as never;

function shmField() {
  return screen.getByRole("spinbutton", {
    name: deployLocale.containerOverrides.shm,
  }) as HTMLInputElement;
}
function helpFor(size: string) {
  return deployLocale.containerOverrides.shmEmptyDefault.replace(
    "{{size}}",
    size,
  );
}

// What an empty /dev/shm field promises before the server has evaluated the
// draft is the larger of the engine default and the template spec's own figure.
for (const [name, size] of [
  ["large-shm", "400"],
  ["small-shm", "16"],
  ["no-shm", "16"],
] as const) {
  test(`Quick form: spec ${name} shows ${size} GiB as the /dev/shm default`, () => {
    render(
      <I18nextProvider i18n={i18n}>
        <Form
          initialValues={{
            engine: "vllm",
            command_mode: "engine",
            template_id: 7,
            template_spec_name: name,
            template_spec_source: "upstream",
          }}
        >
          <DeployQuickForm
            templates={[template]}
            selectedModel={model}
            profile={vllm}
            engine="vllm"
            images={[]}
            result={null}
          />
        </Form>
      </I18nextProvider>,
    );
    assert.equal(shmField().placeholder, size);
    assert.ok(screen.getByText(helpFor(size)));
  });
}

test("Quick form: no template selected shows the engine default", () => {
  render(
    <I18nextProvider i18n={i18n}>
      <Form initialValues={{ engine: "vllm", command_mode: "engine" }}>
        <DeployQuickForm
          templates={[template]}
          selectedModel={model}
          profile={vllm}
          engine="vllm"
          images={[]}
          result={null}
        />
      </Form>
    </I18nextProvider>,
  );
  assert.equal(shmField().placeholder, "16");
  assert.ok(screen.getByText(helpFor("16")));
});

test("Quick form: the evaluated size replaces the template hint", () => {
  render(
    <I18nextProvider i18n={i18n}>
      <Form
        initialValues={{
          engine: "vllm",
          command_mode: "engine",
          template_id: 7,
          template_spec_name: "large-shm",
          template_spec_source: "upstream",
        }}
      >
        <DeployQuickForm
          templates={[template]}
          selectedModel={model}
          profile={vllm}
          engine="vllm"
          images={[]}
          result={{ shm_bytes: 419 * gib } as never}
        />
      </Form>
    </I18nextProvider>,
  );
  assert.equal(shmField().placeholder, "419");
});

for (const [stated, size] of [
  [400 * gib, "400"],
  [4 * gib, "16"],
  [undefined, "16"],
] as const) {
  test(`Expert form: template shm_bytes ${stated ?? "absent"} shows ${size} GiB`, async () => {
    let form: FormInstance | undefined;
    function Host() {
      const [instance] = Form.useForm();
      form = instance;
      return (
        <Form form={instance} initialValues={{ engine: "vllm", image_mode: "auto", command_mode: "engine",
          served_name: "keep-this-name", replicas: 2 }}>
          <DeployImageFields
            engines={{ data: [vllm], loading: false }}
            templateShmBytes={stated}
            imageMode="auto"
            inferenceSeries={[]}
            imageSeriesLoading={false}
            catalogVersions={[]}
            autoResolutionText="automatic"
            compatibilityWarnings={[]}
          />
        </Form>
      );
    }
    render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
    // The real page retains engine in Form's store without mounting a field.
    assert.equal(document.querySelector('[id="engine"]'), null);
    assert.equal(form!.getFieldValue("engine"), "vllm");
    assert.equal(form!.getFieldValue("container_shm_gib"), undefined);
    assert.equal(shmField().value, "");
    await waitFor(() => assert.equal(shmField().placeholder, size));
    assert.ok(screen.getByText(helpFor(size)));
    const user = userEvent.setup({ document: dom.window.document });
    await user.type(shmField(), "3.5");
    assert.equal(form!.getFieldValue("container_shm_gib"), 3.5);
    const values = form!.getFieldsValue(true);
    assert.equal(values.engine, "vllm");
    assert.equal(values.served_name, "keep-this-name");
    assert.equal(values.replicas, 2);
    const payload = buildPayload(values).payload;
    const shmBytes = payload.container_overrides!.shm_bytes;
    assert.equal(typeof shmBytes, "number");
    assert.equal(shmBytes, 3.5 * gib);
  });
}
