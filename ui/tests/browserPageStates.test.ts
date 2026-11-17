import assert from "node:assert/strict";
import test from "node:test";
// @ts-expect-error Browser harness module has no TypeScript declaration.
import { states } from "./browser/page-states.mjs";

test("browser walker yields a full-page detail before returning to the parent", async () => {
  let url = "";
  const visits: string[] = [];
  const firstLink = { count: async () => 1, click: async () => { url = "http://example.test/monitor/d/7"; } };
  const page = {
    goto: async (next: string) => { url = next; visits.push(next); },
    url: () => url,
    waitForLoadState: async () => {}, waitForTimeout: async () => {},
    locator: (selector: string) => selector.startsWith("tbody") ? { first: () => firstLink } : { count: async () => 0 },
  };
  const walk = states(page, "http://example.test", "/monitor");
  assert.deepEqual((await walk.next()).value, { id: "/monitor", kind: "page" });
  assert.deepEqual((await walk.next()).value, { id: "/monitor · detail", kind: "detail-page" });
  assert.equal(url, "http://example.test/monitor/d/7");
  assert.equal((await walk.next()).done, true);
  assert.deepEqual(visits, ["http://example.test/monitor", "http://example.test/monitor"]);
});
