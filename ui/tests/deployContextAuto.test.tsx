import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import type { EngineParamSpec, EngineProfile } from "../src/api.ts";

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployParamField = (await import("../src/pages/deploy/DeployParamField.tsx")).default;
const { EngineDecidesContext } = await import("../src/pages/deploy/deployParamApplicability.ts");
const { buildPayload, draftToFormValues } = await import("../src/pages/deploy/deployHelpers.ts");
const { editPayload } = await import("../src/pages/deploy/deployEditPayload.ts");
const { i18n } = await createI18n(["deploy"]);
const automatic = i18n.t("shared.automatic", { ns: "deploy" });
const contextSpec: EngineParamSpec = {
  key: "max_model_len", flag: "--max-model-len", type: "integer", control: "number",
  automatic: true, default: -1, label: { en: "Maximum context", zh: "最大上下文" },
};

function open(values: Record<string, unknown>, { engineDecides = false, spec = contextSpec, engine = "vllm" } = {}) {
  let form!: FormInstance;
  const profile = { name: engine, param_specs: [spec] } as EngineProfile;
  function Host() {
    const [instance] = Form.useForm();
    form = instance;
    return <Form form={form} initialValues={{ engine: profile.name, ...values }}>
      <EngineDecidesContext.Provider value={engineDecides ? [spec.key] : []}>
        <DeployParamField spec={spec} />
      </EngineDecidesContext.Provider>
    </Form>;
  }
  render(<I18nextProvider i18n={i18n}><Host /></I18nextProvider>);
  return {
    form: () => form,
    payload: () => buildPayload(form.getFieldsValue(true), profile).payload,
    context: () => screen.getByRole("spinbutton", { name: "Maximum context" }) as HTMLInputElement,
  };
}

afterEach(cleanup);

test("a fitted context displays automatic, can become fixed, and explicitly returns to automatic", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const view = open({ max_model_len: -1 });
  assert.equal(view.context().value, automatic);
  assert.equal(view.context().getAttribute("aria-valuetext"), automatic);
  assert.deepEqual(view.payload().quick_params, { max_model_len: -1 });
  await user.clear(view.context());
  await user.type(view.context(), "16384");
  await user.tab();
  assert.equal(view.context().value, "16384");
  assert.deepEqual(view.payload().quick_params, { max_model_len: 16384 });

  await user.click(screen.getByRole("button", { name: automatic }));
  await waitFor(() => assert.equal(view.context().value, automatic));
  await user.click(view.context());
  await user.tab();
  assert.deepEqual(view.payload().quick_params, { max_model_len: -1 });
  assert.ok(!(view.payload().quick_params_unset as string[] | undefined)?.includes("max_model_len"));
});

test("an incompatible or unknown launch offers no automatic override button", async () => {
  const user = userEvent.setup({ document: dom.window.document });
  const view = open({}, { engineDecides: true });
  assert.equal(view.context().value, "");
  assert.equal(screen.queryAllByRole("button", { name: automatic }).length, 0);
  await user.type(view.context(), "8192");
  await user.tab();
  assert.deepEqual(view.payload().quick_params, { max_model_len: 8192 });
});

test("SGLang context retains its ordinary numeric input", () => {
  const { default: _default, ...spec } = contextSpec;
  const view = open({ max_model_len: 32768 }, { engine: "sglang", spec: { ...spec, flag: "--context-length" } });
  assert.equal(view.context().value, "32768");
  assert.equal(screen.queryAllByRole("button", { name: automatic }).length, 0);
  assert.deepEqual(view.payload().quick_params, { max_model_len: 32768 });
});

test("editing a stored automatic context preserves its numeric intent", () => {
  const profile = { name: "vllm", param_specs: [contextSpec] } as EngineProfile;
  const request = {
    model_id: 7, engine: "vllm", name: "existing", served_name: "existing", image: "vllm/vllm-openai:v0.30.0",
    placement_mode: "manual", node_uuid: "node-1", quick_params: { max_model_len: -1 },
  };
  const values = draftToFormValues(request, profile);
  const view = open(values, { engineDecides: true });
  assert.equal(view.context().value, automatic);
  const payload = editPayload(view.form().getFieldsValue(true), [], profile, [], { request, locked: [] });
  assert.ok(payload);
  assert.deepEqual(payload.quick_params, { max_model_len: -1 });
});

test("manual command ownership disables the automatic context action", () => {
  const view = open({ command_mode: "manual", max_model_len: -1 });
  assert.equal(view.context().disabled, true);
  assert.equal((screen.getByRole("button", { name: automatic }) as HTMLButtonElement).disabled, true);
});
