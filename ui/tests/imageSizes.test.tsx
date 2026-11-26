import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { dom } from "./helpers/dom.ts";
import {
  catalogRoutes,
  i18n,
  installFetch,
  json,
  systemVersion,
  type Handler,
} from "./helpers/imagesHarness.ts";
import { I18nextProvider } from "react-i18next";

const { MemoryRouter } = await import("react-router-dom");
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: userEvent } = await import("@testing-library/user-event");
const { message } = await import("antd");
const { default: ImagesPage } = await import("../src/pages/ImagesPage.tsx");

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


function summary(key: string) {
  return screen.getByTestId(`image-summary-${key}`);
}

for (const scenario of [
  {
    name: "registry download size is known before the first pull",
    versions: [{ ...systemVersion, size_bytes: undefined, download_size_bytes: 9980152277, present_nodes: 0, local_runtime_size_bytes: 0, local_size_known_nodes: 0 }],
    catalog: "9.3 GiB", local: "0 B", missing: 0, catalogMissing: 0, catalogStatus: "全部体积已知", localStatus: "",
  },
  {
    name: "unknown catalog and local sizes",
    versions: [{ ...systemVersion, size_bytes: undefined, present_nodes: 2, local_runtime_size_bytes: undefined, local_size_known_nodes: undefined }],
    catalog: "—", local: "—", missing: 2, catalogMissing: 1, catalogStatus: "暂无已知体积", localStatus: "暂无已知体积",
  },
  {
    name: "measured local subtotal never multiplies archive size",
    versions: [{ ...systemVersion, size_bytes: 1024, present_nodes: 3, local_runtime_size_bytes: 8192, local_size_known_nodes: 2 }],
    catalog: "1 KiB", local: "8 KiB", missing: 1, catalogMissing: 0, catalogStatus: "全部体积已知", localStatus: "已知小计",
  },
  {
    name: "measured zero is known",
    versions: [{ ...systemVersion, size_bytes: undefined, present_nodes: 1, local_runtime_size_bytes: 0, local_size_known_nodes: 1 }],
    catalog: "—", local: "0 B", missing: 0, catalogMissing: 1, catalogStatus: "暂无已知体积", localStatus: "全部体积已知",
  },
  {
    name: "partial archive totals keep the unknown version count visible",
    versions: [
      { ...systemVersion, size_bytes: 1024, present_nodes: 1, local_runtime_size_bytes: 2048, local_size_known_nodes: 1 },
      { ...systemVersion, id: 99, size_bytes: undefined, present_nodes: 0, local_runtime_size_bytes: 0, local_size_known_nodes: 0 },
    ],
    catalog: "1 KiB", local: "2 KiB", missing: 0, catalogMissing: 1, catalogStatus: "已知小计", localStatus: "全部体积已知",
  },
  {
    name: "size units retain decimal precision in the summary",
    versions: [{ ...systemVersion, size_bytes: 1073741824, present_nodes: 1, local_runtime_size_bytes: 1610612736, local_size_known_nodes: 1 }],
    catalog: "1.0 GiB", local: "1.5 GiB", missing: 0, catalogMissing: 0, catalogStatus: "全部体积已知", localStatus: "全部体积已知",
  },
  { name: "empty catalog is zero", versions: [], catalog: "0 B", local: "0 B", missing: 0, catalogMissing: 0, catalogStatus: "", localStatus: "" },
]) {
  test(scenario.name, async () => {
    renderPage((call) => {
      if (call.path === "/api/v1/images/versions") return json(200, { items: scenario.versions });
      if (scenario.versions.length === 0 && call.path === "/api/v1/images") return json(200, { items: [] });
    });
    await waitFor(() => assert.equal(within(summary("localSize")).getByTestId("image-summary-value").textContent, scenario.local));
    await waitFor(() => assert.equal(within(summary("catalogSize")).getByTestId("image-summary-value").textContent, scenario.catalog));
    await waitFor(() => assert.equal(within(summary("localSize")).getByTestId("image-summary-info").textContent,
      scenario.localStatus + (scenario.missing ? `${scenario.missing} 个副本体积未知` : "")));
    await waitFor(() => assert.equal(within(summary("catalogSize")).getByTestId("image-summary-info").textContent,
      scenario.catalogStatus + (scenario.catalogMissing ? `${scenario.catalogMissing} 个版本体积未知` : "")));
  });
}

test("pending and failed version summaries do not claim zero", async () => {
  let resolve!: (response: Response) => void;
  renderPage((call) => call.path === "/api/v1/images/versions"
    ? new Promise<Response>((done) => { resolve = done; }) : undefined);
  await waitFor(() => assert.ok(resolve));
  assert.ok(summary("localSize").textContent?.includes("—"));
  assert.ok(summary("catalogSize").textContent?.includes("—"));
  resolve(json(500, { error: "inventory unavailable" }));
  await waitFor(() => assert.ok(screen.getByRole("alert").textContent));
  assert.ok(summary("localSize").textContent?.includes("—"));
  assert.equal(within(summary("catalogSize")).getByTestId("image-summary-info").textContent, "");
  assert.equal(within(summary("localSize")).getByTestId("image-summary-info").textContent, "");
});
