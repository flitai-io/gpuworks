import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n } = await createI18n(["settings"]);
const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { api } = await import("../src/api.ts");
const { default: Card } = await import("../src/pages/settings/OutboundPolicyCard.tsx");
const originalMe = api.me, originalFetch = globalThis.fetch;
const policy = { restrict_private_hosts: false, allow_private_hosts: ["models.internal"], editable: true, revision: "r1" };
type Call = { method: string; body?: unknown };
function fixture(handler: (call: Call) => Response | Promise<Response>, role = "admin") {
  const calls: Call[] = [];
  api.me = (async () => ({ id: 1, role, username: role })) as typeof api.me;
  globalThis.fetch = (async (url, init) => {
    assert.equal(String(url), "/api/v1/server/outbound-policy");
    assert.equal(init?.credentials, "include");
    const call = { method: init?.method ?? "GET", body: init?.body ? JSON.parse(String(init.body)) : undefined };
    calls.push(call);
    return handler(call);
  }) as typeof fetch;
  return calls;
}
const json = (status: number, value: unknown) => new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
const show = () => { render(<I18nextProvider i18n={i18n}><Card /></I18nextProvider>); return userEvent.setup(); };
afterEach(() => { cleanup(); api.me = originalMe; globalThis.fetch = originalFetch; });

test("an admin sees loading while the outbound policy request is pending", async () => {
  let finish!: (response: Response) => void;
  const calls = fixture(() => new Promise((resolve) => { finish = resolve; }));
  show();
  await waitFor(() => assert.equal(calls.length, 1));
  assert.ok(await screen.findByText("Loading outbound policy…"));
  finish(json(200, policy));
  await screen.findByRole("switch", { name: "Restrict access to private hosts" });
});


test("non-admins see a permission notice without reading the policy", async () => {
  const calls = fixture(() => assert.fail("non-admin policy request"), "viewer");
  show();
  await screen.findByText("Administrator permission is required to view or change the outbound policy.");
  assert.deepEqual(calls, []);
  assert.equal(screen.queryByRole("switch"), null);
});

test("an admin keeps the allowlist when switching off and submits exact wire fields by keyboard", async () => {
  const calls = fixture((call) => call.method === "GET" ? json(200, { ...policy, restrict_private_hosts: true })
    : json(200, { ...policy, ...(call.body as object), revision: "r2" }));
  const user = show();
  const toggle = await screen.findByRole("switch", { name: "Restrict access to private hosts" });
  toggle.focus();
  await user.keyboard(" ");
  assert.equal(toggle.getAttribute("aria-checked"), "false");
  const textarea = screen.getByRole("textbox", { name: "Allowed private hosts" });
  assert.equal((textarea as HTMLTextAreaElement).value, "models.internal");
  await user.type(textarea, "{Enter} 10.2.0.0/16{Enter}{Enter}127.0.0.1 ");
  const save = screen.getByRole("button", { name: "Save" });
  save.focus();
  await user.keyboard("{Enter}");
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await screen.findByText("Outbound policy saved and applied.");
  assert.deepEqual(calls.filter((c) => c.method === "PUT"), [{ method: "PUT", body: {
    restrict_private_hosts: false, allow_private_hosts: ["models.internal", "10.2.0.0/16", "127.0.0.1"], revision: "r1",
  } }]);
  assert.equal((save as HTMLButtonElement).disabled, true);
  assert.ok(screen.getByText(/always blocked, even when strict mode is off/));
});

test("a failed initial read offers retry and never saves a default over an unknown policy", async () => {
  let gets = 0;
  const calls = fixture(() => ++gets === 1 ? json(503, { code: "SERVER_OUTBOUND_POLICY_UNAVAILABLE", error: "private upstream text" }) : json(200, policy));
  const user = show();
  await screen.findByText(/outbound policy is temporarily unavailable/);
  assert.equal(screen.queryByRole("switch"), null);
  await user.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("switch");
  assert.equal(calls.filter((call) => call.method === "PUT").length, 0);
  assert.equal(screen.queryByText("private upstream text"), null);
});

test("read-only configuration displays saved values and disables mutations", async () => {
  const calls = fixture(() => json(200, { ...policy, editable: false }));
  show();
  const toggle = await screen.findByRole("switch");
  assert.equal((toggle as HTMLButtonElement).disabled, true);
  assert.equal((screen.getByRole("textbox") as HTMLTextAreaElement).disabled, true);
  assert.equal((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled, true);
  assert.ok(screen.getByText(/configuration is read-only/));
  assert.equal(calls.length, 1);
});

for (const [status, code] of [[400, "SERVER_OUTBOUND_POLICY_INVALID"], [409, "SERVER_OUTBOUND_POLICY_READ_ONLY"], [503, "SERVER_OUTBOUND_POLICY_UNAVAILABLE"]] as const) {
  test(`${code} preserves the edited list and offers an explicit reload`, async () => {
    const calls = fixture((call) => call.method === "GET" ? json(200, policy) : json(status, { code, error: "raw private config error" }));
    const user = show();
    const textarea = await screen.findByRole("textbox", { name: "Allowed private hosts" });
    await user.type(textarea, "{Enter}custom.internal");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
    await screen.findByRole("button", { name: "Reload and discard draft" });
    assert.equal((screen.getByRole("textbox") as HTMLTextAreaElement).value, "models.internal\ncustom.internal");
    assert.equal(screen.queryByText("raw private config error"), null);
    assert.equal(calls.filter((call) => call.method === "GET").length, 1);
  });
}

test("a revision conflict preserves the draft and blocks stale saves until explicit reload", async () => {
  let gets = 0;
  const calls = fixture((call) => call.method === "GET" ? json(200, ++gets === 1 ? policy : { ...policy, allow_private_hosts: ["other.internal"], revision: "r3" })
    : json(409, { code: "SERVER_OUTBOUND_POLICY_CONFLICT", error: "conflict" }));
  const user = show();
  const textarea = await screen.findByRole("textbox");
  await user.type(textarea, "{Enter}draft.internal");
  await user.click(screen.getByRole("button", { name: "Save" }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await screen.findByText(/policy changed elsewhere/);
  assert.equal((screen.getByRole("textbox") as HTMLTextAreaElement).value, "models.internal\ndraft.internal");
  assert.equal((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled, true);
  assert.equal(calls.filter((call) => call.method === "GET").length, 1);
  await user.click(screen.getByRole("button", { name: "Reload and discard draft" }));
  await waitFor(() => assert.equal((screen.getByRole("textbox") as HTMLTextAreaElement).value, "other.internal"));
  await user.type(screen.getByRole("textbox"), "{Enter}second.internal");
  await user.click(screen.getByRole("button", { name: "Save" }));
  await user.click(await screen.findByRole("button", { name: "Confirm", exact: true }));
  await waitFor(() => assert.equal(calls.filter((call) => call.method === "PUT").length, 2));
  assert.equal((calls.filter((call) => call.method === "PUT")[1].body as { revision: string }).revision, "r3");
});

test("the card explains defaults and permanent bans in Chinese", async () => {
  const chinese = await createI18n(["settings"], "zh");
  fixture(() => json(200, policy));
  render(<I18nextProvider i18n={chinese.i18n}><Card /></I18nextProvider>);
  const toggle = await screen.findByRole("switch", { name: "限制私网主机访问" });
  assert.equal(toggle.getAttribute("aria-checked"), "false");
  assert.ok(screen.getByText(/普通内网主机默认允许访问/));
  assert.ok(screen.getByText(/关闭严格模式或加入白名单也不例外/));
  assert.ok(screen.getByRole("textbox", { name: "允许访问的私网主机" }));
});
