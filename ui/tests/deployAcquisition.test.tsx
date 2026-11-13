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
for (const name of ["window", "document", "navigator", "HTMLElement", "HTMLTextAreaElement", "SVGElement", "Element", "Node", "MutationObserver", "ShadowRoot"] as const) {
  Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true });
}
Object.defineProperty(globalThis, "getComputedStyle", { value: (element: Element) => dom.window.getComputedStyle(element), configurable: true });
Object.defineProperty(dom.window, "matchMedia", { configurable: true, value: () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {}, dispatchEvent: () => false }) });
class ResizeObserverStub { observe() {} unobserve() {} disconnect() {} }
Object.defineProperty(globalThis, "ResizeObserver", { value: ResizeObserverStub, configurable: true });

const deployLocale = JSON.parse(fs.readFileSync(new URL("../src/i18n/locales/en/deploy.json", import.meta.url), "utf8"));
const i18n = createInstance();
await i18n.use(initReactI18next).init({
  lng: "en",
  fallbackLng: "en",
  resources: { en: { deploy: deployLocale } },
  defaultNS: "deploy",
  interpolation: { escapeValue: false },
});

const { cleanup, render, screen, waitFor, fireEvent } = await import("@testing-library/react");
const { Form } = await import("antd");
type FormInstance = import("antd").FormInstance;
const DeployImageRow = (await import("../src/pages/deploy/DeployImageRow.tsx")).default;
const DeployWeightSourceFields = (await import("../src/pages/deploy/DeployWeightSourceFields.tsx")).default;
const DeployNodeAcquisition = (await import("../src/pages/deploy/DeployNodeAcquisition.tsx")).default;
const { MemoryRouter } = await import("react-router-dom");
const DeployStepSummary = (await import("../src/pages/deploy/DeployStepSummary.tsx")).default;
const VRAMBreakdown = (await import("../src/components/VRAMBreakdown.tsx")).default;

afterEach(() => cleanup());

after(() => { closeChannels(); dom.window.close(); });

// An evaluation that failed returns no acquisition at all. Keeping the loading
// text then leaves the image row spinning on a request that already came back,
// and the rows that only exist for a settled answer stay out of the way.
test("an evaluation that resolved no acquisition reports it instead of waiting", () => {
  const view = render(
    <I18nextProvider i18n={i18n}>
      <MemoryRouter>
        <Form>
          <DeployImageRow acquisition={undefined} evaluated />
          <DeployWeightSourceFields acquisition={undefined} />
          <DeployNodeAcquisition acquisition={undefined} automatic />
        </Form>
      </MemoryRouter>
    </I18nextProvider>,
  );
  assert.equal(screen.queryByText(text("loading")), null, "a finished evaluation must not keep claiming it is still checking");
  assert.equal(screen.getAllByText(text("unavailable")).length, 1, "the image row explains the failure once");
  assert.equal(view.container.querySelectorAll(".deploy-workbench-row").length, 1, "no weights or delivery row without an answer");
  view.unmount();

  renderRow(undefined);
  assert.equal(screen.getAllByText(text("loading")).length, 1, "before the first result the image row is still loading");
  assert.equal(screen.queryByText(text("unavailable")), null);
});

test("an empty canonical image explains that no image is available", () => {
  renderRow({ image: { cache_reason: "image_unavailable" } });
  assert.ok(screen.getByText(text("imageUnavailable")));
  assert.equal(screen.queryByText(/vllm\/vllm-openai:latest/), null);
});

const text = (key: string) => deployLocale.workbench.acquisition[key] as string;

test("deployment evaluation renders startup fixed memory and the informational KV pool", () => {
  const breakdown = {
    weight_bytes: 8 * 1024 ** 3, per_dtype: {}, kv_bytes_per_token: 2048, kv_total_bytes: 0,
    reserve_bytes: 1536 * 1024 ** 2, mm_encoder_bytes: 0,
    per_gpu_bytes: 10 * 1024 ** 3, startup_fixed_bytes: 10 * 1024 ** 3,
    total_bytes: 10 * 1024 ** 3, grade: "measured", tensor_parallel: 1,
    max_model_len: 4096, vram_limit_bytes: 16 * 1024 ** 3, capacity_bytes: 14 * 1024 ** 3, kv_pool_bytes: 4 * 1024 ** 3,
    estimated_kv_tokens: 2_000_000, kv_estimate_confidence: "measured", fits: true,
  };
  render(<I18nextProvider i18n={i18n}><Form><DeployStepSummary result={{ ok: true, breakdown } as never} error={null} loading={false} /></Form></I18nextProvider>);
  assert.ok(screen.getByText(deployLocale.summary.passed));
  assert.ok(screen.getByText(deployLocale.vram.title));
  assert.ok(screen.getByText("Estimate confidence: Measured"));
  assert.ok(screen.getByText("dGPU limit 16.0 GiB"));
  assert.ok(screen.getByText(/Startup fixed memory is about/));
  assert.ok(screen.getByText("Minimum startup requirement: 10.0 GiB"));
  assert.equal(screen.queryByText(/^Used [\d.]+ GiB/), null);
  assert.ok(screen.getByText(/KV pool 4\.0 GiB · estimated/));
  assert.equal(screen.queryByText(/Insufficient VRAM/), null);
});

test("an uncertain KV estimate stays a successful startup evaluation", () => {
  render(<I18nextProvider i18n={i18n}><Form><DeployStepSummary result={{ ok: true, breakdown: {
    weight_bytes: 2 * 1024 ** 3, kv_bytes_per_token: 0, kv_total_bytes: 0,
    reserve_bytes: 1536 * 1024 ** 2, mm_encoder_bytes: 0,
    per_gpu_bytes: 4.4 * 1024 ** 3, total_bytes: 4.4 * 1024 ** 3,
    grade: "rough_upper", tensor_parallel: 1, max_model_len: 262144,
    capacity_bytes: 10.8 * 1024 ** 3, kv_pool_bytes: 6.4 * 1024 ** 3,
    estimated_kv_tokens: 0, kv_estimate_confidence: "rough_upper",
    // The server names the reason as a stable code; the sentence is the client's.
    kv_estimate_reason: "kv_no_measurement", fits: true,
  } } as never} error={null} loading={false} /></Form></I18nextProvider>);
  assert.ok(screen.getByText(deployLocale.summary.passed));
  assert.ok(screen.getByText(/no measured KV cost for this model/));
  assert.equal(screen.queryByRole("alert"), null);
});

for (const language of ["en", "zh"] as const) {
  test(`a KV context warning leaves a successful plan visible in ${language}`, async () => {
    const locale = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${language}/deploy.json`, import.meta.url), "utf8"));
    const localized = createInstance();
    await localized.use(initReactI18next).init({
      lng: language, fallbackLng: language,
      resources: { [language]: { deploy: locale } }, defaultNS: "deploy",
      interpolation: { escapeValue: false },
    });
    render(<I18nextProvider i18n={localized}><Form><DeployStepSummary result={{
      ok: true,
      plan: { ok: true, plan: { tp: 1, gpu_indexes: [0] } },
      warnings: [{ code: "PLANNING_KV_BELOW_CONTEXT", params: { kv_tokens: 1024, max_model_len: 262144 } }],
      breakdown: {
        weight_bytes: 2 * 1024 ** 3, kv_bytes_per_token: 2048, kv_total_bytes: 512 * 1024 ** 2,
        reserve_bytes: 1536 * 1024 ** 2, mm_encoder_bytes: 0,
        per_gpu_bytes: 4.4 * 1024 ** 3, total_bytes: 4.4 * 1024 ** 3,
        grade: "measured", tensor_parallel: 1, max_model_len: 262144,
        capacity_bytes: 4.402 * 1024 ** 3, kv_pool_bytes: 2 * 1024 ** 2,
        estimated_kv_tokens: 1024, max_concurrency: 0,
        kv_estimate_confidence: "measured", fits: true,
      },
    } as never} error={null} loading={false} /></Form></I18nextProvider>);
    assert.ok(screen.getByText(locale.summary.risk));
    const warning = screen.getByRole("alert");
    assert.ok(warning.classList.contains("ant-alert-warning"), "KV estimate must render as a warning");
    assert.match(warning.textContent ?? "", /1024/);
    assert.match(warning.textContent ?? "", /262144/);
    assert.match(warning.textContent ?? "", language === "en" ? /continue with the current settings/ : /按当前配置继续部署/);
    assert.doesNotMatch(warning.textContent ?? "", /PLANNING_KV_BELOW_CONTEXT|\{\{/);
    assert.equal(document.querySelector(".ant-alert-error"), null, "successful startup planning must not show a blocking alert");
    assert.equal(screen.queryByText(locale.summary.passed), null, "a KV risk must remain visible instead of claiming all checks passed");
  });
}

test("a failed evaluation states the scheduler's reason, not a generic memory verdict", () => {
  // The summary used to announce a "startup memory check" for every failure,
  // including ones that have nothing to do with memory. The reason the server
  // returned now occupies that line.
  render(<I18nextProvider i18n={i18n}><Form><DeployStepSummary result={{ ok: false, plan: {
    ok: false, message: { code: "tp_head_count_not_divisible", params: { tp: 5, head_count: 64 } },
    explain: { rejected: [{ tp: 5, reason: { code: "tp_head_count_not_divisible", params: { tp: 5, head_count: 64 } } }] },
  } } as never} error={null} loading={false} /></Form></I18nextProvider>);
  assert.ok(screen.getByText("Tensor parallelism tp=5 cannot divide 64 attention heads. Choose a divisible parallelism value."));
  assert.equal(screen.queryByText(/memory check/i), null);
});

test("a model without a KV cache is reported as such, not as an unavailable estimate", () => {
  render(<I18nextProvider i18n={i18n}><Form><DeployStepSummary result={{ ok: true, breakdown: {
    weight_bytes: 2 * 1024 ** 3, kv_bytes_per_token: 0, kv_total_bytes: 0,
    reserve_bytes: 1536 * 1024 ** 2, mm_encoder_bytes: 0,
    per_gpu_bytes: 4.4 * 1024 ** 3, total_bytes: 4.4 * 1024 ** 3,
    grade: "not_applicable", tensor_parallel: 1, max_model_len: 0,
    capacity_bytes: 10.8 * 1024 ** 3, kv_pool_bytes: 6.4 * 1024 ** 3,
    estimated_kv_tokens: 0, kv_estimate_confidence: "not_applicable",
    kv_estimate_reason: "kv_not_applicable", fits: true,
  } } as never} error={null} loading={false} /></Form></I18nextProvider>);
  assert.ok(screen.getByText("Estimate confidence: Not applicable"));
  assert.ok(screen.getByText(/this model keeps no KV cache/));
  assert.equal(screen.queryByText(/token capacity unavailable/), null);
});

test("measured VRAM view reports actual measured usage instead of startup estimate", () => {
  render(<I18nextProvider i18n={i18n}><VRAMBreakdown breakdown={{
    weight_bytes: 8 * 1024 ** 3, kv_bytes_per_token: 2048, kv_total_bytes: 0,
    reserve_bytes: 1536 * 1024 ** 2, mm_encoder_bytes: 0,
    per_gpu_bytes: 10 * 1024 ** 3, startup_fixed_bytes: 10 * 1024 ** 3,
    total_bytes: 10 * 1024 ** 3, actual_total_bytes: 99 * 1024 ** 3,
    grade: "exact", tensor_parallel: 1, max_model_len: 4096, source: "measured", fits: true,
  }} measured={{ ModelWeightsGiB: 8, KVCacheGiB: 2, KVCacheTokens: 1000, GraphCaptureGiB: 1, MaxConcurrency: 1, NumGPUBlocks: 1, Found: true }} /></I18nextProvider>);
  assert.ok(screen.getByText(deployLocale.vram.actualTotal.replace("{{value}}", "11.0")));
  assert.equal(screen.getAllByText(/11\.0 GiB/).length, 1, "the measured total belongs in the summary once");
  assert.equal(screen.queryByText(deployLocale.vram.measured), null, "the calibration badge already identifies the measured bar");
  assert.ok(screen.getByText(/KV pool 2\.0 GiB · 1,000 tokens/));
  assert.equal(screen.queryByText(/Minimum startup requirement/), null);
});

function renderRow(acquisition?: Parameters<typeof DeployImageRow>[0]["acquisition"]) {
  return render(
    <I18nextProvider i18n={i18n}>
      <Form>
        <DeployImageRow acquisition={acquisition} />
      </Form>
    </I18nextProvider>,
  );
}

const withImage = {
  image: {
    image: "vllm:v1", cached: false,
    options: [] as { image: string; reason: string; compatible: boolean }[],
  },
};

test("the image field offers catalog images and nothing else about fetching it", async () => {
  renderRow({ image: { ...withImage.image, options: [
    { image: "catalog/ascend:v1", reason: "catalog_match", compatible: true },
    { image: "catalog/ascend:v2", reason: "catalog_match", compatible: true },
    { image: "catalog/ascend:new-model", reason: "catalog_match", compatible: true },
  ] } });
  const imageSelect = screen.getByRole("combobox", { name: text("imageOptions") });
  const imageRow = imageSelect.closest(".deploy-workbench-row");
  assert.equal(imageRow?.querySelector(".deploy-workbench-label")?.textContent, text("imageTitle"), "the selector uses the same labeled row as the rest of quick deploy");
  assert.ok(imageRow?.textContent?.includes(text("imageSelectionDescription")), "the row explains automatic selection without adding another control");
  fireEvent.mouseDown(imageSelect);
  const secondCatalogImage = await screen.findByText("catalog/ascend:v2");
  fireEvent.click(secondCatalogImage);
  await waitFor(() => assert.ok(screen.getAllByText("catalog/ascend:v2").length >= 1));
  assert.ok(screen.getByText("catalog/ascend:new-model"), "a same-runtime image remains available when model labels are unfamiliar");
  assert.equal(screen.queryByText(/Pull order/), null);
  assert.equal(screen.queryByText(/registry/i), null, "the image row only picks the image; where it is fetched from is not asked");
});

test("selecting a catalog image chooses its engine when no template is selected", async () => {
  let formInstance: FormInstance | undefined;
  function Harness() {
    const [form] = Form.useForm();
    formInstance = form;
    return <Form form={form} initialValues={{ engine: "vllm" }}>
      <DeployImageRow
        acquisition={{ image: { image: "catalog/vllm:v1", options: [
          { image: "catalog/vllm:v1", reason: "catalog_match", compatible: true },
          { image: "catalog/sglang:v1", reason: "catalog_match", compatible: true },
        ] } }}
        images={[
          { image_ref: "catalog/vllm:v1", engine: "vllm" },
          { image_ref: "catalog/sglang:v1", engine: "sglang" },
        ] as never}
      />
    </Form>;
  }
  render(<I18nextProvider i18n={i18n}><Harness /></I18nextProvider>);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: text("imageOptions") }));
  const sglangOptions = await screen.findAllByText("catalog/sglang:v1");
  fireEvent.click(sglangOptions[sglangOptions.length - 1]);
  await waitFor(() => {
    assert.equal(formInstance?.getFieldValue("image"), "catalog/sglang:v1");
    assert.equal(formInstance?.getFieldValue("engine"), "sglang");
  });
});

test("before the first evaluation the image row explains that it is still checking", () => {
  renderRow(undefined);
  assert.equal(screen.getAllByText(text("loading")).length, 1);
  assert.equal(screen.queryByText(text("weightTitle")), null);
});
