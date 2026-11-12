import assert from "node:assert/strict";
import { after, afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";

const closeChannels = trackMessageChannels();
const { i18n, locale } = await createI18n(["apikeys"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { MemoryRouter } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { analyticsApi } = await import("../src/analyticsApi.ts");
const { default: APIKeysPage } = await import("../src/pages/APIKeysPage.tsx");
const k = locale.apikeys;
const original = { apiKeys: api.apiKeys, platformTokens: api.platformTokens, createApiKey: api.createApiKey, revokeApiKey: api.revokeApiKey, setApiKeyScope: api.setApiKeyScope, deployments: api.deployments };
const all = { all_deployments: true, deployments: [] };
// The server's scope rule (PUT /apikeys/{prefix}/scope, POST /apikeys):
// "all" with IDs, or "some" without, is a 400 API_KEY_SCOPE_INVALID.
function checkScope(scope?: import("../src/api.ts").ApiKeyScope) {
  if (scope && scope.all_deployments === scope.deployment_ids.length > 0) throw new Error("API_KEY_SCOPE_INVALID");
}
const originalKeyUsage = analyticsApi.keyUsage;
const deployments = [
  { id: 7, name: "qwen", served_name: "qwen-chat" },
  { id: 9, name: "llama", served_name: "llama" },
] as unknown as import("../src/api.ts").Deployment[];
function setup(items: import("../src/api.ts").ApiKey[] = []) {
  api.apiKeys = async () => ({ items, total: items.length });
  api.platformTokens = async () => ({ items: [], total: 0 });
  analyticsApi.keyUsage = async () => ({ items: [], time_zone: "UTC" });
  api.deployments = async () => ({ items: deployments, total: deployments.length });
  const created: string[] = [];
  api.createApiKey = async (name) => {
    created.push(name);
    return { key: "gw-ONE_TIME_SECRET", name };
  };
  render(<MemoryRouter><I18nextProvider i18n={i18n}><APIKeysPage /></I18nextProvider></MemoryRouter>);
  return { created, user: userEvent.setup() };
}
afterEach(() => { cleanup(); Object.assign(api, original); analyticsApi.keyUsage = originalKeyUsage; });
after(() => closeChannels());

test("inference usage example declares JSON while preserving its authentication and request body", async () => {
  setup();
  const command = screen.getByText((_, element) => element?.tagName === "PRE" && !!element.textContent?.includes("/v1/chat/completions")).textContent ?? "";
  assert.ok(command.startsWith(`curl ${window.location.origin}/v1/chat/completions`));
  assert.ok(command.includes('-H "Content-Type: application/json"'));
  assert.ok(command.includes('-H "Authorization: Bearer <key>"'));
  const payload = command.match(/-d '([^']+)'/)?.[1];
  assert.ok(payload, "the example includes a JSON request body");
  assert.deepEqual(JSON.parse(payload), { model: "<served_name>", messages: [{ role: "user", content: "hi" }] });
});

test("empty and whitespace inference token names do not mutate or reject outside the field", async () => {
  const { created, user } = setup();
  await user.click(screen.getByRole("button", { name: k.create.button }));
  const dialog = await screen.findByRole("dialog");
  for (const name of ["", "   "]) {
    const input = within(dialog).getByRole("textbox");
    await user.clear(input);
    if (name) await user.type(input, name);
    await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
    assert.ok(await within(dialog).findByText(k.create.nameRequired));
    assert.equal(created.length, 0);
  }
});

test("keyboard creation remains single-flight and preserves the API name value", async () => {
  const { user } = setup();
  let calls = 0;
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  api.createApiKey = async (name) => {
    calls++;
    assert.equal(name, "my-app");
    await pending;
    return { key: "gw-ONE_TIME_SECRET", name };
  };
  await user.click(screen.getByRole("button", { name: k.create.button }));
  const dialog = await screen.findByRole("dialog");
  const input = within(dialog).getByRole("textbox");
  await user.type(input, "my-app");
  fireEvent.keyDown(input, { key: "Enter", code: "Enter", keyCode: 13 });
  fireEvent.keyDown(input, { key: "Enter", code: "Enter", keyCode: 13 });
  await waitFor(() => assert.equal(calls, 1));
  release();
  assert.ok(await screen.findByText("gw-ONE_TIME_SECRET"));
});

test("server refusal preserves the inference token draft and allows a corrected retry", async () => {
  const { user } = setup();
  api.createApiKey = async () => { throw new Error("database is read-only"); };
  await user.click(screen.getByRole("button", { name: k.create.button }));
  const dialog = await screen.findByRole("dialog");
  const input = within(dialog).getByRole("textbox");
  await user.type(input, "retained");
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  assert.ok(await screen.findByText(/database is read-only/));
  assert.equal((input as HTMLInputElement).value, "retained");
  api.createApiKey = async (name) => ({ key: "gw-RETRIED_SECRET", name });
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  assert.ok(await screen.findByText("gw-RETRIED_SECRET"));
});

test("revoke confirmation names the token and warns that the last token closes the gateway", async () => {
  const { user } = setup([{ name: "production-app", prefix: "gw-production", created_at: "2026-10-13T00:00:00Z", ...all }]);
  const row = (await screen.findByText("production-app")).closest("tr")!;
  await user.click(within(row).getByRole("button", { name: k.actions.revoke }));
  const text = i18n.t("apikeys:revoke.confirm", { name: "production-app" });
  assert.match(text, /production-app/);
  assert.match(text, /lose access/);
  // Revoking the last token closes /v1; it does not open it to anonymous callers.
  assert.match(text, /no active tokens remain.*refuses every \/v1 request/);
  assert.doesNotMatch(text, /anonymous access/);
  assert.ok(await screen.findByText(text));
});

test("revoking calls the API once and a revoked token is hidden until asked for", async () => {
  const revoked: string[] = [];
  const items: import("../src/api.ts").ApiKey[] = [
    { name: "live-app", prefix: "gw-live00000", created_at: "2026-10-13T00:00:00Z", ...all },
    { name: "old-app", prefix: "gw-old000000", created_at: "2026-10-01T00:00:00Z", revoked_at: "2026-10-20T08:00:00Z", ...all },
  ];
  const { user } = setup(items);
  api.revokeApiKey = async (prefix) => { revoked.push(prefix); };
  const row = (await screen.findByText("live-app")).closest("tr")!;
  assert.ok(within(row).getByText(k.status.active));
  assert.equal(screen.queryByText("old-app"), null);
  const toggle = screen.getByRole("switch", { name: i18n.t("apikeys:showRevoked", { count: 1 }) });
  await user.click(toggle);
  const old = (await screen.findByText("old-app")).closest("tr")!;
  assert.ok(within(old).getByText(k.status.revoked));
  assert.equal(within(old).queryByRole("button", { name: k.actions.revoke }), null);
  await user.click(within(row).getByRole("button", { name: k.actions.revoke }));
  // The confirmation's own button is the last one with that name.
  await waitFor(() => assert.ok(screen.getAllByRole("button", { name: k.actions.revoke }).length > 1));
  await user.click(screen.getAllByRole("button", { name: k.actions.revoke }).at(-1)!);
  await waitFor(() => assert.deepEqual(revoked, ["gw-live00000"]));
});

test("without revoked tokens there is no revoked toggle", async () => {
  setup([{ name: "live-app", prefix: "gw-live00000", created_at: "2026-10-13T00:00:00Z", ...all }]);
  await screen.findByText("live-app");
  assert.equal(screen.queryByRole("switch"), null);
});

test("the scope column shows all services, the chosen ones, or none left", async () => {
  setup([
    { name: "open-app", prefix: "gw-open00000", created_at: "2026-10-13T00:00:00Z", ...all },
    { name: "scoped-app", prefix: "gw-scope0000", created_at: "2026-10-13T00:00:00Z", all_deployments: false, deployments: [
      { id: 7, name: "qwen", served_name: "qwen-chat" }, { id: 9, name: "llama", served_name: "llama" }, { id: 11, name: "glm", served_name: "glm" },
    ] },
    { name: "orphan-app", prefix: "gw-orphan000", created_at: "2026-10-13T00:00:00Z", all_deployments: false, deployments: [] },
  ]);
  const row = async (name: string) => (await screen.findByText(name)).closest("tr")!;
  assert.ok(within(await row("open-app")).getByText(k.scope.all));
  const scoped = await row("scoped-app");
  assert.ok(within(scoped).getByText("qwen-chat"));
  assert.ok(within(scoped).getByText("llama"));
  assert.equal(within(scoped).queryByText("glm"), null);
  assert.ok(within(scoped).getByText(i18n.t("apikeys:scope.more", { count: 1 })));
  assert.ok(within(await row("orphan-app")).getByText(k.scope.noneLeft));
});

test("creating defaults to all services and a limited token needs at least one", async () => {
  const { user } = setup();
  const sent: unknown[] = [];
  api.createApiKey = async (name, scope) => { checkScope(scope); sent.push({ name, scope }); return { key: "gw-SCOPED_SECRET", name }; };
  await user.click(screen.getByRole("button", { name: k.create.button }));
  let dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByRole("textbox"), "everything");
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  await screen.findByText("gw-SCOPED_SECRET");
  assert.deepEqual(sent, [{ name: "everything", scope: { all_deployments: true, deployment_ids: [] } }]);
  cleanup();

  setup();
  sent.length = 0;
  api.createApiKey = async (name, scope) => { checkScope(scope); sent.push({ name, scope }); return { key: "gw-SCOPED_SECRET", name }; };
  await user.click(screen.getByRole("button", { name: k.create.button }));
  dialog = await screen.findByRole("dialog");
  await user.type(within(dialog).getByRole("textbox"), "limited");
  await user.click(within(dialog).getByRole("radio", { name: k.scope.some }));
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  assert.ok(await within(dialog).findByText(k.scope.required));
  assert.equal(sent.length, 0);
  // Pick a service with the keyboard: open the list, then Enter.
  const combo = within(dialog).getByRole("combobox", { name: k.scope.pick });
  fireEvent.mouseDown(combo);
  await screen.findByText("qwen (qwen-chat)", { selector: ".ant-select-item-option-content" });
  fireEvent.keyDown(combo, { key: "Enter", keyCode: 13 });
  await user.click(within(dialog).getByRole("button", { name: k.actions.create }));
  await waitFor(() => assert.deepEqual(sent, [{ name: "limited", scope: { all_deployments: false, deployment_ids: [7] } }]));
});

test("editing a token's scope starts from its current one and saves the new one", async () => {
  const { user } = setup([{ name: "scoped-app", prefix: "gw-scope0000", created_at: "2026-10-13T00:00:00Z", all_deployments: false, deployments: [{ id: 7, name: "qwen", served_name: "qwen-chat" }] }]);
  const calls: unknown[] = [];
  let fail = true;
  api.setApiKeyScope = async (prefix, scope) => {
    checkScope(scope);
    calls.push({ prefix, scope });
    if (fail) throw new Error("deployment vanished");
    return { name: "scoped-app", prefix, created_at: "2026-10-13T00:00:00Z", ...all };
  };
  const row = (await screen.findByText("scoped-app")).closest("tr")!;
  await user.click(within(row).getByRole("button", { name: i18n.t("apikeys:scope.editAria", { name: "scoped-app" }) }));
  const dialog = await screen.findByRole("dialog");
  assert.ok((within(dialog).getByRole("radio", { name: k.scope.some }) as HTMLInputElement).checked);
  assert.ok(await within(dialog).findByText("qwen (qwen-chat)"));
  await user.click(within(dialog).getByRole("radio", { name: k.scope.all }));
  await user.click(within(dialog).getByRole("button", { name: k.actions.save }));
  assert.ok(await screen.findByText(/deployment vanished/));
  assert.ok(screen.getByRole("dialog"), "a refused save keeps the dialog open");
  fail = false;
  await user.click(within(dialog).getByRole("button", { name: k.actions.save }));
  // antd keeps a closing dialog mounted through its exit animation; the
  // success notice is what says the save went through.
  assert.ok(await screen.findByText(k.scope.saved));
  assert.deepEqual(calls, [
    { prefix: "gw-scope0000", scope: { all_deployments: true, deployment_ids: [] } },
    { prefix: "gw-scope0000", scope: { all_deployments: true, deployment_ids: [] } },
  ]);
});

test("a failed service list keeps all-services usable and says why the list is empty", async () => {
  const { user } = setup();
  api.deployments = async () => { throw new Error("list unavailable"); };
  await user.click(screen.getByRole("button", { name: k.create.button }));
  const dialog = await screen.findByRole("dialog");
  await user.click(within(dialog).getByRole("radio", { name: k.scope.some }));
  assert.ok(await within(dialog).findByText(/list unavailable/));
});
