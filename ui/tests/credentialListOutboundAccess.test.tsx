import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["settings", "common"]);
const { act, cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const Credentials = (await import("../src/pages/settings/pull/Credentials.tsx")).default;
const originalFetch = globalThis.fetch;
const text = locale.settings.pull.credentials;
const policyPath = "/api/v1/server/outbound-policy";
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
type Call = { path: string; method: string; body?: Record<string, unknown> };
const unexpected: string[] = [];
const fixtureErrors: string[] = [];

function fixture(options: {
  restrict?: boolean; allowed?: string[]; editable?: boolean; host?: string; target?: string; site?: boolean;
  status?: "blocked" | "unresolved"; conflict?: boolean; policyUnavailable?: boolean;
  firstPolicyRead?: Promise<void>;
} = {}) {
  const target = options.target ?? "registry.internal";
  const credentials = [
    { id: 9, host: options.host ?? "registry.internal:5000", username: options.site ? "" : "reader", has_secret: true },
    { id: 10, host: "hub.internal", username: "", has_secret: true },
  ];
  let policy = { restrict_private_hosts: options.restrict ?? true, allow_private_hosts: options.allowed ?? ["existing.internal"], editable: options.editable ?? true, revision: "r1" };
  let status = options.status;
  let policyReads = 0;
  const calls: Call[] = [];
  globalThis.fetch = (async (input, init) => {
    const call = { path: String(input), method: init?.method ?? "GET", body: init?.body ? JSON.parse(String(init.body)) : undefined };
    calls.push(call);
    try {
      if (call.path === "/api/v1/pull/credentials" && call.method === "GET") return json({ items: credentials, total: credentials.length });
      if (call.path === "/api/v1/auth/me" && call.method === "GET") return json({ id: 1, username: "admin", role: "admin" });
      if (call.path === policyPath && call.method === "GET") {
        if (++policyReads === 1) await options.firstPolicyRead;
        return options.policyUnavailable ? json({ code: "SERVER_OUTBOUND_POLICY_UNAVAILABLE", error: "settings unavailable" }, 503) : json(policy);
      }
      if (call.path === `${policyPath}/check` && call.method === "POST") {
        assert.deepEqual(call.body, { hosts: [target] });
        assert.equal(policy.restrict_private_hosts, true, "disabled global protection must not run host checks");
        return json({ items: [{ host: target, status: status ?? (policy.allow_private_hosts.includes(target) ? "allowed" : "needs_allow") }], total: 1 });
      }
      if (call.path === policyPath && call.method === "PUT") {
        assert.equal(call.body.revision, policy.revision);
        assert.equal(call.body.restrict_private_hosts, true);
        assert.equal(policy.editable, true, "read-only policy must not be written");
        assert.equal(status, undefined, "permanently blocked or unresolved hosts must not be granted");
        assert.deepEqual(call.body.allow_private_hosts, ["existing.internal", target]);
        if (options.conflict) return json({ code: "SERVER_OUTBOUND_POLICY_CONFLICT", error: "policy changed" }, 409);
        policy = { ...policy, allow_private_hosts: call.body.allow_private_hosts, revision: "r2" };
        return json(policy);
      }
      if (call.path === "/api/v1/pull/credentials/9/test" && call.method === "POST") {
        return !status && (!policy.restrict_private_hosts || policy.allow_private_hosts.includes(target)) ? json({ ok: true })
          : json({ code: "CREDENTIAL_LOGIN_FAILED", error: "outbound URL is not permitted", params: { host: credentials[0].host, detail: "outbound URL is not permitted" } }, 502);
      }
      unexpected.push(`${call.method} ${call.path}`);
      throw new Error(`Unexpected HTTP request: ${call.method} ${call.path}`);
    } catch (error) {
      fixtureErrors.push(String(error));
      throw error;
    }
  }) as typeof fetch;
  const view = render(<I18nextProvider i18n={i18n}><Credentials /></I18nextProvider>);
  return { calls, policy: () => policy, user: userEvent.setup(), host: credentials[0].host,
    setStatus: (next: typeof status) => { status = next; }, unmount: view.unmount };
}

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  assert.deepEqual(unexpected.splice(0), [], "every HTTP request must be accounted for");
  assert.deepEqual(fixtureErrors.splice(0), [], "fixture validation errors must not disappear into UI error handling");
});

test("a credential list login test can allow only its selected host without saving the credential", async () => {
  const f = fixture();
  await f.user.click(await screen.findByRole("button", { name: i18n.t("pull.credentials.testFor", { ns: "settings", host: "registry.internal:5000" }) }));
  const allow = await screen.findByRole("checkbox", { name: /registry\.internal/ });
  assert.equal(f.calls.filter((call) => call.path.endsWith("/test")).length, 0);
  await f.user.click(allow);
  const dialog = screen.getByRole("dialog");
  await f.user.click(within(dialog).getByRole("button", { name: text.test, exact: true }));
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.ok(await screen.findByText(text.testOk));
  assert.deepEqual(f.policy().allow_private_hosts, ["existing.internal", "registry.internal"]);
  for (const call of f.calls.filter((call) => call.path.endsWith("/check"))) {
    assert.deepEqual(call.body, { hosts: ["registry.internal"] });
  }
  assert.deepEqual(f.calls.filter((call) => call.method !== "GET" && !call.path.startsWith(policyPath)), [
    { path: "/api/v1/pull/credentials/9/test", method: "POST", body: undefined },
  ]);
  const put = f.calls.findIndex((call) => call.path === policyPath && call.method === "PUT");
  assert.ok(put >= 0 && put < f.calls.findIndex((call) => call.path.endsWith("/9/test")));
});

async function clickTest(f: ReturnType<typeof fixture>) {
  await f.user.click(await screen.findByRole("button", { name: i18n.t("pull.credentials.testFor", { ns: "settings", host: f.host }) }));
}
const testCalls = (f: ReturnType<typeof fixture>) => f.calls.filter((call) => call.path.endsWith("/test"));
const writes = (f: ReturnType<typeof fixture>) => f.calls.filter((call) => call.method === "PUT");

test("turning off global SSRF protection keeps the original list test direct with no host check or authorization dialog", async () => {
  const f = fixture({ restrict: false });
  await screen.findByRole("button", { name: i18n.t("pull.credentials.testFor", { ns: "settings", host: f.host }) });
  assert.deepEqual(f.calls.map((call) => call.path), ["/api/v1/pull/credentials"], "rendering the list must not check every row");
  await clickTest(f);
  await screen.findByText(text.testOk);
  assert.equal(screen.queryByRole("dialog"), null);
  assert.equal(f.calls.filter((call) => call.path.endsWith("/check")).length, 0);
  assert.equal(f.calls.filter((call) => call.path === "/api/v1/auth/me").length, 0);
  assert.equal(writes(f).length, 0);
  assert.equal(testCalls(f).length, 1);
});

test("an already allowed host tests directly without opening an authorization dialog or changing policy", async () => {
  const f = fixture({ allowed: ["existing.internal", "registry.internal"] });
  await clickTest(f);
  await screen.findByText(text.testOk);
  assert.equal(screen.queryByRole("dialog"), null);
  assert.equal(writes(f).length, 0);
  assert.equal(testCalls(f).length, 1);
  assert.deepEqual(f.calls.filter((call) => call.path.endsWith("/check")).map((call) => call.body), [{ hosts: ["registry.internal"] }]);
});

test("an unavailable policy read leaves the existing login test usable without a credential save or permission write", async () => {
  const f = fixture({ restrict: false, policyUnavailable: true });
  await clickTest(f);
  await screen.findByText(text.testOk);
  assert.equal(screen.queryByRole("dialog"), null);
  assert.equal(writes(f).length, 0);
  assert.equal(testCalls(f).length, 1);
});

test("an unchecked host cannot grant permission or start a login test and Escape dismisses the request", async () => {
  const f = fixture();
  await clickTest(f);
  await screen.findByRole("checkbox", { name: /registry\.internal/ });
  const dialog = screen.getByRole("dialog");
  const run = within(dialog).getByRole("button", { name: text.test, exact: true }) as HTMLButtonElement;
  assert.equal(run.disabled, true);
  await f.user.click(run);
  assert.equal(writes(f).length, 0);
  assert.equal(testCalls(f).length, 0);
  await f.user.keyboard("{Escape}");
  await waitFor(() => assert.equal(screen.queryByRole("dialog"), null));
  assert.equal(writes(f).length, 0);
  assert.equal(testCalls(f).length, 0);
});

for (const entry of [
  { site: false, host: "index.docker.io", target: "registry-1.docker.io", success: text.testOk },
  { site: true, host: "docker.io", target: "docker.io", success: text.testOkToken },
]) {
  test(`the list test grants only the actual ${entry.site ? "site token" : "registry API"} host for ${entry.host}`, async () => {
    const f = fixture(entry);
    await clickTest(f);
    await f.user.click(await screen.findByRole("checkbox", { name: new RegExp(entry.target.replaceAll(".", "\\.")) }));
    await f.user.click(within(screen.getByRole("dialog")).getByRole("button", { name: text.test, exact: true }));
    await screen.findByText(entry.success);
    assert.deepEqual(f.policy().allow_private_hosts, ["existing.internal", entry.target]);
    for (const call of f.calls.filter((call) => call.path.endsWith("/check"))) assert.deepEqual(call.body, { hosts: [entry.target] });
    assert.equal(testCalls(f).length, 1);
    assert.equal(f.calls.filter((call) => /^\/api\/v1\/pull\/credentials(?:\/\d+)?$/.test(call.path) && call.method !== "GET").length, 0);
  });
}

for (const status of ["blocked", "unresolved"] as const) {
  test(`a ${status} selected host cannot be granted or tested from the list dialog`, async () => {
    const f = fixture({ status });
    await clickTest(f);
    const message = i18n.t(`outboundAccess.${status}`, { ns: "common", host: "registry.internal" });
    await screen.findByText(message);
    const dialog = screen.getByRole("dialog");
    assert.equal(within(dialog).queryByRole("checkbox"), null);
    assert.equal((within(dialog).getByRole("button", { name: text.test, exact: true }) as HTMLButtonElement).disabled, true);
    assert.equal(writes(f).length, 0);
    assert.equal(testCalls(f).length, 0);
  });
}

test("a read-only policy explains why its checkbox and login test cannot be enabled", async () => {
  const f = fixture({ editable: false });
  await clickTest(f);
  await screen.findByText(locale.common.outboundAccess.readOnly);
  assert.equal((await screen.findByRole("checkbox", { name: /registry\.internal/ })).getAttribute("data-disabled"), "");
  assert.equal((within(screen.getByRole("dialog")).getByRole("button", { name: text.test, exact: true }) as HTMLButtonElement).disabled, true);
  assert.equal(writes(f).length, 0);
  assert.equal(testCalls(f).length, 0);
});

test("a policy revision conflict leaves the existing allowlist unchanged and does not test the stored login", async () => {
  const f = fixture({ conflict: true });
  await clickTest(f);
  await f.user.click(await screen.findByRole("checkbox", { name: /registry\.internal/ }));
  await f.user.click(within(screen.getByRole("dialog")).getByRole("button", { name: text.test, exact: true }));
  await within(screen.getByRole("dialog")).findByRole("alert");
  assert.equal(writes(f).length, 1);
  assert.deepEqual(f.policy().allow_private_hosts, ["existing.internal"]);
  assert.equal(testCalls(f).length, 0);
});

test("a host becoming permanently blocked after consent is rechecked before granting or testing", async () => {
  const f = fixture();
  await clickTest(f);
  await f.user.click(await screen.findByRole("checkbox", { name: /registry\.internal/ }));
  f.setStatus("blocked");
  await f.user.click(within(screen.getByRole("dialog")).getByRole("button", { name: text.test, exact: true }));
  await within(screen.getByRole("dialog")).findByRole("alert");
  assert.equal(writes(f).length, 0);
  assert.equal(testCalls(f).length, 0);
  assert.deepEqual(f.policy().allow_private_hosts, ["existing.internal"]);
});

test("leaving the credential list while its policy read is pending cannot start a stale login test", async () => {
  let release!: () => void;
  const firstPolicyRead = new Promise<void>((resolve) => { release = resolve; });
  const f = fixture({ restrict: false, firstPolicyRead });
  await clickTest(f);
  await waitFor(() => assert.ok(f.calls.some((call) => call.path === policyPath)));
  f.unmount();
  await act(async () => { release(); });
  assert.equal(testCalls(f).length, 0);
  assert.equal(writes(f).length, 0);
  assert.equal(screen.queryByRole("dialog"), null);
});
