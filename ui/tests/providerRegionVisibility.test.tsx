import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["providers"], "en");
const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen } = await import("@testing-library/react");
const { default: CloudProvidersPage } = await import("../src/pages/CloudProvidersPage.tsx");
const { default: ProviderInstancesTable } = await import("../src/components/ProviderInstancesTable.tsx");
const { api } = await import("../src/api.ts");
type ProviderInstance = import("../src/api.ts").ProviderInstance;

const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });

const instance: ProviderInstance = {
  external_id: "host-1", name: "GPU host", gpu_model: "Test GPU", gpu_count: 1,
  power: "running", price_per_hr: 0, region: "Test region", public_ip: "192.0.2.1",
  ssh_port: 22, ssh_user: "root", ssh_connections: [], kind: "", node_id: 0, actions: ["install"],
};

function fixture(kind: string) {
  Object.assign(api, {
    // The list endpoints answer with the collection envelope {items, total}.
    providers: async () => ({ items: [{ id: 1, name: "Test cloud", kind, category: kind === "zstack" ? "private_cloud" : "public_cloud", state: "valid", instance_count: 1, created_at: "", updated_at: "" }], total: 1 }),
    providerKinds: async () => ({ items: [], total: 0 }),
    providerInstances: async () => ({ items: [instance], total: 1 }),
  });
}

test("private cloud resource list omits region but retains other columns and onboarding", async () => {
  fixture("zstack");
  render(<I18nextProvider i18n={i18n}><MemoryRouter><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
  await screen.findByText("Test cloud");
  fireEvent.click(screen.getByRole("button", { name: /expand row/i }));
  await screen.findByText("GPU host");
  assert.ok(screen.queryByRole("columnheader", { name: "Region" }) === null, "private cloud must omit the region header");
  assert.ok(screen.queryByText("Test region") === null, "private cloud must omit region cells");
  assert.ok(screen.getByText("Test GPU"));
  assert.ok(screen.getByRole("button", { name: "Install Agent" }));
});

test("public cloud resource list retains region", async () => {
  fixture("runpod");
  render(<I18nextProvider i18n={i18n}><MemoryRouter><CloudProvidersPage /></MemoryRouter></I18nextProvider>);
  await screen.findByText("Test cloud");
  fireEvent.click(screen.getByRole("button", { name: /expand row/i }));
  await screen.findByText("GPU host");
  assert.ok(screen.getByRole("columnheader", { name: "Region" }));
  assert.ok(screen.getByText("Test region"));
});

test("resource table retains region when no visibility option is supplied", async () => {
  fixture("runpod");
  render(<I18nextProvider i18n={i18n}><ProviderInstancesTable providerId={1} refreshKey="initial" /></I18nextProvider>);
  await screen.findByText("GPU host");
  assert.ok(screen.getByRole("columnheader", { name: "Region" }));
  assert.ok(screen.getByText("Test region"));
});
