import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import "./helpers/dom.ts";

const { cleanup, render, screen, within } = await import("@testing-library/react");
const { default: DetailSection } = await import("../src/components/detail/DetailSection.tsx");

afterEach(() => cleanup());

test("a detail section is a titled block with its content below the heading", () => {
  render(<DetailSection title="Configuration"><p>Local storage</p></DetailSection>);
  const heading = screen.getByRole("heading", { name: "Configuration" });
  const section = heading.closest("section")!;
  assert.ok(section.classList.contains("detail-section"));
  assert.ok(within(section).getByText("Local storage"));
  assert.equal(heading.contains(screen.getByText("Local storage")), false, "content is not part of the heading");
});

test("a section's extra sits in its heading row", () => {
  render(<DetailSection title="Nodes" extra={<button type="button">View all</button>}><p>6 nodes</p></DetailSection>);
  const heading = screen.getByRole("heading", { name: /Nodes/ });
  assert.ok(within(heading).getByRole("button", { name: "View all" }));
});
