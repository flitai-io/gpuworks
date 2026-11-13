// What the image page sends when a node pulls an image. A pull is a plain
// request: the platform decides the source through the pull settings, so the
// body names none. Only an archive URL and a manual copy from a node carry
// choices, and registry hosts are never offered.
import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { dom } from "./helpers/dom.ts";
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
} from "./helpers/imagesHarness.ts";
import { I18nextProvider } from "react-i18next";

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { message } = await import("antd");
const { default: ImagesPage } = await import("../src/pages/ImagesPage.tsx");

// Archive fixtures model the same signed-in administrator and policy/check
// endpoints as the real source controls. DNS for archive.internal is private;
// 169.254.169.254 is permanently blocked, even when explicitly allow-listed.
function installFetch(handler: Handler) {
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

const z = zhImages;
const ARCHIVE_URL = "https://containerimages.example/vllm-nightly.tar";
let restoreFetch = () => {};
afterEach(() => {
  cleanup();
  message.destroy();
  restoreFetch();
});

type NodeRow = Record<string, unknown>;

// The community series' detail, with gpu-03 (n3) in the given state. The
// version declares a registry reference next to its archive address so a
// leaked registry option would show.
function detailWith(n3: NodeRow, registrySources: string[] = ["quay.io/vllm/vllm-openai:nightly"]) {
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
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/images"]}><ImagesPage /></MemoryRouter></I18nextProvider>);
  return { calls: stub.calls, user: userEvent.setup({ document: dom.window.document }) };
}

async function openNightly(user: ReturnType<typeof userEvent.setup>) {
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
const spaced = (label: string) => (label.length === 2 ? new RegExp(`^${label.split("").join(" ?")}$`) : label);
const PULL = spaced(z.nodeTable.pull);

const posts = (calls: Call[], suffix: string) =>
  calls.filter((call) => call.method === "POST" && call.path.endsWith(suffix));

const ACCEPTED = (call: Call) =>
  call.method === "POST" && /\/api\/v1\/images\/31\/nodes\/n3\/sync(\/retry)?$/.test(call.path)
    ? json(202, { state: "pulling", transfer_id: "t-1" })
    : undefined;

test("pulling to a node sends an empty request and never reads the registry settings", async () => {
  const { calls, user } = renderPage({ state: "absent" }, ACCEPTED);
  const { row } = await openNightly(user);
  await user.click(within(row).getByRole("button", { name: PULL }));
  await waitFor(() => assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync").length, 1));
  assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync")[0].body, "{}", "no source, no registry: the platform decides");
  assert.equal(calls.filter((call) => call.path === "/api/v1/settings").length, 0, "the page no longer reads registry settings for a default source");
  await waitFor(() => assert.ok(screen.queryAllByText(z.action.pullSubmitted).length > 0, "the success toast"));
});

test("the pull action works from the keyboard", async () => {
  const { calls, user } = renderPage({ state: "absent" }, ACCEPTED);
  const { row } = await openNightly(user);
  const pull = within(row).getByRole("button", { name: PULL });
  pull.focus();
  assert.equal(document.activeElement === pull, true, "the pull action is focusable");
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync").length, 1));
  assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync")[0].body, "{}");
});

test("retrying a failed node sends an empty request too", async () => {
  const { calls, user } = renderPage({ state: "failed", message: "pull timed out" }, ACCEPTED);
  const { row } = await openNightly(user);
  await user.click(within(row).getByRole("button", { name: PULL }));
  await waitFor(() => assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync/retry").length, 1));
  assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync/retry")[0].body, "{}", "retry names no source");
  assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync").length, 0);
});

test("the source picker offers automatic, a node copy, a URL and a node file, and no registry host", async () => {
  const { user } = renderPage({ state: "absent" });
  const { row } = await openNightly(user);
  const picker = within(row).getByRole("combobox", { name: z.nodeTable.column.source });
  assert.ok(within(row).getByText(z.nodeTable.sourceAuto), "automatic is the default");
  fireEvent.mouseDown(picker);
  await waitFor(() => assert.ok(document.querySelectorAll(".ant-select-item-option-content").length >= 3));
  const labels = [...document.querySelectorAll(".ant-select-item-option-content")].map((node) => node.textContent);
  assert.deepEqual(labels, [z.nodeTable.sourceAuto, z.nodeTable.peerOption, z.nodeTable.sourceFromURL, z.nodeTable.localOption]);
  const shown = labels.join(" ");
  for (const host of ["Quay", "quay.io", "Huawei", "华为云", "Docker Hub", "docker.io"]) {
    assert.equal(shown.includes(host), false, `${host} is not an option`);
  }
});

test("a stale client's removed-registry error is shown in the user's language", async () => {
  const { user } = renderPage({ state: "absent" }, (call) =>
    call.method === "POST" && call.path === "/api/v1/images/31/nodes/n3/sync"
      ? json(400, {
        error: "image source \"quay\" can no longer be named",
        code: "IMAGE_SOURCE_REMOVED",
        params: { source: "quay", route: "/settings" },
      })
      : undefined);
  const { row } = await openNightly(user);
  await user.click(within(row).getByRole("button", { name: PULL }));
  const text = zhCommon.apiError.IMAGE_SOURCE_REMOVED.replace("{{source}}", "quay");
  await waitFor(() => assert.ok(screen.queryAllByText(text).length > 0, "the localized text, not the English fallback"));
  assert.equal(screen.queryAllByText(/can no longer be named/).length, 0);
  assert.ok(screen.getByText(z.page.operationNotice), "the notice stays on the page until dismissed");
});

test("a pull failure without a product code still shows the server's message", async () => {
  const { user } = renderPage({ state: "absent" }, (call) =>
    call.method === "POST" && call.path === "/api/v1/images/31/nodes/n3/sync" ? json(500, { error: "disk full on gpu-03" }) : undefined);
  const { row } = await openNightly(user);
  await user.click(within(row).getByRole("button", { name: PULL }));
  await waitFor(() => assert.ok(screen.queryAllByText("disk full on gpu-03").length > 0));
});

test("the node table says the source is decided by the pull settings and links to them", async () => {
  const { user } = renderPage({ state: "absent" });
  const { drawer } = await openNightly(user);
  assert.ok(within(drawer).getByText(z.nodeTable.autoNote, { exact: false }));
  const link = within(drawer).getByRole("link", { name: z.nodeTable.autoNoteLink });
  assert.equal(link.getAttribute("href"), "/settings");
});

test("an archive URL import still names the url source with its relay choice", async () => {
  const { calls, user } = renderPage({ state: "absent" }, (call) => {
    if (call.method === "POST" && call.path === "/api/v1/images/preview-archive-url") {
      return json(200, { url: ARCHIVE_URL, size_bytes: 1024 * 1024, repo_tags: [nightlyVersion.image_ref], previewable: true });
    }
    if (call.method === "PATCH" && call.path === "/api/v1/images/versions/31") return json(200, { ...nightlyVersion, registry_sources: [ARCHIVE_URL] });
    return ACCEPTED(call);
  });
  const { row } = await openNightly(user);
  fireEvent.mouseDown(within(row).getByRole("combobox", { name: z.nodeTable.column.source }));
  await user.click(await screen.findByText(z.nodeTable.sourceFromURL, { selector: ".ant-select-item-option-content" }));
  await user.click(within(row).getByRole("button", { name: z.nodeTable.pullFromURL }));
  await user.type(await screen.findByLabelText(z.import.url.label), ARCHIVE_URL);
  await user.click(screen.getByRole("button", { name: spaced(z.import.url.resolve) }));
  await screen.findByText("1.0 MiB");
  await user.click(screen.getByRole("checkbox", { name: z.nodeTable.relay }));
  await user.click(screen.getByRole("button", { name: PULL }));
  await waitFor(() => assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync").length, 1));
  assert.deepEqual(JSON.parse(posts(calls, "/api/v1/images/31/nodes/n3/sync")[0].body ?? ""), {
    source: "url",
    archive_url: ARCHIVE_URL,
    relay: true,
  });
  assert.ok(calls.some((call) => call.method === "PATCH" && call.path === "/api/v1/images/versions/31"), "the address is recorded on the image first");
});

test("a manual copy from a node still names source_node and its rate", async () => {
  const { calls, user } = renderPage({ state: "absent" }, (call) =>
    call.method === "POST" && call.path === "/api/v1/images/31/nodes/n3/sync/probe" ? json(200, { state: "ready" }) : ACCEPTED(call));
  const { row } = await openNightly(user);
  fireEvent.mouseDown(within(row).getByRole("combobox", { name: z.nodeTable.column.source }));
  await user.click(await screen.findByText(z.nodeTable.peerOption, { selector: ".ant-select-item-option-content" }));
  await user.click(within(row).getByRole("button", { name: z.nodeTable.peerAction }));
  const dialog = z.nodeTable.peerDialog;
  await screen.findByText(dialog.description);
  fireEvent.mouseDown(screen.getByRole("combobox", { name: dialog.source }));
  await waitFor(() => assert.ok(screen.getAllByText("gpu-01").length > 1));
  await user.click(screen.getAllByText("gpu-01").at(-1)!);
  await user.click(screen.getByRole("button", { name: z.nodeTable.testTransport }));
  await screen.findByText(dialog.probeOk);
  await user.click(screen.getByRole("button", { name: dialog.start }));
  await waitFor(() => assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync").length, 1));
  assert.deepEqual(JSON.parse(posts(calls, "/api/v1/images/31/nodes/n3/sync")[0].body ?? ""), { source_node: "n1", rate_limit_kib_s: 0 });
});


test("a checked archive address writes an approvable host during resolve", async () => {
  const { default: ImageArchiveURLImport } = await import("../src/pages/images/ImageArchiveURLImport.tsx");
  let allowed = false;
  let writes = 0;
  const stub = installFetch((call) => {
    if (call.method === "POST" && call.path === "/api/v1/images/preview-archive-url") {
      if (!allowed) return json(403, { code: "IMAGE_ARCHIVE_URL_POLICY_DENIED", error: "RAW https://archive.internal/image.tar?token=private-query", params: { host: "archive.internal:443", allow_host: "archive.internal", approvable: true, config_key: "outbound_allow_private_hosts" } });
      return json(200, { url: "http://archive.internal/image.tar", size_bytes: 2048, repo_tags: ["example/app:v1"], previewable: true });
    }
    if (call.method === "GET" && call.path === "/api/v1/server/outbound-policy") {
      return json(200, { restrict_private_hosts: true, allow_private_hosts: allowed ? ["10.0.0.8", "archive.internal"] : ["10.0.0.8"], editable: true, revision: allowed ? "r2" : "r1" });
    }
    if (call.method === "PUT" && call.path === "/api/v1/server/outbound-policy") {
      writes += 1;
      assert.equal(call.body, JSON.stringify({ restrict_private_hosts: true, allow_private_hosts: ["10.0.0.8", "archive.internal"], revision: "r1" }));
      allowed = true;
      return json(200, { restrict_private_hosts: true, allow_private_hosts: ["10.0.0.8", "archive.internal"], editable: true, revision: "r2" });
    }
    return undefined;
  });
  restoreFetch = stub.restore;
  const prefilled: string[] = [];
  render(<I18nextProvider i18n={i18n}><ImageArchiveURLImport versions={[]} onMatched={() => assert.fail("a new reference must prefill")} onPrefill={(preview) => prefilled.push(preview.repo_tags[0])} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(screen.getByLabelText(z.import.url.label), "http://archive.internal/image.tar");
  const allow = await screen.findByRole("checkbox", { name: "自动加入 archive.internal 的白名单" });
  await user.click(screen.getByRole("button", { name: spaced(z.import.url.resolve) }));
  const alert = await screen.findByRole("alert");
  assert.match(alert.textContent ?? "", /archive\.internal:443 需要允许后才能访问/);
  assert.equal(alert.textContent?.includes("白名单"), false);
  assert.equal(screen.queryByText(/RAW|private-query|server\.yaml|outbound_allow_private_hosts/), null);
  assert.equal(writes, 0, "an unchecked address is not written");
  allow.focus();
  await user.keyboard("[Space]");
  assert.equal(allow.getAttribute("aria-checked") ?? String((allow as HTMLInputElement).checked), "true", "the opt-in can be checked from the keyboard");
  await user.click(screen.getByRole("button", { name: /解\s*析/ }));
  await screen.findByText("example/app:v1");
  assert.equal(writes, 1);
  assert.deepEqual(prefilled, ["example/app:v1"]);
});

test("an unrestricted outbound policy hides the allow checkbox and does not write", async () => {
  const { default: ImageArchiveURLImport } = await import("../src/pages/images/ImageArchiveURLImport.tsx");
  let previews = 0;
  const stub = installFetch((call) => {
    if (call.method === "GET" && call.path === "/api/v1/server/outbound-policy") {
      return json(200, { restrict_private_hosts: false, allow_private_hosts: ["10.0.0.8"], editable: true, revision: "r1" });
    }
    if (call.method === "POST" && call.path === "/api/v1/images/preview-archive-url") {
      previews += 1;
      return json(200, { url: "http://archive.internal/image.tar", size_bytes: 2048, repo_tags: ["example/app:v1"], previewable: true });
    }
    return undefined;
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><ImageArchiveURLImport versions={[]} onMatched={() => {}} onPrefill={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(screen.getByLabelText(z.import.url.label), "http://archive.internal/image.tar");
  await user.click(screen.getByRole("button", { name: spaced(z.import.url.resolve) }));
  await screen.findByText("example/app:v1");
  await waitFor(() => assert.ok(stub.calls.some((call) => call.method === "GET" && call.path === "/api/v1/server/outbound-policy")));
  assert.equal(screen.queryByRole("checkbox", { name: "自动加入 archive.internal 的白名单" }), null);
  assert.equal(stub.calls.some((call) => call.method === "PUT"), false);
  assert.equal(previews, 1);
});

test("a reserved archive address explains that the allow list cannot permit it", async () => {
  const { default: ImageArchiveURLImport } = await import("../src/pages/images/ImageArchiveURLImport.tsx");
  const stub = installFetch((call) => {
    if (call.method === "GET" && call.path === "/api/v1/server/outbound-policy") {
      return json(200, { restrict_private_hosts: true, allow_private_hosts: [], editable: true, revision: "r1" });
    }
    return call.method === "POST" && call.path === "/api/v1/images/preview-archive-url"
      ? json(403, { code: "IMAGE_ARCHIVE_URL_POLICY_BLOCKED", error: "RAW http://169.254.169.254/latest", params: { host: "169.254.169.254" } })
      : undefined;
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><ImageArchiveURLImport versions={[]} onMatched={() => {}} onPrefill={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(screen.getByLabelText(z.import.url.label), "http://169.254.169.254/latest");
  await screen.findByText(zhCommon.outboundAccess.blocked.replace("{{host}}", "169.254.169.254"));
  assert.equal(screen.queryByRole("checkbox", { name: /169\.254\.169\.254/ }), null);
  await user.click(screen.getByRole("button", { name: spaced(z.import.url.resolve) }));
  const alert = await screen.findByRole("alert");
  assert.match(alert.textContent ?? "", /169\.254\.169\.254 不能通过允许列表访问/);
  assert.equal(stub.calls.some((call) => call.method === "PUT"), false);
  assert.equal(screen.queryByText(/RAW|latest/), null);
});

test("an upstream archive status names the HTTP status without the source URL", async () => {
  const { default: ImageArchiveURLImport } = await import("../src/pages/images/ImageArchiveURLImport.tsx");
  const stub = installFetch((call) => {
    if (call.method === "GET" && call.path === "/api/v1/server/outbound-policy") {
      return json(200, { restrict_private_hosts: true, allow_private_hosts: [], editable: true, revision: "r1" });
    }
    return call.method === "POST" && call.path === "/api/v1/images/preview-archive-url"
      ? json(502, { code: "IMAGE_ARCHIVE_URL_UPSTREAM_STATUS", error: "RAW https://archive.internal/missing.tar?token=private-query", params: { host: "archive.internal", upstream_status: 404 } })
      : undefined;
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><ImageArchiveURLImport versions={[]} onMatched={() => {}} onPrefill={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(screen.getByLabelText(z.import.url.label), "http://archive.internal/missing.tar");
  await user.click(screen.getByRole("button", { name: spaced(z.import.url.resolve) }));
  const alert = await screen.findByRole("alert");
  assert.match(alert.textContent ?? "", /archive\.internal 返回了 HTTP 404/);
  assert.match(alert.textContent ?? "", /HTTP 502 · IMAGE_ARCHIVE_URL_UPSTREAM_STATUS/);
  assert.ok(await screen.findByRole("checkbox", { name: "自动加入 archive.internal 的白名单" }));
  assert.equal(screen.queryByText(/private-query|missing\.tar|无法读取/), null);
});


test("an archive import hides unstructured upstream error text", async () => {
  const { default: ImageArchiveURLImport } = await import("../src/pages/images/ImageArchiveURLImport.tsx");
  const stub = installFetch((call) => call.method === "POST" && call.path === "/api/v1/images/preview-archive-url"
    ? json(502, { error: "RAW https://archive.internal/image.tar?token=private-query" }) : undefined);
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><ImageArchiveURLImport versions={[]} onMatched={() => {}} onPrefill={() => {}} /></I18nextProvider>);
  const user = userEvent.setup({ document: dom.window.document });
  await user.type(screen.getByLabelText(z.import.url.label), "http://archive.internal/image.tar");
  await user.click(screen.getByRole("button", { name: spaced(z.import.url.resolve) }));
  const alert = await screen.findByRole("alert");
  assert.equal(alert.textContent, "无法读取该镜像归档。");
  assert.equal(screen.queryByText(/RAW|private-query/), null);
});


test("the node archive dialog retains a policy denial code while allowing node-direct acquisition", async () => {
  const { default: ImageArchiveURLDialog } = await import("../src/pages/images/ImageArchiveURLDialog.tsx");
  const stub = installFetch((call) => {
    if (call.method === "GET" && call.path === "/api/v1/server/outbound-policy") {
      return json(200, { restrict_private_hosts: true, allow_private_hosts: [], editable: true, revision: "r1" });
    }
    return call.method === "POST" && call.path === "/api/v1/images/preview-archive-url"
      ? json(403, { code: "IMAGE_ARCHIVE_URL_POLICY_DENIED", error: "RAW https://archive.internal/image.tar?token=private-query", params: { host: "archive.internal", allow_host: "archive.internal", approvable: true, config_key: "outbound_allow_private_hosts" } })
      : undefined;
  });
  restoreFetch = stub.restore;
  const starts: Array<{ url: string; relay: boolean }> = [];
  render(<I18nextProvider i18n={i18n}><ImageArchiveURLDialog open version={nightlyVersion as never} nodeLabel="gpu-03" initialURL="http://archive.internal/image.tar" onCancel={() => {}} onStart={async (url, relay) => { starts.push({ url, relay }); }} /></I18nextProvider>);
  const alert = await screen.findByRole("alert");
  assert.match(alert.textContent ?? "", /archive\.internal 需要允许后才能访问/);
  assert.match(alert.textContent ?? "", /HTTP 403 · IMAGE_ARCHIVE_URL_POLICY_DENIED/);
  assert.equal(alert.textContent?.includes("白名单"), false);
  assert.equal(screen.queryByText(/RAW|private-query|server\.yaml|outbound_allow_private_hosts/), null);
  await screen.findByRole("checkbox", { name: "自动加入 archive.internal 的白名单" });
  const pull = screen.getByRole("button", { name: PULL }) as HTMLButtonElement;
  assert.equal(pull.disabled, false, "management-node policy does not describe the node's direct route");
  const user = userEvent.setup({ document: dom.window.document });
  await user.click(pull);
  await waitFor(() => assert.deepEqual(starts, [{ url: "http://archive.internal/image.tar", relay: false }]));
});


for (const entry of ["import", "dialog"] as const) {
  test(`the ${entry} archive policy guidance is localized in English`, async () => {
    const fs = await import("node:fs");
    for (const ns of ["images", "common"]) {
      const resource = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/en/${ns}.json`, import.meta.url), "utf8"));
      i18n.addResourceBundle("en", ns, resource, true, true);
    }
    await i18n.changeLanguage("en");
    try {
      const stub = installFetch((call) => {
        if (call.method === "GET" && call.path === "/api/v1/server/outbound-policy") {
          return json(200, { restrict_private_hosts: true, allow_private_hosts: [], editable: true, revision: "r1" });
        }
        return call.method === "POST" && call.path === "/api/v1/images/preview-archive-url"
          ? json(403, { code: "IMAGE_ARCHIVE_URL_POLICY_DENIED", error: "RAW https://archive.internal/image.tar?token=private-query", params: { host: "archive.internal", allow_host: "archive.internal", approvable: true, config_key: "outbound_allow_private_hosts" } })
          : undefined;
      });
      restoreFetch = stub.restore;
      if (entry === "import") {
        const { default: Import } = await import("../src/pages/images/ImageArchiveURLImport.tsx");
        render(<I18nextProvider i18n={i18n}><Import versions={[]} onMatched={() => assert.fail("a denied source must not resolve")} onPrefill={() => assert.fail("a denied source must not prefill")} /></I18nextProvider>);
        const user = userEvent.setup({ document: dom.window.document });
        await user.type(screen.getByLabelText("Image archive URL"), "http://archive.internal/image.tar");
        await user.click(screen.getByRole("button", { name: "Resolve" }));
      } else {
        const { default: Dialog } = await import("../src/pages/images/ImageArchiveURLDialog.tsx");
        render(<I18nextProvider i18n={i18n}><Dialog open version={nightlyVersion as never} nodeLabel="gpu-03" initialURL="http://archive.internal/image.tar" onCancel={() => {}} onStart={async () => {}} /></I18nextProvider>);
      }
      const alert = await screen.findByRole("alert");
      assert.match(alert.textContent ?? "", /archive\.internal has to be allowed before it can be used/);
      assert.match(alert.textContent ?? "", /HTTP 403 · IMAGE_ARCHIVE_URL_POLICY_DENIED/);
      await screen.findByRole("checkbox", { name: "Automatically add archive.internal to the allowlist" });
      assert.equal(screen.queryByText(/RAW|private-query|server\.yaml|outbound_allow_private_hosts/), null);
    } finally {
      cleanup();
      await i18n.changeLanguage("zh");
    }
  });
}


test("the node archive dialog hides unstructured upstream error text", async () => {
  const { default: Dialog } = await import("../src/pages/images/ImageArchiveURLDialog.tsx");
  const stub = installFetch((call) => call.method === "POST" && call.path === "/api/v1/images/preview-archive-url"
    ? json(502, { error: "RAW https://archive.internal/image.tar?token=private-query" }) : undefined);
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><Dialog open version={nightlyVersion as never} nodeLabel="gpu-03" initialURL="http://archive.internal/image.tar" onCancel={() => {}} onStart={async () => {}} /></I18nextProvider>);
  const alert = await screen.findByRole("alert");
  assert.equal(alert.textContent, "无法读取该镜像归档。");
  assert.equal(screen.queryByText(/RAW|private-query/), null);
});

test("a node file import sends the path to that node without changing the catalog", async () => {
  const { calls, user } = renderPage({ state: "absent" }, ACCEPTED);
  const { row } = await openNightly(user);
  fireEvent.mouseDown(within(row).getByRole("combobox", { name: z.nodeTable.column.source }));
  await user.click(await screen.findByText(z.nodeTable.localOption));
  await user.click(within(row).getByRole("button", { name: z.nodeTable.localAction }));
  const dialog = await screen.findByRole("dialog", { name: z.nodeTable.localDialog.title.replace("{{node}}", "gpu-03") });
  const start = within(dialog).getByRole("button", { name: z.nodeTable.localDialog.start }) as HTMLButtonElement;
  assert.equal(start.disabled, true, "a path is required before submission");
  await user.type(within(dialog).getByRole("textbox", { name: z.nodeTable.localDialog.path }), "  /data/image with spaces.tar  ");
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync").length, 1));
  assert.deepEqual(JSON.parse(posts(calls, "/api/v1/images/31/nodes/n3/sync")[0].body!), {
    source: "local", archive_path: "/data/image with spaces.tar",
  });
  assert.equal(calls.some((call) => call.method === "PATCH" || call.path.endsWith("/images/import") || call.path.includes("preview-archive")), false,
    "a node-local path must not become a shared acquisition address or create a series");
  await waitFor(() => assert.equal(screen.queryByRole("dialog", { name: z.nodeTable.localDialog.title.replace("{{node}}", "gpu-03") }), null));
});

test("a failed local import reopens the file dialog and keeps the server error visible", async () => {
  const { calls, user } = renderPage({ state: "failed", source: "local" }, (call) =>
    call.method === "POST" && call.path.endsWith("/nodes/n3/sync")
      ? json(409, { error: "archive_path must name a regular file" }) : undefined);
  const { row } = await openNightly(user);
  await user.click(within(row).getByRole("button", { name: z.nodeTable.localAction }));
  const dialog = await screen.findByRole("dialog", { name: z.nodeTable.localDialog.title.replace("{{node}}", "gpu-03") });
  await user.type(within(dialog).getByRole("textbox", { name: z.nodeTable.localDialog.path }), "/data/images");
  await user.click(within(dialog).getByRole("button", { name: z.nodeTable.localDialog.start }));
  await within(dialog).findByText("archive_path must name a regular file");
  assert.equal((within(dialog).getByRole("textbox", { name: z.nodeTable.localDialog.path }) as HTMLInputElement).value, "/data/images");
  assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync").length, 1);
  assert.equal(posts(calls, "/sync/retry").length, 0, "local failures must not fall back to a registry pull");
});

test("a failed node file import can switch explicitly to automatic pulling", async () => {
  const { calls, user } = renderPage({ state: "failed", source: "local" }, ACCEPTED);
  const { row } = await openNightly(user);
  fireEvent.mouseDown(within(row).getByRole("combobox", { name: z.nodeTable.column.source }));
  await user.click(await screen.findByText(z.nodeTable.sourceAuto));
  await user.click(within(row).getByRole("button", { name: PULL }));
  await waitFor(() => assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync").length, 1));
  assert.equal(posts(calls, "/api/v1/images/31/nodes/n3/sync")[0].body, "{}");
  assert.equal(posts(calls, "/sync/retry").length, 0, "changing the source starts a fresh pull");
});
