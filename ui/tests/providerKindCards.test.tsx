import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["providers"], "en");
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen } = await import("@testing-library/react");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { api } = await import("../src/api.ts");
type ProviderKindInfo = import("../src/api.ts").ProviderKindInfo;

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

const capabilities = {
  can_list_instances: true, can_power_control: true, can_install_agent: true, can_create: true,
  can_delete: true, can_inject_user_data: false, has_pricing: true,
};

function fixture(kinds: ProviderKindInfo[]) {
  Object.assign(api, {
    providers: async () => ({ items: [], total: 0 }),
    providerKinds: async () => ({ items: kinds, total: kinds.length }),
  });
}

test("provider type cards show the vendor, its logo and its homepage, without capability tags", async () => {
  fixture([
    { kind: "runpod", label: { en: "RunPod", zh: "RunPod" }, homepage: "https://www.runpod.io", capabilities, credential_fields: [], server_dial_supported: false },
    { kind: "newcloud", label: { en: "New Cloud", zh: "新云" }, capabilities, credential_fields: [], server_dial_supported: false },
  ]);
  const { container } = render(<I18nextProvider i18n={i18n}><MemoryRouter><CloudProvidersPage /></MemoryRouter></I18nextProvider>);

  const add = await screen.findByRole("button", { name: "Add RunPod" });
  assert.equal(add.querySelector("img")?.getAttribute("src"), "/provider-logos/runpod.webp");
  const homepage = screen.getByRole("link", { name: "www.runpod.io" });
  assert.equal(homepage.getAttribute("href"), "https://www.runpod.io");
  assert.equal(homepage.getAttribute("target"), "_blank");

  // A kind without a bundled mark or a homepage still renders, with its initial.
  const other = screen.getByRole("button", { name: "Add New Cloud" });
  assert.equal(other.querySelector("img"), null);
  assert.equal(other.textContent, "NNew Cloud");
  assert.equal(container.querySelectorAll(".provider-kind-homepage").length, 1);

  assert.equal(screen.queryByText("Can synchronize"), null);
  assert.equal(screen.queryByText(/Credential fields/), null);

  fireEvent.click(add);
  assert.ok(await screen.findByRole("dialog"));
});

test("public and private clouds share one list, each with its platform type, and sync together", async () => {
  const synced: number[] = [];
  Object.assign(api, {
    providers: async () => ({
      items: [
        { id: 1, name: "Pods", kind: "runpod", category: "public_cloud", state: "valid", instance_count: 3, created_at: "", updated_at: "" },
        { id: 2, name: "Hosts", kind: "zstack", category: "private_cloud", state: "valid", instance_count: 2, created_at: "", updated_at: "" },
      ],
      total: 2,
    }),
    providerKinds: async () => ({ items: [
      { kind: "runpod", label: { en: "RunPod", zh: "RunPod" }, homepage: "https://www.runpod.io", capabilities, credential_fields: [], server_dial_supported: false },
      { kind: "zstack", label: { en: "ZStack Cloud", zh: "ZStack Cloud" }, homepage: "https://www.zstack.io", capabilities, credential_fields: [], server_dial_supported: true },
    ], total: 2 }),
    providerInstances: async () => ({ items: [], total: 0 }),
    syncProvider: async (id: number) => { synced.push(id); return { count: 0, instances: [] }; },
  });
  render(<I18nextProvider i18n={i18n}><MemoryRouter><CloudProvidersPage /></MemoryRouter></I18nextProvider>);

  await screen.findByText("Pods");
  assert.ok(screen.getByText("Hosts"));
  assert.ok(screen.getByRole("columnheader", { name: "Platform type" }));
  assert.ok(screen.getByText("Public cloud"));
  assert.ok(screen.getByText("Private cloud"));
  assert.ok(screen.getByText(/Accounts: 2 · Instances: 5/));
  assert.equal(screen.queryByText(/Invalid: /), null);
  assert.equal(screen.queryByRole("alert"), null);

  fireEvent.click(screen.getByRole("button", { name: "Sync All" }));
  await screen.findByText(/Synchronization complete|synchronized/i);
  assert.deepEqual(synced, [1, 2]);
});

test("an invalid account shows in the summary and raises the warning above the list", async () => {
  Object.assign(api, {
    providers: async () => ({
      items: [{ id: 2, name: "Hosts", kind: "zstack", category: "private_cloud", state: "invalid", instance_count: 2, created_at: "", updated_at: "" }],
      total: 1,
    }),
    providerKinds: async () => ({ items: [
      { kind: "zstack", label: { en: "ZStack Cloud", zh: "ZStack Cloud" }, homepage: "https://www.zstack.io", capabilities, credential_fields: [], server_dial_supported: true },
    ], total: 1 }),
    providerInstances: async () => ({ items: [], total: 0 }),
  });
  render(<I18nextProvider i18n={i18n}><MemoryRouter><CloudProvidersPage /></MemoryRouter></I18nextProvider>);

  await screen.findByText("Hosts");
  assert.ok(screen.getByText("Invalid: 1"));
  assert.ok(screen.getByText("Some external platforms have errors"));
  const add = screen.getByRole("button", { name: "Add ZStack Cloud" });
  assert.equal(add.querySelector("img")?.getAttribute("src"), "/provider-logos/zstack.svg");
});
