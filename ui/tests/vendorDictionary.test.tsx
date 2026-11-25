import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import React from "react";

import { createI18n } from "./helpers/dom.ts";

await createI18n(["common"]);
const { act, cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { loadVendorDictionary, useVendorDictionary, vendorDictionaryRetry } = await import("../src/hooks/useVendorDictionary.ts");
const { compareVendors, setVendorDictionary, vendorBrand, vendorDictionary, vendorFallbackName, vendorForFramework, vendorNamed } = await import("../src/vendors.ts");

// A failed load is left alone for this long; the tests wait it out.
vendorDictionaryRetry.ms = 20;
const retryWindow = () => new Promise((resolve) => setTimeout(resolve, vendorDictionaryRetry.ms * 3));

const served = JSON.parse(fs.readFileSync(new URL("./fixtures/accelerator-vendors.json", import.meta.url), "utf8"));

test.afterEach(() => { cleanup(); setVendorDictionary(served.items); });

test("vendors, their order and their frameworks come from the server's dictionary", () => {
  assert.deepEqual(vendorDictionary().map((entry) => entry.id), served.items.map((entry: { id: string }) => entry.id));
  assert.equal(vendorForFramework("dtk"), "hygon");
  assert.equal(vendorForFramework(" HGGC "), "ppu");
  assert.equal(vendorForFramework("Apple"), "apple");
  assert.equal(vendorBrand("ppu"), "T-Head");
  assert.equal(vendorForFramework("opencl"), "unknown");
  assert.deepEqual(["zeta", "ascend", "amd", "nvidia"].sort((a, b) => compareVendors(a, b)), ["nvidia", "amd", "ascend", "zeta"]);
});

test("a vendor the server adds is known without a change to the console", () => {
  setVendorDictionary([...served.items, { id: "example", brand: "Example", frameworks: ["exf"], runtimes: [], features: [] }]);
  assert.equal(vendorForFramework("exf"), "example");
  assert.equal(vendorBrand("example"), "Example");
  assert.equal(compareVendors("example", "zeta"), -1);
});

test("before the dictionary arrives every vendor is unknown and sorts by name", () => {
  setVendorDictionary([]);
  assert.equal(vendorForFramework("cuda"), "unknown");
  assert.equal(vendorBrand("nvidia"), undefined);
  assert.deepEqual(["nvidia", "amd"].sort((a, b) => compareVendors(a, b)), ["amd", "nvidia"]);
});

test("without the dictionary a logo is still drawn for a vendor's own id, and not for a framework", async () => {
  const { logoVendor } = await import("../src/components/VendorLogo.tsx");
  setVendorDictionary([]);
  assert.equal(logoVendor("nvidia"), "nvidia");
  assert.equal(logoVendor(" Kunlunxin "), "kunlunxin");
  assert.equal(logoVendor("cuda"), "unknown");
  assert.equal(logoVendor("example"), "unknown");
  assert.equal(logoVendor(undefined), "unknown");
  setVendorDictionary(served.items);
  assert.equal(logoVendor("cuda"), "nvidia");
});

function Order() {
  const entries = useVendorDictionary();
  return <output>{entries.length === 0 ? "empty" : entries.map((entry) => entry.brand).join(",")}</output>;
}

test("a failed load is not repeated at once, is tried again later, and a loaded dictionary is not asked for again", async () => {
  setVendorDictionary([]);
  let calls = 0;
  const failing = () => { calls += 1; return Promise.reject(new Error("offline")); };
  await loadVendorDictionary(failing);
  // Rows that mount after a failure do not each send a request.
  await loadVendorDictionary(failing);
  await loadVendorDictionary(failing);
  assert.equal(calls, 1);
  assert.equal(vendorDictionary().length, 0);
  await retryWindow();
  await loadVendorDictionary(() => { calls += 1; return Promise.resolve({ items: served.items.slice(0, 2) }); });
  assert.deepEqual(vendorDictionary().map((entry) => entry.id), ["nvidia", "amd"]);
  await loadVendorDictionary(() => { calls += 1; return Promise.resolve({ items: [] }); });
  assert.equal(calls, 2);
});

test("a page left open through a failed load gets its vendors without being opened again", async () => {
  setVendorDictionary([]);
  await retryWindow();
  let calls = 0;
  // The second answer is held until the test has looked at the page, so
  // what it sees between the two requests does not depend on timing.
  let release = () => {};
  const held = new Promise<void>((resolve) => { release = resolve; });
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () => {
    calls += 1;
    if (calls === 1) throw new Error("offline");
    await held;
    return new Response(JSON.stringify({ items: served.items.slice(0, 2), total: 2 }), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  try {
    render(<Order />);
    await waitFor(() => assert.equal(calls, 2));
    assert.equal(screen.getByRole("status").textContent, "empty");
    release();
    await waitFor(() => assert.equal(screen.getByRole("status").textContent, "NVIDIA,AMD"));
    assert.equal(calls, 2);
  } finally {
    release();
    globalThis.fetch = realFetch;
  }
});

test("with nothing reading the dictionary a failed load is not retried on its own", async () => {
  setVendorDictionary([]);
  await retryWindow();
  let calls = 0;
  await loadVendorDictionary(() => { calls += 1; return Promise.reject(new Error("offline")); });
  await retryWindow();
  assert.equal(calls, 1);
});

test("the dictionary is asked of the server's vendor endpoint", async () => {
  setVendorDictionary([]);
  await retryWindow();
  const asked: string[] = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async (input: RequestInfo | URL) => {
    asked.push(String(input));
    return new Response(JSON.stringify({ items: served.items, total: served.items.length }), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof fetch;
  try {
    await loadVendorDictionary();
  } finally {
    globalThis.fetch = realFetch;
  }
  assert.deepEqual(asked, ["/api/v1/accelerator-vendors"]);
  assert.equal(vendorDictionary().length, served.items.length);
});

test("a vendor's own id names it without the dictionary; with it, an unlisted id is unknown", () => {
  setVendorDictionary([]);
  assert.equal(vendorNamed(" NVIDIA "), "nvidia");
  assert.equal(vendorNamed("ascend"), "ascend");
  assert.equal(vendorNamed(""), "unknown");
  assert.equal(vendorFallbackName("example"), "EXAMPLE");
  setVendorDictionary([...served.items, { id: "example", brand: "Example", frameworks: [], runtimes: [], features: [] }]);
  assert.equal(vendorNamed("example"), "example");
  assert.equal(vendorNamed("intel"), "unknown");
  assert.equal(vendorFallbackName("example"), "Example");
});

test("the dashboard keeps cards of different vendors apart before the dictionary arrives", async () => {
  const { vendorGroups } = await import("../src/pages/dashboard/OverviewParts.tsx");
  const card = (vendor: string, name: string) => ({ vendor, name, memory_total_bytes: 1 << 30 });
  const gpus = { items: [card("nvidia", "A100"), card("ascend", "910B"), card("ascend", "910B")] } as never;
  setVendorDictionary([]);
  assert.deepEqual(vendorGroups(gpus, "?").map((group) => [group.vendor, group.cards]), [["ascend", 2], ["nvidia", 1]]);
  setVendorDictionary(served.items);
  assert.deepEqual(vendorGroups(gpus, "?").map((group) => [group.vendor, group.cards]), [["ascend", 2], ["nvidia", 1]]);
});

test("a logo drawn for a framework changes to the vendor's when the dictionary arrives", async () => {
  const { default: VendorLogo } = await import("../src/components/VendorLogo.tsx");
  setVendorDictionary([]);
  await retryWindow();
  const { container } = render(<VendorLogo framework="cuda" />);
  assert.ok(container.querySelector(".vendor-logo-unknown"));
  act(() => setVendorDictionary(served.items));
  assert.ok(container.querySelector(".vendor-logo-nvidia"));
});

test("a component that reads the dictionary renders again when it arrives", () => {
  setVendorDictionary([]);
  render(<Order />);
  assert.equal(screen.getByRole("status").textContent, "empty");
  act(() => setVendorDictionary(served.items.slice(0, 2)));
  assert.equal(screen.getByRole("status").textContent, "NVIDIA,AMD");
});
