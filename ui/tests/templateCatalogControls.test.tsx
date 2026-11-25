import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import type { DetectedTemplateChip, InferenceTemplateChip } from "../src/api";
const { i18n } = await createI18n(["inferenceTemplates"]);
const { cleanup, render, screen, within, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { TemplateChipPicker } = await import("../src/pages/inferenceTemplates/TemplateChipPicker");
const { TemplateHardwareFilters } = await import("../src/pages/inferenceTemplates/TemplateHardwareFilters");
const { chipKey } = await import("../src/pages/inferenceTemplateFlat");
afterEach(cleanup);
const detected: DetectedTemplateChip = { vendor: "nvidia", name: "NVIDIA H100", cc: "9.0" };
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
  assert.ok(screen.getByRole("dialog", { name: "Manage followed chips", exact: true }));
  const model = screen.getByRole("combobox", { name: "GPU model" });
  await user.click(model);
  assert.equal(screen.getByRole("option", { name: /H100/ }).getAttribute("aria-disabled"), "true");
  await user.click(screen.getByRole("option", { name: "A100" }));
  await user.keyboard("{Escape}");
  await user.click(screen.getByRole("button", { name: "Add", exact: true }));
  screen.getByRole("combobox", { name: "GPU vendor" }).focus();
  await user.keyboard("{ArrowDown}");
  await user.click(await screen.findByRole("option", { name: "AMD", exact: true }));
  await user.click(screen.getByRole("combobox", { name: "GPU model" }));
  await user.click(await screen.findByRole("option", { name: /gfx1151/ }));
  await user.keyboard("{Escape}");
  await user.click(screen.getByRole("button", { name: "Add", exact: true }));
  await user.click(screen.getByRole("button", { name: "Remove A100" }));
  await user.click(screen.getByRole("button", { name: "Save selection" }));
  assert.deepEqual(saved, [{ vendor: "amd", name: "AMD Radeon 8060S", cc: undefined, arch_id: "gfx1151" }]);
});

test("custom names stay pending until added and cancelling never submits the changes", async () => {
  let adds = 0, closes = 0;
  render(wrap(<TemplateChipPicker catalog={catalog} existing={[]} detectedKeys={new Set()} onClose={() => { closes++; }} onApply={() => { adds++; }} />));
  const user = userEvent.setup();
  const model = screen.getByRole("combobox", { name: "GPU model" });
  await user.type(model, "Lab GPU{Enter}");
  assert.equal(screen.getByRole("button", { name: "Save selection" }).hasAttribute("disabled"), true);
  await user.click(screen.getByRole("button", { name: "Add", exact: true }));
  assert.ok(screen.getByRole("button", { name: "Remove Lab GPU" }));
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
  await user.type(screen.getByRole("combobox", { name: "GPU model" }), "a100{Enter}");
  await user.click(screen.getByRole("button", { name: "Add", exact: true }));
  await user.click(screen.getByRole("button", { name: "Save selection" }));
  assert.deepEqual(saved, [{ vendor: "nvidia", name: "NVIDIA A100", cc: "8", arch_id: undefined }]);
});

test("saved hypothetical models can be removed even after leaving the catalog, while detected models stay locked", async () => {
  const hypothetical = { vendor: "amd", name: "AMD Radeon 8060S", arch_id: "gfx1151" };
  let saved: DetectedTemplateChip[] | undefined;
  render(wrap(<TemplateChipPicker catalog={[catalog[0]]} existing={[detected, hypothetical]} detectedKeys={new Set([chipKey(detected)])} onClose={() => {}} onApply={chips => { saved = chips; }} />));
  const user = userEvent.setup();
  const added = within(screen.getByRole("region", { name: "Added chips" }));
  assert.ok(added.getByText("H100"));
  assert.ok(added.getByText("Detected in this environment"));
  assert.ok(added.getByText("Added chips (2)"));
  await user.click(screen.getByRole("button", { name: "Clear all" }));
  assert.ok(added.getByText("Added chips (1)"));
  assert.ok(added.getByText("H100"));
  assert.equal(added.queryByRole("button", { name: "Remove H100" }), null);
  assert.equal(added.getByRole("button", { name: "Clear all" }).hasAttribute("disabled"), true);
  await user.click(screen.getByRole("button", { name: "Save selection" }));
  assert.deepEqual(saved, []);
});

test("saved chips arriving after opening the picker stay checked and cancelling a removal does not save", async () => {
  const hypothetical = { vendor: "nvidia", name: "NVIDIA A100", cc: "8" };
  let saves = 0, closes = 0;
  const props = { detectedKeys: new Set<string>(), onClose() { closes++; }, onApply() { saves++; } };
  const { rerender } = render(wrap(<TemplateChipPicker {...props} catalog={[]} existing={[]} />));
  rerender(wrap(<TemplateChipPicker {...props} catalog={catalog} existing={[hypothetical]} />));
  const remove = screen.getByRole("button", { name: "Remove A100" });
  assert.equal(screen.getByRole("button", { name: "Save selection" }).hasAttribute("disabled"), true);
  const user = userEvent.setup();
  await user.click(remove);
  await user.click(screen.getByRole("button", { name: "Cancel", exact: true }));
  assert.equal(closes, 1);
  assert.equal(saves, 0);
});

test("typing an exact model keeps Enter on that model after hovering another match, and switching vendors retains the draft", async () => {
  const choices = ["H20", "H200"].map(name => ({ ...catalog[0], name: "NVIDIA " + name }));
  let saved: DetectedTemplateChip[] = [];
  render(wrap(<TemplateChipPicker catalog={choices} existing={[]} detectedKeys={new Set()} onClose={() => {}} onApply={chips => { saved = chips; }} />));
  const user = userEvent.setup();
  const model = () => screen.getByRole("combobox", { name: "GPU model" });
  const selectVendor = async (name: string) => {
    screen.getByRole("combobox", { name: "GPU vendor" }).focus();
    await user.keyboard("{ArrowDown}");
    await user.click(await screen.findByRole("option", { name, exact: true }));
    await waitFor(() => assert.equal(screen.getByRole("combobox", { name: "GPU vendor" }).getAttribute("aria-expanded"), "false"));
  };
  await user.type(model(), "H20");
  await user.hover(screen.getByRole("option", { name: "H200", exact: true }));
  await user.keyboard("{Enter}");
  assert.ok(screen.getByRole("button", { name: "Remove H20" }));
  if (model().getAttribute("aria-expanded") === "true") await user.keyboard("{Escape}");
  await selectVendor("AMD");
  await selectVendor("NVIDIA");
  assert.ok(screen.getByRole("button", { name: "Remove H20" }));
  await user.click(screen.getByRole("button", { name: "Add", exact: true }));
  await user.click(screen.getByRole("button", { name: "Save selection" }));
  assert.deepEqual(saved.map(chip => chip.name), ["NVIDIA H20"]);
});
