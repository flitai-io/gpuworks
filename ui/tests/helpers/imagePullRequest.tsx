// Shared fixture for the image pull request tests (imagePullRequest, imagePullArchive,
// imagePullNodeFile): each test file imports it, so each file keeps its own isolated DOM
// and cleanup hooks in its own test process, and stays within the per-file runtime budget.
//
// What the image page sends when a node pulls an image. A pull is a plain
// request: the platform decides the source through the pull settings, so the
// body names none. Only an archive URL and a manual copy from a node carry
// choices, and registry hosts are never offered.
import assert from "node:assert/strict";
import { test } from "node:test";
import React from "react";
import { dom } from "./dom.ts";
import {
  catalogRoutes,
  communitySeries,
  i18n,
  installFetch as installHTTPFixture,
  json,
  nightlyVersion,
  zhCommon,
  zhImages,
  type Call,
  type Handler,
} from "./imagesHarness.ts";
import { I18nextProvider } from "react-i18next";

export const { MemoryRouter } = await import("react-router-dom");
export const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
export const { default: userEvent } = await import("@testing-library/user-event");
export const { message } = await import("antd");
export const { default: ImagesPage } = await import("../../src/pages/ImagesPage.tsx");

// Archive fixtures model the same signed-in administrator and policy/check
// endpoints as the real source controls. DNS for archive.internal is private;
// 169.254.169.254 is permanently blocked, even when explicitly allow-listed.
export function installFetch(handler: Handler) {
  let policy = { restrict_private_hosts: false, allow_private_hosts: [] as string[], editable: true, revision: "r1" };
  return installHTTPFixture(async (call) => {
    const response = await handler(call);
    if (response) {
      if (call.path === "/api/v1/server/outbound-policy" && response.ok) policy = await response.clone().json();
      return response;
    }
    if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(200, policy);
    if (call.path === "/api/v1/server/outbound-policy/check") {
      const { hosts } = JSON.parse(call.body!);
      return json(200, { items: hosts.map((host: string) => ({ host, status: host === "169.254.169.254" ? "blocked"
        : policy.restrict_private_hosts && host === "archive.internal" && !policy.allow_private_hosts.includes(host) ? "needs_allow" : "allowed" })), total: hosts.length });
    }
    return undefined;
  });
}

export const z = zhImages;
export const ARCHIVE_URL = "https://containerimages.example/vllm-nightly.tar";

export type NodeRow = Record<string, unknown>;

// The community series' detail, with gpu-03 (n3) in the given state. The
// version declares a registry reference next to its archive address so a
// leaked registry option would show.
export function detailWith(n3: NodeRow, registrySources: string[] = ["quay.io/vllm/vllm-openai:nightly"]) {
  const row = (uuid: string, name: string, extra: NodeRow) => ({ node_uuid: uuid, node_name: name, node_state: "online", ...extra });
  return {
    ...communitySeries,
    versions: [{
      ...nightlyVersion,
      registry_sources: registrySources,
      nodes: [
        row("n1", "gpu-01", { state: "present", image_digest_id: "sha256:0123456789abcdef" }),
        row("n2", "gpu-02", { state: "present", image_digest_id: "sha256:0123456789abcdef" }),
        row("n3", "gpu-03", n3),
      ],
    }],
  };
}

// Each test file passes the setter of its own `restoreFetch`, so its afterEach
// hook restores the fetch this page installed.
export const makeRenderPage = (setRestore: (restore: () => void) => void) =>
  function renderPage(n3: NodeRow, ...handlers: Handler[]) {
  const detail = detailWith(n3);
  const stub = installFetch(async (call) => {
    for (const handler of handlers) {
      const res = await handler(call);
      if (res) return res;
    }
    if (call.method === "GET" && call.path === "/api/v1/images/3") return json(200, detail);
    return catalogRoutes(call);
  });
  setRestore(stub.restore);
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/images"]}><ImagesPage /></MemoryRouter></I18nextProvider>);
  return { calls: stub.calls, user: userEvent.setup({ document: dom.window.document }) };
  };

export async function openNightly(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("tab", { name: "社区镜像" }));
  const row = await waitFor(() => {
    const button = screen.getByRole("button", { name: communitySeries.name });
    assert.ok(!button.closest(".ant-spin-blur"), "the series list is still loading");
    return button;
  });
  await user.click(row);
  const drawer = (await screen.findByRole("dialog", { name: new RegExp(communitySeries.name) }));
  await user.click(await within(drawer).findByRole("link", { name: nightlyVersion.image_ref }));
  const node = await within(drawer).findByText("gpu-03");
  return { drawer, row: node.closest("tr") as HTMLElement };
}

// antd puts a space between the two characters of a Chinese button label.
export const spaced = (label: string) => (label.length === 2 ? new RegExp(`^${label.split("").join(" ?")}$`) : label);
export const PULL = spaced(z.nodeTable.pull);

export const posts = (calls: Call[], suffix: string) =>
  calls.filter((call) => call.method === "POST" && call.path.endsWith(suffix));

export const ACCEPTED = (call: Call) =>
  call.method === "POST" && /\/api\/v1\/images\/31\/nodes\/n3\/sync(\/retry)?$/.test(call.path)
    ? json(202, { state: "pulling", transfer_id: "t-1" })
    : undefined;

export { assert, test, I18nextProvider, React, dom, catalogRoutes, communitySeries, i18n, json, nightlyVersion, zhCommon, zhImages };
export type { Call, Handler };
