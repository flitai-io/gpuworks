import assert from "node:assert/strict";
import { test } from "node:test";
import { openDrawer, renderPage } from "./helpers/imageAddDrawerHarness.tsx";
import { communityRepos, json, vllmOpenAIBrowse, zhImages } from "./helpers/imagesHarness.ts";
import { c } from "./helpers/imageAddDrawerCases.ts";

const { screen, waitFor, within } = await import("@testing-library/react");
const { formatTime } = await import("../src/i18n/datetime.ts");

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
