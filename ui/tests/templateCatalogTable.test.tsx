import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { MemoryRouter } from "react-router-dom";

const { i18n } = await createI18n(["inferenceTemplates", "common"]);
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { TemplateCatalogTable } = await import("../src/pages/inferenceTemplates/TemplateCatalogTable");

afterEach(cleanup);

const spec = (overrides: Record<string, unknown>) => ({
  id: 1, source: "upstream", name: "spec", scenario: "balanced", engine: "vllm", image: "vllm/image:latest", quantization: "fp8",
  weight_sources: [{ provider: "huggingface", repo_id: "org/model" }], gpus: { vendor: "nvidia", name: "H100" },
  ...overrides,
});
const model = (modelID: string, title: string, specs: unknown[]) => ({ id: modelID.length, model_id: modelID, display_title: title, source: "upstream", specs });

function renderTable(models: unknown[]) {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter>
    <TemplateCatalogTable models={models as never} catalogQuery="" onRefresh={() => {}} />
  </MemoryRouter></I18nextProvider>);
}

async function filterOptions(user: ReturnType<typeof userEvent.setup>, column: string) {
  await user.click(screen.getByRole("button", { name: `Filter by ${column}` }));
  await screen.findAllByRole("checkbox");
  return screen.getAllByRole("checkbox").map(box => box.closest("label")?.textContent?.trim() || box.getAttribute("aria-label") || "");
}

test("vendor spellings collapse to one filter option that matches every spelling, and an empty vendor offers none", async () => {
  const user = userEvent.setup();
  renderTable([
    model("a/upper", "Upper", [spec({ gpus: { vendor: "NVIDIA ", name: "H100" } })]),
    model("b/lower", "Lower", [spec({ gpus: { vendor: "nvidia", name: "A100" } })]),
    model("c/blank", "Blank", [spec({ gpus: { vendor: "", name: "?" } })]),
    model("d/amd", "Amd", [spec({ gpus: { vendor: "amd", name: "MI300X" } })]),
  ]);
  assert.deepEqual(await filterOptions(user, "GPU vendor"), ["NVIDIA", "AMD"], "in the product's vendor order");
  await user.click(screen.getByRole("checkbox", { name: /NVIDIA/ }));
  await waitFor(() => assert.equal(screen.queryByRole("link", { name: "Amd" }), null));
  assert.ok(screen.getByRole("link", { name: "Upper" }));
  assert.ok(screen.getByRole("link", { name: "Lower" }));
  assert.equal(screen.queryByRole("link", { name: "Blank" }), null);
});

test("a spec without a quantization is offered as UNKNOWN, the label its cell shows, and filtering by it keeps the model", async () => {
  const user = userEvent.setup();
  renderTable([
    model("a/plain", "Plain", [spec({ quantization: "" })]),
    model("b/fp8", "Fp8", [spec({ quantization: "fp8" })]),
  ]);
  assert.ok(screen.getByText("UNKNOWN · org/model"));
  assert.deepEqual((await filterOptions(user, "Weight artifacts")).sort(), ["FP8", "UNKNOWN"]);
  await user.click(screen.getByRole("checkbox", { name: /UNKNOWN/ }));
  await waitFor(() => assert.equal(screen.queryByRole("link", { name: "Fp8" }), null));
  assert.ok(screen.getByRole("link", { name: "Plain" }));
});

test("a weight artifact names its first source and lists every download source on focus; another mirror stays its own artifact", async () => {
  const user = userEvent.setup();
  renderTable([model("org/model", "Model", [
    spec({ id: 1, weight_sources: [{ provider: "huggingface", repo_id: "org/model" }, { provider: "modelscope", repo_id: "org-ms/model" }] }),
    spec({ id: 2, weight_sources: [{ provider: "huggingface", repo_id: "org/model" }, { provider: "modelscope", repo_id: "org-ms/model" }] }),
    spec({ id: 3, weight_sources: [{ provider: "huggingface", repo_id: "org/model" }] }),
  ])]);
  const badges = screen.getAllByText("FP8 · org/model");
  assert.equal(badges.length, 2, "same quantization and sources are one artifact; a different source set is another");
  const mirrored = badges.map(badge => badge.closest<HTMLElement>("[tabindex]")).find(Boolean);
  assert.ok(mirrored, "the artifact with download sources is reachable from the keyboard");
  await user.hover(mirrored);
  const tip = await screen.findByRole("tooltip");
  assert.match(tip.textContent ?? "", /huggingface: org\/model/);
  assert.match(tip.textContent ?? "", /modelscope: org-ms\/model/);
});

test("the vendor filter lists vendors in the server's order once the dictionary arrives", async () => {
  const fs = await import("node:fs");
  const { act } = await import("@testing-library/react");
  const { setVendorDictionary } = await import("../src/vendors.ts");
  const served = JSON.parse(fs.readFileSync(new URL("./fixtures/accelerator-vendors.json", import.meta.url), "utf8"));
  setVendorDictionary([]);
  try {
    renderTable([
      model("org/a", "A", [spec({ gpus: { vendor: "nvidia", name: "H100" } })]),
      model("org/b", "B", [spec({ gpus: { vendor: "amd", name: "MI300X" } })]),
    ]);
    act(() => setVendorDictionary(served.items));
    const user = userEvent.setup();
    assert.deepEqual(await filterOptions(user, "GPU vendor"), ["NVIDIA", "AMD"]);
  } finally {
    setVendorDictionary(served.items);
  }
});
