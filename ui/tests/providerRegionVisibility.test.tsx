import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { account, instance, runpodKind, servePlatforms, zstackKind } from "./helpers/platformFixtures.ts";
import { cellUnder, columnTitles, instanceRow } from "./helpers/platformDetail.ts";
import { list } from "./helpers/listPage.ts";

// A private cloud's instances have no region to speak of: opened from the
// External Platforms page (ui/src/pages/CloudProvidersPage.tsx), its account
// lists them without a Region column, and without the region under an
// instance's name (ui/src/pages/platforms/InstanceRow.tsx); a public cloud's
// keep both. Everything else of the row is the same.

const { i18n } = await createI18n(["providers", "kubernetes"], "en");
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { api } = await import("../src/api.ts");

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

const host = instance({ external_id: "host-1", name: "GPU host", gpu_model: "Test GPU", region: "Test region", kind: "", actions: ["install"] });

async function openAccount(kind: "zstack" | "runpod") {
  servePlatforms(api, { providers: [account(1, "Test cloud", kind, { instance_count: 1 })], kinds: [kind === "zstack" ? zstackKind : runpodKind] });
  Object.assign(api, { providerInstances: async () => list([host]) });
  render(<I18nextProvider i18n={i18n}><MemoryRouter><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
  await screen.findByText("Test cloud");
  await userEvent.setup({ document: dom.window.document }).click(screen.getByRole("button", { name: "Expand row" }));
  return instanceRow("GPU host");
}

test("a private cloud's instance list leaves the region out, column and all, and keeps the other columns and onboarding", async () => {
  const row = await openAccount("zstack");
  assert.deepEqual(await columnTitles(), ["Instance", "GPU", "Status", "Connection", "Actions"]);
  assert.equal(screen.queryAllByText(/Test region/).length, 0, "no region in a cell nor under the name");
  assert.equal((await cellUnder(row, "GPU")).textContent, "Test GPU ×1");
  assert.ok(screen.getByRole("button", { name: "Install Agent" }));
});

test("a public cloud's instance list keeps the region: its column, and under the name for a detail too narrow for the column", async () => {
  const row = await openAccount("runpod");
  assert.deepEqual(await columnTitles(), ["Instance", "GPU", "Status", "Region", "Connection", "Actions"]);
  assert.equal((await cellUnder(row, "Region")).textContent, "Test region");
  assert.equal((await cellUnder(row, "Instance")).textContent, "GPU hosthost-1 · Test region");
});
