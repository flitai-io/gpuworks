import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { field } from "./helpers/platformDialog.ts";

// Editing a cloud account (ui/src/components/ProviderConfigModal.tsx): the
// dialog reads the saved configuration each time it opens and is locked until
// it has it; what it shows of it, what a save sends, and what a failed read or
// a failed save leaves behind.

const { i18n } = await createI18n(["providers"], "en");
const { act, cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: ProviderConfigModal } = await import("../src/components/ProviderConfigModal.tsx");
const { api } = await import("../src/api.ts");
type Config = import("../src/api.ts").ProviderEditConfig;
type Provider = import("../src/api.ts").CloudProvider;
type KindInfo = import("../src/api.ts").ProviderKindInfo;
const original = { ...api };
afterEach(() => { cleanup(); Object.assign(api, original); });
const kindInfos: KindInfo[] = [{
  kind: "compshare", label: { en: "CompShare" }, category: "public_cloud", resources: [{ type: "vm" }, { type: "container" }], server_dial_supported: true,
  capabilities: { can_list_instances: true, can_power_control: true, can_install_agent: true, can_create: true, can_delete: true, can_inject_user_data: false, has_pricing: true },
  credential_fields: [
    { name: "endpoint", label: { en: "API Endpoint" }, placeholder: { en: "https://api.compshare.cn" }, secret: false, required: false },
    { name: "public_key", label: { en: "Public Key" }, secret: false, required: true },
    { name: "private_key", label: { en: "Private Key" }, secret: true, required: true },
  ],
}];
const saved: Config = { id: 1, kind: "compshare", name: "account", endpoint: "https://proxy.example.test:8443/cloud", public_key: "public-id", extra: {} };
const provider = (id = 1): Provider => ({ id, name: "list-name", kind: "compshare", category: "public_cloud", state: "valid", instance_count: 0, resources: [{ type: "vm" }, { type: "container" }], node_count: 0, created_at: "", updated_at: "" });
const view = (open = true, id = 1, kinds = kindInfos) => <I18nextProvider i18n={i18n}><ProviderConfigModal open={open} provider={provider(id)} kindInfos={kinds} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => {}} /></I18nextProvider>;
// A label's required mark ("Name *") is part of its text.
const input = (name: string) => screen.getByLabelText(field(name)) as HTMLInputElement;
const FIELDS = ["Name", "API Endpoint", "Public Key", "Private Key"];
const locked = () => FIELDS.map((name) => input(name).disabled);
const save = () => screen.getByRole("button", { name: /Save/ }) as HTMLButtonElement;

test("edit waits for saved config, displays proxy and public key, and leaves secrets blank", async () => {
  let resolve!: (value: Config) => void;
  api.provider = () => new Promise((done) => { resolve = done; });
  render(view());
  // Until the saved configuration is read nothing can be typed or saved: a save now would send the list's name over the saved one.
  assert.equal(save().disabled, true);
  assert.deepEqual(locked(), [true, true, true, true]);
  assert.equal((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled, false, "waiting can be given up");
  assert.equal(screen.queryAllByRole("alert").length, 0, "still reading is not failing");
  await act(async () => resolve(saved));
  await waitFor(() => assert.equal(save().disabled, false));
  assert.deepEqual(locked(), [false, false, false, false]);
  assert.equal(input("API Endpoint").value, saved.endpoint);
  assert.equal(input("Public Key").value, "public-id");
  assert.deepEqual([input("Private Key").type, input("Private Key").value], ["password", ""]);
  assert.equal(input("Name").value, "account");
});

test("an edit asks for a saved secret only to replace it: it is not marked required, the dialog says blanks keep their values, and the name still is", async () => {
  api.provider = async () => saved;
  render(view());
  await waitFor(() => assert.equal(save().disabled, false));
  const labels = Object.fromEntries(FIELDS.map((name) => [name, screen.getByText(name, { selector: "label" }).textContent]));
  assert.deepEqual(labels, { "Name": "Name *", "API Endpoint": "API Endpoint", "Public Key": "Public Key", "Private Key": "Private Key" });
  assert.deepEqual(FIELDS.map((name) => input(name).getAttribute("aria-required")), ["true", "false", "false", "false"]);
  assert.ok(screen.getByText("Credential fields left blank will retain their current values."));
  assert.equal(screen.getByRole("heading", { level: 3 }).textContent, "Credentials");
});

test("saving an edit names no new account to the page", async () => {
  api.provider = async () => saved;
  api.updateProvider = async (id, body) => ({ id, kind: saved.kind, name: body.name });
  const args: unknown[][] = [];
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open provider={provider()} kindInfos={kindInfos} providerLabel="Public Cloud" onClose={() => {}} onSaved={(...each: unknown[]) => { args.push(each); }} /></I18nextProvider>);
  await waitFor(() => assert.equal(save().disabled, false));
  fireEvent.click(save());
  await waitFor(() => assert.equal(args.length, 1));
  assert.deepEqual(args[0], [undefined], "an edit has nothing to open");
});

test("changing only name retains the override and never invents secret values", async () => {
  api.provider = async () => saved;
  const calls: unknown[] = [];
  api.updateProvider = async (id, body) => { calls.push({ id, body }); return { id, kind: saved.kind, name: body.name }; };
  render(view());
  await waitFor(() => assert.equal(save().disabled, false));
  fireEvent.change(input("Name"), { target: { value: "renamed" } });
  fireEvent.click(save());
  await waitFor(() => assert.equal(calls.length, 1));
  // The name and what the form shows of the saved configuration; no kind (an account keeps its platform), no secret, no empty extra.
  assert.deepEqual(calls[0], { id: 1, body: { name: "renamed", endpoint: saved.endpoint, public_key: "public-id" } });
});

test("a field cleared in an edit is not sent, so the server keeps what it had; a typed secret is", async () => {
  api.provider = async () => saved;
  const calls: unknown[] = [];
  api.updateProvider = async (id, body) => { calls.push({ id, body }); return { id, kind: saved.kind, name: body.name }; };
  render(view());
  await waitFor(() => assert.equal(save().disabled, false));
  fireEvent.change(input("API Endpoint"), { target: { value: "" } });
  fireEvent.change(input("Private Key"), { target: { value: "new-secret" } });
  fireEvent.click(save());
  await waitFor(() => assert.equal(calls.length, 1));
  assert.deepEqual(calls[0], { id: 1, body: { name: "account", public_key: "public-id", private_key: "new-secret" } });
});

test("an edit without a name is refused at the field and nothing is sent", async () => {
  api.provider = async () => saved;
  const calls: unknown[] = [];
  api.updateProvider = async (id, body) => { calls.push(body); return { id, kind: saved.kind, name: body.name }; };
  render(view());
  await waitFor(() => assert.equal(save().disabled, false));
  fireEvent.change(input("Name"), { target: { value: "" } });
  fireEvent.click(save());
  assert.equal((await screen.findByRole("alert")).textContent, "Enter a name");
  assert.ok(document.activeElement === input("Name"), "the refused field takes the focus");
  assert.equal(calls.length, 0);
});

test("a refused save is said in the dialog as an update that failed, keeps what was typed, and can be sent again", async () => {
  api.provider = async () => saved;
  let attempts = 0;
  let savedCount = 0;
  api.updateProvider = async (id, body) => {
    attempts += 1;
    if (attempts === 1) throw new Error("credential rejected");
    return { id, kind: saved.kind, name: body.name };
  };
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open provider={provider()} kindInfos={kindInfos} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => { savedCount += 1; }} /></I18nextProvider>);
  await waitFor(() => assert.equal(save().disabled, false));
  fireEvent.change(input("Name"), { target: { value: "renamed" } });
  fireEvent.change(input("Private Key"), { target: { value: "typed-secret" } });
  fireEvent.click(save());
  assert.equal((await within(screen.getByRole("dialog")).findByRole("alert")).textContent, "Update failed: credential rejected");
  assert.equal(savedCount, 0);
  assert.deepEqual([input("Name").value, input("Private Key").value, input("API Endpoint").value], ["renamed", "typed-secret", saved.endpoint]);
  assert.deepEqual([...locked(), save().disabled], [false, false, false, false, false]);
  fireEvent.click(save());
  await waitFor(() => assert.equal(savedCount, 1));
  assert.equal(attempts, 2);
});

test("while the save is on its way the dialog is locked and cannot be closed", async () => {
  api.provider = async () => saved;
  let answer!: (value: { id: number; kind: string; name: string }) => void;
  api.updateProvider = () => new Promise((done) => { answer = done; });
  const seen = { closed: 0, saved: 0 };
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open provider={provider()} kindInfos={kindInfos} providerLabel="Public Cloud" onClose={() => { seen.closed += 1; }} onSaved={() => { seen.saved += 1; }} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await waitFor(() => assert.equal(save().disabled, false));
  await user.click(save());
  const cancel = screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement;
  const closeMark = within(screen.getByRole("dialog")).getByRole("button", { name: "Close" }) as HTMLButtonElement;
  await waitFor(() => assert.equal(cancel.disabled, true));
  assert.deepEqual([closeMark.disabled, save().disabled, ...locked()], [true, true, true, true, true, true]);
  await user.keyboard("{Escape}");
  assert.deepEqual(seen, { closed: 0, saved: 0 });
  await act(async () => answer({ id: 1, kind: saved.kind, name: saved.name }));
  await waitFor(() => assert.deepEqual(seen, { closed: 0, saved: 1 }, "the owner closes the dialog once the save is in"));
});

test("reopening reads saved values again, preserving empty endpoint instead of the placeholder", async () => {
  let current = saved;
  api.provider = async () => current;
  const rendered = render(view());
  await waitFor(() => assert.equal(input("API Endpoint").value, saved.endpoint));
  rendered.rerender(view(false));
  current = { ...saved, endpoint: "" };
  rendered.rerender(view());
  await waitFor(() => assert.equal(save().disabled, false));
  assert.equal(input("API Endpoint").value, "");
  assert.equal(input("API Endpoint").placeholder, "https://api.compshare.cn");
});

test("failed read blocks saving, and closing and reopening recovers", async () => {
  api.provider = async () => { throw new Error("unavailable"); };
  const calls: unknown[] = [];
  api.updateProvider = async (id, body) => { calls.push(body); return { id, kind: saved.kind, name: body.name }; };
  const rendered = render(view());
  // Said in the dialog, with what to do about it; nothing can be typed over a configuration that was never read.
  assert.equal((await screen.findByRole("alert")).textContent, "Unable to load saved configuration. Close this dialog and try again.");
  assert.equal(save().disabled, true);
  assert.deepEqual(locked(), [true, true, true, true]);
  fireEvent.click(save());
  fireEvent.submit(screen.getByRole("dialog").querySelector("form") as HTMLFormElement);
  assert.equal(calls.length, 0);
  assert.equal((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled, false, "the dialog can be closed");
  rendered.rerender(view(false));
  api.provider = async () => saved;
  rendered.rerender(view());
  await waitFor(() => assert.equal(save().disabled, false));
  assert.equal(screen.queryAllByRole("alert").length, 0);
  assert.deepEqual(locked(), [false, false, false, false]);
  assert.equal(input("API Endpoint").value, saved.endpoint);
});

test("late response from another account cannot overwrite the active account", async () => {
  let first!: (value: Config) => void;
  api.provider = (id) => id === 1 ? new Promise((done) => { first = done; }) : Promise.resolve({ ...saved, id: 2, endpoint: "https://second.example.test" });
  const rendered = render(view());
  rendered.rerender(view(true, 2));
  await waitFor(() => assert.equal(input("API Endpoint").value, "https://second.example.test"));
  await act(async () => first(saved));
  assert.equal(input("API Endpoint").value, "https://second.example.test");
});

test("schema arriving after detail cannot clear the saved endpoint, public key or name", async () => {
  api.provider = async () => saved;
  const rendered = render(view(true, 1, []));
  await waitFor(() => assert.equal(input("Name").value, "account"));
  assert.equal(save().disabled, true);
  rendered.rerender(view());
  await waitFor(() => assert.equal(input("API Endpoint").value, saved.endpoint));
  assert.equal(input("Public Key").value, "public-id");
  assert.equal(input("Name").value, "account");
});


test("schema-declared nonsecret extras are filled without exposing secret extras", async () => {
  const aliyun: KindInfo[] = [{ ...kindInfos[0], kind: "aliyun", credential_fields: [
    { name: "role_arn", label: { en: "Role ARN" }, secret: false, required: false, extra: true },
    { name: "regions", label: { en: "Regions" }, secret: false, required: false, extra: true },
    { name: "private_key", label: { en: "Access Key Secret" }, secret: true, required: true },
  ] }];
  api.provider = async () => ({ ...saved, kind: "aliyun", endpoint: "", extra: { role_arn: "acs:role/readonly", regions: "cn-a,cn-b" } });
  render(<I18nextProvider i18n={i18n}><ProviderConfigModal open provider={{ ...provider(), kind: "aliyun" }} kindInfos={aliyun} providerLabel="Public Cloud" onClose={() => {}} onSaved={() => {}} /></I18nextProvider>);
  await waitFor(() => assert.equal(save().disabled, false));
  assert.equal(input("Role ARN").value, "acs:role/readonly");
  assert.equal(input("Regions").value, "cn-a,cn-b");
  assert.equal(input("Access Key Secret").value, "");
});
