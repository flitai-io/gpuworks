import { afterEach } from "node:test";
import { fireEvent, screen, waitFor, within, message, z, openNightly, PULL, posts, ACCEPTED, assert, test, json, zhCommon, makeRenderPage, cleanup } from "./helpers/imagePullRequest.tsx";
// What the image page sends when a node pulls an image, part one: a plain pull sends an empty
// request, the source picker, and how pull failures read. The shared fixture is in
// helpers/imagePullRequest.tsx.

let restoreFetch = () => {};
afterEach(() => {
  cleanup();
  message.destroy();
  restoreFetch();
});
const renderPage = makeRenderPage((restore) => { restoreFetch = restore; });

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
