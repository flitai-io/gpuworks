import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { dom } from "./helpers/dom.ts";
import {
  catalogRoutes,
  communitySeries,
  i18n,
  installFetch,
  json,
  legacySeries,
  legacyVersion,
  nightlyVersion,
  seriesDetails,
  systemSeries,
  systemVersion,
  zhCommon,
  zhImages,
  type Handler,
} from "./helpers/imagesHarness.ts";
import { I18nextProvider } from "react-i18next";

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { message } = await import("antd");
const { default: ImagesPage } = await import("../src/pages/ImagesPage.tsx");

const z = zhImages;
let restoreFetch = () => {};
afterEach(() => {
  cleanup();
  message.destroy();
  restoreFetch();
});

function renderPage(...handlers: Handler[]) {
  const stub = installFetch(async (call) => {
    for (const handler of handlers) {
      const res = await handler(call);
      if (res) return res;
    }
    return catalogRoutes(call);
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/images"]}><ImagesPage /></MemoryRouter></I18nextProvider>);
  return { calls: stub.calls, user: userEvent.setup({ document: dom.window.document }) };
}

const SYNC_RESULT = {
  catalog_version: "2026-10-08.1a2b3c4d", created: 3, updated: 22, deleted: 71, deprecated: 1, skipped: [],
  series: [systemSeries],
};
const RESULT_TOAST = "新增 3 · 更新 22 · 删除 71 · 转为旧版本 1";

// antd keeps closing toasts in the DOM under jsdom (no transition end), so a
// toast is proven by the count of its text going up, not by its presence.
async function expectNewToast(text: string, action: () => Promise<void>) {
  const before = screen.queryAllByText(text).length;
  await action();
  await waitFor(() => assert.ok(screen.queryAllByText(text).length > before, `toast ${text}`));
}

async function openSeries(user: ReturnType<typeof userEvent.setup>, name: string) {
  // Rows of the first read render a moment before the table's loading veil
  // (pointer-events: none) lifts; a slow run would click into the veil.
  const row = await waitFor(() => {
    const button = screen.getByRole("button", { name });
    assert.ok(!button.closest(".ant-spin-blur"), "the series list is still loading");
    return button;
  });
  await user.click(row);
  return await screen.findByRole("dialog", { name: new RegExp(name) });
}

// A node row's image ID, age and the actions on a held image sit in its
// expanded row; open every row of the tab in view.
async function expandNodeRows(user: ReturnType<typeof userEvent.setup>, drawer: HTMLElement) {
  const pane = await waitFor(() => {
    const active = within(drawer).queryByRole("tabpanel");
    assert.ok(active && within(active).queryAllByRole("button", { name: z.nodeTable.expand }).length > 0, "the node rows are listed");
    return active;
  });
  for (const button of within(pane).getAllByRole("button", { name: z.nodeTable.expand })) await user.click(button);
}

test("the page names three image sources and offers no offline package or manifest URL", async () => {
  renderPage();
  for (const label of ["系统镜像", "社区镜像", "自定义镜像"]) assert.ok(screen.getByRole("tab", { name: label }), label);
  await screen.findByRole("button", { name: systemSeries.name });

  assert.ok(screen.getByRole("button", { name: "检查更新" }));
  assert.ok(screen.getByRole("button", { name: /上传 manifest/ }));
  assert.equal(screen.queryByText(/离线包/) !== null, false, "no offline package banner or count");
  assert.equal(document.querySelector('input[name="image_catalog_manifest_url"]') !== null, false, "no manifest URL input");
  assert.equal(screen.queryAllByRole("textbox").length, 0, "the system tab has nothing to type an address into");
  for (const header of ["用途", "校验"]) {
    assert.equal(screen.queryByRole("columnheader", { name: header }) !== null, false, `no ${header} column`);
  }
  // The shared list's trailing actions column: details, and no delete for a catalog series.
  const row = screen.getByRole("button", { name: systemSeries.name }).closest("tr")!;
  assert.ok(within(row).getByRole("button", { name: zhImages.seriesTable.details }));
  assert.equal(within(row).queryByRole("button", { name: "删除" }), null, "an official series is not deleted from the list");
});

test("the series list shows loading, then its rows; a failed load says so; an empty tab is empty", async () => {
  let release: (() => void) | undefined;
  const held = new Promise<void>((resolve) => { release = resolve; });
  const { user } = renderPage(async (call) => {
    if (call.method === "GET" && call.path === "/api/v1/images") {
      await held;
      return undefined;
    }
    return undefined;
  });
  await waitFor(() => assert.ok(screen.queryByRole("status"), "the table shows it is loading"));
  release?.();
  await screen.findByRole("button", { name: systemSeries.name });

  // The custom tab has no series in this catalog.
  await user.click(screen.getByRole("tab", { name: "自定义镜像" }));
  await waitFor(() => assert.ok(screen.getAllByText(zhCommon.dataTable.empty).length > 0));
  assert.ok(screen.getByRole("button", { name: /添加自定义镜像/ }));
  cleanup();
  restoreFetch();

  renderPage((call) => (call.path === "/api/v1/images" ? json(500, { error: "database is locked" }) : undefined));
  await screen.findByText(z.page.loadFailed);
  assert.ok(screen.getByText("database is locked"));
});

test("check for updates posts no body and reports the four counts", async () => {
  const { calls, user } = renderPage((call) =>
    call.method === "POST" && call.path === "/api/v1/images/sync" ? json(200, SYNC_RESULT) : undefined);
  await screen.findByRole("button", { name: systemSeries.name });
  const listReads = calls.filter((call) => call.path === "/api/v1/images").length;

  // Keyboard: focus the button and press Enter.
  await expectNewToast(RESULT_TOAST, async () => {
    screen.getByRole("button", { name: "检查更新" }).focus();
    await user.keyboard("{Enter}");
  });

  const sync = calls.filter((call) => call.path === "/api/v1/images/sync");
  assert.equal(sync.length, 1);
  assert.equal(sync[0].method, "POST");
  assert.equal(sync[0].body, undefined, "the catalog comes from the distribution channel, not a request field");
  await screen.findByText(z.page.catalog.version.replace("{{version}}", "2026-10-08.1a2b3c4d"));
  await waitFor(() => assert.ok(calls.filter((call) => call.path === "/api/v1/images").length > listReads, "the list is read again"));
});

test("an unreachable distribution channel shows the localized reason", async () => {
  const detail = "no distribution source served a valid nightly index (https://dl.gpuworks.ai: GET https://dl.gpuworks.ai/nightly/index.json: 503 Service Unavailable)";
  const { user } = renderPage((call) =>
    call.path === "/api/v1/images/sync"
      ? json(502, { error: detail, code: "UPDATE_SOURCES_UNAVAILABLE", params: { detail } })
      : undefined);
  await screen.findByRole("button", { name: systemSeries.name });
  await expectNewToast(
    `没有一个发布源返回可用的发布索引：${detail}。没有做任何修改。`,
    () => user.click(screen.getByRole("button", { name: "检查更新" })),
  );
});

test("uploading a manifest posts the JSON as is and shares the result toast; an invalid one is refused", async () => {
  let reply = json(200, SYNC_RESULT);
  const { calls, user } = renderPage((call) =>
    call.method === "POST" && call.path === "/api/v1/images/import" ? reply : undefined);
  await screen.findByRole("button", { name: systemSeries.name });
  const input = document.querySelector('input[type="file"]') as HTMLInputElement;
  assert.equal(input.accept, ".json,application/json", "only JSON manifests are accepted");

  const manifest = JSON.stringify({ schema_version: 3, catalog_version: "2026-10-08.1a2b3c4d", series: [] });
  await expectNewToast(RESULT_TOAST, () => user.upload(input, new File([manifest], "manifest.json", { type: "application/json" })));
  const posted = calls.filter((call) => call.path === "/api/v1/images/import");
  assert.equal(posted.length, 1);
  assert.equal(posted[0].contentType, "application/json");
  assert.equal(posted[0].body, manifest);

  reply = json(400, { error: "invalid manifest", code: "IMAGE_MANIFEST_INVALID", params: { detail: "schema_version 1 is not supported" } });
  // rc-upload replaces its input after every upload.
  const again = await waitFor(() => {
    const next = document.querySelector('input[type="file"]') as HTMLInputElement;
    assert.ok(next && !next.disabled);
    return next;
  });
  await expectNewToast("镜像 manifest 无效：schema_version 1 is not supported", () =>
    user.upload(again, new File(["{\"schema_version\":1}"], "old.json", { type: "application/json" })));
});

test("system images are read-only and an older one shows its references", async () => {
  const { user } = renderPage();
  const drawer = await openSeries(user, legacySeries.name);
  await within(drawer).findByText("旧版本 · 1 个引用");
  assert.equal(within(drawer).queryByRole("button", { name: /删除系列/ }) !== null, false);
  await user.click(within(drawer).getByRole("link", { name: "gpustack/runner:cuda12.9-vllm0.25.1" }));
  // Picking the version opens the node tab on it.
  await waitFor(() => assert.ok(drawer.querySelector('.ant-select-selection-item[title="gpustack/runner:cuda12.9-vllm0.25.1"]')));
  assert.equal(within(drawer).queryByRole("button", { name: /删除版本/ }) !== null, false);
  assert.equal(within(drawer).queryByText(/常用候选|常用参数/) !== null, false, "no series defaults editor");
});

test("a community series is deleted from its detail drawer after confirming", async () => {
  const { calls, user } = renderPage((call) =>
    call.method === "DELETE" && call.path === "/api/v1/image-series/3" ? new Response(null, { status: 204 }) : undefined);
  await user.click(screen.getByRole("tab", { name: "社区镜像" }));
  const drawer = await openSeries(user, communitySeries.name);
  await user.click(within(drawer).getByRole("button", { name: /删除系列/ }));
  const confirm = await waitFor(() => {
    const button = [...document.querySelectorAll(".ant-popover:not(.ant-popover-hidden) button")]
      .find((node) => node.textContent?.replace(/\s/g, "") === "删除");
    assert.ok(button);
    return button as HTMLElement;
  });
  await user.click(confirm);
  await waitFor(() => assert.ok(calls.some((call) => call.method === "DELETE" && call.path === "/api/v1/image-series/3")));
});

test("a rolling tag shows each node's image ID and age, flags differing IDs, and pulls again with force only", async () => {
  const { calls, user } = renderPage((call) =>
    call.method === "POST" && /\/api\/v1\/images\/31\/nodes\/n\d\/sync$/.test(call.path)
      ? json(202, { state: "pulling", transfer_id: "t-2" })
      : undefined);
  await user.click(screen.getByRole("tab", { name: "社区镜像" }));
  const drawer = await openSeries(user, communitySeries.name);
  await within(drawer).findByText("vllm/vllm-openai:nightly：2 个节点的 digest 不一致");
  await user.click(within(drawer).getByRole("link", { name: nightlyVersion.image_ref }));

  await expandNodeRows(user, drawer);
  assert.ok(within(drawer).getByText("0123456789ab"), "12 characters, without the sha256: prefix");
  assert.ok(within(drawer).getByText("fedcba987654"));
  assert.ok(within(drawer).getByText("5 分钟前"));
  assert.ok(within(drawer).getByText("3 小时前"));

  const repull = within(drawer).getAllByRole("button", { name: "重新拉取" });
  assert.equal(repull.length, 2, "only the nodes that hold the image can pull it again");
  const detailReads = calls.filter((call) => call.path === "/api/v1/images/3").length;
  fireEvent.click(repull[0]);
  await waitFor(() => assert.ok(calls.some((call) => call.path === "/api/v1/images/31/nodes/n1/sync")));
  const sent = calls.find((call) => call.path === "/api/v1/images/31/nodes/n1/sync");
  assert.equal(sent?.method, "POST");
  // The server picks the source of a forced re-pull; the page names none.
  assert.equal(sent?.body, '{"force":true}');
  // The node rows come back from the series detail after the action.
  await waitFor(() => assert.ok(calls.filter((call) => call.path === "/api/v1/images/3").length > detailReads, "the detail is read again"));
});

test("an immutable system tag offers no pull again on a node that has it", async () => {
  const { user } = renderPage();
  const drawer = await openSeries(user, systemSeries.name);
  await user.click(within(drawer).getByRole("link", { name: "gpuworks/gpuworks-runner:cuda13.0-vllm0.30.0-r2" }));
  await expandNodeRows(user, drawer);
  await within(drawer).findByText("aaaaaaaaaaaa");
  assert.equal(within(drawer).queryByRole("button", { name: "重新拉取" }) !== null, false);
  assert.equal(within(drawer).queryByText(/digest 不一致/) !== null, false);
});

test("the catalog list carries counts only: version coverage and the summary read present_nodes / total_nodes", async () => {
  renderPage();
  await screen.findByRole("button", { name: systemSeries.name });
  // Distributed: the official tab's versions hold 1 + 0 present copies; the
  // local size is the reported runtime subtotal, independent of archive size.
  const stat = (title: string) =>
    [...document.querySelectorAll(".ant-statistic")].find((node) => node.querySelector(".ant-statistic-title")?.textContent === title)
      ?.querySelector(".ant-statistic-content")?.textContent;
  await waitFor(() => assert.equal(stat(z.page.summary.distributed), "1"));
  assert.equal(stat(z.page.summary.localSize), "2.0 GiB");

  // Hold the detail so the drawer lists the series' versions from the catalog
  // list: the coverage column still reads the counts.
  cleanup();
  restoreFetch();
  let release: (() => void) | undefined;
  const held = new Promise<void>((resolve) => { release = resolve; });
  const { user } = renderPage(async (call) => {
    if (call.path !== "/api/v1/images/1") return undefined;
    await held;
    return json(200, seriesDetails["/api/v1/images/1"]);
  });
  const drawer = await openSeries(user, systemSeries.name);
  const row = await waitFor(() => {
    const found = within(drawer).getByRole("link", { name: systemVersion.image_ref }).closest("tr");
    assert.ok(found);
    return found as HTMLElement;
  });
  assert.ok(within(row).getByText("1 / 3"), "present / total from the counts");
  await user.click(within(drawer).getByRole("link", { name: systemVersion.image_ref }));
  await waitFor(() => assert.ok(drawer.querySelector(".detail-section .ant-spin-spinning"), "the node table waits for the detail"));
  assert.equal(within(drawer).queryByText("gpu-01") !== null, false, "no node row comes from the list");
  release?.();
  await within(drawer).findByText("gpu-01");
  assert.ok(within(drawer).getByText("gpu-02"));
  assert.ok(within(drawer).getByText("gpu-03"));
});

test("opening a series reads its node rows from GET /images/{series_id}", async () => {
  const { calls, user } = renderPage();
  await user.click(screen.getByRole("tab", { name: "社区镜像" }));
  const drawer = await openSeries(user, communitySeries.name);
  await waitFor(() => assert.ok(calls.some((call) => call.method === "GET" && call.path === "/api/v1/images/3")));
  await user.click(within(drawer).getByRole("link", { name: nightlyVersion.image_ref }));
  // The digest column and node names exist only in the detail answer.
  await expandNodeRows(user, drawer);
  await within(drawer).findByText("0123456789ab");
  for (const name of ["gpu-01", "gpu-02", "gpu-03"]) assert.ok(within(drawer).getByText(name), name);
  assert.ok(await within(drawer).findByText("2 / 3"));
});

test("while a node of the open series is transferring, the detail is polled fast and the list is not", async () => {
  let progress = 1_000;
  const moving = (call: { method: string; path: string }) => {
    if (call.method !== "GET" || call.path !== "/api/v1/images/3") return undefined;
    progress += 1_000_000;
    const detail = seriesDetails["/api/v1/images/3"] as { versions: Array<{ nodes: Array<Record<string, unknown>> }> };
    return json(200, {
      ...detail,
      versions: [{
        ...detail.versions[0],
        size_bytes: 100_000_000,
        nodes: detail.versions[0].nodes.map((node) =>
          node.node_uuid === "n3" ? { ...node, state: "pulling", transferred_bytes: progress } : node),
      }],
    });
  };
  // jsdom reports a hidden document, and the page never polls a hidden tab.
  Object.defineProperty(document, "hidden", { value: false, configurable: true });
  try {
    const { calls, user } = renderPage(moving);
    await user.click(screen.getByRole("tab", { name: "社区镜像" }));
    const drawer = await openSeries(user, communitySeries.name);
    await user.click(within(drawer).getByRole("link", { name: nightlyVersion.image_ref }));
    await within(drawer).findByRole("button", { name: z.nodeTable.cancel });
    const listReads = calls.filter((call) => call.path === "/api/v1/images/versions").length;
    const detailReads = calls.filter((call) => call.path === "/api/v1/images/3").length;
    // No action is taken: the 2 s detail poll alone moves the progress. The
    // assertion is on the movement, not on a value, so it holds however many
    // polls a slow run fits in before it looks.
    const shownProgress = () => {
      // Only gpu-03 is transferring; read its row alone.
      const row = within(drawer).queryByText("gpu-03")?.closest("tr");
      const cell = row ? within(row as HTMLElement).queryByText(/^[\d.]+ (?:KiB|MiB) \//) : null;
      const done = /^([\d.]+) (KiB|MiB) \//.exec(cell?.textContent ?? "");
      return done ? Number.parseFloat(done[1]) * (done[2] === "MiB" ? 1024 : 1) : -1;
    };
    await waitFor(() => assert.ok(shownProgress() >= 0, "progress is shown"));
    const before = shownProgress();
    await waitFor(() => assert.ok(calls.filter((call) => call.path === "/api/v1/images/3").length > detailReads, "the detail is polled"), { timeout: 4_000 });
    await waitFor(() => assert.ok(shownProgress() > before, `progress moved past ${before} KiB`), { timeout: 4_000 });
    // The list keeps its own 10 s poll, which a slow run may well reach, so
    // the claim is on the rates: over the same window the detail is read more
    // often than the list.
    const detailDelta = () => calls.filter((call) => call.path === "/api/v1/images/3").length - detailReads;
    await waitFor(() => assert.ok(detailDelta() >= 2, "the detail is polled again"), { timeout: 6_000 });
    const listDelta = calls.filter((call) => call.path === "/api/v1/images/versions").length - listReads;
    assert.ok(listDelta < detailDelta(), `the list keeps its calm interval (list +${listDelta}, detail +${detailDelta()})`);
  } finally {
    delete (document as unknown as { hidden?: boolean }).hidden;
  }
});

// A version the catalog dropped stays while a node holds it, with no
// reference: it reads as an older version, and the tooltip says why.
test("an older version nothing references is labelled without a count", async () => {
  const { user } = renderPage(async (call) =>
    call.path === "/api/v1/images/2"
      ? json(200, { ...legacySeries, versions: [{ ...legacyVersion, reference_count: 0, present_nodes: 1, nodes: [] }] })
      : undefined,
  );
  const drawer = await openSeries(user, legacySeries.name);
  const tag = await within(drawer).findByText("旧版本");
  assert.equal(within(drawer).queryByText(/个引用/), null);
  await user.hover(tag);
  await screen.findByText("目录 2026-10-08.1a2b3c4d 起已有更新的版本，这个版本只为仍在用它的部署和已拉取的节点保留");
});
