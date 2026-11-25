import { afterEach } from "node:test";
import { cleanup, fireEvent, render, screen, waitFor, within, userEvent, installFetch, z, openNightly, PULL, posts, ACCEPTED, assert, test, I18nextProvider, dom, i18n, json, nightlyVersion, zhCommon, React, makeRenderPage, message } from "./helpers/imagePullRequest.tsx";
// What the image page sends when a node pulls an image, part three: the node archive dialog
// and a node file import. The shared fixture is in helpers/imagePullRequest.tsx.

let restoreFetch = () => {};
afterEach(() => {
  cleanup();
  message.destroy();
  restoreFetch();
});
const renderPage = makeRenderPage((restore) => { restoreFetch = restore; });

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

test("replacing a present node image requires an absolute path and sends force", async () => {
  const { calls, user } = renderPage({ state: "present" }, ACCEPTED);
  const { row } = await openNightly(user);
  await user.click(within(row).getByRole("button", { name: z.nodeTable.expand }));
  await user.click(await screen.findByRole("button", { name: z.nodeTable.localDialog.replace }));
  const dialog = within(await screen.findByRole("dialog", { name: z.nodeTable.localDialog.title.replace("{{node}}", "gpu-03") }));
  const input = dialog.getByRole("textbox", { name: z.nodeTable.localDialog.path });
  const confirm = dialog.getByRole("button", { name: z.nodeTable.localDialog.replace }) as HTMLButtonElement;
  await user.type(input, "image.tar");
  assert.equal(confirm.disabled, true);
  await dialog.findByText(z.import.local.pathRequired);
  await user.clear(input);
  await user.type(input, "/data/new.tar");
  await user.click(confirm);
  await waitFor(() => assert.deepEqual(posts(calls, "/nodes/n3/sync").map(call => JSON.parse(call.body!)), [
    { source: "local", archive_path: "/data/new.tar", force: true },
  ]));
});

test("a failed local import reopens the file dialog and keeps the server error visible", async () => {
  const { calls, user } = renderPage({ state: "failed", source: "local" }, (call) =>
    call.method === "POST" && call.path.endsWith("/nodes/n3/sync/retry")
      ? json(409, { code: "IMAGE_ARCHIVE_LOAD_FAILED", params: { reason: "not_regular" } }) : undefined);
  const { row } = await openNightly(user);
  await user.click(within(row).getByRole("button", { name: z.nodeTable.localAction }));
  const dialog = await screen.findByRole("dialog", { name: z.nodeTable.localDialog.title.replace("{{node}}", "gpu-03") });
  await user.type(within(dialog).getByRole("textbox", { name: z.nodeTable.localDialog.path }), "/data/images");
  await user.click(within(dialog).getByRole("button", { name: z.nodeTable.localDialog.start }));
  await within(dialog).findByText(z.archivePreviewReason.not_regular);
  assert.equal((within(dialog).getByRole("textbox", { name: z.nodeTable.localDialog.path }) as HTMLInputElement).value, "/data/images");
  assert.deepEqual(posts(calls, "/sync/retry").map(call => JSON.parse(call.body!)), [
    { source: "local", archive_path: "/data/images" },
  ]);
  assert.equal(posts(calls, "/sync").length, 0);
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

test("a node file dialog rejects whitespace and can be cancelled without starting an import", async () => {
  const { calls, user } = renderPage({ state: "failed", source: "local" }, ACCEPTED);
  const { row } = await openNightly(user);
  await user.click(within(row).getByRole("button", { name: z.nodeTable.localAction }));
  const dialog = await screen.findByRole("dialog", { name: z.nodeTable.localDialog.title.replace("{{node}}", "gpu-03") });
  await user.type(within(dialog).getByRole("textbox", { name: z.nodeTable.localDialog.path }), "   ");
  assert.equal((within(dialog).getByRole("button", { name: z.nodeTable.localDialog.start }) as HTMLButtonElement).disabled, true);
  await user.keyboard("{Enter}");
  await user.click(within(dialog).getByRole("button", { name: z.common.cancel }));
  await waitFor(() => assert.equal(dialog.isConnected, false));
  assert.equal(posts(calls, "/sync").length + posts(calls, "/sync/retry").length, 0);
});

test("a pending node file import blocks duplicate submits, edits and dismissal", async () => {
  let resolve!: (response: Response) => void;
  const pending = new Promise<Response>((done) => { resolve = done; });
  const { calls, user } = renderPage({ state: "failed", source: "local" }, (call) =>
    call.method === "POST" && call.path.endsWith("/nodes/n3/sync/retry") ? pending : undefined);
  const { row } = await openNightly(user);
  await user.click(within(row).getByRole("button", { name: z.nodeTable.localAction }));
  const dialog = await screen.findByRole("dialog", { name: z.nodeTable.localDialog.title.replace("{{node}}", "gpu-03") });
  const input = within(dialog).getByRole("textbox", { name: z.nodeTable.localDialog.path }) as HTMLInputElement;
  const start = within(dialog).getByRole("button", { name: z.nodeTable.localDialog.start }) as HTMLButtonElement;
  await user.type(input, "/data/runner.tar");
  try {
    await user.dblClick(start);
    await waitFor(() => assert.equal(posts(calls, "/sync/retry").length, 1));
    assert.equal(start.disabled, true);
    assert.equal(input.disabled, true);
    assert.equal((within(dialog).getByRole("button", { name: z.common.cancel }) as HTMLButtonElement).disabled, true);
    assert.equal((within(dialog).getByRole("button", { name: zhCommon.actions.close }) as HTMLButtonElement).disabled, true);
    await user.keyboard("{Escape}");
    assert.equal(dialog.isConnected, true);
    assert.equal(posts(calls, "/sync/retry").length, 1);
  } finally {
    resolve(json(202, { state: "syncing", transfer_id: "local-1" }));
  }
  await waitFor(() => assert.equal(dialog.isConnected, false));
});
