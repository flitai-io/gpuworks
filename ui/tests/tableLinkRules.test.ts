import assert from "node:assert/strict";
import test from "node:test";
import { JSDOM } from "jsdom";
// Browser-computed style is mocked here to exercise rule selection, not CSS.
// The ui-check job uses this exact function against the built UI in Chromium.
// @ts-expect-error Browser check module has no TypeScript declaration.
import { tableLinkFindings } from "./browser/table-link-rules.mjs";

function check(markup: string) {
  const dom = new JSDOM(`<table><tbody><tr>${markup}</tr></tbody></table>`);
  const previous = Object.getOwnPropertyDescriptors(globalThis);
  const color = (el: HTMLElement): string => {
    if (el.style.color) return el.style.color.includes("--fg-brand") ? "brand" : el.style.color;
    return el.parentElement ? color(el.parentElement) : "black";
  };
  Object.defineProperties(globalThis, {
    document: { configurable: true, value: dom.window.document },
    NodeFilter: { configurable: true, value: dom.window.NodeFilter },
    getComputedStyle: { configurable: true, value: (el: HTMLElement) => ({ color: color(el), visibility: "visible", opacity: "1" }) },
  });
  dom.window.HTMLElement.prototype.getBoundingClientRect = () => ({ x: 0, y: 0, width: 30, height: 20 }) as DOMRect;
  try { return tableLinkFindings(); }
  finally {
    for (const key of ["document", "NodeFilter", "getComputedStyle"]) {
      if (previous[key]) Object.defineProperty(globalThis, key, previous[key]);
      else Reflect.deleteProperty(globalThis, key);
    }
    dom.window.close();
  }
}

test("browser gate catches plain name buttons, black anchors and child color overrides", () => {
  for (const markup of [
    '<td class="resource-table-name"><button>Name</button></td>',
    '<td><a href="/nodes">Node</a></td>',
    '<td><a href="/nodes" style="color:var(--fg-brand)"><span style="color:black">Node</span></a></td>',
  ]) assert.equal(check(markup)[0]?.rule, "table-link-color");
});

test("browser gate accepts themed text and excludes disabled, icon and action buttons", () => {
  assert.deepEqual(check('<td class="resource-table-name"><button style="color:var(--fg-brand)"><span>Name</span></button></td><td><a href="/nodes" style="color:var(--fg-brand)">Node</a></td>'), []);
  assert.deepEqual(check('<td class="resource-table-name"><button style="color:var(--brand)">Legacy primary</button></td>'), []);
  assert.deepEqual(check('<td class="resource-table-name"><button disabled>Name</button><button aria-label="Copy"><svg /></button></td><td><button>Delete</button><a href="/nodes" aria-disabled="true">Node</a><a class="ant-btn" href="/export">Export</a><span>Plain</span></td>'), []);
});
