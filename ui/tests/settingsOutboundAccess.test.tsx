import { createI18n } from "./helpers/dom.ts";
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";

const { i18n, locale } = await createI18n(["settings", "common"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: AIConfigModal } = await import("../src/pages/settings/AIConfigModal.tsx");
const { default: TelemetryCard } = await import("../src/pages/settings/TelemetryCard.tsx");
const { default: CredentialDrawer } = await import("../src/pages/settings/pull/CredentialDrawer.tsx");
const { default: PullRuleDrawer } = await import("../src/pages/settings/pull/PullRuleDrawer.tsx");

const originalFetch = globalThis.fetch;
const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
type Call = { path: string; method: string; body?: Record<string, unknown> };
type Policy = { restrict_private_hosts: boolean; allow_private_hosts: string[]; editable: boolean; revision: string };
const unexpectedRequests: string[] = [];

function outboundFixture(reply?: (call: Call, policy: Policy) => Response | undefined, privateHosts: string[] = []) {
  const calls: Call[] = [];
  let policy: Policy = { restrict_private_hosts: true, allow_private_hosts: ["existing.internal"], editable: true, revision: "r1" };
  globalThis.fetch = (async (input, init) => {
    const path = String(input);
    const method = init?.method ?? "GET";
    const body = init?.body ? JSON.parse(String(init.body)) as Record<string, unknown> : undefined;
    calls.push({ path, method, body });
    if (path === "/api/v1/auth/me" && method === "GET") return json({ id: 1, username: "admin", role: "admin" });
    if (path === "/api/v1/nodes?page=1&page_size=50&state=online" && method === "GET") return json({ items: [], page: 1, page_size: 50, total: 0 });
    if (path === "/api/v1/server/outbound-policy" && method === "GET") return json(policy);
    if (path === "/api/v1/server/outbound-policy/check" && method === "POST") {
      assert.ok(Array.isArray(body?.hosts));
      const hosts = body.hosts as string[];
      return json({ items: hosts.map((host) => ({ host, status: host === "169.254.169.254" ? "blocked" : (host.endsWith(".internal") || privateHosts.includes(host)) && !policy.allow_private_hosts.includes(host) ? "needs_allow" : "allowed" })), total: hosts.length });
    }
    if (path === "/api/v1/server/outbound-policy" && method === "PUT") {
      assert.equal(body?.revision, policy.revision);
      assert.equal(body?.restrict_private_hosts, true);
      assert.ok(Array.isArray(body?.allow_private_hosts));
      policy = { ...policy, allow_private_hosts: body.allow_private_hosts as string[], revision: "r2" };
      return json(policy);
    }
    if (path === "/api/v1/ai/configs/test" && method === "POST") {
      const host = new URL(String(body?.base_url)).hostname;
      return json({ outcome: policy.allow_private_hosts.includes(host) ? "ok" : "blocked", base_url: body?.base_url, latency_ms: 3 });
    }
    const response = reply?.({ path, method, body }, policy);
    if (response) return response;
    unexpectedRequests.push(`${method} ${path}`);
    throw new Error(`Unexpected HTTP request: ${method} ${path}`);
  }) as typeof fetch;
  return { calls, policy: () => policy };
}

afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
  assert.deepEqual(unexpectedRequests.splice(0), [], "the HTTP fixture must account for every request");
});

test("the collector test can allow its endpoint without treating the browser trace link as a server target", async () => {
  const status = { active: false, traces_url: "", exported: 0, dropped: 0, failed: 0, queued: 0, last_error: "", last_error_at: null, last_export_at: null, setting_error: "" };
  const fixture = outboundFixture((call, policy) => {
    if (call.path === "/api/v1/telemetry/export" && call.method === "GET") return json({ enabled: false, endpoint: "http://collector.internal:4318", headers: [], sample_ratio: 1, trace_link: "https://grafana.internal/t/{trace_id}", status });
    if (call.path === "/api/v1/telemetry/export/test" && call.method === "POST") return json({ ok: policy.allow_private_hosts.includes("collector.internal"), trace_id: "test-span", traces_url: "http://collector.internal:4318/v1/traces", error: "" });
    return undefined;
  });
  render(<I18nextProvider i18n={i18n}><TelemetryCard /></I18nextProvider>);
  const user = userEvent.setup();
  const allow = await screen.findByRole("checkbox", { name: /collector\.internal/ });
  await user.click(allow);
  await user.click(screen.getByRole("button", { name: locale.settings.telemetry.test }));
  await screen.findByText(locale.settings.telemetry.testOK);
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal", "collector.internal"]);
  const checks = fixture.calls.filter((call) => call.path.endsWith("/check"));
  assert.ok(checks.length > 0);
  for (const call of checks) assert.deepEqual(call.body, { hosts: ["collector.internal"] });
});

test("an AI service edit explicitly allows its current host before testing without replacing existing permissions", async () => {
  const fixture = outboundFixture();
  render(<I18nextProvider i18n={i18n}><AIConfigModal open editing={{
    id: 7, name: "Internal AI", preset: "custom", base_url: "http://ai.internal:8080/v1", model: "qwen", is_default: true,
  }} onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
  const user = userEvent.setup();
  const dialog = screen.getByRole("dialog");
  const allow = await within(dialog).findByRole("checkbox", { name: /ai\.internal/ });
  assert.equal(allow.getAttribute("aria-checked"), "false");
  await user.click(allow);
  await user.click(within(dialog).getByRole("button", { name: locale.settings.ai.test }));
  await within(dialog).findByText(locale.settings.ai.testOk);
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal", "ai.internal"]);
  assert.deepEqual(fixture.calls.filter((call) => call.method === "PUT"), [{
    path: "/api/v1/server/outbound-policy", method: "PUT", body: {
      restrict_private_hosts: true, allow_private_hosts: ["existing.internal", "ai.internal"], revision: "r1",
    },
  }]);
});

test("editing registry credentials can explicitly allow the login-test host while keeping its stored secret", async () => {
  const credential = { id: 9, host: "registry.internal:5000", username: "reader", has_secret: true };
  const fixture = outboundFixture((call, policy) => {
    if (call.path === "/api/v1/pull/credentials/9" && call.method === "PUT") {
      assert.deepEqual(call.body, { host: "registry.internal:5000", username: "reader", secret: "" });
      return json(credential);
    }
    if (call.path === "/api/v1/pull/credentials/9/test" && call.method === "POST") {
      return policy.allow_private_hosts.includes("registry.internal") ? json({ ok: true }) : json({ code: "CREDENTIAL_LOGIN_FAILED", error: "outbound URL is not permitted" }, 502);
    }
    return undefined;
  });
  render(<I18nextProvider i18n={i18n}><CredentialDrawer credential={credential} onClose={() => {}} onChanged={() => {}} /></I18nextProvider>);
  const user = userEvent.setup();
  await user.click(await screen.findByRole("checkbox", { name: /registry\.internal/ }));
  await user.click(screen.getByRole("button", { name: locale.settings.pull.credentials.saveAndTest }));
  await screen.findByText(locale.settings.pull.credentials.testOk);
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal", "registry.internal"]);
  const checks = fixture.calls.filter((call) => call.path.endsWith("/check"));
  assert.ok(checks.length > 0);
  for (const call of checks) assert.deepEqual(call.body, { hosts: ["registry.internal"] });
});

test("Docker Hub registry aliases request permission for the actual registry API host while preserving the entered credential host", async () => {
  const fixture = outboundFixture((call, policy) => {
    if (call.path === "/api/v1/pull/credentials" && call.method === "POST") return json({ id: 13, host: "docker.io", username: "reader", has_secret: true }, 201);
    if (call.path === "/api/v1/pull/credentials/13" && call.method === "PUT") return json({ id: 13, host: "docker.io", username: "reader", has_secret: true });
    if (call.path === "/api/v1/pull/credentials/13/test" && call.method === "POST") return policy.allow_private_hosts.includes("registry-1.docker.io") ? json({ ok: true }) : json({ code: "CREDENTIAL_LOGIN_FAILED", error: "outbound URL is not permitted" }, 502);
    return undefined;
  }, ["registry-1.docker.io"]);
  render(<I18nextProvider i18n={i18n}><CredentialDrawer credential={null} onClose={() => {}} onChanged={() => {}} /></I18nextProvider>);
  const user = userEvent.setup();
  fireEvent.change(screen.getByLabelText(locale.settings.pull.credentials.host), { target: { value: " INDEX.DOCKER.IO " } });
  fireEvent.change(screen.getByLabelText(locale.settings.pull.credentials.username), { target: { value: "reader" } });
  fireEvent.change(screen.getByLabelText(locale.settings.pull.credentials.secret), { target: { value: "synthetic-password" } });
  await user.click(await screen.findByRole("checkbox", { name: /registry-1\.docker\.io/ }));
  await user.click(screen.getByRole("button", { name: locale.settings.pull.credentials.saveAndTest }));
  await screen.findByText(locale.settings.pull.credentials.testOk);
  assert.equal(fixture.calls.find((call) => call.path === "/api/v1/pull/credentials" && call.method === "POST")?.body?.host, "INDEX.DOCKER.IO");
  fireEvent.change(screen.getByLabelText(locale.settings.pull.credentials.host), { target: { value: " DoCkEr.Io " } });
  await user.click(screen.getByRole("button", { name: locale.settings.pull.credentials.saveAndTest }));
  await waitFor(() => assert.ok(fixture.calls.some((call) => call.path === "/api/v1/pull/credentials/13" && call.method === "PUT")));
  assert.equal(fixture.calls.find((call) => call.path === "/api/v1/pull/credentials/13" && call.method === "PUT")?.body?.host, "DoCkEr.Io");
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal", "registry-1.docker.io"]);
  for (const call of fixture.calls.filter((call) => call.path.endsWith("/check"))) assert.deepEqual(call.body, { hosts: ["registry-1.docker.io"] });
});

test("a site token for Docker Hub keeps the original site host instead of using the registry API alias", async () => {
  const credential = { id: 14, host: "docker.io", username: "", has_secret: true };
  const fixture = outboundFixture((call) => {
    if (call.path === "/api/v1/pull/credentials/14" && call.method === "PUT") return json(credential);
    return undefined;
  }, ["registry-1.docker.io"]);
  render(<I18nextProvider i18n={i18n}><CredentialDrawer credential={credential} onClose={() => {}} onChanged={() => {}} /></I18nextProvider>);
  const user = userEvent.setup();
  await waitFor(() => assert.ok(fixture.calls.some((call) => call.path.endsWith("/check"))));
  assert.equal(screen.queryByRole("checkbox", { name: /registry-1\.docker\.io/ }), null);
  await user.click(screen.getByRole("button", { name: locale.settings.pull.action.save, exact: true }));
  await waitFor(() => assert.ok(fixture.calls.some((call) => call.path === "/api/v1/pull/credentials/14" && call.method === "PUT")));
  for (const call of fixture.calls.filter((call) => call.path.endsWith("/check"))) assert.deepEqual(call.body, { hosts: ["docker.io"] });
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal"]);
});

for (const enteredHost of ["https://docker.io/", "https://index.docker.io/", "registry.hub.docker.com", " HTTPS://REGISTRY-1.DOCKER.IO/ "]) {
  for (const site of [false, true]) {
    test(`credential normalization grants only the actual ${site ? "site" : "registry"} host for ${enteredHost.trim()}`, async () => {
      const target = site ? "docker.io" : "registry-1.docker.io";
      const fixture = outboundFixture((call) => {
        if (call.path === "/api/v1/pull/credentials" && call.method === "POST") return json({ id: 15, host: "docker.io", username: site ? "" : "reader", has_secret: true }, 201);
        return undefined;
      }, [target]);
      render(<I18nextProvider i18n={i18n}><CredentialDrawer credential={null} onClose={() => {}} onChanged={() => {}} /></I18nextProvider>);
      const user = userEvent.setup();
      if (site) await user.click(screen.getByRole("radio", { name: locale.settings.pull.credentials.typeSite, exact: true }));
      fireEvent.change(screen.getByLabelText(locale.settings.pull.credentials.host), { target: { value: enteredHost } });
      if (!site) fireEvent.change(screen.getByLabelText(locale.settings.pull.credentials.username), { target: { value: "reader" } });
      fireEvent.change(screen.getByLabelText(site ? locale.settings.pull.credentials.token : locale.settings.pull.credentials.secret), { target: { value: "synthetic-secret" } });
      await user.click(await screen.findByRole("checkbox", { name: new RegExp(target.replaceAll(".", "\\.")) }));
      await user.click(screen.getByRole("button", { name: locale.settings.pull.action.save, exact: true }));
      await waitFor(() => assert.ok(fixture.calls.some((call) => call.path === "/api/v1/pull/credentials" && call.method === "POST")));
      assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal", target]);
      assert.deepEqual(fixture.calls.find((call) => call.path === "/api/v1/pull/credentials" && call.method === "POST")?.body, { host: enteredHost.trim(), username: site ? "" : "reader", secret: "synthetic-secret" });
      for (const call of fixture.calls.filter((call) => call.path.endsWith("/check"))) assert.deepEqual(call.body, { hosts: [target] });
    });
  }
}

test("a management-node site rule can allow only the selected candidate when its saved rule is edited", async () => {
  const rule = { id: 4, position: 1, kind: "url" as const, match: "https://source.internal", endpoints: ["https://mirror.internal/models"], allow_upstream: true, control_plane: true, note: "", node_requirements: {}, matched_nodes: 0 };
  const fixture = outboundFixture((call) => {
    if (call.path.startsWith("/api/v1/nodes/facets") && call.method === "GET") return json({ total: 0, labels: [], system_labels: [] });
    if (call.path === "/api/v1/pull/source-rules/4" && call.method === "PUT") return json({ ...rule, ...call.body });
    return undefined;
  });
  render(<I18nextProvider i18n={i18n}><PullRuleDrawer rule={rule} onClose={() => {}} onSaved={() => {}} /></I18nextProvider>);
  const user = userEvent.setup();
  const allowMirror = await screen.findByRole("checkbox", { name: /mirror\.internal/ });
  assert.ok(screen.getByRole("checkbox", { name: /source\.internal/ }));
  await user.click(allowMirror);
  await user.click(screen.getByRole("button", { name: locale.settings.pull.action.save }));
  await waitFor(() => assert.ok(fixture.calls.some((call) => call.path === "/api/v1/pull/source-rules/4" && call.method === "PUT")));
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal", "mirror.internal"]);
  assert.deepEqual(fixture.calls.find((call) => call.path === "/api/v1/pull/source-rules/4")?.body, {
    match: "https://source.internal", endpoints: ["https://mirror.internal/models"], allow_upstream: true,
    note: "", node_requirements: {}, control_plane: true,
  });
});

test("creating an AI service saves its explicit host permission by keyboard before storing the new service", async () => {
  const fixture = outboundFixture((call) => {
    if (call.path === "/api/v1/ai/configs" && call.method === "POST") return json({
      id: 8, name: call.body?.name, preset: call.body?.preset, base_url: call.body?.base_url,
      model: call.body?.model, is_default: call.body?.is_default,
    }, 201);
    return undefined;
  });
  render(<I18nextProvider i18n={i18n}><AIConfigModal open onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
  const user = userEvent.setup();
  const dialog = screen.getByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText(locale.settings.ai.name), { target: { value: "Internal AI" } });
  fireEvent.change(within(dialog).getByLabelText(locale.settings.ai.baseUrl), { target: { value: "http://new-ai.internal:8080/v1" } });
  fireEvent.change(within(dialog).getByLabelText(locale.settings.ai.upstreamKey), { target: { value: "synthetic-key" } });
  const allow = await within(dialog).findByRole("checkbox", { name: /new-ai\.internal/ });
  allow.focus();
  await user.keyboard(" ");
  await user.click(within(dialog).getByRole("button", { name: locale.settings.common.add }));
  await waitFor(() => assert.ok(fixture.calls.some((call) => call.path === "/api/v1/ai/configs" && call.method === "POST")));
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal", "new-ai.internal"]);
  assert.equal(fixture.calls.find((call) => call.path === "/api/v1/ai/configs")?.body?.api_key, "synthetic-key");
  for (const call of fixture.calls.filter((call) => call.path.endsWith("/check"))) {
    assert.ok(!JSON.stringify(call.body).includes("synthetic-key"));
  }
});

test("changing an AI address clears the previous host consent and an unchecked test cannot change the allowlist", async () => {
  const fixture = outboundFixture();
  render(<I18nextProvider i18n={i18n}><AIConfigModal open editing={{
    id: 7, name: "Internal AI", preset: "custom", base_url: "http://ai.internal/v1", model: "qwen", is_default: true,
  }} onClose={() => {}} onDone={() => {}} /></I18nextProvider>);
  const user = userEvent.setup();
  await user.click(await screen.findByRole("checkbox", { name: /ai\.internal/ }));
  fireEvent.change(screen.getByLabelText(locale.settings.ai.baseUrl), { target: { value: "http://other-ai.internal/v1" } });
  const next = await screen.findByRole("checkbox", { name: /other-ai\.internal/ });
  assert.equal(next.getAttribute("aria-checked"), "false");
  await user.click(screen.getByRole("button", { name: locale.settings.ai.test }));
  await screen.findByText(locale.settings.ai.testBlocked);
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal"]);
  assert.equal(fixture.calls.filter((call) => call.method === "PUT").length, 0);
  assert.equal(fixture.calls.find((call) => call.path === "/api/v1/ai/configs/test")?.body?.base_url, "http://other-ai.internal/v1");
});

test("a newly created credential can allow its current host on save without leaking its secret to permission checks", async () => {
  const fixture = outboundFixture((call) => {
    if (call.path === "/api/v1/pull/credentials" && call.method === "POST") return json({ id: 10, host: call.body?.host, username: call.body?.username, has_secret: true }, 201);
    return undefined;
  });
  render(<I18nextProvider i18n={i18n}><CredentialDrawer credential={null} onClose={() => {}} onChanged={() => {}} /></I18nextProvider>);
  const user = userEvent.setup();
  fireEvent.change(screen.getByLabelText(locale.settings.pull.credentials.host), { target: { value: "new-registry.internal:5000" } });
  fireEvent.change(screen.getByLabelText(locale.settings.pull.credentials.username), { target: { value: "reader" } });
  fireEvent.change(screen.getByLabelText(locale.settings.pull.credentials.secret), { target: { value: "synthetic-password" } });
  await user.click(await screen.findByRole("checkbox", { name: /new-registry\.internal/ }));
  await user.click(screen.getByRole("button", { name: locale.settings.pull.action.save, exact: true }));
  await waitFor(() => assert.ok(fixture.calls.some((call) => call.path === "/api/v1/pull/credentials" && call.method === "POST")));
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal", "new-registry.internal"]);
  assert.deepEqual(fixture.calls.find((call) => call.path === "/api/v1/pull/credentials")?.body, { host: "new-registry.internal:5000", username: "reader", secret: "synthetic-password" });
  for (const call of fixture.calls.filter((call) => call.path.endsWith("/check"))) assert.deepEqual(call.body, { hosts: ["new-registry.internal"] });
});

test("a node-only site rule creation does not modify management-node permissions", async () => {
  const fixture = outboundFixture((call) => {
    if (call.path.startsWith("/api/v1/nodes/facets") && call.method === "GET") return json({ total: 0, labels: [], system_labels: [] });
    if (call.path === "/api/v1/pull/source-rules" && call.method === "POST") return json({ id: 5, position: 1, kind: "url", matched_nodes: 0, ...call.body }, 201);
    return undefined;
  });
  render(<I18nextProvider i18n={i18n}><PullRuleDrawer rule={null} onClose={() => {}} onSaved={() => {}} /></I18nextProvider>);
  const user = userEvent.setup();
  await user.click(screen.getByRole("radio", { name: locale.settings.pull.rules.kindUrl }));
  fireEvent.change(screen.getByLabelText(locale.settings.pull.rules.match), { target: { value: "https://source.internal" } });
  fireEvent.change(screen.getByLabelText(locale.settings.pull.rules.endpointN.replace("{{n}}", "1")), { target: { value: "https://mirror.internal/models" } });
  await user.click(screen.getByRole("checkbox", { name: locale.settings.pull.rules.controlPlane, exact: true }));
  await user.click(screen.getByRole("button", { name: locale.settings.pull.action.save, exact: true }));
  await waitFor(() => assert.ok(fixture.calls.some((call) => call.path === "/api/v1/pull/source-rules" && call.method === "POST")));
  assert.equal(fixture.calls.find((call) => call.path === "/api/v1/pull/source-rules")?.body?.control_plane, false);
  assert.equal(fixture.calls.filter((call) => call.path === "/api/v1/server/outbound-policy" && call.method === "PUT").length, 0);
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal"]);
});

test("saving a collector configuration can allow its endpoint before the exporter is enabled", async () => {
  const status = { active: false, traces_url: "", exported: 0, dropped: 0, failed: 0, queued: 0, last_error: "", last_error_at: null, last_export_at: null, setting_error: "" };
  const fixture = outboundFixture((call) => {
    if (call.path === "/api/v1/telemetry/export" && call.method === "GET") return json({ enabled: false, endpoint: "http://collector.internal:4318", headers: [], sample_ratio: 1, trace_link: "", status });
    if (call.path === "/api/v1/telemetry/export" && call.method === "PUT") return json({ ...call.body, headers: [], status });
    return undefined;
  });
  render(<I18nextProvider i18n={i18n}><TelemetryCard /></I18nextProvider>);
  const user = userEvent.setup();
  await user.click(await screen.findByRole("checkbox", { name: /collector\.internal/ }));
  await user.click(screen.getByRole("button", { name: locale.settings.common.save, exact: true }));
  await screen.findByText(locale.settings.telemetry.saved);
  assert.deepEqual(fixture.policy().allow_private_hosts, ["existing.internal", "collector.internal"]);
  assert.equal(fixture.calls.find((call) => call.path === "/api/v1/telemetry/export" && call.method === "PUT")?.body?.enabled, false);
});
