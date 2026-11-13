import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { GPU, GPUCandidates, Node as GPUNode, EvaluateResponse } from "../src/api.ts";

const { render, screen, cleanup, waitFor, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { Form } = await import("antd");
const { GPUGrid } = await import("../src/pages/deploy/DeployChoiceCards.tsx");
const { gpuSelectOptions } = await import("../src/pages/deploy/gpuOptions.ts");
const { gpuResourceHint } = await import("../src/pages/deploy/gpuResourceHint.ts");
const ResourceStep = (await import("../src/pages/deploy/DeployResourceStep.tsx")).default;
const { api } = await import("../src/api.ts");
const { installFleet } = await import("./helpers/fakeFleet.ts");
const { i18n } = await createI18n(["deploy", "nodes"]);
const t = i18n.getFixedT("en", "deploy");
const tc = i18n.getFixedT("en", "common");
const originalAPI = { ...api };
afterEach(() => { cleanup(); Object.assign(api, originalAPI); });
after(() => dom.window.close());

const gpus = [0, 1].map((index) => ({ index, uuid: `gpu-${index}`, name: "Test GPU", memory_total_bytes: 64 * 1024 ** 3, schedulable: true, health_status: "OK" })) as GPU[];
const node = { uuid: "node-a", name: "node-a", state: "online", gpu_count: 2, gpus } as GPUNode;
const candidates = (available: boolean, uuid = node.uuid): GPUCandidates => ({ node_uuid: uuid, items: [
  { index: 0, available, ...(!available ? { reason: { code: "no_free_gpu" } } : {}) },
  { index: 1, available: true },
] });
let form: ReturnType<typeof Form.useForm>[0];
function Grid({ value, pending = false, uuid = node.uuid, selected = [], cards = gpus }: { value?: GPUCandidates; pending?: boolean; uuid?: string; selected?: number[]; cards?: GPU[] }) {
  [form] = Form.useForm();
  return <I18nextProvider i18n={i18n}><Form form={form} initialValues={{ node_selection_mode: "manual", gpu_indexes: selected }}>
    <GPUGrid node={{ ...node, uuid, gpus: cards }} resourceContext={{ nodeUUID: uuid, candidates: value, pending, t }} />
  </Form></I18nextProvider>;
}

test("resource-unavailable cards show a reason but remain selectable by keyboard", async () => {
  render(<Grid value={candidates(false)} />);
  const busy = screen.getByRole("button", { name: "0" }) as HTMLButtonElement;
  assert.equal(busy.disabled, false);
  assert.ok(busy.classList.contains("is-degraded"));
  assert.match(busy.title, /Resources unavailable: This GPU has no free resources/);
  assert.ok(!(screen.getByRole("button", { name: "1" }) as HTMLButtonElement).classList.contains("is-degraded"));
  busy.focus();
  await userEvent.setup({ document: dom.window.document }).keyboard(" ");
  assert.deepEqual(form.getFieldValue("gpu_indexes"), [0]);
});

test("whole-GPU allocation has a specific bilingual reason without mislabeling shared memory", async () => {
  for (const [lng, wholeText, memoryText] of [
    ["zh", "该 GPU 已被推理服务整卡占用", "可用显存 8.0 GiB，不足所需 16.5 GiB"],
    ["en", "This GPU is fully allocated to an inference service", "Available memory 8.0 GiB is below the required 16.5 GiB"],
  ] as const) {
    const { i18n: localized } = await createI18n(["deploy"], lng);
    const translate = localized.getFixedT(lng, "deploy");
    for (const [code, expected] of [["gpu_whole_allocated", wholeText], ["no_fit", memoryText], ["no_free_gpu", ""]]) {
      const value: GPUCandidates = { node_uuid: node.uuid, items: [{ index: 0, available: false, reason: { code, params: { available_gib: "8.0", required_gib: "16.5" } } }] };
      const context = { nodeUUID: node.uuid, candidates: value, t: translate };
      const hint = gpuResourceHint(gpus[0], context);
      assert.equal(hint?.unavailable, true);
      if (expected) assert.ok(hint?.text.includes(expected), hint?.text);
      if (code !== "gpu_whole_allocated") assert.ok(!hint?.text.includes(wholeText), hint?.text);
      const option = gpuSelectOptions(gpus, localized.getFixedT(lng, "common"), context)[0];
      assert.equal(option.disabled, false);
      if (expected) assert.ok(option.label.includes(expected), option.label);
    }
  }
});

test("a whole-GPU allocation warning stays selectable and removable", async () => {
  const value: GPUCandidates = { node_uuid: node.uuid, items: [{ index: 0, available: false, reason: { code: "gpu_whole_allocated" } }] };
  render(<Grid value={value} />);
  const card = screen.getByRole("button", { name: "0" }) as HTMLButtonElement;
  assert.equal(card.disabled, false);
  assert.match(card.title, /This GPU is fully allocated to an inference service/);
  assert.ok(card.classList.contains("is-degraded"));
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(card);
  assert.deepEqual(form.getFieldValue("gpu_indexes"), [0]);
  await user.click(card);
  assert.deepEqual(form.getFieldValue("gpu_indexes"), []);
});

test("inventory schedulability gates selection regardless of evaluated resource availability", () => {
  const view = render(<Grid value={candidates(true)} />);
  for (const schedulable of [false, true]) {
    for (const available of [false, true]) {
      view.rerender(<Grid value={candidates(available)} cards={gpus.map((gpu) => ({ ...gpu, schedulable }))} />);
      const button = screen.getByRole("button", { name: "0" }) as HTMLButtonElement;
      assert.equal(button.disabled, !schedulable);
      assert.equal(button.classList.contains("is-degraded"), schedulable && !available);
    }
  }
});

test("a shareable dGPU result clears the resource warning without changing selection", async () => {
  const view = render(<Grid value={candidates(false)} />);
  await userEvent.setup({ document: dom.window.document }).click(screen.getByRole("button", { name: "0" }));
  view.rerender(<Grid value={candidates(true)} />);
  const card = screen.getByRole("button", { name: "0" }) as HTMLButtonElement;
  assert.equal(card.disabled, false);
  assert.ok(!card.classList.contains("is-degraded"));
  assert.doesNotMatch(card.title, /Resources unavailable/);
  assert.deepEqual(form.getFieldValue("gpu_indexes"), [0]);
});

test("a selected card that loses resources stays removable and may be selected again", async () => {
  const view = render(<Grid value={candidates(true)} selected={[0]} />);
  view.rerender(<Grid value={candidates(false)} />);
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(screen.getByRole("button", { name: "0" }));
  assert.deepEqual(form.getFieldValue("gpu_indexes"), []);
  await user.click(screen.getByRole("button", { name: "0" }));
  assert.deepEqual(form.getFieldValue("gpu_indexes"), [0]);
});

test("a selected card that becomes unschedulable can be removed but not selected again", async () => {
  render(<Grid value={candidates(false)} selected={[0]} cards={gpus.map((gpu) => ({ ...gpu, schedulable: false }))} />);
  const button = screen.getByRole("button", { name: "0" }) as HTMLButtonElement;
  assert.equal(button.disabled, false);
  await userEvent.setup({ document: dom.window.document }).click(button);
  assert.deepEqual(form.getFieldValue("gpu_indexes"), []);
  await waitFor(() => assert.equal(button.disabled, true));
});

test("pending, missing, partial and old-node feedback means unknown without disabling cards", () => {
  const view = render(<Grid value={candidates(false)} pending />);
  const assertUnknown = () => {
    const button = screen.getByRole("button", { name: "0" }) as HTMLButtonElement;
    assert.equal(button.disabled, false);
    assert.match(button.title, /Resource availability has not been evaluated/);
    assert.doesNotMatch(button.title, /Resources unavailable/);
  };
  assertUnknown();
  view.rerender(<Grid value={candidates(false)} uuid="node-b" />); assertUnknown();
  view.rerender(<Grid />); assertUnknown();
  view.rerender(<Grid value={{ node_uuid: node.uuid, items: [] }} />); assertUnknown();
});

test("dropdown availability is informational and missing device indexes remain disabled", () => {
  for (const schedulable of [false, true]) {
    for (const available of [false, true]) {
      const cards = gpus.map((gpu) => ({ ...gpu, schedulable }));
      const result = gpuSelectOptions(cards, tc, { nodeUUID: node.uuid, candidates: candidates(available), t });
      assert.equal(result[0].disabled, !schedulable);
      assert.equal(result[0].value, 0);
      if (schedulable && !available) assert.match(result[0].label, /This GPU has no free resources/);
    }
  }
  const unnumbered = [{ ...gpus[0], index: -1 }];
  assert.equal(gpuSelectOptions(unnumbered, tc, { nodeUUID: node.uuid, candidates: candidates(true), t })[0].disabled, true);
  assert.equal(gpuSelectOptions([{ ...gpus[0], schedulable: false }], tc, { nodeUUID: node.uuid, selected: [0], t })[0].disabled, false);
});

test("expert form annotates unavailable resources and remains usable when evaluation fails", async () => {
  installFleet(api as never, [node] as never);
  function Expert({ value, pending = false }: { value?: GPUCandidates; pending?: boolean }) {
    [form] = Form.useForm();
    return <I18nextProvider i18n={i18n}><Form form={form} initialValues={{ node_selection_mode: "manual", gpu_indexes: [] }}>
      <ResourceStep selectedNode={node} result={{ ok: false, gpu_candidates: value } as EvaluateResponse} evaluating={pending} />
    </Form></I18nextProvider>;
  }
  const view = render(<Expert value={candidates(false)} />);
  const select = screen.getByRole("combobox", { name: t("resource.gpuSelection") });
  fireEvent.mouseDown(select);
  const busy = await screen.findByText(/GPU 0 · Test GPU.*Resources unavailable/);
  assert.ok(!busy.closest(".ant-select-item-option")?.classList.contains("ant-select-item-option-disabled"));
  fireEvent.click(busy);
  await waitFor(() => assert.deepEqual(form.getFieldValue("gpu_indexes"), [0]));
  view.rerender(<Expert pending />);
  fireEvent.mouseDown(select);
  const unknown = await screen.findByText(/GPU 1 · Test GPU.*has not been evaluated/);
  assert.ok(!unknown.closest(".ant-select-item-option")?.classList.contains("ant-select-item-option-disabled"));
});

test("multi-node selection keeps its inventory behavior without manual single-node feedback", async () => {
  installFleet(api as never, [node] as never);
  render(<I18nextProvider i18n={i18n}><Form initialValues={{ node_selection_mode: "manual", nnodes: 2 }}>
    <ResourceStep selectedNode={node} result={null} />
  </Form></I18nextProvider>);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: t("resource.gpuSelection") }));
  const free = await screen.findByText("GPU 0 · Test GPU");
  assert.ok(!free.closest(".ant-select-item-option")?.classList.contains("ant-select-item-option-disabled"));
});
