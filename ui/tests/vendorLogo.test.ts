import assert from "node:assert/strict";
import { access } from "node:fs/promises";
import test from "node:test";

import { vendorForFramework } from "../src/components/VendorLogo.tsx";
import { VENDOR_ASSETS } from "../src/components/vendorAssets.ts";

test("all accelerator vendors resolve through the shared logo asset map", () => {
  assert.equal(vendorForFramework("dtk"), "hygon");
  assert.equal(vendorForFramework("xre"), "kunlunxin");
  assert.equal(vendorForFramework("KUNLUNXIN"), "kunlunxin");
  assert.equal(vendorForFramework("HYGON"), "hygon");
  assert.equal(VENDOR_ASSETS.hygon, "/vendor-logos/hygon.webp");
  assert.equal(VENDOR_ASSETS.kunlunxin, "/vendor-logos/kunlunxin.png");
  assert.equal(vendorForFramework("hggc"), "ppu");
  assert.ok(VENDOR_ASSETS.thead);
});

test("Apple silicon resolves from its metal framework to the Apple mark", () => {
  assert.equal(vendorForFramework("metal"), "apple");
  assert.equal(vendorForFramework("Apple"), "apple");
  assert.equal(VENDOR_ASSETS.apple, "/vendor-logos/apple.svg");
});

test("every vendor mark is a file shipped under public/vendor-logos", async () => {
  for (const vendor of Object.keys(VENDOR_ASSETS)) {
    const asset = VENDOR_ASSETS[vendor];
    assert.match(asset, /^\/vendor-logos\/[a-z]+\.(svg|png|webp)$/, vendor);
    await access(new URL(`../public${asset}`, import.meta.url));
  }
});
