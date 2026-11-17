import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import ts from "typescript";

// Static guard for ResourceTable name-column renderers. Runtime/browser tests
// still verify inherited CSS, disabled controls, and the resulting navigation.
function violations(source: string): string[] {
  const file = ts.createSourceFile("page.tsx", source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const errors: string[] = [];
  const property = (node: ts.ObjectLiteralExpression, name: string) => node.properties.find(
    (item): item is ts.PropertyAssignment => ts.isPropertyAssignment(item) && item.name.getText(file).replace(/["']/g, "") === name,
  )?.initializer;
  function visit(node: ts.Node) {
    if (ts.isObjectLiteralExpression(node) && property(node, "role")?.getText(file).replace(/["']/g, "") === "name") {
      const render = property(node, "render");
      if (render) {
        const check = (element: ts.Node) => {
          if (ts.isJsxOpeningElement(element) || ts.isJsxSelfClosingElement(element)) {
            const tag = element.tagName.getText(file);
            const attrs = element.attributes.properties.filter(ts.isJsxAttribute);
            const attr = (name: string) => attrs.find(item => item.name.getText(file) === name);
            const value = (name: string) => {
              const init = attr(name)?.initializer;
              return init && ts.isStringLiteral(init) ? init.text : undefined;
            };
            // Icon controls (copy, expansion, menus) are actions, not names.
            if ((tag === "Button" || tag === "button") && attr("onClick") && !attr("iconOnly")) {
              if (value("variant") !== "link" && value("type") !== "link" && !value("className")?.split(/\s+/).includes("text-fg-brand")) {
                errors.push(`line ${file.getLineAndCharacterOfPosition(element.getStart(file)).line + 1}: clickable name must use a link variant or text-fg-brand`);
              }
            }
          }
          ts.forEachChild(element, check);
        };
        check(render);
      }
    }
    ts.forEachChild(node, visit);
  }
  visit(file);
  return errors;
}

function files(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => entry.isDirectory()
    ? files(join(dir, entry.name)) : entry.name.endsWith(".tsx") ? [join(dir, entry.name)] : []);
}

test("all resource name buttons across product pages use a themed link affordance", () => {
  const root = new URL("../src/pages/", import.meta.url).pathname;
  const errors = files(root).flatMap(path => violations(readFileSync(path, "utf8")).map(error => `${path.slice(root.length)}:${error}`));
  assert.deepEqual(errors, []);
});

test("the name-column guard catches ghost/plain buttons but permits links, text and actions", () => {
  const column = (render: string, role = "name") => `const cols = [{role:"${role}", render:()=>${render}}]`;
  assert.equal(violations(column('<Button variant="ghost" onClick={open}>Name</Button>')).length, 1);
  assert.equal(violations(column('<button onClick={open}>Name</button>')).length, 1);
  for (const markup of ['<Button variant="link" onClick={open}>Name</Button>', '<Button type="link" onClick={open}>Name</Button>', '<Link to="/nodes">Name</Link>', '<span>Name</span>', '<Button iconOnly onClick={copy} />']) {
    assert.deepEqual(violations(column(markup)), []);
  }
  assert.deepEqual(violations(column('<Button variant="ghost" onClick={remove}>Remove</Button>', "actions")), []);
});
