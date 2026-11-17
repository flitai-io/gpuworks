import assert from "node:assert/strict";
import fs from "node:fs";
import { after, afterEach, test } from "node:test";
import { JSDOM } from "jsdom";
import React from "react";
import { createInstance } from "i18next";
import { I18nextProvider, initReactI18next } from "react-i18next";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import type { PreflightSummary } from "../src/api.ts";

// The preflight RPC's orchestration is gone from the form: the multi-node
// summary arrives on the evaluate response and renders read-only. These cases
// render the published fixture shapes and assert what the deployer sees.

const closeChannels = trackMessageChannels();
const dom = new JSDOM("<!doctype html><html><body></body></html>", {
  url: "http://localhost/",
});
for (const name of [
  "window",
  "document",
  "navigator",
  "HTMLElement",
  "HTMLInputElement",
  "SVGElement",
  "Element",
  "Node",
  "MutationObserver",
  "ShadowRoot",
] as const) {
  Object.defineProperty(globalThis, name, {
    value: dom.window[name],
    configurable: true,
  });
}
Object.defineProperty(globalThis, "getComputedStyle", {
  value: (element: Element) => dom.window.getComputedStyle(element),
  configurable: true,
});
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({
    matches: false,
    addListener() {},
    removeListener() {},
    addEventListener() {},
    removeEventListener() {},
    dispatchEvent() {
      return false;
    },
  }),
});
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
Object.defineProperty(globalThis, "ResizeObserver", {
  value: ResizeObserverStub,
  configurable: true,
});

const deployLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/deploy.json", import.meta.url),
    "utf8",
  ),
);
const commonLocale = JSON.parse(
  fs.readFileSync(
    new URL("../src/i18n/locales/en/common.json", import.meta.url),
    "utf8",
  ),
);
const i18n = createInstance();
await i18n
  .use(initReactI18next)
  .init({
    lng: "en",
    fallbackLng: "en",
    resources: { en: { deploy: deployLocale, common: commonLocale } },
    defaultNS: "common",
    interpolation: { escapeValue: false },
  });

const { cleanup, render, screen, waitFor } =
  await import("@testing-library/react");
const { Form } = await import("antd");
const DeployResourceStep = (
  await import("../src/pages/deploy/DeployResourceStep.tsx")
).default;

const multiNodeFixture = JSON.parse(
  fs.readFileSync(
    new URL("./fixtures/evaluate/multi_node.json", import.meta.url),
    "utf8",
  ),
) as {
  preflight?: PreflightSummary;
};
assert.equal(
  multiNodeFixture.preflight?.ok,
  true,
  "the captured multi-node draft preflights clean",
);
assert.ok(
  (multiNodeFixture.preflight?.checks ?? []).length > 0 &&
    (multiNodeFixture.preflight?.checks ?? []).every((check) => check.status === "pass"),
  "the captured clean summary lists what it checked, every check passing",
);

function PreflightHost({
  preflight,
}: {
  preflight?: PreflightSummary;
}) {
  return (
    <I18nextProvider i18n={i18n}>
      <Form layout="vertical" initialValues={{ nnodes: 2 }}>
        <DeployResourceStep nodes={[]} preflight={preflight} result={null} />
      </Form>
    </I18nextProvider>
  );
}

afterEach(cleanup);
after(() => {
  closeChannels();
  dom.window.close();
});

test("a clean multi-node draft renders the passing preflight tag", async () => {
  render(<PreflightHost preflight={multiNodeFixture.preflight} />);
  await waitFor(() =>
    assert.ok(screen.getByText(deployLocale.resource.preflightPassed)),
  );
  // Passing checks stay in the summary tag; they are not listed one by one.
  assert.equal(screen.queryByText(commonLocale.checkStatus.pass), null);
});

test("a failing summary names the failed check and the checks it kept from running", async () => {
  render(
    <PreflightHost
      preflight={{
        ok: false,
        checks: [
          { id: "node_count", status: "pass", code: "PREFLIGHT_NODE_COUNT_OK", params: { count: 2 }, message: "2 nodes selected" },
          { id: "node_list", status: "fail", code: "PREFLIGHT_DUPLICATE_NODE", params: { node: "node-a" }, message: "duplicate node node-a" },
          { id: "port_range", status: "skip", code: "PREFLIGHT_BLOCKED", params: { blocked_by: "node_list" }, message: "not checked", blocked_by: ["node_list"] },
          { id: "shared_network", status: "warn", code: "FUTURE_PREFLIGHT_CODE", message: "A future fallback" },
          { id: "future", status: "na", code: "FUTURE_PREFLIGHT_CODE", message: "Does not apply" },
        ],
      }}
    />,
  );
  await waitFor(() =>
    assert.ok(screen.getByText(deployLocale.resource.preflightFailed)),
  );
  // The text comes from the code, not the English fallback.
  assert.ok(screen.getByText("Node “node-a” is selected more than once.", { exact: false }));
  assert.equal(screen.queryByText("duplicate node node-a", { exact: false }), null);
  assert.ok(screen.getByText(commonLocale.apiError.PREFLIGHT_BLOCKED, { exact: false }));
  assert.ok(screen.getByText("A future fallback", { exact: false }), "an unknown code falls back to the message");
  for (const status of ["fail", "skip", "warn", "na"] as const) {
    assert.ok(screen.getByText(commonLocale.checkStatus[status]), status);
  }
  assert.equal(screen.queryByText(commonLocale.checkStatus.pass), null);
  assert.equal(screen.queryByText("2 nodes are selected.", { exact: false }), null);
});

test("a draft without a preflight answer renders no verdict", async () => {
  render(<PreflightHost />);
  assert.equal(screen.queryByText(deployLocale.resource.preflightPassed), null);
  assert.equal(screen.queryByText(deployLocale.resource.preflightFailed), null);
});
