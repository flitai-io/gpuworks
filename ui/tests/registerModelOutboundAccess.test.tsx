import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["models"]);
const { cleanup, render, screen, fireEvent, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { RegisterModal } = await import("../src/pages/models/RegisterModelModal.tsx");
const originalFetch = globalThis.fetch;
type Call = { path: string; method: string; body?: unknown };
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const preview = {
  source_url: "http://models.internal:8080/Qwen", source_id: "Qwen", name: "Qwen",
  arch: "Qwen2ForCausalLM", precision: "float16", context_length: 2048,
  weight_bytes: 128, files_count: 4, input_modalities: ["text"],
};

type FixtureOptions = {
  role?: string;
  restrict?: boolean;
  editable?: boolean;
  status?: "needs_allow" | "blocked" | "unresolved";
  concurrentChange?: boolean;
};
function fixture(options: FixtureOptions = {}) {
  const calls: Call[] = [];
  let policy = { restrict_private_hosts: options.restrict ?? true, allow_private_hosts: ["retained.corp"], editable: options.editable ?? true, revision: "r1" };
  let checks = 0;
  let revision = 1;
  globalThis.fetch = (async (input, init) => {
    const path = String(input);
    const call = { path, method: init?.method ?? "GET", body: init?.body ? JSON.parse(String(init.body)) : undefined };
    calls.push(call);
    if (path === "/api/v1/auth/me") return json(200, { id: 1, username: options.role ?? "admin", role: options.role ?? "admin" });
    if (path.startsWith("/api/v1/server/outbound-policy") && options.role === "viewer") return json(403, { code: "PERMISSION_DENIED", error: "administrator permission required" });
    if (path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(200, policy);
    if (path === "/api/v1/server/outbound-policy/check" && call.method === "POST") {
      const hosts = (call.body as { hosts: string[] }).hosts;
      if (++checks === 2 && options.concurrentChange) policy = { ...policy, allow_private_hosts: ["retained.corp", "concurrent.corp"], revision: "r2" };
      return json(200, { items: hosts.map((requested) => {
        const host = requested === "::ffff:7f00:1" ? "127.0.0.1" : requested;
        return { host, status: !policy.restrict_private_hosts || policy.allow_private_hosts.includes(host) ? "allowed" : options.status ?? "needs_allow" };
      }), total: hosts.length });
    }
    if (path === "/api/v1/server/outbound-policy" && call.method === "PUT") {
      const next = call.body as typeof policy;
      if (next.revision !== policy.revision) return json(409, { code: "SERVER_OUTBOUND_POLICY_CONFLICT", error: "policy changed" });
      policy = { ...policy, ...next, revision: `r${++revision}` };
      return json(200, policy);
    }
    if (path === "/api/v1/models/preview-url" && call.method === "POST") {
      return !policy.restrict_private_hosts || policy.allow_private_hosts.includes("models.internal")
        ? json(200, preview)
        : json(502, { code: "HTTP_DIRECTORY_UNREACHABLE", error: "the directory could not be read as an HTTP directory index" });
    }
    return json(404, { error: "Not Found" });
  }) as typeof fetch;
  return calls;
}

afterEach(() => { cleanup(); globalThis.fetch = originalFetch; });

test("a directory import can explicitly allow its host and keep existing policy entries before parsing", async () => {
  const calls = fixture();
  const user = userEvent.setup();
  render(<I18nextProvider i18n={i18n}><RegisterModal open onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: locale.models.column.source }));
  fireEvent.click(screen.getByText(locale.models.source.url));
  fireEvent.change(await screen.findByRole("textbox", { name: locale.models.register.directoryURL }), { target: { value: "http://models.internal:8080/Qwen/" } });
  const checkbox = await screen.findByRole("checkbox", { name: /models\.internal/ });
  assert.deepEqual(calls.filter((call) => call.method === "PUT"), []);
  checkbox.focus();
  await user.keyboard(" ");
  assert.equal(checkbox.getAttribute("aria-checked"), "true");
  assert.deepEqual(calls.filter((call) => call.method === "PUT"), [], "selecting permission alone does not save it");
  await user.click(screen.getByRole("button", { name: locale.models.register.parseDirectory, exact: true }));
  await screen.findByText(locale.models.register.urlParsed);
  await waitFor(() => assert.equal(calls.filter((call) => call.path === "/api/v1/models/preview-url").length, 1));
  assert.deepEqual(calls.filter((call) => call.method === "PUT"), [{
    path: "/api/v1/server/outbound-policy", method: "PUT", body: {
      restrict_private_hosts: true, allow_private_hosts: ["retained.corp", "models.internal"], revision: "r1",
    },
  }]);
  const checks = calls.filter((call) => call.path.endsWith("/check"));
  assert.ok(checks.length > 0);
  for (const check of checks) assert.deepEqual(check.body, { hosts: ["models.internal"] }, "only the hostname crosses the permission-check boundary");
});

async function openDirectory(address = "http://models.internal:8080/Qwen/") {
  render(<I18nextProvider i18n={i18n}><RegisterModal open onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: locale.models.column.source }));
  fireEvent.click(screen.getByText(locale.models.source.url));
  const input = await screen.findByRole("textbox", { name: locale.models.register.directoryURL });
  fireEvent.change(input, { target: { value: address } });
  return input;
}

test("an unchecked directory host is not added when parsing is denied", async () => {
  const calls = fixture();
  const user = userEvent.setup();
  await openDirectory();
  const checkbox = await screen.findByRole("checkbox", { name: /models\.internal/ });
  assert.equal(checkbox.getAttribute("aria-checked"), "false");
  await user.click(screen.getByRole("button", { name: locale.models.register.parseDirectory, exact: true }));
  await waitFor(() => assert.equal(calls.filter((call) => call.path === "/api/v1/models/preview-url").length, 1));
  assert.deepEqual(calls.filter((call) => call.method === "PUT"), []);
});

test("a changed directory hostname does not inherit the previous consent", async () => {
  const calls = fixture();
  const user = userEvent.setup();
  const input = await openDirectory();
  await user.click(await screen.findByRole("checkbox", { name: /models\.internal/ }));
  fireEvent.change(input, { target: { value: "http://second.internal/Qwen/" } });
  const second = await screen.findByRole("checkbox", { name: /second\.internal/ });
  assert.equal(second.getAttribute("aria-checked"), "false");
  await user.click(screen.getByRole("button", { name: locale.models.register.parseDirectory, exact: true }));
  await waitFor(() => assert.equal(calls.filter((call) => call.path === "/api/v1/models/preview-url").length, 1));
  assert.deepEqual(calls.filter((call) => call.method === "PUT"), []);
});

test("a permanently blocked directory host cannot be selected for authorization", async () => {
  const calls = fixture({ status: "blocked" });
  await openDirectory("http://169.254.169.254/Qwen/");
  await screen.findByText("169.254.169.254 cannot be allowed by the outbound policy.");
  assert.equal(screen.queryByRole("checkbox"), null);
  assert.deepEqual(calls.filter((call) => call.method === "PUT"), []);
});

test("an unresolved directory host offers retry without an authorization checkbox", async () => {
  const calls = fixture({ status: "unresolved" });
  await openDirectory();
  await screen.findByRole("button", { name: "Check again" });
  assert.equal(screen.queryByRole("checkbox"), null);
  assert.deepEqual(calls.filter((call) => call.method === "PUT"), []);
});

test("a read-only policy shows a disabled checkbox and a settings path", async () => {
  const calls = fixture({ editable: false });
  await openDirectory();
  const checkbox = await screen.findByRole("checkbox", { name: /models\.internal/ });
  assert.equal(checkbox.getAttribute("aria-disabled"), "true");
  fireEvent.click(checkbox);
  assert.equal(checkbox.getAttribute("aria-checked"), "false");
  assert.equal(screen.getByRole("link", { name: "Outbound access policy" }).getAttribute("href"), "/settings#outbound-policy");
  assert.deepEqual(calls.filter((call) => call.method === "PUT"), []);
});

test("a viewer gets an administrator hint without reading or changing the policy", async () => {
  const calls = fixture({ role: "viewer" });
  await openDirectory();
  await screen.findByText("An administrator can allow private source hosts.");
  assert.equal(screen.queryByRole("checkbox"), null);
  assert.deepEqual(calls.filter((call) => call.path.startsWith("/api/v1/server/outbound-policy")), []);
});

test("default open mode parses without adding a host or checking DNS permissions", async () => {
  const calls = fixture({ restrict: false });
  const user = userEvent.setup();
  await openDirectory();
  await waitFor(() => assert.ok(calls.some((call) => call.path === "/api/v1/server/outbound-policy")));
  assert.equal(screen.queryByRole("checkbox"), null);
  await user.click(screen.getByRole("button", { name: locale.models.register.parseDirectory, exact: true }));
  await screen.findByText(locale.models.register.urlParsed);
  assert.deepEqual(calls.filter((call) => call.path.endsWith("/check") || call.method === "PUT"), []);
});

test("an open directory form can authorize after another administrator enables strict mode", async () => {
  const calls = fixture({ restrict: false });
  const user = userEvent.setup();
  await openDirectory();
  await waitFor(() => assert.ok(calls.some((call) => call.path === "/api/v1/server/outbound-policy")));
  await waitFor(() => assert.equal(screen.queryByRole("status"), null));
  assert.equal(screen.queryByRole("checkbox"), null);
  const changed = await fetch("/api/v1/server/outbound-policy", { method: "PUT", body: JSON.stringify({
    restrict_private_hosts: true, allow_private_hosts: ["retained.corp"], revision: "r1",
  }) });
  assert.equal(changed.status, 200);
  await user.click(screen.getByRole("button", { name: locale.models.register.parseDirectory, exact: true }));
  const checkbox = await screen.findByRole("checkbox", { name: /models\.internal/ });
  assert.equal(checkbox.getAttribute("aria-checked"), "false");
  assert.equal(calls.filter((call) => call.method === "PUT").length, 1, "only the other administrator changed the policy so far");
  await user.click(checkbox);
  await user.click(screen.getByRole("button", { name: locale.models.register.parseDirectory, exact: true }));
  await screen.findByText(locale.models.register.urlParsed);
  assert.deepEqual(calls.filter((call) => call.method === "PUT")[1].body, {
    restrict_private_hosts: true, allow_private_hosts: ["retained.corp", "models.internal"], revision: "r2",
  });
});

test("a concurrent policy edit is not overwritten and the directory operation does not start", async () => {
  const calls = fixture({ concurrentChange: true });
  const user = userEvent.setup();
  await openDirectory();
  await user.click(await screen.findByRole("checkbox", { name: /models\.internal/ }));
  await user.click(screen.getByRole("button", { name: locale.models.register.parseDirectory, exact: true }));
  await waitFor(() => assert.ok(screen.getAllByRole("alert").some((node) => /changed/i.test(node.textContent ?? ""))));
  const saved = await (await fetch("/api/v1/server/outbound-policy")).json();
  assert.deepEqual(saved.allow_private_hosts, ["retained.corp", "concurrent.corp"]);
  assert.deepEqual(calls.filter((call) => call.path === "/api/v1/models/preview-url"), []);
  assert.equal(calls.filter((call) => call.method === "PUT").length, 1);
});

test("an IPv4-mapped IPv6 directory uses the same canonical host as the server policy", async () => {
  const calls = fixture();
  render(<I18nextProvider i18n={i18n}><RegisterModal open onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: locale.models.column.source }));
  fireEvent.click(screen.getByText(locale.models.source.url));
  fireEvent.change(await screen.findByRole("textbox", { name: locale.models.register.directoryURL }), { target: { value: "http://[::ffff:127.0.0.1]:8080/Qwen/" } });
  await screen.findByRole("checkbox", { name: /127\.0\.0\.1/ });
  const checks = calls.filter((call) => call.path.endsWith("/check"));
  assert.ok(checks.length > 0);
  for (const check of checks) assert.deepEqual(check.body, { hosts: ["127.0.0.1"] });
  assert.deepEqual(calls.filter((call) => call.method === "PUT"), []);
});

test("an internationalized directory asks for the Punycode address before offering permission", async () => {
  const calls = fixture();
  const input = await openDirectory("http://bücher.internal:8080/Qwen/");
  await screen.findByText("Use these ASCII/Punycode hostnames in the source addresses: xn--bcher-kva.internal.");
  assert.equal(screen.queryByRole("checkbox"), null);
  assert.deepEqual(calls.filter((call) => call.path.endsWith("/check") || call.method === "PUT"), []);
  fireEvent.change(input, { target: { value: "http://xn--bcher-kva.internal:8080/Qwen/" } });
  const checkbox = await screen.findByRole("checkbox", { name: /xn--bcher-kva\.internal/ });
  assert.equal(checkbox.getAttribute("aria-checked"), "false");
});
