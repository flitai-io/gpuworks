import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { createI18n } from "./helpers/dom.ts";
import { I18nextProvider } from "react-i18next";

const { i18n } = await createI18n(["common"]);
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: VendorLogo, VendorMarkImage, hasVendorMark } = await import("../src/components/VendorLogo.tsx");
const { VENDOR_ASSETS, VENDOR_LOGO_INK } = await import("../src/components/vendorAssets.ts");

afterEach(cleanup);

const inkClass = (img: HTMLElement) => [...(img.parentElement?.classList ?? [])].filter((name) => name.startsWith("vendor-mark"));

test("VendorLogo draws each mark with the class for its declared ink tone", () => {
  render(<I18nextProvider i18n={i18n}><VendorLogo vendor="nvidia" /><VendorLogo vendor="kunlunxin" /><VendorLogo vendor="ppu" /></I18nextProvider>);
  assert.deepEqual(inkClass(screen.getByRole("img", { name: "NVIDIA" })), ["vendor-mark", "vendor-mark-ink-dark"]);
  assert.deepEqual(inkClass(screen.getByRole("img", { name: "Kunlunxin" })), ["vendor-mark", "vendor-mark-ink-light"]);
  const ppu = screen.getByRole("img", { name: "T-Head" });
  assert.equal(ppu.getAttribute("src"), VENDOR_ASSETS.thead, "PPU cards use the T-Head mark");
  assert.deepEqual(inkClass(ppu), ["vendor-mark", `vendor-mark-ink-${VENDOR_LOGO_INK.thead}`]);
});

test("VendorMarkImage keeps the caller's sizing and draws nothing for a vendor without a mark", () => {
  const { container } = render(<><VendorMarkImage vendor="amd" alt="AMD" style={{ height: 12 }} /><VendorMarkImage vendor="unknown" alt="Unknown" /></>);
  const img = screen.getByRole("img", { name: "AMD" });
  assert.equal(img.style.height, "12px");
  assert.equal(container.querySelectorAll("img").length, 1);
  assert.equal(hasVendorMark("unknown"), false);
  assert.equal(hasVendorMark("ppu"), true);
});

test("every mapped mark declares an ink tone", () => {
  assert.deepEqual(Object.keys(VENDOR_LOGO_INK).sort(), Object.keys(VENDOR_ASSETS).sort());
});
