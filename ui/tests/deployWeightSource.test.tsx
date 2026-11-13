import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";

const dom = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost/",
});
for (const name of [
  "window",
  "document",
  "navigator",
  "HTMLElement",
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
    dispatchEvent() {
      return false;
    },
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
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale, models: JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/models.json", import.meta.url), "utf8")) } },
  defaultNS: "deploy",
  interpolation: { escapeValue: false },
});
const { act, cleanup, fireEvent, render, screen, waitFor } = await import(
  "@testing-library/react"
);
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const DeployWeightSourceModal = (await import("../src/pages/deploy/DeployWeightSourceModal.tsx")).default;
const useTemplateWeightSource = (
  await import("../src/pages/deploy/useTemplateWeightSource.ts")
).default;
import type { TemplateWeightModels, WeightSource } from "../src/api";
import { fakeModels, type FixtureModel } from "./helpers/fakeModels.ts";

const apiAny = api as unknown as Record<string, unknown>;
const originals = { models: api.models, modelsPage: api.modelsPage, model: api.model, quickRegisterModel: api.quickRegisterModel, templateWeightModels: api.templateWeightModels };
afterEach(() => {
  cleanup();
  Object.assign(api, originals);
});
after(() => dom.window.close());

// The spec the mounted form selected, as the server's catalog holds it.
let declared: { source: string; name: string; weight_sources: WeightSource[] } | undefined;

// The page answers lookups from a registry the test controls; `hold` makes
// each lookup wait until the test releases (or fails) it.
function serve(registry: FixtureModel[], { hold = false } = {}) {
  const fake = fakeModels(registry);
  const held: Array<{ release: () => void; fail: (error: Error) => void }> = [];
  const registered: unknown[] = [];
  apiAny.models = fake.models_all;
  apiAny.model = fake.model;
  apiAny.modelsPage = fake.modelsPage;
  const answer = fake.templateWeightModels((templateID, source, name) =>
    templateID === 3 && declared && declared.source === source && declared.name === name ? declared.weight_sources : undefined);
  apiAny.templateWeightModels = (templateID: number, source: string, name: string): Promise<TemplateWeightModels> => {
    if (!hold) return answer(templateID, source, name);
    return new Promise((resolve, reject) => {
      // The response belongs to the spec requested, even if the user changes
      // the selected template while its delivery is held.
      answer(templateID, source, name).then((value) => {
        held.push({ release: () => resolve(value), fail: reject });
      }, reject);
    });
  };
  apiAny.quickRegisterModel = async (payload: Record<string, unknown>) => {
    registered.push(payload);
    if (payload.source !== "huggingface" && payload.source !== "modelscope") {
      throw new Error("quick registration requires a Hugging Face or ModelScope source");
    }
    const model = { id: 100 + registered.length, source: payload.source, source_id: payload.source_id, artifact_subfolder: payload.artifact_subfolder };
    registry.push(model as FixtureModel);
    return model;
  };
  return { fake, held, registered };
}

const spec = (weight_sources: unknown[], engine = "vllm") =>
  ({ spec: { name: "balanced", source: "upstream", engine, image: "img/vllm", weight_sources } }) as never;
const twoSources = spec([
  { provider: "huggingface", repo_id: "org/model" },
  { provider: "modelscope", repo_id: "org/model" },
]);
const oneSource = spec([{ provider: "modelscope", repo_id: "Org/Model" }]);

function Host({ selection, engine = "vllm", initialSource }: { selection: unknown; engine?: string; initialSource?: Record<string, unknown> }) {
  const [form] = Form.useForm();
  if (selection) declared = (selection as { spec: typeof declared }).spec;
  const state = useTemplateWeightSource({
    form,
    template: { id: 3 } as never,
    selection: selection as never,
    engineProfile: { name: engine } as never,
  });
  const values = Form.useWatch([], { form, preserve: true }) as Record<string, unknown> | undefined;
  return (
    <Form form={form} initialValues={{ model_source: initialSource }}>
      <span data-testid="modal-state">
        {state.modalOpen ? "open" : "closed"}
      </span>
      <span data-testid="source-choice">{JSON.stringify(values?.model_source ?? {})}</span>
      <span data-testid="model-id">{String(values?.model_id ?? "")}</span>
      <span data-testid="template-id">{String(values?.template_id ?? "")}</span>
      <span data-testid="applied-source">{state.appliedModel ? `${state.appliedModel.source}:${state.appliedModel.source_id}` : ""}</span>
      <DeployWeightSourceModal
        open={state.modalOpen}
        loading={state.registering}
        selection={selection as never}
        choice={state.choice}
        matches={state.matches}
        useModel={state.useModel}
        onUseModel={state.setUseModel}
        onChoice={state.setChoice}
        onCancel={() => state.setModalOpen(false)}
        onConfirm={state.confirm}
      />
    </Form>
  );
}

const mount = (selection: unknown, engine?: string) =>
  render(
    <I18nextProvider i18n={i18n}>
      <Host selection={selection} engine={engine} />
    </I18nextProvider>,
  );
const text = (id: string) => screen.getByTestId(id).textContent;
const settle = () => act(() => new Promise((resolve) => setTimeout(resolve, 30)));
const okButton = (name: string) => screen.getByRole("button", { name }) as HTMLButtonElement;
const chooseSource = (label: string) => {
  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Choose a model source" }));
  fireEvent.click(screen.getByText(label, { selector: ".ant-select-item-option-content" }));
};
const useModelLabel = deployLocale.weightSourceModal.useModel;
const fromTemplateLabel = deployLocale.weightSourceModal.registerFromTemplate;

test("a local-only template without registered weights explains what is missing without registering a model", async () => {
  const { fake, registered } = serve([]);
  mount(spec([{ provider: "local", repo_id: "org/private-model" }]));
  await waitFor(() => assert.equal(fake.weightLookups.length, 1));
  await settle();
  assert.deepEqual(registered, [], "a logical local identity is not a downloadable repository");
  assert.ok(screen.getByText("This template references registered local weights; it does not include the model files. Register the existing model directory in Models, then select the template again."));
  assert.equal(okButton(useModelLabel).disabled, true);
  assert.equal(screen.queryByRole("combobox", { name: "Choose a model source" }), null);
  assert.equal(text("model-id"), "");
});

test("a template with local and public weights offers only downloadable sources for registration", async () => {
  const { registered } = serve([]);
  mount(spec([{ provider: "local", repo_id: "org/private-model" }, { provider: "huggingface", repo_id: "org/public-model" }]));
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  fireEvent.mouseDown(screen.getByRole("combobox", { name: "Choose a model source" }));
  assert.equal(screen.queryByText("Hugging Face · org/private-model") === null, true, "local weights must not be disguised as Hugging Face");
  fireEvent.click(screen.getByText("Hugging Face · org/public-model", { selector: ".ant-select-item-option-content" }));
  fireEvent.click(okButton(deployLocale.action.registerAndDeploy));
  await waitFor(() => assert.equal(text("model-id"), "101"));
  assert.deepEqual(registered.map((value) => (value as { source: string; source_id: string }).source_id), ["org/public-model"]);
  assert.equal(text("applied-source"), "huggingface:org/public-model");
});

test("a local-only template confirms the registered local model without offering remote registration", async () => {
  const { registered } = serve([
    { id: 9, source: "local", source_id: "org/private-model", name: "My local weights", presence: { ready_storages: 1 } },
    { id: 10, source: "huggingface", source_id: "org/private-model", name: "A different public registration" },
  ]);
  mount(spec([{ provider: "local", repo_id: "org/private-model" }]));
  await screen.findByRole("radio", { name: /My local weights/ });
  assert.equal(screen.queryByRole("radio", { name: /A different public registration/ }) === null, true);
  assert.equal(screen.queryByRole("radio", { name: fromTemplateLabel }) === null, true, "a local-only template has no remote registration choice");
  assert.equal(text("model-id"), "", "the local model still requires confirmation");
  fireEvent.click(okButton(useModelLabel));
  await waitFor(() => assert.equal(text("model-id"), "9"));
  assert.equal(text("template-id"), "3");
  assert.equal(text("applied-source"), "local:org/private-model");
  assert.deepEqual(registered, []);
});

test("a previous source confirmation cannot register or replace weights after selecting a local template", async () => {
  const { held, registered } = serve([{ id: 9, source: "local", source_id: "org/private-model", name: "My local weights" }], { hold: true });
  const view = mount(twoSources);
  await waitFor(() => assert.equal(held.length, 1));
  await act(async () => held[0].release());
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  chooseSource("Hugging Face · org/model");
  fireEvent.click(okButton(deployLocale.action.registerAndDeploy));
  await waitFor(() => assert.equal(held.length, 2));
  const localSelection = { spec: { name: "local-spec", source: "user", engine: "vllm", image: "local-image", weight_sources: [{ provider: "local", repo_id: "org/private-model" }] } };
  view.rerender(<I18nextProvider i18n={i18n}><Host selection={localSelection} /></I18nextProvider>);
  await waitFor(() => assert.equal(held.length, 3));
  await act(async () => held[2].release());
  await screen.findByRole("radio", { name: /My local weights/ });
  await act(async () => held[1].release());
  await settle();
  assert.deepEqual(registered, [], "the obsolete source query must not start registration");
  assert.equal(text("model-id"), "");
  fireEvent.click(okButton(useModelLabel));
  await waitFor(() => assert.equal(text("model-id"), "9"));
  assert.equal(text("applied-source"), "local:org/private-model");
});

test("an older lookup cannot replace the registered choices of the current local template", async () => {
  const { held, registered } = serve([{ id: 9, source: "local", source_id: "org/private-model", name: "My local weights" }], { hold: true });
  const view = mount(oneSource);
  await waitFor(() => assert.equal(held.length, 1));
  const localSelection = { spec: { name: "local-spec", source: "user", engine: "vllm", image: "local-image", weight_sources: [{ provider: "local", repo_id: "org/private-model" }] } };
  view.rerender(<I18nextProvider i18n={i18n}><Host selection={localSelection} /></I18nextProvider>);
  await waitFor(() => assert.equal(held.length, 2));
  await act(async () => held[1].release());
  await screen.findByRole("radio", { name: /My local weights/ });
  await act(async () => held[0].release());
  await settle();
  assert.deepEqual(registered, []);
  assert.equal(text("model-id"), "");
  fireEvent.click(okButton(useModelLabel));
  await waitFor(() => assert.equal(text("model-id"), "9"));
});

test("returning to a source while its old registration finishes asks to use the registered model without duplicating it", async () => {
  const registry: FixtureModel[] = [];
  const { fake, registered } = serve(registry);
  let complete!: () => void;
  apiAny.quickRegisterModel = async (payload: Record<string, unknown>) => {
    registered.push(payload);
    assert.equal(payload.source, "modelscope");
    return new Promise((resolve) => {
      complete = () => {
        const model = { id: 71, source: "modelscope", source_id: "Org/Model", name: "Registered while away" };
        registry.push(model);
        resolve(model);
      };
    });
  };
  const view = mount(oneSource);
  await waitFor(() => assert.equal(registered.length, 1));
  const localSelection = { spec: { name: "local-spec", source: "user", engine: "vllm", image: "local-image", weight_sources: [{ provider: "local", repo_id: "org/private-model" }] } };
  view.rerender(<I18nextProvider i18n={i18n}><Host selection={localSelection} /></I18nextProvider>);
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  view.rerender(<I18nextProvider i18n={i18n}><Host selection={oneSource} /></I18nextProvider>);
  await waitFor(() => assert.equal(fake.weightLookups.length, 3));
  await act(async () => complete());
  await screen.findByRole("radio", { name: /Registered while away/ });
  assert.equal(registered.length, 1);
  assert.equal(text("model-id"), "", "the obsolete registration response cannot confirm the model for the user");
  fireEvent.click(okButton(useModelLabel));
  await waitFor(() => assert.equal(text("model-id"), "71"));
});

test("no registered model matches and several sources are declared: the user chooses, nothing is preselected", async () => {
  const { registered } = serve([{ id: 4, source: "modelscope", source_id: "org/other" }]);
  mount(twoSources);
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  assert.ok(screen.getByText(deployLocale.action.selectWeightSource, { selector: ".ant-modal-title" }));
  assert.equal(okButton(deployLocale.action.registerAndDeploy).disabled, true, "no source is chosen yet");
  assert.equal(screen.queryByText("ModelScope · org/model", { selector: ".ant-select-selection-item" }), null, "ModelScope is not the default");
  await settle();
  assert.equal(registered.length, 0);
  chooseSource("ModelScope · org/model");
  await waitFor(() => assert.equal(okButton(deployLocale.action.registerAndDeploy).disabled, false));
  fireEvent.click(okButton(deployLocale.action.registerAndDeploy));
  await waitFor(() => assert.equal(text("model-id"), "101"));
  assert.deepEqual(registered.map((r) => (r as { source: string }).source), ["modelscope"]);
  await waitFor(() => assert.equal(text("template-id"), "3"));
  await waitFor(() => assert.equal(text("applied-source"), "modelscope:org/model"), "the form shows where the weights come from");
});

test("one registered model matches: it is listed with where its files are and preselected", async () => {
  const { registered } = serve([{ id: 7, source: "huggingface", source_id: "org/model", name: "My HF copy", presence: { ready_nodes: 2, ready_storages: 1 } }]);
  mount(twoSources);
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  assert.ok(screen.getByText("My HF copy"));
  assert.ok(screen.getByText("2 node(s) · 1 shared storage(s)"));
  assert.equal((screen.getByRole("radio", { name: /My HF copy/ }) as HTMLInputElement).checked, true);
  await settle();
  assert.equal(text("model-id"), "", "nothing is used before the user confirms");
  fireEvent.click(okButton(useModelLabel));
  await waitFor(() => assert.equal(text("model-id"), "7"));
  await waitFor(() => assert.equal(text("template-id"), "3"));
  assert.equal(text("modal-state"), "closed");
  assert.equal(text("applied-source"), "huggingface:org/model");
  assert.equal(registered.length, 0);
});

test("several registered models match: none is preselected and the user picks one", async () => {
  const { registered } = serve([
    { id: 7, source: "huggingface", source_id: "org/model", name: "HF copy" },
    { id: 8, source: "modelscope", source_id: "ORG/Model", name: "MS copy" },
  ]);
  mount(twoSources);
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  assert.equal(okButton(useModelLabel).disabled, true);
  assert.equal((screen.getByRole("radio", { name: /HF copy/ }) as HTMLInputElement).checked, false);
  assert.equal((screen.getByRole("radio", { name: /MS copy/ }) as HTMLInputElement).checked, false);
  fireEvent.click(screen.getByRole("radio", { name: /MS copy/ }));
  fireEvent.click(okButton(useModelLabel));
  await waitFor(() => assert.equal(text("model-id"), "8"));
  await waitFor(() => assert.equal(text("template-id"), "3"));
  assert.equal(registered.length, 0);
});

test("declining the registered model lists the template's sources and registers the chosen one", async () => {
  const { registered } = serve([{ id: 7, source: "huggingface", source_id: "org/model", name: "HF copy" }]);
  mount(twoSources);
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  assert.equal(screen.queryByRole("combobox", { name: "Choose a model source" }), null, "sources are not offered while a model is preselected");
  fireEvent.click(screen.getByRole("radio", { name: fromTemplateLabel }));
  assert.equal(okButton(deployLocale.action.registerAndDeploy).disabled, true);
  chooseSource("ModelScope · org/model");
  fireEvent.click(okButton(deployLocale.action.registerAndDeploy));
  await waitFor(() => assert.equal(text("model-id"), "101"));
  assert.deepEqual(registered.map((r) => (r as { source: string }).source), ["modelscope"]);
});

test("a single source with a matching registered model asks for confirmation instead of using it silently", async () => {
  const { registered, fake } = serve([{ id: 9, source: "modelscope", source_id: "org/model", name: "Shared copy" }]);
  mount(oneSource);
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  await settle();
  assert.equal(text("model-id"), "");
  fireEvent.click(okButton(useModelLabel));
  await waitFor(() => assert.equal(text("model-id"), "9"));
  assert.equal(text("template-id"), "3");
  assert.equal(registered.length, 0);
  assert.deepEqual(fake.weightLookups, [[3, "upstream", "balanced"]], "the spec's registered models are asked of the server once");
});

test("a single source with no registered match registers directly, once, without a dialog (R2, R3, R8)", async () => {
  const { held, registered } = serve([{ id: 4, source: "modelscope", source_id: "org/other" }], { hold: true });
  const view = mount(oneSource);
  await waitFor(() => assert.equal(held.length, 1));
  await settle();
  assert.equal(registered.length, 0, "nothing is registered while the lookup is pending");
  await act(async () => held[0].release());
  await waitFor(() => assert.equal(text("model-id"), "101"));
  await waitFor(() => assert.equal(text("template-id"), "3"), "the new model applies the template");
  assert.equal(text("modal-state"), "closed", "the registration was not offered as a choice");
  view.rerender(
    <I18nextProvider i18n={i18n}>
      <Host selection={oneSource} />
    </I18nextProvider>,
  );
  await settle();
  assert.equal(registered.length, 1);
  assert.equal(text("modal-state"), "closed", "the model registered here is not asked about afterwards");
});

test("a failed lookup registers nothing and asks nothing (R4)", async () => {
  const { held, registered } = serve([], { hold: true });
  mount(oneSource);
  await waitFor(() => assert.equal(held.length, 1));
  await act(async () => held[0].fail(new Error("server down")));
  await settle();
  assert.equal(registered.length, 0);
  assert.equal(text("model-id"), "");
  assert.equal(text("modal-state"), "closed");
});

test("a failed lookup on confirming a source registers nothing and keeps the dialog (R4, R5)", async () => {
  const { held, registered } = serve([], { hold: true });
  mount(twoSources);
  await waitFor(() => assert.equal(held.length, 1));
  await act(async () => held[0].release());
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  chooseSource("Hugging Face · org/model");
  fireEvent.click(okButton(deployLocale.action.registerAndDeploy));
  await waitFor(() => assert.equal(held.length, 2));
  await act(async () => held[1].fail(new Error("server down")));
  await settle();
  assert.equal(registered.length, 0);
  assert.equal(text("model-id"), "");
  assert.equal(text("modal-state"), "open");
});

test("a confirmed model waits for its engine profile before the template is written (R6)", async () => {
  const { registered } = serve([{ id: 9, source: "modelscope", source_id: "org/model" }]);
  const view = mount(oneSource, "sglang");
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  fireEvent.click(okButton(useModelLabel));
  await settle();
  assert.equal(text("template-id"), "", "the engine profile of another engine does not apply the template");
  view.rerender(
    <I18nextProvider i18n={i18n}>
      <Host selection={oneSource} engine="vllm" />
    </I18nextProvider>,
  );
  await waitFor(() => assert.equal(text("template-id"), "3"));
  assert.equal(text("model-id"), "9");
  assert.equal(registered.length, 0);
});

test("subfolders: a whole-repo model serves a subfolder request, another subfolder does not (R7)", async () => {
  const subfolder = spec([{ provider: "modelscope", repo_id: "org/model", subfolder: "int4" }]);
  const whole = serve([{ id: 9, source: "modelscope", source_id: "org/model" }]);
  const view = mount(subfolder);
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  fireEvent.click(okButton(useModelLabel));
  await waitFor(() => assert.equal(text("model-id"), "9"));
  assert.equal(whole.registered.length, 0);
  view.unmount();
  cleanup();
  const other = serve([{ id: 9, source: "modelscope", source_id: "org/model", artifact_subfolder: "int8" }]);
  mount(subfolder);
  await waitFor(() => assert.equal(text("model-id"), "101"));
  assert.deepEqual(other.registered.map((r) => (r as { artifact_subfolder: string }).artifact_subfolder), ["int4"]);
});


test("selecting a running spec clears a previous model-only provider it does not declare", async () => {
  serve([{ id: 7, source: "local", source_id: "org/model" }]);
  const view = render(<I18nextProvider i18n={i18n}>
    <Host selection={undefined} initialSource={{ provider: "local", storage_id: 9 }} />
  </I18nextProvider>);
  await waitFor(() => assert.deepEqual(JSON.parse(text("source-choice")!), { provider: "local", storage_id: 9 }));
  view.rerender(<I18nextProvider i18n={i18n}>
    <Host selection={twoSources} initialSource={{ provider: "local", storage_id: 9 }} />
  </I18nextProvider>);
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  assert.deepEqual(JSON.parse(text("source-choice")!), { storage_id: 9 },
    "the spec's next evaluation must not send the old local provider; storage choice is preserved");
  fireEvent.click(okButton(useModelLabel));
  await waitFor(() => assert.equal(text("applied-source"), "local:org/model"));
});

test("selecting a running spec preserves an explicit provider it still declares", async () => {
  serve([{ id: 7, source: "local", source_id: "org/model" }]);
  const view = render(<I18nextProvider i18n={i18n}>
    <Host selection={undefined} initialSource={{ provider: "huggingface", storage_id: 9 }} />
  </I18nextProvider>);
  view.rerender(<I18nextProvider i18n={i18n}>
    <Host selection={twoSources} initialSource={{ provider: "huggingface", storage_id: 9 }} />
  </I18nextProvider>);
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  assert.deepEqual(JSON.parse(text("source-choice")!), { provider: "huggingface", storage_id: 9 });
});

test("same-name matches distinguish revisions and select the registration already on shared storage", async () => {
  const revision = "release-ready-1234567890abcdef1234567890abcdef12345678";
  const { registered, fake } = serve([
    { id: 71, name: "Identical name", source: "huggingface", source_id: "org/model", revision: "release-missing", presence: { ready_nodes: 0, ready_storages: 0 } },
    { id: 72, name: "Identical name", source: "huggingface", source_id: "org/model", revision, presence: { ready_nodes: 0, ready_storages: 1 } },
  ]);
  mount(twoSources);
  await waitFor(() => assert.equal(text("modal-state"), "open"));
  assert.ok(screen.queryByText(`Revision: ${revision}`), "full revisions identify the intended registration");
  assert.ok(document.querySelector(`[title="${revision}"]`));
  fireEvent.click(screen.getByRole("radio", { name: new RegExp(revision) }));
  fireEvent.click(okButton(useModelLabel));
  await waitFor(() => assert.equal(text("model-id"), "72"));
  assert.equal(registered.length, 0, "existing shared-storage registration is reused");
  assert.equal(fake.listedAll, 0);
  assert.deepEqual(fake.pages, [], "the form does not page through the model list itself");
});
