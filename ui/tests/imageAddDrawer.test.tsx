import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { dom } from "./helpers/dom.ts";
import {
  catalogRoutes,
  communityRepos,
  i18n,
  imageEngines,
  installFetch,
  json,
  vllmOpenAIBrowse,
  zhImages,
  type Handler,
} from "./helpers/imagesHarness.ts";
import { I18nextProvider } from "react-i18next";

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { message } = await import("antd");
const { default: ImagesPage } = await import("../src/pages/ImagesPage.tsx");
const { formatTime } = await import("../src/i18n/datetime.ts");

const c = zhImages.import.community;

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

async function openDrawer(user: ReturnType<typeof userEvent.setup>, tab: "社区镜像" | "自定义镜像") {
  await user.click(screen.getByRole("tab", { name: tab }));
  await user.click(await screen.findByRole("button", { name: tab === "社区镜像" ? /添加社区镜像/ : /添加自定义镜像/ }));
  const title = zhImages.import.title[tab === "社区镜像" ? "community" : "custom"];
  return await waitFor(() => {
    const heading = [...document.querySelectorAll(".ant-drawer-title")].find((node) => node.textContent === title);
    assert.ok(heading, `drawer ${title}`);
    return heading.closest(".ant-drawer-content") as HTMLElement;
  });
}

function selectOptions() {
  return [...document.querySelectorAll(".ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option")]
    .map((node) => node.getAttribute("title") || node.textContent || "");
}

test("custom archive acquisition addresses are authorized only after the image fields are valid", async () => {
  let allowed = false;
  const { calls, user } = renderPage((call) => {
    if (call.path === "/api/v1/auth/me") return json(200, { id: 1, username: "admin", role: "admin" });
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "GET") return json(200, { restrict_private_hosts: true, allow_private_hosts: ["existing.internal"], editable: true, revision: "r1" });
    if (call.path === "/api/v1/server/outbound-policy/check") return json(200, { items: [{ host: "archive.internal", status: allowed ? "allowed" : "needs_allow" }], total: 1 });
    if (call.path === "/api/v1/server/outbound-policy" && call.method === "PUT") {
      assert.deepEqual(JSON.parse(call.body || "{}"), { restrict_private_hosts: true, allow_private_hosts: ["existing.internal", "archive.internal"], revision: "r1" });
      allowed = true;
      return json(200, { restrict_private_hosts: true, allow_private_hosts: ["existing.internal", "archive.internal"], editable: true, revision: "r2" });
    }
    if (call.path === "/api/v1/images/import") return json(200, { items: [], total: 0 });
    return undefined;
  });
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = await openDrawer(user, "自定义镜像");
  const sources = within(drawer).getByRole("combobox", { name: zhImages.import.registrySources });
  await user.type(sources, "registry.internal/team/runner:v1,");
  await user.type(sources, "https://archive.internal/runner.tar,");
  const allow = await within(drawer).findByRole("checkbox", { name: /archive\.internal/ });
  await user.click(allow);
  await user.click(within(drawer).getByRole("button", { name: zhImages.import.addImage }));
  await within(drawer).findByText(zhImages.import.imageRefRequired);
  assert.equal(calls.some((call) => call.method === "PUT"), false, "invalid forms cannot grant a host");
  await user.type(within(drawer).getByRole("textbox", { name: zhImages.import.imageRef }), "example/runner:v1");
  await choose(within(drawer).getByRole("combobox", { name: zhImages.import.engine }), "vLLM");
  await choose(within(drawer).getByRole("combobox", { name: zhImages.import.framework }), "CUDA");
  await user.type(within(drawer).getByRole("textbox", { name: zhImages.import.frameworkVersion }), "13.0");
  await user.click(within(drawer).getByRole("button", { name: zhImages.import.addImage }));
  await waitFor(() => assert.ok(calls.some((call) => call.path === "/api/v1/images/import")));
  assert.equal(calls.filter((call) => call.method === "PUT").length, 1);
  const body = JSON.parse(calls.find((call) => call.path === "/api/v1/images/import")!.body!);
  assert.deepEqual(body.series[0].versions[0].registry_sources, ["registry.internal/team/runner:v1", "https://archive.internal/runner.tar"]);
  assert.ok(calls.filter((call) => call.path.endsWith("/check")).every((call) => JSON.parse(call.body!).hosts.every((host: string) => host !== "registry.internal")), "node registry sources do not request management permissions");
});

async function choose(field: HTMLElement, title: string) {
  fireEvent.mouseDown(field);
  const option = await waitFor(() => {
    const found = [...document.querySelectorAll(".ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-select-item-option")]
      .find((node) => node.getAttribute("title") === title);
    assert.ok(found, `option ${title}`);
    return found as HTMLElement;
  });
  fireEvent.click(option);
}

test("the custom engine list is GET /images/engines as served, and the import sends no purpose", async () => {
  const { calls, user } = renderPage((call) =>
    call.method === "POST" && call.path === "/api/v1/images/import"
      ? json(200, { items: [{ id: 9, name: "gpuworks/strata", engine: "callable", purpose: "inference", framework: "cuda", source: "custom", enabled: true, version_count: 1, present_nodes: 0, total_nodes: 0 }], total: 1 })
      : undefined);
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = await openDrawer(user, "自定义镜像");
  await waitFor(() => assert.ok(calls.some((call) => call.path === "/api/v1/images/engines")));
  assert.equal(calls.some((call) => call.path === "/api/v1/engines"), false, "the deploy engine profiles are not read");

  const engine = within(drawer).getByRole("combobox", { name: zhImages.import.engine });
  fireEvent.mouseDown(engine);
  // The server's labels, in the server's order.
  await waitFor(() => assert.deepEqual(selectOptions(), imageEngines.map((option) => option.label)));
  assert.deepEqual(selectOptions(), ["vLLM", "vLLM Omni", "SGLang", "Callable"]);
  fireEvent.mouseDown(engine);

  await user.type(within(drawer).getByRole("textbox", { name: zhImages.import.imageRef }), "gpuworks/strata:0.1-gw4");
  await choose(engine, "Callable");
  // The framework is required: without it the image would never be recommended.
  await user.click(within(drawer).getByRole("button", { name: zhImages.import.addImage }));
  await within(drawer).findByText(zhImages.import.frameworkRequired);
  assert.equal(calls.some((call) => call.path === "/api/v1/images/import"), false, "nothing is sent while the form is invalid");

  await choose(within(drawer).getByRole("combobox", { name: zhImages.import.framework }), "CUDA");
  await user.click(within(drawer).getByRole("button", { name: zhImages.import.addImage }));
  await within(drawer).findByText("请输入框架版本");
  assert.equal(calls.some((call) => call.path === "/api/v1/images/import"), false,
    "the real import API requires a version whenever a framework is declared");
  await user.type(within(drawer).getByRole("textbox", { name: zhImages.import.frameworkVersion }), "13.0");
  await user.click(within(drawer).getByRole("button", { name: zhImages.import.addImage }));
  await waitFor(() => assert.ok(calls.some((call) => call.path === "/api/v1/images/import")));
  const body = JSON.parse(calls.find((call) => call.path === "/api/v1/images/import")?.body ?? "{}");
  assert.equal(body.series.length, 1);
  const series = body.series[0];
  assert.equal(series.engine, "callable");
  assert.equal(series.framework, "cuda");
  assert.equal(series.source, "custom");
  assert.equal("purpose" in series, false, "purpose is left to the server default");
  assert.equal("common_args" in series || "common_env" in series, false);
  assert.deepEqual(series.versions, [{
    engine_version: "", framework_version: "13.0", arch: "multi", image_ref: "gpuworks/strata:0.1-gw4",
    source: "custom", rolling_tag: false, registry_sources: [],
  }]);
});

test("community cards list every upstream tag, search narrows them, and the keyboard picks one", async () => {
  let releaseRepos: (() => void) | undefined;
  const reposHeld = new Promise<void>((resolve) => { releaseRepos = resolve; });
  let releaseTags: (() => void) | undefined;
  const tagsHeld = new Promise<void>((resolve) => { releaseTags = resolve; });
  const { calls, user } = renderPage(async (call) => {
    if (call.path === "/api/v1/images/community/repos") {
      await reposHeld;
      return json(200, { items: communityRepos, total: communityRepos.length });
    }
    if (call.path === "/api/v1/images/community/browse?repo=vllm%2Fvllm-openai") {
      await tagsHeld;
      return json(200, vllmOpenAIBrowse);
    }
    if (call.method === "POST" && call.path === "/api/v1/images/import") return json(200, { items: [], total: 0 });
    return undefined;
  });
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = await openDrawer(user, "社区镜像");
  await within(drawer).findByText(c.reposLoading);
  releaseRepos?.();

  // Six cards; the DCU card carries its three harbor paths.
  for (const path of [
    "vllm/vllm-openai", "vllm/vllm-omni", "lmsysorg/sglang", "quay.io/ascend/vllm-ascend", "quay.io/ascend/sglang",
    "harbor.sourcefind.cn:5443/dcu/admin/base/vllm", "harbor.sourcefind.cn:5443/dcu/admin/base/custom", "harbor.sourcefind.cn:5443/dcu/admin/base/sglang",
  ]) {
    assert.ok(await within(drawer).findByRole("button", { name: path }), path);
  }

  await user.click(within(drawer).getByRole("button", { name: "vllm/vllm-openai" }));
  await within(drawer).findByText(c.tagsLoading.replace("{{repo}}", "vllm/vllm-openai"));
  releaseTags?.();
  await within(drawer).findByText("vllm/vllm-openai · 共 5 个 tag");
  for (const tag of ["v0.11.0", "qwen38", "nightly", "latest", "v0.10.2"]) {
    assert.ok(within(drawer).getByRole("button", { name: tag }), `${tag} is listed, filtered by nothing`);
  }
  assert.equal(within(drawer).getAllByText(zhImages.import.rolling).length, 2, "nightly and latest are marked rolling");
  assert.ok(within(drawer).getByText(zhImages.tag.upstreamGone));
  assert.ok(within(drawer).getByText(formatTime("2026-09-28T12:30:00Z")), "last_updated is shown");

  const search = within(drawer).getByRole("textbox", { name: c.search });
  await user.click(search);
  await user.keyboard("qwen38");
  await waitFor(() => assert.equal(within(drawer).queryByRole("button", { name: "nightly" }), null));
  assert.ok(within(drawer).getByRole("button", { name: "qwen38" }));
  await user.clear(search);
  await user.keyboard("no-such-tag");
  await within(drawer).findByText(c.noMatch);
  await user.clear(search);
  await user.keyboard("qwen38");

  // Tab from the search box to the tag and press Enter.
  const qwen = await within(drawer).findByRole("button", { name: "qwen38" });
  await waitFor(async () => {
    await user.tab();
    assert.equal(document.activeElement, qwen);
  });
  await user.keyboard("{Enter}");
  const ref = within(drawer).getByRole("textbox", { name: zhImages.import.imageRef }) as HTMLInputElement;
  await waitFor(() => assert.equal(ref.value, "vllm/vllm-openai:qwen38"));

  await user.type(within(drawer).getByRole("textbox", { name: zhImages.import.frameworkVersion }), "13.0");
  await user.click(within(drawer).getByRole("button", { name: zhImages.import.addImage }));
  await waitFor(() => assert.ok(calls.some((call) => call.method === "POST" && call.path === "/api/v1/images/import")));
  const series = JSON.parse(calls.find((call) => call.path === "/api/v1/images/import")?.body ?? "{}").series[0];
  assert.equal(series.engine, "vllm");
  assert.equal(series.framework, "cuda");
  assert.equal(series.source, "community");
  assert.equal(series.versions[0].image_ref, "vllm/vllm-openai:qwen38");
  assert.equal(series.versions[0].rolling_tag, false);
});

test("a rolling community tag is added as a rolling tag", async () => {
  const { calls, user } = renderPage((call) => {
    if (call.path === "/api/v1/images/community/repos") return json(200, { items: communityRepos, total: communityRepos.length });
    if (call.path === "/api/v1/images/community/browse?repo=vllm%2Fvllm-openai") return json(200, vllmOpenAIBrowse);
    if (call.method === "POST" && call.path === "/api/v1/images/import") return json(200, { items: [], total: 0 });
    return undefined;
  });
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = await openDrawer(user, "社区镜像");
  await user.click(await within(drawer).findByRole("button", { name: "vllm/vllm-openai" }));
  await user.click(await within(drawer).findByRole("button", { name: "nightly" }));
  await user.type(within(drawer).getByRole("textbox", { name: zhImages.import.frameworkVersion }), "13.0");
  await user.click(within(drawer).getByRole("button", { name: zhImages.import.addImage }));
  await waitFor(() => assert.ok(calls.some((call) => call.path === "/api/v1/images/import")));
  const version = JSON.parse(calls.find((call) => call.path === "/api/v1/images/import")?.body ?? "{}").series[0].versions[0];
  assert.equal(version.image_ref, "vllm/vllm-openai:nightly");
  assert.equal(version.rolling_tag, true);
});

test("a sibling path lists its own tags, an empty repository says so, and an unreachable one shows why", async () => {
  const { calls, user } = renderPage((call) => {
    if (call.path === "/api/v1/images/community/repos") return json(200, { items: communityRepos, total: communityRepos.length });
    if (call.path.startsWith("/api/v1/images/community/browse?repo=harbor.sourcefind.cn")) {
      return json(200, { repo: "harbor.sourcefind.cn:5443/dcu/admin/base/sglang", registry: "oci", engine: "vllm", framework: "dtk", tags: [] });
    }
    if (call.path === "/api/v1/images/community/browse?repo=registry.example%2Fprivate%2Frunner") {
      return json(400, { error: "community registry requires credentials" });
    }
    return undefined;
  });
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = await openDrawer(user, "社区镜像");
  await user.click(await within(drawer).findByRole("button", { name: "harbor.sourcefind.cn:5443/dcu/admin/base/sglang" }));
  await within(drawer).findByText(c.noTags);
  assert.ok(calls.some((call) => call.path === `/api/v1/images/community/browse?repo=${encodeURIComponent("harbor.sourcefind.cn:5443/dcu/admin/base/sglang")}`));

  // Any other repository can be typed in; Enter lists its tags.
  await user.click(within(drawer).getByRole("textbox", { name: c.otherRepo }));
  await user.keyboard("registry.example/private/runner{Enter}");
  await within(drawer).findByText(c.tagsFailed.replace("{{repo}}", "registry.example/private/runner"));
  assert.ok(within(drawer).getByText("community registry requires credentials"));
});

test("a failed repository list is reported in the drawer", async () => {
  const { user } = renderPage((call) =>
    call.path === "/api/v1/images/community/repos" ? json(500, { error: "settings unreadable" }) : undefined);
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = await openDrawer(user, "社区镜像");
  await within(drawer).findByText(c.reposFailed);
  assert.ok(within(drawer).getByText("settings unreadable"));
  // Typing a repository still works without the cards.
  assert.ok(within(drawer).getByRole("textbox", { name: c.otherRepo }));
});

test("the engine list offers exactly what the server serves, labels and order included", async () => {
  const served = [{ name: "sglang", label: "SGLang" }, { name: "vllm", label: "vLLM (server label)" }];
  const { user } = renderPage((call) => (call.path === "/api/v1/images/engines" ? json(200, { items: served, total: served.length }) : undefined));
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = await openDrawer(user, "自定义镜像");
  const engine = within(drawer).getByRole("combobox", { name: zhImages.import.engine });
  fireEvent.mouseDown(engine);
  await waitFor(() => assert.deepEqual(selectOptions(), ["SGLang", "vLLM (server label)"]));
});

test("the engine list shows it is loading, and a failed read leaves it empty with the reason", async () => {
  let release: (() => void) | undefined;
  const held = new Promise<void>((resolve) => { release = resolve; });
  const { user } = renderPage(async (call) => {
    if (call.path !== "/api/v1/images/engines") return undefined;
    await held;
    return json(500, { error: "engine dictionary unavailable" });
  });
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = await openDrawer(user, "自定义镜像");
  await waitFor(() => assert.ok(drawer.querySelector(".ant-select-arrow-loading"), "the engine select shows it is loading"));
  release?.();

  await within(drawer).findByText(zhImages.import.enginesFailed);
  assert.ok(within(drawer).getByText("engine dictionary unavailable"));
  assert.equal(drawer.querySelector(".ant-select-arrow-loading") !== null, false, "loading stops on failure");
  const engine = within(drawer).getByRole("combobox", { name: zhImages.import.engine });
  fireEvent.mouseDown(engine);
  await waitFor(() => assert.ok(document.querySelector(".ant-select-dropdown:not(.ant-select-dropdown-hidden) .ant-empty"), "the dropdown is empty"));
  assert.deepEqual(selectOptions(), [], "no engine is made up when the dictionary cannot be read");
});
