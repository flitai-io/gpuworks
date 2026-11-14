import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React, { useState } from "react";
import { I18nextProvider } from "react-i18next";
import { kindCategories } from "./helpers/platformFixtures.ts";
import { createI18n, dom } from "./helpers/dom.ts";
import { describedBy, field, freshPopupFramesPerTest, platformChoice } from "./helpers/platformDialog.ts";

// The Add dialog's platform choice must keep what the user picked: the
// initial kind is applied when the dialog opens, not again after every
// choice. And what a vendor's form does with what was typed: what it refuses
// at the field, what it sends, what it says when the server refuses, and when
// it can be closed. Both kinds here are public clouds, so the choice is made
// in the vendors' list; the platform type tabs above it are in
// tests/providerConfigModalConnects.test.tsx.

const { i18n } = await createI18n(["providers"], "en");
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { api } = await import("../src/api.ts");
type KindInfo = import("../src/api.ts").ProviderKindInfo;

const apiOriginal = { ...api };
await freshPopupFramesPerTest();
afterEach(() => { cleanup(); Object.assign(api, apiOriginal); });

const capabilities = {
  can_list_instances: true, can_power_control: false, can_install_agent: false,
  can_create: false, can_delete: false, can_inject_user_data: false, has_pricing: false,
};
const kindInfos: KindInfo[] = [
  {
    kind: "autodl", label: { en: "AutoDL" }, category: "public_cloud", resources: [{ type: "container" }], capabilities, server_dial_supported: false,
    credential_fields: [
      { name: "api_key", label: { en: "SSH Password" }, secret: true, required: true },
      { name: "endpoint", label: { en: "API Endpoint" }, secret: false, required: false, placeholder: { en: "https://api.autodl.com" }, default_value: "https://api.autodl.com" },
    ],
  },
  {
    kind: "runpod", label: { en: "RunPod" }, category: "public_cloud", resources: [{ type: "container", product: "Pod" }], capabilities, server_dial_supported: false,
    credential_fields: [
      { name: "api_key", label: { en: "RunPod API Key" }, secret: true, required: true },
      { name: "endpoint", label: { en: "API Endpoint" }, secret: false, required: false, placeholder: { en: "https://api.runpod.io/v2" }, default_value: "https://api.runpod.io/v2" },
    ],
  },
];

function mount(defaultKind: string | null) {
  render(
    <I18nextProvider i18n={i18n}>
      <ProviderConfigModal open defaultKind={defaultKind} kindInfos={kindInfos} categories={kindCategories(kindInfos)} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => {}} />
    </I18nextProvider>,
  );
  return userEvent.setup({ document: dom.window.document });
}

// The vendors' list is a combobox named by the field's label; what it shows closed is the chosen platform.
const choice = platformChoice();
const chosen = () => screen.getByRole("combobox", { name: "External platform" }).textContent ?? "";
const input = (label: string) => screen.getByLabelText(field(label)) as HTMLInputElement;
const add = () => screen.getByRole("button", { name: "Add" }) as HTMLButtonElement;

test("opened without a preselected kind, the first kind shows and another can be chosen", async () => {
  const user = mount(null);
  await waitFor(() => assert.equal(chosen(), "AutoDL"));
  assert.ok(input("SSH Password"));
  // The credentials section holds only the inputs, under its own heading; no capability tags.
  assert.deepEqual(within(screen.getByRole("dialog")).getAllByRole("heading").map((heading) => [heading.tagName, heading.textContent]), [["H2", "Add Public Cloud"], ["H3", "Credentials"]]);
  assert.equal(screen.queryAllByText("Capabilities").length, 0);
  assert.equal(screen.queryAllByText(/Can synchronize instances|One-click agent installation/).length, 0);
  // What this vendor's API leaves out is said before it looks like a broken key.
  assert.ok(screen.getByText(/^AutoDL's API lists only Pro container instances/));

  await choice.pick(user, "RunPod");

  await waitFor(() => assert.equal(chosen(), "RunPod"));
  assert.ok(input("RunPod API Key"));
  assert.equal(screen.queryAllByText("SSH Password").length, 0, "the first kind's credential fields are gone");
  assert.equal(screen.queryAllByText(/^AutoDL's API lists only/).length, 0, "nor its note");
  assert.ok(screen.getByText("Only Secure Cloud pods are supported; Community Cloud pods are not."));
  assert.equal(screen.queryAllByText("Credential fields left blank will retain their current values.").length, 0, "a new account has no saved values to keep");
});

test("opened from a kind card, switching away from that kind sticks", async () => {
  const user = mount("runpod");
  await waitFor(() => assert.equal(chosen(), "RunPod"));
  assert.ok(input("RunPod API Key"));

  await choice.pick(user, "AutoDL");

  await waitFor(() => assert.equal(chosen(), "AutoDL"));
  assert.ok(input("SSH Password"));
  assert.equal(screen.queryAllByText("RunPod API Key").length, 0);
});

test("the chosen kind and typed credentials are what the dialog submits", async () => {
  const sent: unknown[] = [];
  Object.assign(api, { addProvider: async (body: unknown) => { sent.push(body); return { id: 1, kind: "runpod", name: "rp" }; } });
  const user = mount("autodl");
  await waitFor(() => assert.equal(chosen(), "AutoDL"));
  await choice.pick(user, "RunPod");
  await waitFor(() => assert.equal(chosen(), "RunPod"));

  fireEvent.change(screen.getByPlaceholderText("For example, My RunPod"), { target: { value: "rp" } });
  fireEvent.change(input("RunPod API Key"), { target: { value: "secret-key" } });
  fireEvent.click(add());

  await waitFor(() => assert.equal(sent.length, 1));
  // The kind, the name, and the kind's fields that hold something: nothing else travels.
  assert.deepEqual(sent[0], { kind: "runpod", name: "rp", api_key: "secret-key", endpoint: "https://api.runpod.io/v2" });
});

test("a field left empty is not sent, and extra travels only when one of its fields holds something", async () => {
  const sent: unknown[] = [];
  Object.assign(api, { addProvider: async (body: unknown) => { sent.push(body); return { id: 1, kind: "autodl", name: "a" }; } });
  const custom: KindInfo[] = [{ ...kindInfos[0], credential_fields: [
    { name: "api_key", label: { en: "API Key" }, secret: true, required: true },
    { name: "endpoint", label: { en: "API Endpoint" }, secret: false, required: false, default_value: "https://api.example.com" },
    { name: "region", label: { en: "Region" }, secret: false, required: false, extra: true },
    { name: "role_arn", label: { en: "Role ARN" }, secret: false, required: false, extra: true },
  ] }];
  const fill = async () => {
    render(<I18nextProvider i18n={i18n}><ProviderConfigModal open defaultKind={null} kindInfos={custom} categories={kindCategories(custom)} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => {}} /></I18nextProvider>);
    await waitFor(() => assert.equal(input("API Endpoint").value, "https://api.example.com"));
    fireEvent.change(input("Name"), { target: { value: "a" } });
    fireEvent.change(input("API Key"), { target: { value: "key" } });
    // The default address is cleared: the server applies its own.
    fireEvent.change(input("API Endpoint"), { target: { value: "" } });
  };
  await fill();
  fireEvent.click(add());
  await waitFor(() => assert.equal(sent.length, 1));
  assert.deepEqual(sent[0], { kind: "autodl", name: "a", api_key: "key" }, "no empty endpoint, and no extra at all");

  cleanup();
  await fill();
  fireEvent.change(input("Role ARN"), { target: { value: "acs:role/readonly" } });
  fireEvent.click(add());
  await waitFor(() => assert.equal(sent.length, 2));
  assert.deepEqual(sent[1], { kind: "autodl", name: "a", api_key: "key", extra: { role_arn: "acs:role/readonly" } }, "the empty Region is left out of extra");
});

test("required fields refuse submission inline and a corrected form can be submitted", async () => {
  const sent: unknown[] = [];
  api.addProvider = async (body) => { sent.push(body); return { id: 1, kind: "runpod", name: body.name }; };
  mount("runpod");
  await waitFor(() => assert.equal(chosen(), "RunPod"));
  // A required field says so in its label and to assistive technology; an optional one does neither.
  assert.equal(screen.getByText("Name", { selector: "label" }).textContent, "Name *");
  assert.equal(screen.getByText("RunPod API Key", { selector: "label" }).textContent, "RunPod API Key *");
  assert.equal(screen.getByText("API Endpoint", { selector: "label" }).textContent, "API Endpoint");
  assert.deepEqual([input("Name"), input("RunPod API Key"), input("API Endpoint")].map((each) => each.getAttribute("aria-required")), ["true", "true", "false"]);
  // Each label sits above its control.
  for (const name of ["Name", "RunPod API Key", "API Endpoint"]) {
    assert.ok(screen.getByText(name, { selector: "label" }).compareDocumentPosition(input(name)) & Node.DOCUMENT_POSITION_FOLLOWING, `${name}: label first`);
  }

  fireEvent.click(add());
  await waitFor(() => {
    assert.ok(screen.getByText("Enter a name"));
    assert.ok(screen.getByText("Enter RunPod API Key"));
  });
  // Each refusal is announced under its own field and belongs to it.
  assert.deepEqual(screen.getAllByRole("alert").map((alert) => alert.textContent), ["Enter a name", "Enter RunPod API Key"]);
  assert.equal(describedBy(input("Name")), "Enter a name");
  assert.equal(describedBy(input("RunPod API Key")), "Enter RunPod API Key");
  assert.deepEqual([input("Name"), input("RunPod API Key"), input("API Endpoint")].map((each) => each.getAttribute("aria-invalid")), ["true", "true", "false"]);
  assert.equal(input("Name").compareDocumentPosition(screen.getByText("Enter a name")) & Node.DOCUMENT_POSITION_FOLLOWING, Node.DOCUMENT_POSITION_FOLLOWING, "the reason is below the field");
  assert.ok(document.activeElement === input("Name"), "the first refused field takes the focus");
  assert.equal(sent.length, 0);
  assert.equal(add().disabled, false);

  fireEvent.change(input("Name"), { target: { value: "corrected" } });
  fireEvent.click(add());
  await waitFor(() => assert.deepEqual(screen.getAllByRole("alert").map((alert) => alert.textContent), ["Enter RunPod API Key"]));
  await waitFor(() => assert.ok(document.activeElement === input("RunPod API Key"), "now the key is the first refused field"));
  assert.equal(sent.length, 0);
  fireEvent.change(input("RunPod API Key"), { target: { value: "valid-key" } });
  fireEvent.click(add());
  await waitFor(() => assert.equal(sent.length, 1));
  assert.deepEqual(sent[0], { kind: "runpod", name: "corrected", api_key: "valid-key", endpoint: "https://api.runpod.io/v2" });
  assert.equal(screen.queryAllByRole("alert").length, 0);
});

test("an API refusal stays visible and preserves the form for a successful retry", async () => {
  let attempts = 0;
  let saved = 0;
  api.addProvider = async (body) => {
    attempts++;
    if (attempts === 1) throw new Error("provider credential rejected");
    return { id: 1, kind: "runpod", name: body.name };
  };
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open defaultKind="runpod" kindInfos={kindInfos} categories={kindCategories(kindInfos)} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => { saved++; }} /></I18nextProvider>);
  await waitFor(() => assert.equal(chosen(), "RunPod"));
  fireEvent.change(input("Name"), { target: { value: "retry" } });
  fireEvent.change(input("RunPod API Key"), { target: { value: "key" } });
  fireEvent.click(add());
  // The refusal is a notice at the foot of the form, inside the dialog: it stays until the next attempt, where a toast would be gone.
  const refusal = await within(screen.getByRole("dialog")).findByRole("alert");
  assert.equal(refusal.textContent, "Add failed: provider credential rejected");
  assert.ok(input("RunPod API Key").compareDocumentPosition(refusal) & Node.DOCUMENT_POSITION_FOLLOWING, "below the fields");
  assert.equal(saved, 0);
  assert.equal(input("Name").value, "retry");
  assert.equal(input("RunPod API Key").value, "key");
  assert.deepEqual([input("Name").disabled, input("RunPod API Key").disabled, add().disabled], [false, false, false], "the form can be corrected and sent again");
  fireEvent.click(add());
  await waitFor(() => assert.equal(saved, 1));
  assert.equal(attempts, 2);
  await waitFor(() => assert.equal(screen.queryAllByRole("alert").length, 0, "the refusal of the first attempt is gone"));
});

test("a new account is named to the page once added, by its id and kind, and only once it is", async () => {
  const saved: unknown[][] = [];
  api.addProvider = async (body) => ({ id: 42, kind: "runpod", name: body.name });
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open defaultKind="runpod" kindInfos={kindInfos} categories={kindCategories(kindInfos)} providerLabel="Public Cloud" onClose={() => {}} onSaved={(...args: unknown[]) => { saved.push(args); }} /></I18nextProvider>);
  await waitFor(() => assert.equal(chosen(), "RunPod"));
  fireEvent.change(input("Name"), { target: { value: "rp" } });
  fireEvent.change(input("RunPod API Key"), { target: { value: "key" } });
  fireEvent.click(add());
  await waitFor(() => assert.equal(saved.length, 1));
  assert.deepEqual(saved[0], [{ id: 42, kind: "runpod" }], "the id and kind the server gave the account, nothing else");
});

test("a refusal belongs to the kind it was said of: choosing another platform takes it away", async () => {
  api.addProvider = async () => { throw new Error("provider credential rejected"); };
  const user = mount("runpod");
  await waitFor(() => assert.equal(chosen(), "RunPod"));
  fireEvent.change(input("Name"), { target: { value: "retry" } });
  fireEvent.change(input("RunPod API Key"), { target: { value: "key" } });
  fireEvent.click(add());
  assert.ok(await screen.findByText("Add failed: provider credential rejected"));
  await choice.pick(user, "AutoDL");
  await waitFor(() => assert.equal(screen.queryAllByRole("alert").length, 0));
  assert.equal(input("Name").value, "retry", "the name typed so far stays");
  assert.equal(input("SSH Password").value, "", "the other kind's key does not");
});

// The page opens one dialog for Add (optionally preselecting a kind) and Edit,
// and closes it by resetting all three props together, as CloudProvidersPage
// does. These cases drive the dialog through that lifecycle.
type Controls = { openAdd: (kind?: string) => void; openEdit: (provider: { id: number; kind: string; name: string }) => void; close: () => void };
let controls: Controls;
let closes = 0;
function Page({ kinds }: { kinds: KindInfo[] }) {
  const [open, setOpen] = useState(false);
  const [defaultKind, setDefaultKind] = useState<string | null>(null);
  const [provider, setProvider] = useState<{ id: number; kind: string; name: string } | null>(null);
  controls = {
    openAdd: (kind) => { setProvider(null); setDefaultKind(kind ?? null); setOpen(true); },
    openEdit: (next) => { setDefaultKind(null); setProvider(next); setOpen(true); },
    close: () => { closes += 1; setOpen(false); setDefaultKind(null); setProvider(null); },
  };
  return <ProviderConfigModal open={open} provider={provider as never} defaultKind={defaultKind} kindInfos={kinds} categories={kindCategories(kinds)} providerLabel="Public Cloud" onClose={controls.close} onSaved={controls.close} />;
}
const page = (kinds: KindInfo[]) => <I18nextProvider i18n={i18n}><Page kinds={kinds} /></I18nextProvider>;
const user = () => userEvent.setup({ document: dom.window.document });
const noDialog = () => waitFor(() => assert.equal(screen.queryAllByRole("dialog").length, 0, "the dialog closed"));

test("a pick does not outlive the dialog: reopening applies the preselected kind again", async () => {
  render(page(kindInfos));
  act(() => controls.openAdd("runpod"));
  await waitFor(() => assert.equal(chosen(), "RunPod"));
  await choice.pick(user(), "AutoDL");
  await waitFor(() => assert.equal(chosen(), "AutoDL"));
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await noDialog();
  act(() => controls.openAdd("runpod"));
  await waitFor(() => assert.equal(chosen(), "RunPod"));
});

test("adding after editing starts from the first kind with an empty name", async () => {
  api.provider = async () => ({ id: 1, kind: "runpod", name: "my-rp", endpoint: "", public_key: "", extra: {} });
  render(page(kindInfos));
  act(() => controls.openEdit({ id: 1, kind: "runpod", name: "my-rp" }));
  await waitFor(() => assert.ok(screen.getByDisplayValue("my-rp")));
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await noDialog();
  act(() => controls.openAdd());
  await waitFor(() => assert.equal(chosen(), "AutoDL"));
  assert.equal(screen.queryAllByDisplayValue("my-rp").length, 0);
});

test("kinds that arrive after the dialog opened seed it once, and a refreshed list keeps the pick", async () => {
  const { rerender } = render(page([]));
  act(() => controls.openAdd("runpod"));
  // Nothing to choose from yet: the dialog is open on its name alone, with no platform choice to make.
  await screen.findByRole("dialog");
  assert.equal(screen.queryAllByRole("combobox").length + screen.queryAllByRole("tab").length, 0);
  rerender(page(kindInfos));
  await waitFor(() => assert.equal(chosen(), "RunPod"));
  await choice.pick(user(), "AutoDL");
  rerender(page([...kindInfos]));
  await waitFor(() => assert.equal(chosen(), "AutoDL"));
});

const endpointInput = () => input("API Endpoint");

test("new forms use schema defaults, switch defaults, and keep secrets blank", async () => {
  render(page(kindInfos));
  act(() => controls.openAdd("runpod"));
  await waitFor(() => assert.equal(endpointInput().value, "https://api.runpod.io/v2"));
  assert.deepEqual([input("RunPod API Key").type, input("RunPod API Key").value], ["password", ""]);
  fireEvent.change(endpointInput(), { target: { value: "https://proxy.example.test/v2" } });
  fireEvent.change(input("RunPod API Key"), { target: { value: "do-not-reuse" } });
  await choice.pick(user(), "AutoDL");
  await waitFor(() => assert.equal(endpointInput().value, "https://api.autodl.com"));
  assert.deepEqual([input("SSH Password").type, input("SSH Password").value], ["password", ""]);
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await noDialog();
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
  fireEvent.change(input("RunPod API Key"), { target: { value: "key" } });
  fireEvent.change(endpointInput(), { target: { value: "https://proxy.example.test:8443/custom/v2" } });
  fireEvent.click(add());
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
  await waitFor(() => assert.equal(input("Region").value, "region-a"));
  assert.equal(endpointInput().value, "");
  assert.equal(endpointInput().placeholder, "https://api.example.com");
  assert.equal(input("API Key").value, "");
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
  await waitFor(() => assert.equal(input("Name").value, "loaded-account"));
  const kind = screen.getByRole("textbox", { name: "External platform" }) as HTMLInputElement;
  assert.ok(screen.getByLabelText("External platform", { exact: true }) === kind, "the label names that textbox");
  assert.equal(kind.disabled, true);
  assert.equal(kind.value, "RunPod");
  assert.equal(screen.queryAllByRole("combobox").length, 0, "an account's platform is not chosen again");
});

// A request that has not been answered yet: the test answers or refuses it when it is ready to.
function pending<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

// A private cloud beside the two public ones: a second platform type to switch to.
const privateCloud: KindInfo = { ...kindInfos[0], kind: "zstack", label: { en: "ZStack Cloud" }, category: "private_cloud", resources: [{ type: "host" }] };

test("while the account is being added the dialog cannot be closed and its fields, platform types and vendors' list are locked; a refusal gives them back", async () => {
  const request = pending<{ id: number; kind: string; name: string }>();
  const sent: Array<{ kind: string }> = [];
  api.addProvider = ((body: { kind: string }) => { sent.push(body); return request.promise; }) as never;
  closes = 0;
  render(page([...kindInfos, privateCloud]));
  act(() => controls.openAdd("runpod"));
  await waitFor(() => assert.equal(chosen(), "RunPod"));
  const u = user();
  await u.type(input("Name"), "slow");
  await u.type(input("RunPod API Key"), "key");
  await u.click(add());

  const dialog = within(screen.getByRole("dialog"));
  const cancel = () => dialog.getByRole("button", { name: "Cancel" }) as HTMLButtonElement;
  const closeMark = () => dialog.getByRole("button", { name: "Close" }) as HTMLButtonElement;
  await waitFor(() => assert.equal(cancel().disabled, true, "the request is on its way"));
  assert.deepEqual([closeMark().disabled, add().disabled], [true, true], "neither the close mark nor a second Add");
  assert.deepEqual([input("Name").disabled, input("RunPod API Key").disabled, input("API Endpoint").disabled], [true, true, true]);
  // Nor another platform: the list is out of use, and so is every platform type's tab, to the pointer and to the keyboard.
  assert.equal(screen.getByRole("combobox", { name: "External platform" }).hasAttribute("disabled"), true);
  const tabs = () => screen.getAllByRole("tab").map((tab) => [tab.textContent, tab.getAttribute("aria-selected"), tab.getAttribute("aria-disabled")]);
  assert.deepEqual(tabs(), [["Public cloud", "true", "true"], ["Private cloud", "false", "true"]]);
  await u.click(screen.getByRole("tab", { name: "Private cloud" }));
  screen.getByRole("tab", { name: "Public cloud" }).focus();
  await u.keyboard("{ArrowRight}");
  assert.deepEqual(tabs(), [["Public cloud", "true", "true"], ["Private cloud", "false", "true"]]);
  assert.equal(chosen(), "RunPod", "what is being added is what the dialog still shows");
  await u.keyboard("{Escape}");
  await u.click(closeMark());
  await u.click(cancel());
  assert.equal(closes, 0, "Escape, the close mark and Cancel are all refused");
  assert.equal(screen.queryAllByRole("dialog").length, 1);
  assert.deepEqual(sent.map((body) => body.kind), ["runpod"]);

  await act(async () => request.reject(new Error("provider credential rejected")));
  assert.ok(await dialog.findByText("Add failed: provider credential rejected"));
  assert.deepEqual([cancel().disabled, closeMark().disabled, add().disabled, input("Name").disabled], [false, false, false, false]);
  assert.deepEqual(tabs().map(([, , disabled]) => disabled), ["false", "false"]);
  assert.equal(screen.getByRole("combobox", { name: "External platform" }).hasAttribute("disabled"), false);
  await u.keyboard("{Escape}");
  await noDialog();
  assert.equal(closes, 1);
});

test("a click outside the dialog does not close it, so what was typed is not lost to a stray click; Escape, the close mark and Cancel do", async () => {
  closes = 0;
  render(page(kindInfos));
  const u = user();
  for (const close of [
    () => u.keyboard("{Escape}"),
    () => u.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Close" })),
    () => u.click(screen.getByRole("button", { name: "Cancel" })),
  ]) {
    act(() => controls.openAdd("runpod"));
    await waitFor(() => assert.equal(chosen(), "RunPod"));
    await u.type(input("Name"), "typed");
    const before = closes;
    await u.click(document.querySelector('[data-slot="dialog-backdrop"]') as HTMLElement);
    assert.equal(closes, before, "the backdrop is not a way out");
    assert.equal(input("Name").value, "typed");
    await close();
    await noDialog();
    assert.equal(closes, before + 1);
  }
});
