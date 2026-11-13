import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { test } from "node:test";

const html = readFileSync(new URL("../index.html", import.meta.url), "utf8");
const boot = html.match(/<script>([\s\S]*?)<\/script>/)?.[1];
assert.ok(boot);
function initialize(values: Record<string, string>, unavailable = false) {
  const documentElement = { dataset: {} as Record<string, string>, style: {} as Record<string, string>, classList: { toggle() {} } };
  runInNewContext(boot!, {
    document: { documentElement },
    localStorage: { getItem: (key: string) => {
      if (unavailable) throw new Error("Storage unavailable");
      return values[key] ?? null;
    } },
  });
  return documentElement;
}
test("dark preview requires explicit opt-in before application modules load", () => {
  assert.equal(initialize({ "gpuworks.theme": "dark" }).dataset.theme, "light");
  const dark = initialize({ "gpuworks.themePreview": "1", "gpuworks.theme": "dark" });
  assert.equal(dark.dataset.theme, "dark");
  assert.equal(dark.style.colorScheme, "dark");
  assert.ok(html.indexOf(boot!) < html.indexOf('type="module"'));
});
test("unavailable storage and invalid preferences retain the public light theme", () => {
  assert.equal(initialize({}, true).dataset.theme, "light");
  assert.equal(initialize({ "gpuworks.themePreview": "1", "gpuworks.theme": "invalid" }).dataset.theme, "light");
});

// The Zeron CLI writes the CSS variables into app.css; antd and the charts read
// the same values from zeron.generated.ts. Both come from one pinned Zeron
// commit, so they must agree, and the CSS must stay within the browser floor.
const css = readFileSync(new URL("../src/app.css", import.meta.url), "utf8");
const { zeronTokens } = await import("../src/theme/zeron.generated.ts");
function block(selector: string) {
  const body = css.match(new RegExp(`\\n${selector.replace(".", "\\.")} \\{([\\s\\S]*?)\\n\\}`))?.[1];
  assert.ok(body, `app.css has a ${selector} block`);
  return Object.fromEntries([...body.matchAll(/(--[\w-]+):\s*([^;]+);/g)].map(([, k, v]) => [k.slice(2), v.trim().replace(/#[0-9a-fA-F]{3,8}\b/g, (h) => h.toLowerCase())]));
}
test("app.css and the TS tokens carry the same values in both modes", () => {
  for (const [selector, mode] of [[":root", "light"], [".dark", "dark"]] as const) {
    const declared = block(selector);
    const expected = zeronTokens[mode] as Record<string, string>;
    const mismatched = Object.keys(expected).filter((key) => declared[key] !== expected[key]);
    assert.deepEqual(mismatched, [], `${mode}: ${mismatched.slice(0, 5).join(", ")}`);
  }
});
test("app.css uses no light-dark(): the console supports Chrome 121 and Firefox 123", () => {
  assert.equal(css.includes("light-dark("), false);
});
test("dark preview also sets the class Zeron's dark variant keys off", () => {
  const values = { "gpuworks.themePreview": "1", "gpuworks.theme": "dark" };
  const documentElement = { dataset: {} as Record<string, string>, style: {} as Record<string, string>, classList: { added: [] as string[], toggle(name: string, on: boolean) { if (on) this.added.push(name); } } };
  runInNewContext(boot!, { document: { documentElement }, localStorage: { getItem: (key: string) => (values as Record<string, string>)[key] ?? null } });
  assert.deepEqual(documentElement.classList.added, ["dark"]);
});
