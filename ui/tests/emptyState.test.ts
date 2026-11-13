import assert from "node:assert/strict";
import { JSDOM } from "jsdom";
import React from "react";
import { test, afterEach } from "node:test";

const dom = new JSDOM("<!doctype html><html><body></body></html>", { url: "http://localhost/" });
Object.defineProperty(globalThis, "window", { value: dom.window, configurable: true });
Object.defineProperty(globalThis, "document", { value: dom.window.document, configurable: true });
Object.defineProperty(globalThis, "navigator", { value: dom.window.navigator, configurable: true });

const { cleanup, render, screen } = await import("@testing-library/react");
const { default: EmptyState } = await import("../src/components/EmptyState.tsx");

afterEach(() => cleanup());

test("EmptyState renders user-visible description and optional action", () => {
  render(React.createElement(EmptyState, {
    kind: "models",
    description: React.createElement("span", null, "No models yet"),
    action: React.createElement("button", { type: "button" }, "Add model"),
  }));
  assert.equal(screen.getByText("No models yet").textContent, "No models yet");
  assert.equal(screen.getByRole("button", { name: "Add model" }).textContent, "Add model");
});

test("each kind draws its Zeron illustration, the resource stack for resource lists and the general one otherwise, never an image file", () => {
  const cases = [["nodes", "resources"], ["models", "resources"], ["deployments", "resources"], ["data", "general"], [undefined, "general"]] as const;
  for (const [kind, variant] of cases) {
    const { container } = render(React.createElement(EmptyState, { kind, description: "Nothing here" }));
    const drawn = [...container.querySelectorAll('[data-slot="empty-media"] [data-slot="empty-illustration"]')];
    assert.deepEqual(drawn.map((illustration) => [illustration.tagName, illustration.getAttribute("data-variant")]), [["svg", variant]], `kind ${kind}`);
    // The drawing is decoration: assistive technology hears only the description.
    assert.equal(drawn[0].getAttribute("aria-hidden"), "true", `kind ${kind}`);
    assert.equal(container.querySelectorAll("img").length, 0, `kind ${kind}`);
    cleanup();
  }
});

test("EmptyState omits action and applies compact class", () => {
  const { container } = render(React.createElement(EmptyState, {
    description: "Nothing here",
    compact: true,
  }));
  assert.equal(container.firstElementChild?.getAttribute("data-density"), "compact");
  assert.equal(container.firstElementChild?.getAttribute("data-scope"), "inline");
  assert.equal(screen.queryByRole("button"), null);
});
