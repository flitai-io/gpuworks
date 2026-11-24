import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { build } from "vite";

// Resolve the dependency used by the schema library, not a separate test copy.
const require = createRequire(import.meta.url);
const schemaRequire = createRequire(require.resolve("json-schema-library/package.json"));
const merge = schemaRequire("deepmerge");
const payload = () => JSON.parse('{"__proto__":{"injected":true},"safe":"value"}');

function assertSafe(value: Record<string, unknown>) {
  assert.equal(Object.getPrototypeOf(value), Object.prototype);
  assert.equal(value.injected, undefined);
  assert.equal(Object.hasOwn(value, "__proto__"), false);
  assert.equal(({} as Record<string, unknown>).injected, undefined);
}

test("schema merge rejects prototype poisoning with a null-prototype target", () => {
  const result = merge(Object.create(null), payload());
  assertSafe(result);
  assert.equal(result.safe, "value");
});

test("schema merge rejects prototype poisoning below null and primitive defaults", () => {
  for (const initial of [null, undefined, false, 0, ""]) {
    const result = merge({ session: initial }, { session: payload() });
    assertSafe(result.session);
    assert.equal(result.session.safe, "value");
  }
});

test("schema merge rejects own prototype keys in both target and source", () => {
  for (const [target, source] of [[payload(), {}], [{}, payload()], [payload(), payload()]]) {
    const result = merge(target, source);
    assertSafe(result);
    assert.equal(result.safe, "value");
  }
});

test("schema merge protects nested array values and merge.all", () => {
  const result = merge.all([{ sessions: [payload()] }, { sessions: [payload()] }]);
  assert.equal(result.sessions.length, 2);
  result.sessions.forEach(assertSafe);
});

test("schema merge preserves schema objects, symbols, and custom array policies", () => {
  const symbol = Symbol("metadata");
  const target = { properties: { name: { type: "string" } }, required: ["name"], [symbol]: { a: 1 } };
  const source = { properties: { count: { type: "integer" } }, required: ["count"], [symbol]: { b: 2 } };
  const result = merge(target, source, { arrayMerge: (_: unknown[], values: unknown[]) => values });
  assert.deepEqual(result, {
    properties: { name: { type: "string" }, count: { type: "integer" } },
    required: ["count"],
    [symbol]: { a: 1, b: 2 },
  });
  assert.deepEqual(target.required, ["name"]);
  assert.notEqual(result.properties.name, target.properties.name);
});

test("production bundler uses the patched merge and retains YAML validation and completion", async () => {
  const entry = fileURLToPath(new URL("./security-yaml-entry.js", import.meta.url));
  const result = await build({
    root: fileURLToPath(new URL("../", import.meta.url)),
    logLevel: "silent",
    plugins: [{
      name: "security-yaml-entry",
      resolveId: (id) => id === entry ? entry : undefined,
      load: (id) => id === entry ? `
        export { EditorState } from "@codemirror/state";
        export { CompletionContext } from "@codemirror/autocomplete";
        export { ensureSyntaxTree } from "@codemirror/language";
        export { yamlSchema, yamlSchemaLinter, yamlCompletion } from "codemirror-json-schema/yaml";
        export { default as schemaMerge } from "json-schema-library/dist/module/lib/utils/merge.js";
      ` : undefined,
    }],
    build: {
      write: false,
      minify: false,
      lib: { entry, formats: ["es"] },
      rollupOptions: { output: { inlineDynamicImports: true } },
    },
  });
  const builds = Array.isArray(result) ? result : [result];
  const chunks = builds.flatMap((output) => {
    assert.ok("output" in output);
    return output.output.filter((item) => item.type === "chunk");
  });
  const modules = chunks.flatMap((chunk) => Object.keys(chunk.modules));
  assert.ok(modules.some((id) => id.endsWith("/vendor/deepmerge/index.cjs")));
  assert.ok(modules.some((id) => id.includes("/json-schema-library/dist/module/")));
  assert.ok(!modules.some((id) => id.endsWith("/jsonSchemaLibrary.js")), "bundled CommonJS contains an unpatched copy");
  assert.equal(chunks.length, 1);
  const bundled = await import(`data:text/javascript;base64,${Buffer.from(chunks[0].code).toString("base64")}`);
  assertSafe(bundled.schemaMerge({ session: null }, { session: payload() }).session);

  const schema = { type: "object", properties: { engine: { type: "string", enum: ["vllm", "sglang"] } } };
  // CodeMirror parses a new state under a 20 ms wall-clock budget and keeps an
  // unfinished tree if it is exceeded. A runner stalled mid-parse (GC, CPU
  // contention) then leaves the linter and completion reading a partial tree,
  // so completion came back with no options. Finish the parse, then take the
  // state forward one empty transaction, which is what publishes the finished
  // tree to syntaxTree(state) (ensureSyntaxTree only advances the context).
  const stateFor = (doc: string) => {
    const created = bundled.EditorState.create({ doc, extensions: bundled.yamlSchema(schema) });
    assert.ok(bundled.ensureSyntaxTree(created, created.doc.length, 10_000), "YAML syntax tree did not finish parsing");
    return created.update({}).state;
  };
  assert.deepEqual(bundled.yamlSchemaLinter()({ state: stateFor("engine: vllm") }), []);
  const invalid = bundled.yamlSchemaLinter()({ state: stateFor("engine: unknown") });
  assert.ok(invalid.some((error: { severity: string }) => error.severity === "error"));
  const state = stateFor("engine: v");
  const completion = bundled.yamlCompletion()(new bundled.CompletionContext(state, state.doc.length, true));
  assert.ok(completion.options.some((option: { label: string }) => option.label.includes("vllm")), JSON.stringify(completion));
});
