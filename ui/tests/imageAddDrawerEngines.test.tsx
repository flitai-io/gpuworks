import assert from "node:assert/strict";
import { test } from "node:test";
import { chooseOption } from "./helpers/zeronSelect.ts";
import { openDrawer, renderPage } from "./helpers/imageAddDrawerHarness.tsx";
import { communitySeries, json, nightlyVersion, zhImages } from "./helpers/imagesHarness.ts";
import { pickNodes } from "./helpers/nodePicker.ts";
import { selectOptions } from "./helpers/imageAddDrawerCases.ts";

const { fireEvent, screen, waitFor, within } = await import("@testing-library/react");

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

test("a node runtime image is picked from its namespace, inspected and registered", async () => {
  const ui = zhImages.import.runtime;
  const ref = "registry.example:5000/team/runner:v1";
  const identity = "sha256:" + "a".repeat(64);
  const endpoint = "/api/v1/images/nodes/worker-id/runtime-images";
  const { user, calls } = renderPage(call => {
    if (call.path.startsWith("/api/v1/nodes?")) return json(200, { items: [{ uuid: "worker-id", name: "GPU Worker", state: "online" }], total: 1 });
    if (call.path.startsWith(endpoint + "?")) {
      const ns = new URLSearchParams(call.path.split("?")[1]).get("namespace") || "gpuworks";
      return json(200, {
        runtime: "nerdctl", target_namespace: "gpuworks", namespace: ns, namespaces: ["gpuworks", "default"],
        items: ns === "gpuworks" ? [] : [{ ref, identity, size: "7.9 GB" }],
      });
    }
    if (call.path === endpoint + "/inspect") return json(200, { image_id: identity, metadata: { engine: "vllm", engine_version: "0.30.0", framework: "cuda", framework_version: "13.0", arch: "amd64" } });
    if (call.path === "/api/v1/images/import") return json(200, { items: [{ ...communitySeries, versions: [{ ...nightlyVersion, image_ref: ref, nodes: [{ node_uuid: "worker-id", state: "syncing", source: "runtime", transferred_bytes: 1024 }] }] }], total: 1 });
    return undefined;
  });
  const drawer = within(await openDrawer(user, "自定义镜像"));
  await chooseOption(drawer.getByRole("combobox", { name: zhImages.import.method }), ui.title);
  await pickNodes(drawer.getByRole("combobox", { name: zhImages.nodeTable.column.node }), "GPU Worker");
  await drawer.findByText(ui.empty);
  const submit = drawer.getByRole("button", { name: zhImages.import.addImage }) as HTMLButtonElement;
  assert.equal(submit.disabled, true);
  const namespace = drawer.getByRole("combobox", { name: ui.namespace });
  await chooseOption(namespace, "default");
  const radio = await drawer.findByRole("radio", { name: ref }) as HTMLInputElement;
  assert.equal(radio.checked, false);
  await user.click(radio);
  await drawer.findByText(ui.ready);
  const engineVersion = drawer.getByRole("textbox", { name: zhImages.import.engineVersion }) as HTMLInputElement;
  assert.equal(engineVersion.value, "0.30.0");
  await user.clear(engineVersion);
  await user.type(engineVersion, "0.31.0");
  await chooseOption(namespace, "gpuworks");
  await drawer.findByText(ui.empty);
  assert.equal(engineVersion.value, "");
  assert.equal(submit.disabled, true);
  await chooseOption(namespace, "default");
  await user.click(await drawer.findByRole("radio", { name: ref }));
  await drawer.findByText(ui.ready);
  await user.click(submit);
  await waitFor(() => assert.equal(calls.filter(c => c.path === "/api/v1/images/import").length, 1));
  const payload = JSON.parse(calls.find(c => c.path === "/api/v1/images/import")!.body!);
  await screen.findByText(ui.copying);
  assert.deepEqual(payload.runtime_image, {
    node_uuid: "worker-id", runtime: "nerdctl", namespace: "default", target_namespace: "gpuworks", ref, identity,
  });
  assert.equal(payload.series[0].versions[0].image_ref, ref);
  assert.equal(calls.filter(c => c.path.endsWith("/sync")).length, 0);
});
