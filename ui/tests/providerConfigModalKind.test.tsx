import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

// The Add dialog's Type select must keep what the user picked: the initial
// kind is applied when the dialog opens, not again after every choice.

const { i18n } = await createI18n(["providers"], "en");
const { act, cleanup, fireEvent, render, screen, waitFor } = await import("@testing-library/react");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { api } = await import("../src/api.ts");
type KindInfo = import("../src/api.ts").ProviderKindInfo;

const apiOriginal = { ...api };
afterEach(() => { cleanup(); Object.assign(api, apiOriginal); });

const capabilities = {
  can_list_instances: true, can_power_control: false, can_install_agent: false,
  can_create: false, can_delete: false, can_inject_user_data: false, has_pricing: false,
};
const kindInfos: KindInfo[] = [
  {
    kind: "autodl", label: { en: "AutoDL" }, capabilities, server_dial_supported: false,
    credential_fields: [
      { name: "api_key", label: { en: "SSH Password" }, secret: true, required: true },
      { name: "endpoint", label: { en: "API Endpoint" }, secret: false, required: false, placeholder: { en: "https://api.autodl.com" }, default_value: "https://api.autodl.com" },
    ],
  },
  {
    kind: "runpod", label: { en: "RunPod" }, capabilities, server_dial_supported: false,
    credential_fields: [
      { name: "api_key", label: { en: "RunPod API Key" }, secret: true, required: true },
      { name: "endpoint", label: { en: "API Endpoint" }, secret: false, required: false, placeholder: { en: "https://api.runpod.io/v2" }, default_value: "https://api.runpod.io/v2" },
    ],
  },
];

function mount(defaultKind: string | null) {
  render(
    <I18nextProvider i18n={i18n}>
      <ProviderConfigModal open defaultKind={defaultKind} kindInfos={kindInfos} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => {}} />
    </I18nextProvider>,
  );
}

// typeSelect is the Type select's selector element and selectedLabel the
// text it currently shows.
const typeSelect = () => document.querySelector(".ant-select-selector") as HTMLElement;
const selectedLabel = () => typeSelect().querySelector(".ant-select-selection-item")?.textContent ?? "";

async function pick(label: string) {
  fireEvent.mouseDown(typeSelect());
  await waitFor(() => assert.ok(document.querySelectorAll(".ant-select-item-option").length >= 2));
  const option = [...document.querySelectorAll(".ant-select-item-option")].find((el) => el.textContent === label) as HTMLElement;
  fireEvent.click(option);
}

test("opened without a preselected kind, the first kind shows and another can be chosen", async () => {
  mount(null);
  await waitFor(() => assert.equal(selectedLabel(), "AutoDL"));
  assert.ok(screen.getByText("SSH Password"));
  // The credentials section holds only the inputs, no capability tags.
  assert.equal(screen.queryByText("Capabilities"), null);
  assert.equal(screen.queryByText(/Can synchronize instances|One-click agent installation/), null);

  await pick("RunPod");

  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  assert.ok(screen.getByText("RunPod API Key"));
  assert.equal(screen.queryByText("SSH Password"), null, "the first kind's credential fields are gone");
});

test("opened from a kind card, switching away from that kind sticks", async () => {
  mount("runpod");
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  assert.ok(screen.getByText("RunPod API Key"));

  await pick("AutoDL");

  await waitFor(() => assert.equal(selectedLabel(), "AutoDL"));
  assert.ok(screen.getByText("SSH Password"));
  assert.equal(screen.queryByText("RunPod API Key"), null);
});

test("the chosen kind and typed credentials are what the dialog submits", async () => {
  const sent: unknown[] = [];
  Object.assign(api, { addProvider: async (body: unknown) => { sent.push(body); return { id: 1, kind: "runpod", name: "rp" }; } });
  mount("autodl");
  await waitFor(() => assert.equal(selectedLabel(), "AutoDL"));
  await pick("RunPod");
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));

  fireEvent.change(screen.getByPlaceholderText("For example, My RunPod"), { target: { value: "rp" } });
  fireEvent.change(document.querySelector("input[type=password]") as HTMLElement, { target: { value: "secret-key" } });
  fireEvent.click(screen.getByRole("button", { name: "Add" }));

  await waitFor(() => assert.equal(sent.length, 1));
  const body = sent[0] as Record<string, unknown>;
  assert.equal(body.kind, "runpod");
  assert.equal(body.name, "rp");
  assert.equal(body.api_key, "secret-key");
  assert.equal(body.endpoint, "https://api.runpod.io/v2");
});

test("required fields refuse submission inline and a corrected form can be submitted", async () => {
  const sent: unknown[] = [];
  api.addProvider = async (body) => { sent.push(body); return { id: 1, kind: "runpod", name: body.name }; };
  mount("runpod");
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));

  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  await waitFor(() => {
    assert.ok(screen.getByText("Enter a name"));
    assert.ok(screen.getByText("Enter RunPod API Key"));
  });
  assert.equal(sent.length, 0);
  assert.equal((screen.getByRole("button", { name: "Add" }) as HTMLButtonElement).disabled, false);

  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "corrected" } });
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  await waitFor(() => assert.ok(screen.getByText("Enter RunPod API Key")));
  assert.equal(sent.length, 0);
  fireEvent.change(screen.getByLabelText("RunPod API Key"), { target: { value: "valid-key" } });
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  await waitFor(() => assert.equal(sent.length, 1));
  assert.deepEqual(sent[0], { kind: "runpod", name: "corrected", api_key: "valid-key", endpoint: "https://api.runpod.io/v2" });
});

test("an API refusal stays visible and preserves the form for a successful retry", async () => {
  let attempts = 0;
  let saved = 0;
  api.addProvider = async (body) => {
    attempts++;
    if (attempts === 1) throw new Error("provider credential rejected");
    return { id: 1, kind: "runpod", name: body.name };
  };
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open defaultKind="runpod" kindInfos={kindInfos} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => { saved++; }} /></I18nextProvider>);
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "retry" } });
  fireEvent.change(screen.getByLabelText("RunPod API Key"), { target: { value: "key" } });
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  await waitFor(() => assert.ok(screen.getByText("Add failed: provider credential rejected")));
  assert.equal(saved, 0);
  assert.equal((screen.getByLabelText("Name") as HTMLInputElement).value, "retry");
  assert.equal((screen.getByLabelText("RunPod API Key") as HTMLInputElement).value, "key");
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  await waitFor(() => assert.equal(saved, 1));
  assert.equal(attempts, 2);
});

// The page opens one dialog for Add (optionally preselecting a kind) and Edit,
// and closes it by resetting all three props together, as CloudProvidersPage
// does. These cases drive the dialog through that lifecycle.
type Controls = { openAdd: (kind?: string) => void; openEdit: (provider: { id: number; kind: string; name: string }) => void; close: () => void };
let controls: Controls;
function Page({ kinds }: { kinds: KindInfo[] }) {
  const [open, setOpen] = useState(false);
  const [defaultKind, setDefaultKind] = useState<string | null>(null);
  const [provider, setProvider] = useState<{ id: number; kind: string; name: string } | null>(null);
  controls = {
    openAdd: (kind) => { setProvider(null); setDefaultKind(kind ?? null); setOpen(true); },
    openEdit: (next) => { setDefaultKind(null); setProvider(next); setOpen(true); },
    close: () => { setOpen(false); setDefaultKind(null); setProvider(null); },
  };
  return <ProviderConfigModal open={open} provider={provider as never} defaultKind={defaultKind} kindInfos={kinds} providerLabel="Public Cloud" onClose={controls.close} onSaved={controls.close} />;
}
const page = (kinds: KindInfo[]) => <I18nextProvider i18n={i18n}><Page kinds={kinds} /></I18nextProvider>;

test("a pick does not outlive the dialog: reopening applies the preselected kind again", async () => {
  render(page(kindInfos));
  act(() => controls.openAdd("runpod"));
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  await pick("AutoDL");
  await waitFor(() => assert.equal(selectedLabel(), "AutoDL"));
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  act(() => controls.openAdd("runpod"));
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
});

test("adding after editing starts from the first kind with an empty name", async () => {
  api.provider = async () => ({ id: 1, kind: "runpod", name: "my-rp", endpoint: "", public_key: "", extra: {} });
  render(page(kindInfos));
  act(() => controls.openEdit({ id: 1, kind: "runpod", name: "my-rp" }));
  await waitFor(() => assert.ok(screen.getByDisplayValue("my-rp")));
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  act(() => controls.openAdd());
  await waitFor(() => assert.equal(selectedLabel(), "AutoDL"));
  assert.equal(screen.queryByDisplayValue("my-rp"), null);
});

test("kinds that arrive after the dialog opened seed it once, and a refreshed list keeps the pick", async () => {
  const { rerender } = render(page([]));
  act(() => controls.openAdd("runpod"));
  rerender(page(kindInfos));
  await waitFor(() => assert.equal(selectedLabel(), "RunPod"));
  await pick("AutoDL");
  rerender(page([...kindInfos]));
  await waitFor(() => assert.equal(selectedLabel(), "AutoDL"));
});

const endpointInput = () => screen.getByLabelText("API Endpoint") as HTMLInputElement;

test("new forms use schema defaults, switch defaults, and keep secrets blank", async () => {
  render(page(kindInfos));
  act(() => controls.openAdd("runpod"));
  await waitFor(() => assert.equal(endpointInput().value, "https://api.runpod.io/v2"));
  assert.equal((document.querySelector("input[type=password]") as HTMLInputElement).value, "");
  fireEvent.change(endpointInput(), { target: { value: "https://proxy.example.test/v2" } });
  fireEvent.change(document.querySelector("input[type=password]")!, { target: { value: "do-not-reuse" } });
  await pick("AutoDL");
  await waitFor(() => assert.equal(endpointInput().value, "https://api.autodl.com"));
  assert.equal((document.querySelector("input[type=password]") as HTMLInputElement).value, "");
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  act(() => controls.openAdd("runpod"));
  await waitFor(() => assert.equal(endpointInput().value, "https://api.runpod.io/v2"));
});

test("late schemas seed defaults once and refresh preserves a custom or cleared endpoint", async () => {
  const { rerender } = render(page([]));
  act(() => controls.openAdd("runpod"));
  rerender(page(kindInfos));
  await waitFor(() => assert.equal(endpointInput().value, "https://api.runpod.io/v2"));
  for (const value of ["https://proxy.example.test:8443/custom/v2", ""]) {
    fireEvent.change(endpointInput(), { target: { value } });
    rerender(page(structuredClone(kindInfos)));
    await waitFor(() => assert.equal(endpointInput().value, value));
  }
});

test("custom endpoint is submitted unchanged", async () => {
  const sent: Record<string, unknown>[] = [];
  Object.assign(api, { addProvider: async (body: Record<string, unknown>) => { sent.push(body); return {}; } });
  mount("runpod");
  await waitFor(() => assert.equal(endpointInput().value, "https://api.runpod.io/v2"));
  fireEvent.change(screen.getByPlaceholderText("For example, My RunPod"), { target: { value: "proxy" } });
  fireEvent.change(document.querySelector("input[type=password]")!, { target: { value: "key" } });
  fireEvent.change(endpointInput(), { target: { value: "https://proxy.example.test:8443/custom/v2" } });
  fireEvent.click(screen.getByRole("button", { name: "Add" }));
  await waitFor(() => assert.equal(sent.length, 1));
  assert.equal(sent[0].endpoint, "https://proxy.example.test:8443/custom/v2");
});

test("placeholder examples and secret defaults never become credentials", async () => {
  const custom: KindInfo[] = [{ ...kindInfos[0], credential_fields: [
    { name: "endpoint", label: { en: "API Endpoint" }, secret: false, required: false, placeholder: { en: "https://api.example.com" } },
    { name: "api_key", label: { en: "API Key" }, secret: true, required: true, default_value: "never-copy" },
    { name: "region", label: { en: "Region" }, secret: false, required: false, extra: true, default_value: "region-a" },
  ] }];
  render(page(custom));
  act(() => controls.openAdd());
  await waitFor(() => assert.equal((screen.getByLabelText("Region") as HTMLInputElement).value, "region-a"));
  assert.equal(endpointInput().value, "");
  assert.equal((screen.getByLabelText("API Key") as HTMLInputElement).value, "");
});

test("create defaults do not fill an edit form with a vendor endpoint", async () => {
  render(page(kindInfos));
  act(() => controls.openEdit({ id: 1, kind: "runpod", name: "saved-proxy" }));
  await waitFor(() => assert.ok(screen.getByDisplayValue("saved-proxy")));
  assert.equal(endpointInput().value, "", "edit values must come from saved configuration, not create defaults");
});

test("the fixed edit kind is a labeled disabled textbox after saved config loads", async () => {
  api.provider = async () => ({ id: 7, kind: "runpod", name: "loaded-account", endpoint: "https://proxy.example.test/v2", public_key: "", extra: {} });
  render(page(kindInfos));
  act(() => controls.openEdit({ id: 7, kind: "runpod", name: "list-account" }));
  await waitFor(() => assert.equal((screen.getByLabelText("Name") as HTMLInputElement).value, "loaded-account"));
  const kind = screen.getByRole("textbox", { name: "External platform", exact: true }) as HTMLInputElement;
  assert.equal(screen.getByLabelText("External platform", { exact: true }), kind);
  assert.equal(kind.disabled, true);
  assert.equal(kind.value, "RunPod");
});
