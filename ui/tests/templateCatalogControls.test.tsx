import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { DetectedTemplateChip, InferenceTemplateChip } from "../src/api";
const { i18n } = await createI18n(["inferenceTemplates"]);
const { cleanup, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { TemplateChipPicker } = await import("../src/pages/inferenceTemplates/TemplateChipPicker");
const { TemplateHardwareFilters } = await import("../src/pages/inferenceTemplates/TemplateHardwareFilters");
const { chipKey } = await import("../src/pages/inferenceTemplateFlat");
afterEach(cleanup);
const detected: DetectedTemplateChip = { vendor: "nvidia", name: "NVIDIA H100", cc: "9" };
const catalog: InferenceTemplateChip[] = [
  { vendor: "nvidia", name: "NVIDIA H100", cc: 9, category: "server", architecture: "Hopper" },
  { vendor: "nvidia", name: "NVIDIA A100", cc: 8, category: "server", architecture: "Ampere" },
  { vendor: "amd", name: "AMD Radeon 8060S", arch_id: "gfx1151", category: "consumer", architecture: "RDNA" },
];
const wrap = (children: React.ReactNode) => <I18nextProvider i18n={i18n}>{children}</I18nextProvider>;

test("hardware groups show one vendor logo for multiple models without explanatory legends", () => {
  const other = { vendor: "nvidia", name: "NVIDIA A100", cc: "8" };
  const { container } = render(wrap(<TemplateHardwareFilters chips={[detected, other]} detectedKeys={new Set([chipKey(detected)])} value="" onChange={() => {}} />));
  assert.equal(container.querySelectorAll('img[alt="NVIDIA"]').length, 1);
  assert.ok(screen.getByRole("button", { name: "H100" }));
  assert.ok(screen.getByRole("button", { name: "A100" }));
  assert.ok(screen.queryByText("Detected in this environment") === null);
  assert.ok(screen.queryByText("Hypothetical model") === null);
  assert.equal(container.querySelectorAll("[title='Detected in this environment'], [title='Hypothetical model']").length, 0);
});

test("GPU picker preserves selections across vendors, supports removal, and submits complete hardware identities", async () => {
  let saved: DetectedTemplateChip[] = [];
  render(wrap(<TemplateChipPicker catalog={catalog} existing={[detected]} detectedKeys={new Set([chipKey(detected)])} onClose={() => {}} onApply={chips => { saved = chips; }} />));
  const user = userEvent.setup();
  assert.ok(screen.getByRole("dialog", { name: "Edit chips", exact: true }));
  assert.equal(screen.getByRole("checkbox", { name: /H100/ }).getAttribute("aria-disabled"), "true");
  await user.click(screen.getByRole("checkbox", { name: "A100" }));
  await user.click(screen.getByRole("button", { name: "AMD", exact: true }));
  await user.click(screen.getByRole("checkbox", { name: /gfx1151/ }));
  await user.click(screen.getByRole("button", { name: "Remove A100" }));
  await user.click(screen.getByRole("button", { name: "Save selection" }));
  assert.deepEqual(saved, [{ vendor: "amd", name: "AMD Radeon 8060S", cc: undefined, arch_id: "gfx1151" }]);
});

test("search finds models across brands and cancelling never submits the staged selection", async () => {
  let adds = 0, closes = 0;
  render(wrap(<TemplateChipPicker catalog={catalog} existing={[]} detectedKeys={new Set()} onClose={() => { closes++; }} onApply={() => { adds++; }} />));
  const user = userEvent.setup();
  await user.type(screen.getByRole("textbox", { name: "Search vendor or model" }), "8060");
  await user.click(screen.getByRole("checkbox", { name: /gfx1151/ }));
  await user.click(screen.getByRole("button", { name: "Cancel", exact: true }));
  assert.equal(closes, 1); assert.equal(adds, 0);
});

test("opening the picker before the catalog arrives selects the first available brand when it loads", async () => {
  let saved: DetectedTemplateChip[] = [];
  const props = { existing: [], detectedKeys: new Set<string>(), onClose() {}, onApply(chips: DetectedTemplateChip[]) { saved = chips; } };
  const { rerender } = render(wrap(<TemplateChipPicker {...props} catalog={[]} />));
  assert.equal(screen.queryAllByRole("checkbox").length, 0);
  rerender(wrap(<TemplateChipPicker {...props} catalog={catalog} />));
  const user = userEvent.setup();
  await user.click(screen.getByRole("checkbox", { name: "A100" }));
  await user.click(screen.getByRole("button", { name: "Save selection" }));
  assert.deepEqual(saved, [{ vendor: "nvidia", name: "NVIDIA A100", cc: "8", arch_id: undefined }]);
});

test("saved hypothetical models can be removed even after leaving the catalog, while detected models stay locked", async () => {
  const hypothetical = { vendor: "amd", name: "AMD Radeon 8060S", arch_id: "gfx1151" };
  let saved: DetectedTemplateChip[] | undefined;
  render(wrap(<TemplateChipPicker catalog={[catalog[0]]} existing={[detected, hypothetical]} detectedKeys={new Set([chipKey(detected)])} onClose={() => {}} onApply={chips => { saved = chips; }} />));
  const user = userEvent.setup();
  assert.equal(screen.getByRole("checkbox", { name: /H100/ }).getAttribute("aria-disabled"), "true");
  await user.click(screen.getByRole("button", { name: "AMD", exact: true }));
  const checkbox = screen.getByRole("checkbox", { name: /gfx1151/ });
  assert.equal(checkbox.getAttribute("aria-checked"), "true");
  await user.click(checkbox);
  await user.click(screen.getByRole("button", { name: "Save selection" }));
  assert.deepEqual(saved, []);
});

test("saved chips arriving after opening the picker stay checked and cancelling a removal does not save", async () => {
  const hypothetical = { vendor: "nvidia", name: "NVIDIA A100", cc: "8" };
  let saves = 0, closes = 0;
  const props = { detectedKeys: new Set<string>(), onClose() { closes++; }, onApply() { saves++; } };
  const { rerender } = render(wrap(<TemplateChipPicker {...props} catalog={[]} existing={[]} />));
  rerender(wrap(<TemplateChipPicker {...props} catalog={catalog} existing={[hypothetical]} />));
  const checkbox = screen.getByRole("checkbox", { name: "A100" });
  assert.equal(checkbox.getAttribute("aria-checked"), "true");
  assert.equal(screen.getByRole("button", { name: "Save selection" }).hasAttribute("disabled"), true);
  const user = userEvent.setup();
  await user.click(checkbox);
  await user.click(screen.getByRole("button", { name: "Cancel", exact: true }));
  assert.equal(closes, 1);
  assert.equal(saves, 0);
});
