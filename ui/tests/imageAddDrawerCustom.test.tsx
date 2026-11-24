import assert from "node:assert/strict";
import { test } from "node:test";
import { chooseOption, openSelect } from "./helpers/zeronSelect.ts";
import { choose, openDrawer, renderPage } from "./helpers/imageAddDrawerHarness.tsx";
import { imageEngines, json, zhImages } from "./helpers/imagesHarness.ts";
import { selectOptions } from "./helpers/imageAddDrawerCases.ts";

const { act, fireEvent, screen, waitFor, within } = await import("@testing-library/react");

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
  await chooseOption(within(drawer).getByRole("combobox", { name: zhImages.import.method }), zhImages.import.url.title);
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

test("the custom engine list is GET /images/engines as served, and the import sends no purpose", async () => {
  const { calls, user } = renderPage((call) =>
    call.method === "POST" && call.path === "/api/v1/images/import"
      ? json(200, { items: [{ id: 9, name: "gpuworks/strata", engine: "callable", purpose: "inference", framework: "cuda", source: "custom", enabled: true, version_count: 1, present_nodes: 0, total_nodes: 0 }], total: 1 })
      : undefined);
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = await openDrawer(user, "自定义镜像");
  await chooseOption(within(drawer).getByRole("combobox", { name: zhImages.import.method }), zhImages.import.url.title);
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

test("switching methods discards a late URL preview and allows a fresh one after switching back", async () => {
  const url = "https://images.example/runner.tar";
  const pending = Promise.withResolvers<void>();
  const { user, calls } = renderPage(async (call) => {
    if (call.path === "/api/v1/images/preview-archive-url") {
      await pending.promise;
      return json(200, { url, repo_tags: ["example/archive:v1"], metadata_by_tag: { "example/archive:v1": { engine_version: "1.2.3" } }, size_bytes: 1024, previewable: true });
    }
    return undefined;
  });
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  const drawer = within(await openDrawer(user, "自定义镜像"));
  const method = drawer.getByRole("combobox", { name: zhImages.import.method });
  await chooseOption(method, zhImages.import.url.title);
  const preview = async () => {
    await user.type(drawer.getByRole("textbox", { name: zhImages.import.url.label }), url);
    await user.click(drawer.getByRole("button", { name: /解\s*析/ }));
  };
  await preview();
  await waitFor(() => assert.ok(calls.some((call) => call.path.endsWith("/preview-archive-url"))));
  const add = drawer.getByRole("button", { name: zhImages.import.addImage }) as HTMLButtonElement;
  assert.equal(add.disabled, true, "adding must wait for the active HTTP preview");
  await user.click(add);
  assert.equal(calls.filter(call => call.path === "/api/v1/images/import").length, 0);
  assert.deepEqual(await openSelect(method), [zhImages.import.local.enable, zhImages.import.url.title, zhImages.import.runtime.title]);
  await user.keyboard("{Home}{Enter}");
  assert.equal(method.textContent, zhImages.import.local.enable);
  const imageRef = drawer.getByRole("textbox", { name: zhImages.import.imageRef }) as HTMLInputElement;
  await user.type(imageRef, "example/node:v1");
  await act(async () => { pending.resolve(); });
  assert.equal(imageRef.value, "example/node:v1");
  await waitFor(() => assert.equal(method.getAttribute("aria-expanded"), "false"));
  await chooseOption(method, zhImages.import.url.title);
  assert.equal(method.textContent, zhImages.import.url.title);
  assert.equal(Boolean(drawer.queryByRole("textbox", { name: zhImages.nodeTable.localDialog.path })), false);
  await preview();
  await waitFor(() => assert.equal(imageRef.value, "example/archive:v1"));
  const version = drawer.getByRole("textbox", { name: zhImages.import.engineVersion }) as HTMLInputElement;
  assert.equal(version.value, "1.2.3");
  fireEvent.change(drawer.getByRole("textbox", { name: zhImages.import.url.label }), { target: { value: url + ".new" } });
  assert.equal(version.value, "");
});
