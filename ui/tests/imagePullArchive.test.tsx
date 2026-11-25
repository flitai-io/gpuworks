import { afterEach } from "node:test";
import { fireEvent, render, screen, waitFor, within, userEvent, installFetch, z, ARCHIVE_URL, openNightly, spaced, PULL, posts, ACCEPTED, assert, test, I18nextProvider, dom, i18n, json, nightlyVersion, zhCommon, React, makeRenderPage, cleanup, message } from "./helpers/imagePullRequest.tsx";
// What the image page sends when a node pulls an image, part two: an archive URL import, a
// manual copy from a node, and the outbound policy and upstream errors an archive address
// raises. The shared fixture is in helpers/imagePullRequest.tsx.

let restoreFetch = () => {};
afterEach(() => {
  cleanup();
  message.destroy();
  restoreFetch();
});
const renderPage = makeRenderPage((restore) => { restoreFetch = restore; });

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
