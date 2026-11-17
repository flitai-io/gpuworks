import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { EngineParamSpec, EngineProfile, KVBackendProfile } from "../src/api.ts";

const { cleanup, fireEvent, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
const { api } = await import("../src/api.ts");
const { list } = await import("./helpers/listPage.ts");
const DeployKvOffloadSection = (await import("../src/pages/deploy/DeployKvOffloadSection.tsx")).default;
const KVTierPanel = (await import("../src/components/KVTierPanel.tsx")).default;
const { buildPayload } = await import("../src/pages/deploy/deployHelpers.ts");
const { i18n, locale } = await createI18n(["deploy", "deployments"]);

// A backend id no source file names: the form must render and project it
// from the server's profile alone.
const synthetic: KVBackendProfile = {
  id: "synthetic-cache-7",
  label: "Synthetic Cache",
  replaces: ["kv_offloading_size", "kv_disk_offload", "shared_kv_dir"],
  pd_roles: ["prefill"],
};
const spec = (key: string, extra: Partial<EngineParamSpec> = {}) =>
  ({ key, flag: `--${key}`, label: key, type: "number", group: "performance", ...extra }) as EngineParamSpec;
const memorySpec = spec("kv_offloading_size");
const diskSpec = spec("kv_disk_offload", { type: "boolean", flag: "--kv-transfer-config", depends_on: "kv_offloading_size" });
const dirSpec = spec("shared_kv_dir", { type: "string", flag: "--kv-transfer-config", depends_on: "kv_disk_offload" });
const selectorSpec = spec("kv_offload_backend", { type: "enum", flag: "", options: [{ value: synthetic.id, label: { en: synthetic.label } }] });
const profile = {
  name: "engine-x",
  param_specs: [memorySpec, diskSpec, dirSpec, selectorSpec],
  kv_backends: [synthetic],
} as unknown as EngineProfile;

let form: import("antd").FormInstance;
function Host({ backends, values = {} }: { backends?: KVBackendProfile[]; values?: Record<string, unknown> }) {
  [form] = Form.useForm();
  return (
    <Form form={form} initialValues={values}>
      <DeployKvOffloadSection kvMemorySpec={memorySpec} kvDiskSpec={diskSpec} kvDiskCompanionSpecs={[dirSpec]} manualCommand={false} backends={backends} />
    </Form>
  );
}
function mount(backends?: KVBackendProfile[], values?: Record<string, unknown>) {
  render(<I18nextProvider i18n={i18n}><Host backends={backends} values={values} /></I18nextProvider>);
}
const card = (title: string) => screen.getByText(title, { exact: true }).closest("button") as HTMLButtonElement;
const active = (title: string) => card(title).classList.contains("is-active");
const originalApi = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originalApi); });

test("no kv_backends (profile loading or none listed) renders exactly the engine's own cards", () => {
  for (const backends of [undefined, []]) {
    mount(backends);
    const titles = [...document.querySelectorAll(".deploy-choice-card strong")].map((node) => node.textContent);
    assert.deepEqual(titles, [locale.deploy.workbench.kv.disabled, locale.deploy.workbench.kv.memory, locale.deploy.workbench.kv.disk]);
    cleanup();
  }
});

test("a backend the UI has never seen renders from the profile and selecting it tombstones its replaces", () => {
  mount([synthetic], { kv_offloading_size: 16, kv_memory_tier_enabled: true });
  assert.equal(active(locale.deploy.workbench.kv.memory), true);
  assert.ok(screen.getByText(locale.deploy.workbench.kv.backendReplaces, { exact: true }));
  fireEvent.click(card("Synthetic Cache"));
  assert.equal(form.getFieldValue("kv_offload_backend"), synthetic.id);
  assert.equal(active("Synthetic Cache"), true);
  assert.equal(active(locale.deploy.workbench.kv.memory), false, "the engine's own tiers go dark under a backend");
  // The memory size the deployer typed survives in the draft, off the wire.
  assert.equal(form.getFieldValue("kv_offloading_size"), 16);
  const { payload, quickParamsUnset } = projected();
  assert.deepEqual(payload, { kv_offload_backend: synthetic.id });
  assert.deepEqual([...quickParamsUnset].sort(), [...synthetic.replaces].sort());
  // Picking an engine tier again clears the selection.
  fireEvent.click(card(locale.deploy.workbench.kv.memory));
  assert.equal(form.getFieldValue("kv_offload_backend"), "");
  const back = projected();
  assert.deepEqual(back.payload, { kv_offloading_size: 16 });
  assert.ok(back.quickParamsUnset.includes("kv_offload_backend"));
});

test("a selection the target's profile no longer lists is cleared, not sent", () => {
  // The deployer picked the backend, then moved to a target whose profile
  // does not list it: the cards fall back to the engine's own tiers, and the
  // payload must match them instead of carrying a hidden selection.
  mount([], { kv_offload_backend: synthetic.id, kv_offloading_size: 16, kv_memory_tier_enabled: true });
  assert.equal(active(locale.deploy.workbench.kv.memory), true);
  const { payload } = buildPayload({ engine: "engine-x", ...form.getFieldsValue(true) }, { ...profile, kv_backends: [] } as EngineProfile);
  assert.deepEqual(payload.quick_params, { kv_offloading_size: 16 });
  assert.ok((payload.quick_params_unset ?? []).includes("kv_offload_backend"));
  // Choosing an engine card clears the hidden value too.
  fireEvent.click(card(locale.deploy.workbench.kv.disabled));
  assert.equal(form.getFieldValue("kv_offload_backend"), "");
});

function projected() {
  const { payload } = buildPayload({ engine: "engine-x", ...form.getFieldsValue(true) }, profile);
  return { payload: payload.quick_params as Record<string, unknown>, quickParamsUnset: (payload.quick_params_unset ?? []) as string[] };
}

test("a backend card is reachable and selected with the keyboard", async () => {
  mount([synthetic]);
  const user = userEvent.setup({ document });
  const target = card("Synthetic Cache");
  target.focus();
  assert.equal(document.activeElement, target);
  await user.keyboard("{Enter}");
  assert.equal(form.getFieldValue("kv_offload_backend"), synthetic.id);
});

test("the instance panel names the backend that keeps the cache", async () => {
  api.metricsQuery = (async () => list([])) as typeof api.metricsQuery;
  render(<I18nextProvider i18n={i18n}><KVTierPanel params={{ kv_offload_backend: synthetic.id, kv_offloading_size: 8 }} instanceUuid="i" backend={{ id: synthetic.id, label: synthetic.label }} /></I18nextProvider>);
  const managed = locale.deployments.kvTier.managedBy.replace("{{label}}", "Synthetic Cache");
  assert.equal(screen.getAllByText(managed, { exact: true }).length, 2);
  cleanup();
  // An instance the server names no backend for keeps no offload at all.
  render(<I18nextProvider i18n={i18n}><KVTierPanel params={{ kv_offload_backend: synthetic.id, kv_offloading_size: 8 }} instanceUuid="i" /></I18nextProvider>);
  assert.equal(screen.getAllByText(locale.deployments.kvTier.disabled, { exact: true }).length, 2);
  cleanup();
  render(<I18nextProvider i18n={i18n}><KVTierPanel params={{}} instanceUuid="i" /></I18nextProvider>);
  assert.equal(screen.getAllByText(locale.deployments.kvTier.disabled, { exact: true }).length, 2);
});
