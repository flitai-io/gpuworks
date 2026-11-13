import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { dom } from "./helpers/dom.ts";
import { catalogRoutes, i18n, installFetch, json, zhImages, type Handler } from "./helpers/imagesHarness.ts";
import { I18nextProvider } from "react-i18next";

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { message } = await import("antd");
const { default: ImagesPage } = await import("../src/pages/ImagesPage.tsx");

const repos = ["gpuworks/gpuworks-runner", "quay.io/gpuworks/gpuworks-runner", "swr.example/team/gpuworks-runner"];
const tags = [
  { tag: "cuda13.0-vllm0.29.0-arm64-slot01", engine: "vllm", engine_version: "0.29.0", framework: "cuda", framework_version: "13.0", arch: "arm64" },
  { tag: "cann9.0-sglang0.5.15.post1-amd64-diffusion-slot16", engine: "sglang", engine_version: "0.5.15.post1", framework: "cann", framework_version: "9.0", arch: "amd64" },
  { tag: "cann8.5-vllm0.18.0-arm64-omni-slot14", engine: "vllm-omni", engine_version: "0.18.0", framework: "cann", framework_version: "8.5", arch: "arm64" },
  { tag: "latest", rolling_tag: true },
];
let restoreFetch = () => {};
afterEach(() => { cleanup(); message.destroy(); restoreFetch(); });

function mount(handler?: Handler) {
  const stub = installFetch(async (call) => {
    const response = await handler?.(call);
    if (response) return response;
    if (call.path === "/api/v1/images/community/repos") return json(200, { items: repos.map((repo) => ({ repo, registry: "oci", engine: "", framework: "" })), total: 3 });
    if (call.path.startsWith("/api/v1/images/community/browse?")) return json(200, {
      repo: new URL(call.path, "http://localhost").searchParams.get("repo"), registry: "oci", engine: "", tags,
    });
    if (call.method === "POST" && call.path === "/api/v1/images/import") return json(200, { items: [], total: 0 });
    return catalogRoutes(call);
  });
  restoreFetch = stub.restore;
  render(<I18nextProvider i18n={i18n}><MemoryRouter initialEntries={["/images"]}><ImagesPage /></MemoryRouter></I18nextProvider>);
  return { calls: stub.calls, user: userEvent.setup({ document: dom.window.document }) };
}

async function open(user: ReturnType<typeof userEvent.setup>) {
  await screen.findByRole("button", { name: "vLLM · CUDA" });
  await user.click(screen.getByRole("tab", { name: "社区镜像" }));
  await user.click(await screen.findByRole("button", { name: /添加社区镜像/ }));
  return await screen.findByRole("button", { name: repos[0], exact: true });
}

function values() {
  const input = (id: string) => (document.getElementById(id) as HTMLInputElement | null)?.value ?? "";
  const selected = (id: string) => document.getElementById(id)?.closest(".ant-select")?.querySelector(".ant-select-selection-item")?.textContent ?? "";
  return { engine: selected("engine"), version: input("engine_version"), framework: selected("framework"), frameworkVersion: input("framework_version"), arch: selected("arch") };
}

test("mixed repositories show actual stack badges and fill, replace and submit tag metadata", async () => {
  const { calls, user } = mount();
  const first = await open(user);
  for (const repo of repos) {
    const button = await screen.findByRole("button", { name: repo, exact: true });
    const card = button.closest(".ant-card") as HTMLElement;
    for (const label of ["vllm", "sglang", "vllm-omni", "cuda", "cann"]) {
      await within(card).findByText(label, { selector: ".ant-tag" });
    }
  }
  await user.click(first);
  await user.click(await screen.findByRole("button", { name: tags[0].tag, exact: true }));
  await waitFor(() => assert.deepEqual(values(), { engine: "vLLM", version: "0.29.0", framework: "CUDA", frameworkVersion: "13.0", arch: "arm64" }));
  await user.click(screen.getByRole("button", { name: tags[2].tag, exact: true }));
  await waitFor(() => assert.deepEqual(values(), { engine: "vLLM Omni", version: "0.18.0", framework: "CANN", frameworkVersion: "8.5", arch: "arm64" }));
  await user.click(screen.getByRole("button", { name: "latest", exact: true }));
  await waitFor(() => assert.deepEqual(values(), { engine: "", version: "", framework: "", frameworkVersion: "", arch: "multi" }));
  await user.click(screen.getByRole("button", { name: tags[1].tag, exact: true }));
  await waitFor(() => assert.deepEqual(values(), { engine: "SGLang", version: "0.5.15.post1", framework: "CANN", frameworkVersion: "9.0", arch: "amd64" }));
  const frameworkVersion = screen.getByRole("textbox", { name: zhImages.import.frameworkVersion });
  await user.clear(frameworkVersion);
  await user.type(frameworkVersion, "9.1");
  await user.click(screen.getByRole("button", { name: zhImages.import.addImage, exact: true }));
  await waitFor(() => assert.equal(calls.some((call) => call.method === "POST" && call.path === "/api/v1/images/import"), true));
  const body = JSON.parse(calls.find((call) => call.method === "POST" && call.path === "/api/v1/images/import")!.body!).series[0];
  assert.equal(body.engine, "sglang");
  assert.equal(body.framework, "cann");
  assert.equal(body.versions[0].engine_version, "0.5.15.post1");
  assert.equal(body.versions[0].framework_version, "9.1", "user override survives automatic badge loading");
  assert.equal(body.versions[0].arch, "amd64");
  assert.equal(body.versions[0].image_ref, `${repos[0]}:${tags[1].tag}`);
});

test("a failed background browse can be retried by selecting its repository", async () => {
  let requests = 0;
  const { user } = mount((call) => {
    if (call.path === `/api/v1/images/community/browse?repo=${encodeURIComponent(repos[0])}` && ++requests === 1) {
      return json(400, { error: "registry temporarily unavailable" });
    }
    return undefined;
  });
  const button = await open(user);
  const card = button.closest(".ant-card") as HTMLElement;
  await waitFor(() => assert.equal(card.querySelector('[title="registry temporarily unavailable"]') !== null, true));
  await user.click(button);
  await screen.findByRole("button", { name: tags[0].tag, exact: true });
  await within(card).findByText("vllm", { selector: ".ant-tag" });
  await waitFor(() => assert.equal(card.querySelector('[title="registry temporarily unavailable"]') !== null, false));
  assert.equal(requests, 2);
});
