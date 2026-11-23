import { dom, createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { installPullFake } from "./helpers/pullFake.ts";

const { i18n, locale } = await createI18n(["settings", "nodes"]);
const { cleanup, render, screen, waitFor, within, fireEvent } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const Credentials = (await import("../src/pages/settings/pull/Credentials.tsx")).default;

const text = locale.settings.pull.credentials as Record<string, string>;
const action = locale.settings.pull.action as Record<string, string>;
const common = locale.common.apiError as Record<string, string>;
const saveName = new RegExp(`^(loading)?\\s*${action.save}$`);
const fill = (template: string, values: Record<string, string | number>) => i18n.t(template, values);

const fakes: { restore(): void }[] = [];
afterEach(() => {
  cleanup();
  while (fakes.length) fakes.pop()!.restore();
});

const CREDENTIALS = [
  { id: 1, host: "harbor.corp.example", username: "ci-pull", has_secret: true, secret: "s3cret-token" },
  { id: 2, host: "bad.registry.example", username: "ci", has_secret: true, secret: "wrong" },
  { id: 3, host: "down.registry.example", username: "ci", has_secret: true, secret: "whatever" },
  { id: 4, host: "nopass.example", username: "ci", has_secret: false },
  { id: 5, host: "huggingface.co", username: "", has_secret: true, secret: "hf_good" },
  { id: 6, host: "bad.hub.example", username: "", has_secret: true, secret: "hf_bad" },
];

function setup(credentials: Parameters<typeof installPullFake>[1] = { credentials: CREDENTIALS }) {
  const fake = installPullFake(api as unknown as Record<string, unknown>, credentials);
  fakes.push(fake);
  render(<I18nextProvider i18n={i18n}><Credentials /></I18nextProvider>);
  return { fake, user: userEvent.setup({ document: dom.window.document }) };
}
const rows = () => within(screen.getByRole("region", { name: text.tableLabel })).getAllByRole("row").slice(1);
const rowText = (row: HTMLElement) => row.textContent ?? "";

test("the list never shows a secret, only whether one is stored", async () => {
  setup();
  await waitFor(() => assert.equal(rows().length, 6));
  const page = document.body.textContent ?? "";
  assert.ok(!page.includes("s3cret-token") && !page.includes("wrong"));
  assert.ok(rowText(rows()[0]).includes("harbor.corp.example") && rowText(rows()[0]).includes("ci-pull") && rowText(rows()[0]).includes(text.secretStored));
  assert.ok(rowText(rows()[3]).includes(text.secretMissing) && rowText(rows()[3]).includes("ci"));
  assert.ok(rowText(rows()[4]).includes("huggingface.co") && rowText(rows()[4]).includes("—"), "a token has no user name, shown as a dash");
  assert.equal((within(rows()[3]).getByRole("button", { name: fill(text.testFor, { host: "nopass.example" }) }) as HTMLButtonElement).disabled, true, "nothing to test without a password");
});

test("loading, empty and failed states", async () => {
  const fake = installPullFake(api as unknown as Record<string, unknown>, { credentials: [] });
  fakes.push(fake);
  fake.failNext("credentials", new Error("forbidden"));
  render(<I18nextProvider i18n={i18n}><Credentials /></I18nextProvider>);
  assert.ok(document.querySelector(".ant-spin-spinning"));
  const user = userEvent.setup({ document: dom.window.document });
  await screen.findByText(fill("pull.loadFailed", { ns: "settings", detail: "forbidden" }));
  await user.click(screen.getByRole("button", { name: action.retry }));
  await screen.findByText(text.empty);
});

test("the password is hidden by default, can be revealed by button or keyboard, and is cleared after saving", async () => {
  const { fake, user } = setup({ credentials: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.host), "registry.example");
  await user.type(within(drawer).getByLabelText(text.username), "robot");
  const secret = within(drawer).getByLabelText(text.secret) as HTMLInputElement;
  await user.type(secret, "hunter2");
  assert.equal(secret.type, "password");
  const toggle = within(drawer).getByRole("button", { name: text.showSecret });
  toggle.focus();
  await user.keyboard("{Enter}");
  assert.equal(secret.type, "text");
  assert.equal(within(drawer).getByRole("button", { name: text.hideSecret }).getAttribute("aria-pressed"), "true");
  await user.click(within(drawer).getByRole("button", { name: text.hideSecret }));
  assert.equal(secret.type, "password");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.equal(rows().length, 1));
  assert.deepEqual(fake.calls.find((call) => call.method === "createCredential")?.args[0], { host: "registry.example", username: "robot", secret: "hunter2" });
  assert.ok(!(document.body.textContent ?? "").includes("hunter2"));
  // Reopening shows no password; saving without typing one keeps the stored one.
  await user.click(within(rows()[0]).getByRole("button", { name: action.edit }));
  const edit = await screen.findByRole("dialog");
  const field = within(edit).getByLabelText(text.secret) as HTMLInputElement;
  assert.equal(field.value, "");
  assert.equal(field.placeholder, text.secretStored);
  assert.ok(within(edit).getByText(text.secretKeepHelp));
  await user.click(within(edit).getByRole("button", { name: saveName }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.deepEqual(fake.calls.find((call) => call.method === "updateCredential")?.args[1], { host: "registry.example", username: "robot", secret: "" });
  assert.equal(fake.credentials[0].secret, "hunter2", "the server kept the stored secret");
});

test("a missing password on a new credential is refused by the server at the password field", async () => {
  const { fake, user } = setup({ credentials: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.host), "registry.example");
  await user.type(within(drawer).getByLabelText(text.username), "robot");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await within(drawer).findByText(fill("pull.invalidField", { ns: "settings", field: locale.settings.pull.fields.secret, detail: i18n.t("pullSettingReason.invalid_value", { ns: "common" }) }));
  assert.equal(fake.credentials.length, 0);
  assert.ok(screen.getByRole("dialog"), "the drawer stays open");
});

test("testing a login reports success, a rejected login and an unreachable registry differently", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 6));
  await user.click(screen.getByRole("button", { name: fill(text.testFor, { host: "harbor.corp.example" }) }));
  await waitFor(() => assert.ok(rowText(rows()[0]).includes(text.testOk)));
  assert.deepEqual(fake.calls.find((call) => call.method === "testCredential")?.args, [1]);

  await user.click(screen.getByRole("button", { name: fill(text.testFor, { host: "bad.registry.example" }) }));
  await waitFor(() => assert.ok(rowText(rows()[1]).includes(fill(common.CREDENTIAL_LOGIN_REJECTED, { host: "bad.registry.example" }))));

  await user.click(screen.getByRole("button", { name: fill(text.testFor, { host: "down.registry.example" }) }));
  await waitFor(() => assert.ok(rowText(rows()[2]).includes(fill(common.CREDENTIAL_LOGIN_FAILED, { host: "down.registry.example", detail: "dial tcp: i/o timeout" }))));
  assert.ok(rowText(rows()[0]).includes(text.testOk), "one result does not erase another");
});

test("save and test stores the login, then tries it from the server and shows the outcome in the drawer", async () => {
  const { fake, user } = setup({ credentials: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.host), "bad.example");
  await user.type(within(drawer).getByLabelText(text.username), "robot");
  await user.type(within(drawer).getByLabelText(text.secret), "nope");
  await user.click(within(drawer).getByRole("button", { name: new RegExp(text.saveAndTest) }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await within(drawer).findByText(fill(common.CREDENTIAL_LOGIN_REJECTED, { host: "bad.example" }));
  assert.equal(fake.credentials.length, 1, "the login was stored first");
  assert.ok(screen.getByRole("dialog"), "the drawer stays open to fix it");
  // Fix the registry; the second save replaces the first instead of creating a duplicate.
  const registry = within(drawer).getByLabelText(text.host);
  await user.clear(registry);
  await user.type(registry, "good.example");
  await user.click(within(drawer).getByRole("button", { name: new RegExp(text.saveAndTest) }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await within(drawer).findByText(text.testOk);
  assert.equal(fake.credentials.length, 1);
  assert.equal(fake.credentials[0].host, "good.example");
  assert.equal(fake.calls.filter((call) => call.method === "createCredential").length, 1);
});

test("deleting asks first and then removes the credential", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 6));
  await user.click(screen.getByRole("button", { name: fill(text.deleteFor, { host: "nopass.example" }) }));
  await screen.findByText(fill(text.deleteConfirm, { host: "nopass.example" }));
  await user.click(screen.getByRole("button", { name: action.cancel }));
  assert.equal(fake.calls.filter((call) => call.method === "deleteCredential").length, 0);
  await user.click(screen.getByRole("button", { name: fill(text.deleteFor, { host: "nopass.example" }) }));
  const popover = document.querySelector(".ant-popconfirm:not(.ant-popover-hidden)") as HTMLElement;
  await user.click(within(popover).getByRole("button", { name: action.delete }));
  await waitFor(() => assert.equal(rows().length, 5));
  assert.deepEqual(fake.calls.find((call) => call.method === "deleteCredential")?.args, [4]);
});

test("Escape closes the drawer without saving", async () => {
  const { fake, user } = setup({ credentials: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.host), "registry.example");
  fireEvent.keyDown(within(drawer).getByLabelText(text.host), { key: "Escape", keyCode: 27 });
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.equal(fake.calls.filter((call) => call.method === "createCredential").length, 0);
});

test("the type column tells a registry login from a model site token", async () => {
  setup();
  await waitFor(() => assert.equal(rows().length, 6));
  assert.ok(rowText(rows()[0]).includes(text.typeRegistry));
  assert.ok(rowText(rows()[4]).includes(text.typeSite));
  assert.ok(!rowText(rows()[4]).includes(text.typeRegistry));
});

test("a model site token is created with only a host and a token, and an empty user name", async () => {
  const { fake, user } = setup({ credentials: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  assert.ok(within(drawer).getByLabelText(text.username), "a registry login is the default type");
  await user.click(within(drawer).getByRole("radio", { name: text.typeSite }));
  assert.equal(within(drawer).queryByLabelText(text.username), null, "a token has no user name field");
  assert.ok(within(drawer).getByText(text.typeSiteHelp));
  const host = within(drawer).getByLabelText(text.host) as HTMLInputElement;
  assert.equal(host.placeholder, text.hostPlaceholderSite);
  await user.type(host, " huggingface.co ");
  await user.type(within(drawer).getByLabelText(text.token), "hf_abc");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.equal(rows().length, 1));
  assert.deepEqual(fake.calls.find((call) => call.method === "createCredential")?.args[0], { host: "huggingface.co", username: "", secret: "hf_abc" });
  assert.ok(rowText(rows()[0]).includes(text.typeSite));
});

test("a registry login needs a user name before anything is sent", async () => {
  const { fake, user } = setup({ credentials: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  assert.equal((within(drawer).getByLabelText(text.host) as HTMLInputElement).placeholder, text.hostPlaceholderRegistry);
  await user.type(within(drawer).getByLabelText(text.host), "ghcr.io");
  await user.type(within(drawer).getByLabelText(text.secret), "pw");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await within(drawer).findByText(text.usernameRequired);
  assert.equal(fake.calls.filter((call) => call.method === "createCredential").length, 0);
});

test("a refused host is shown at the host field", async () => {
  const { fake, user } = setup({ credentials: [{ id: 1, host: "ghcr.io", username: "robot", has_secret: true }] });
  await waitFor(() => assert.equal(rows().length, 1));
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  await user.type(within(drawer).getByLabelText(text.host), "ghcr.io");
  await user.type(within(drawer).getByLabelText(text.username), "robot");
  await user.type(within(drawer).getByLabelText(text.secret), "pw");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await within(drawer).findByText(fill("pull.invalidField", { ns: "settings", field: locale.settings.pull.fields.host, detail: i18n.t("pullSettingReason.invalid_value", { ns: "common" }) }));
  assert.equal(fake.credentials.length, 1);
});

test("editing a token keeps its type and, left blank, its stored token", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 6));
  await user.click(within(rows()[4]).getByRole("button", { name: action.edit }));
  const drawer = await screen.findByRole("dialog");
  assert.equal(within(drawer).queryByRole("radio"), null, "the type cannot change");
  assert.equal(within(drawer).queryByLabelText(text.username), null);
  assert.ok(within(drawer).getByText(text.tokenKeepHelp));
  assert.equal((within(drawer).getByLabelText(text.token) as HTMLInputElement).value, "");
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.deepEqual(fake.calls.find((call) => call.method === "updateCredential")?.args, [5, { host: "huggingface.co", username: "", secret: "" }]);
  assert.equal(fake.credentials[4].secret, "hf_good");
});

test("editing a registry login keeps its user name", async () => {
  const { fake, user } = setup();
  await waitFor(() => assert.equal(rows().length, 6));
  await user.click(within(rows()[0]).getByRole("button", { name: action.edit }));
  const drawer = await screen.findByRole("dialog");
  assert.equal((within(drawer).getByLabelText(text.username) as HTMLInputElement).value, "ci-pull");
  assert.equal(within(drawer).queryByRole("radio"), null);
  await user.click(within(drawer).getByRole("button", { name: saveName }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.deepEqual(fake.calls.find((call) => call.method === "updateCredential")?.args, [1, { host: "harbor.corp.example", username: "ci-pull", secret: "" }]);
});

test("testing a site token says the token is valid or was rejected", async () => {
  const { user } = setup();
  await waitFor(() => assert.equal(rows().length, 6));
  await user.click(screen.getByRole("button", { name: fill(text.testFor, { host: "huggingface.co" }) }));
  await waitFor(() => assert.ok(rowText(rows()[4]).includes(text.testOkToken)));
  assert.ok(!rowText(rows()[4]).includes(text.testOk + " "), "not worded as a login");
  await user.click(screen.getByRole("button", { name: fill(text.testFor, { host: "bad.hub.example" }) }));
  await waitFor(() => assert.ok(rowText(rows()[5]).includes(fill(common.CREDENTIAL_LOGIN_REJECTED, { host: "bad.hub.example" }))));
});

test("save and test of a token shows the token result in the drawer", async () => {
  const { fake, user } = setup({ credentials: [] });
  await screen.findByText(text.empty);
  await user.click(screen.getByRole("button", { name: new RegExp(text.new) }));
  const drawer = await screen.findByRole("dialog");
  await user.click(within(drawer).getByRole("radio", { name: text.typeSite }));
  await user.type(within(drawer).getByLabelText(text.host), "huggingface.co");
  await user.type(within(drawer).getByLabelText(text.token), "hf_ok");
  await user.click(within(drawer).getByRole("button", { name: new RegExp(text.saveAndTest) }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await within(drawer).findByText(text.testOkToken);
  assert.equal(fake.credentials[0].username, "");
});

test("a login failure without a host still reads sensibly", () => {
  const text = fill(common.CREDENTIAL_LOGIN_FAILED, { detail: "dial tcp: i/o timeout" });
  assert.ok(text.includes("dial tcp: i/o timeout") && !text.includes("{{"));
});
