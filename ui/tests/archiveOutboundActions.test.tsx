import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { dom } from "./helpers/dom.ts";
import { i18n, installFetch, json, nightlyVersion, zhImages } from "./helpers/imagesHarness.ts";
import { api } from "../src/api.ts";

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { default: Dialog } = await import("../src/pages/images/ImageArchiveURLDialog.tsx");
let restoreFetch = () => {};
afterEach(() => { cleanup(); restoreFetch(); });

test("explicit archive consent grants the current host before direct relay submission without previewing", async () => {
  const events: string[] = [];
  let allowed = false;
  const stub = installFetch((call) => {
    if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(200, { restrict_private_hosts: true, allow_private_hosts: allowed ? ["retained.corp", "archive.internal"] : ["retained.corp"], editable: true, revision: allowed ? "r2" : "r1" });
    if (call.path === "/api/v1/server/outbound-policy/check") {
      const { hosts } = JSON.parse(call.body!);
      return json(200, { items: hosts.map((host: string) => ({ host, status: allowed ? "allowed" : "needs_allow" })), total: hosts.length });
    }
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "PUT") {
      assert.deepEqual(JSON.parse(call.body!), { restrict_private_hosts: true, allow_private_hosts: ["retained.corp", "archive.internal"], revision: "r1" });
      events.push("allow"); allowed = true;
      return json(200, { restrict_private_hosts: true, allow_private_hosts: ["retained.corp", "archive.internal"], editable: true, revision: "r2" });
    }
    if (call.path === "/api/v1/images/31/nodes/n3/sync") {
      events.push("relay");
      assert.deepEqual(JSON.parse(call.body!), { source: "url", archive_url: "http://archive.internal:5443/large.tar", relay: true });
      return allowed ? json(202, { state: "pulling" }) : json(403, { error: "outbound URL is not permitted" });
    }
    return undefined;
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><Dialog open version={nightlyVersion as never} nodeLabel="gpu-03" initialURL="" onCancel={() => {}} onStart={async (url, relay) => { await api.syncImage(31, "n3", "url", { archive_url: url, relay }); }} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(screen.getByRole("textbox", { name: zhImages.import.url.label }), "http://archive.internal:5443/large.tar");
  await user.click(await screen.findByRole("checkbox", { name: /archive\.internal/ }));
  await user.click(screen.getByRole("checkbox", { name: zhImages.nodeTable.relay }));
  await user.click(screen.getByRole("button", { name: /拉\s*取/ }));
  await waitFor(() => assert.ok(stub.calls.some((call) => call.path.endsWith("/sync"))));
  assert.deepEqual(events, ["allow", "relay"]);
  assert.equal(stub.calls.some((call) => call.path.endsWith("/preview-archive-url")), false);
});

test("an open archive import refreshes strict mode and can grant without reopening", async () => {
  let restrict = false;
  let allowed = false;
  const stub = installFetch((call) => {
    if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(200, { restrict_private_hosts: restrict, allow_private_hosts: allowed ? ["retained.corp", "archive.internal"] : ["retained.corp"], editable: true, revision: allowed ? "r3" : "r2" });
    if (call.path.endsWith("/outbound-policy/check")) {
      const { hosts } = JSON.parse(call.body!);
      return json(200, { items: hosts.map((host: string) => ({ host, status: allowed ? "allowed" : "needs_allow" })), total: hosts.length });
    }
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "PUT") {
      assert.deepEqual(JSON.parse(call.body!), { restrict_private_hosts: true, allow_private_hosts: ["retained.corp", "archive.internal"], revision: "r2" });
      allowed = true;
      return json(200, { restrict_private_hosts: true, allow_private_hosts: ["retained.corp", "archive.internal"], editable: true, revision: "r3" });
    }
    if (call.path.endsWith("/preview-archive-url")) return allowed
      ? json(200, { url: "http://archive.internal/image.tar", size_bytes: 1024, repo_tags: ["example/new:v1"], previewable: true })
      : json(403, { code: "IMAGE_ARCHIVE_URL_POLICY_DENIED", error: "host not allowed", params: { host: "archive.internal", approvable: true, allow_host: "archive.internal" } });
    return undefined;
  });
  restoreFetch = stub.restore;
  const { default: Import } = await import("../src/pages/images/ImageArchiveURLImport.tsx");
  render(<I18nextProvider i18n={i18n}><Import versions={[]} onMatched={() => {}} onPrefill={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(screen.getByRole("textbox", { name: zhImages.import.url.label }), "http://archive.internal/image.tar");
  await waitFor(() => assert.ok(stub.calls.some((call) => call.path === "/api/v1/server/outbound-policy")));
  assert.equal(screen.queryByRole("checkbox"), null);
  restrict = true;
  await user.click(screen.getByRole("button", { name: /解\s*析/ }));
  await screen.findByRole("alert");
  const checkbox = screen.queryByRole("checkbox", { name: /archive\.internal/ });
  assert.ok(checkbox, "a newly enabled global restriction offers consent in the existing form");
  assert.equal(stub.calls.some((call) => call.method === "PUT"), false);
  await user.click(checkbox);
  await user.click(screen.getByRole("button", { name: /解\s*析/ }));
  await screen.findByText("example/new:v1");
  assert.equal(stub.calls.filter((call) => call.method === "PUT").length, 1);
});

for (const relay of [false, true]) {
  test(`an unrestricted archive ${relay ? "relay" : "node-direct"} submit skips host checks and grants`, async () => {
    const stub = installFetch((call) => {
      if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
      if (call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(200, { restrict_private_hosts: false, allow_private_hosts: ["retained.corp"], editable: true, revision: "r1" });
      if (call.path.endsWith("/sync")) return json(202, { state: "pulling" });
      return undefined;
    });
    restoreFetch = stub.restore;
    render(<I18nextProvider i18n={i18n}><Dialog open version={nightlyVersion as never} nodeLabel="gpu-03" initialURL="" onCancel={() => {}} onStart={async (url, throughServer) => { await api.syncImage(31, "n3", "url", { archive_url: url, relay: throughServer }); }} /></I18nextProvider>);
    const user = userEvent.setup({ document: dom.window.document });
    await user.type(screen.getByRole("textbox", { name: zhImages.import.url.label }), "http://archive.internal/image.tar");
    await waitFor(() => assert.ok(stub.calls.some((call) => call.path === "/api/v1/server/outbound-policy")));
    assert.equal(screen.queryByRole("checkbox", { name: /archive\.internal/ }), null);
    if (relay) await user.click(screen.getByRole("checkbox", { name: zhImages.nodeTable.relay }));
    await user.click(screen.getByRole("button", { name: /拉\s*取/ }));
    await waitFor(() => assert.ok(stub.calls.some((call) => call.path.endsWith("/sync"))));
    assert.equal(stub.calls.some((call) => call.path.endsWith("/outbound-policy/check") || call.method === "PUT"), false);
    assert.equal(stub.calls.some((call) => call.path.endsWith("/preview-archive-url")), false);
  });
}

test("node-direct submission remains available when management policy cannot be read", async () => {
  const stub = installFetch((call) => {
    if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(503, { code: "SERVER_OUTBOUND_POLICY_UNAVAILABLE", error: "policy unavailable" });
    if (call.path.endsWith("/sync")) return json(202, { state: "pulling" });
    return undefined;
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><Dialog open version={nightlyVersion as never} nodeLabel="gpu-03" initialURL="" onCancel={() => {}} onStart={async (url, relay) => { await api.syncImage(31, "n3", "url", { archive_url: url, relay }); }} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(screen.getByRole("textbox", { name: zhImages.import.url.label }), "http://archive.internal/image.tar");
  await screen.findByRole("alert");
  await user.click(screen.getByRole("button", { name: /拉\s*取/ }));
  await waitFor(() => assert.ok(stub.calls.some((call) => call.path.endsWith("/sync"))));
  assert.equal(stub.calls.some((call) => call.path.endsWith("/outbound-policy/check") || call.method === "PUT"), false);
});
