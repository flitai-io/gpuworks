import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import "./helpers/dom.ts";

const { cleanup, render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: CellTags } = await import("../src/components/resourceTable/CellTags.tsx");

afterEach(() => cleanup());

const tags = ["vllm", "sglang", "llama.cpp", "mindie"].map((text) => ({ key: text, text }));

test("no tags read as the empty mark, or the caller's", () => {
  const { container, rerender } = render(<CellTags tags={[]} />);
  assert.equal(container.textContent, "—");
  rerender(<CellTags tags={[]} empty="none" />);
  assert.equal(container.textContent, "none");
});

test("tags within the limit all show, with no overflow count", () => {
  render(<CellTags tags={tags.slice(0, 2)} />);
  assert.ok(screen.getByText("vllm"));
  assert.ok(screen.getByText("sglang"));
  assert.equal(screen.queryByText(/^\+\d+$/), null);
});

test("past the limit the row keeps the first ones and counts the rest, named on hover", async () => {
  const user = userEvent.setup();
  render(<CellTags tags={tags} limit={2} />);
  assert.ok(screen.getByText("vllm"));
  assert.ok(screen.getByText("sglang"));
  assert.equal(screen.queryByText("llama.cpp"), null, "the rest wait for the hover");
  await user.hover(screen.getByText("+2"));
  const rest = await screen.findByText("llama.cpp");
  assert.ok(rest.closest(".resource-cell-tags-rest")?.textContent?.includes("mindie"));
});

test("a tag's hover text is its own tooltip", async () => {
  const user = userEvent.setup();
  render(<CellTags tags={[{ key: "a", text: "A100", tooltip: "NVIDIA A100-SXM4-80GB" }]} />);
  await user.hover(screen.getByText("A100"));
  assert.ok(await screen.findByRole("tooltip", { name: "NVIDIA A100-SXM4-80GB" }));
});

test("without its own tooltip a tag's hover text is its text", async () => {
  const user = userEvent.setup();
  render(<CellTags tags={[{ key: "b", text: "H100" }]} />);
  await user.hover(screen.getByText("H100"));
  assert.ok(await screen.findByRole("tooltip", { name: "H100" }));
});
