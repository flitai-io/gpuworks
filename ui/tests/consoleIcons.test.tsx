import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import "./helpers/dom.ts";

const { cleanup, render } = await import("@testing-library/react");
const { ConsoleIcon } = await import("../src/ui-kit/consoleIcons.ts");

afterEach(() => cleanup());

const drawing = (svg: SVGSVGElement) => [...svg.querySelectorAll("path, circle, rect, line, polyline, polygon")]
  .map((shape) => shape.outerHTML).join("");

test("every console icon draws an svg at the size its slot gives", () => {
  for (const [name, Icon] of Object.entries(ConsoleIcon)) {
    const { container, unmount } = render(<Icon size={16} />);
    const svg = container.querySelector("svg");
    assert.ok(svg, `${name} renders an svg`);
    assert.equal(svg.getAttribute("width"), "16", `${name} takes the slot's size`);
    assert.equal(svg.getAttribute("height"), "16", `${name} takes the slot's size`);
    assert.ok(drawing(svg).length > 0, `${name} draws something`);
    unmount();
  }
});

test("console icons are distinct: no two names draw the same picture", () => {
  const seen = new Map<string, string>();
  for (const [name, Icon] of Object.entries(ConsoleIcon)) {
    const { container, unmount } = render(<Icon />);
    const picture = drawing(container.querySelector("svg")!);
    assert.equal(seen.get(picture), undefined, `${name} draws the same icon as ${seen.get(picture)}`);
    seen.set(picture, name);
    unmount();
  }
});

test("a console icon passes the stroke width and decorative attributes through", () => {
  const { container } = render(<ConsoleIcon.filter strokeWidth={1.5} aria-hidden className="text-fg-subtle" />);
  const svg = container.querySelector("svg")!;
  assert.equal(svg.getAttribute("aria-hidden"), "true");
  assert.match(svg.getAttribute("class") ?? "", /text-fg-subtle/);
  assert.ok([...svg.querySelectorAll("[stroke-width]")].every((shape) => shape.getAttribute("stroke-width") === "1.5"), "the stroke width reaches every stroke");
});
