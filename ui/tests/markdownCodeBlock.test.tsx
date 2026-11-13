import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n } from "./helpers/dom.ts";

const { i18n, locale } = await createI18n(["common"]);
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: MarkdownContent } = await import("../src/components/MarkdownContent.tsx");
const copyLabel = locale.common.copy.code as string;

const fenced = "Intro with `inline code`.\n\n```python\nprint(\"a\")\nprint(\"b\")\n```\n\nAfter.";

function renderMarkdown(content: string, props: Partial<React.ComponentProps<typeof MarkdownContent>> = {}) {
  return render(<I18nextProvider i18n={i18n}><MarkdownContent content={content} {...props} /></I18nextProvider>);
}

type ClipboardMode = { api?: "ok" | "reject"; exec?: boolean };

// installClipboard models the three environments a copy can meet: a secure
// context (Clipboard API works), a context whose API rejects, and plain HTTP
// (no navigator.clipboard at all). The selection fallback records what was
// selected when "copy" ran, which is what the browser would put on the clipboard.
function installClipboard({ api, exec }: ClipboardMode) {
  const copied: string[] = [];
  const clipboard = api === undefined ? undefined : {
    writeText: async (text: string) => {
      if (api === "reject") throw new DOMException("denied", "NotAllowedError");
      copied.push(text);
    },
  };
  Object.defineProperty(navigator, "clipboard", { value: clipboard, configurable: true });
  Object.defineProperty(document, "execCommand", {
    configurable: true,
    value: (command: string) => {
      if (command !== "copy" || exec === undefined) return false;
      if (exec) copied.push((document.activeElement as HTMLTextAreaElement).value);
      return exec;
    },
  });
  return copied;
}

afterEach(() => {
  cleanup();
  delete (navigator as { clipboard?: unknown }).clipboard;
  delete (document as { execCommand?: unknown }).execCommand;
});

test("fenced blocks get a language header and copy button; inline code does not", () => {
  renderMarkdown(fenced);
  const buttons = screen.getAllByRole("button", { name: copyLabel });
  assert.equal(buttons.length, 1);
  const block = document.querySelector(".markdown-code-block");
  assert.ok(block);
  assert.equal(block.querySelector(".markdown-code-language")?.textContent, "python");
  assert.ok(block.querySelector("pre > code.language-python"));
  const inline = screen.getByText("inline code");
  assert.equal(inline.tagName, "CODE");
  assert.ok(inline.closest(".markdown-code-block") === null, "inline code has no block chrome");
});

test("a block without a language is labelled text", () => {
  renderMarkdown("```\nplain\n```");
  assert.equal(document.querySelector(".markdown-code-language")?.textContent, "text");
});

test("copy writes the exact block source through the Clipboard API", async () => {
  const copied = installClipboard({ api: "ok" });
  renderMarkdown(fenced);
  fireEvent.click(screen.getByRole("button", { name: copyLabel }));
  await waitFor(() => assert.deepEqual(copied, ["print(\"a\")\nprint(\"b\")"]));
  await waitFor(() => assert.ok(screen.getByRole("button", { name: copyLabel }).querySelector("[aria-label=check]"), "copied feedback"));
});

test("plain HTTP copies through the selection fallback", async () => {
  const copied = installClipboard({ exec: true });
  renderMarkdown(fenced);
  fireEvent.click(screen.getByRole("button", { name: copyLabel }));
  await waitFor(() => assert.deepEqual(copied, ["print(\"a\")\nprint(\"b\")"]));
  assert.ok(document.querySelector("body > textarea") === null, "the selection textarea is removed");
});

test("a rejected Clipboard API falls back to the selection copy", async () => {
  const copied = installClipboard({ api: "reject", exec: true });
  renderMarkdown(fenced);
  fireEvent.click(screen.getByRole("button", { name: copyLabel }));
  await waitFor(() => assert.deepEqual(copied, ["print(\"a\")\nprint(\"b\")"]));
});

test("when both paths fail the code is offered pre-selected for manual copy", async () => {
  const user = userEvent.setup();
  installClipboard({ api: "reject", exec: false });
  renderMarkdown(fenced);
  const trigger = screen.getByRole("button", { name: copyLabel });
  fireEvent.click(trigger);
  const dialog = await screen.findByRole("dialog", { name: locale.common.copy.manualTitle });
  assert.ok(dialog.textContent?.includes(locale.common.copy.manualHint));
  const editor = await waitFor(() => {
    const found = within(dialog).getByRole("textbox") as HTMLTextAreaElement;
    assert.equal(found.value, "print(\"a\")\nprint(\"b\")");
    return found;
  });
  assert.equal(editor.readOnly, true);
  assert.ok(!trigger.querySelector("[aria-label=check]"), "no copied feedback");
  await user.click(within(dialog).getByRole("button", { name: locale.common.copy.done }));
  await waitFor(() => assert.ok(screen.queryByRole("dialog") === null, "dialog closes"));
});

test("the copy button is reachable and operable from the keyboard", async () => {
  // userEvent.setup() installs its own clipboard stub, so set it up first.
  const user = userEvent.setup();
  const copied = installClipboard({ api: "ok" });
  renderMarkdown(fenced);
  await user.tab();
  assert.ok(document.activeElement === screen.getByRole("button", { name: copyLabel }), "first tab stop is the copy button");
  await user.keyboard("{Enter}");
  await waitFor(() => assert.equal(copied.length, 1));
});

test("hand-written pre in README HTML renders unchanged", () => {
  renderMarkdown("<pre>raw preformatted</pre>", { allowHtml: true });
  assert.ok(screen.queryByRole("button", { name: copyLabel }) === null, "no copy button");
  assert.equal(screen.getByText("raw preformatted").tagName, "PRE");
});

test("caller actions receive the block identity next to the copy button", () => {
  const seen: Array<{ language: string; code: string; startOffset?: number }> = [];
  const content = "Lead.\n\n```HTML\n<p>hi</p>\n```\n";
  renderMarkdown(content, {
    codeBlockActions: (block) => {
      seen.push(block);
      return <button type="button">Preview</button>;
    },
  });
  assert.deepEqual(seen.at(-1), { language: "html", code: "<p>hi</p>", startOffset: content.indexOf("```"), closed: true });
  const header = document.querySelector(".markdown-code-actions");
  assert.deepEqual([...header!.querySelectorAll("button")].map((button) => button.textContent || button.getAttribute("aria-label")), ["Preview", copyLabel]);
});

test("a growing reply keeps its existing code blocks mounted", async () => {
  const copied = installClipboard({ api: "ok" });
  const view = renderMarkdown(fenced);
  const block = document.querySelector(".markdown-code-block");
  fireEvent.click(screen.getByRole("button", { name: copyLabel }));
  await waitFor(() => assert.equal(copied.length, 1));
  await waitFor(() => assert.ok(screen.getByRole("button", { name: copyLabel }).querySelector("[aria-label=check]")));
  // A streamed chunk re-renders the whole reply with more text.
  view.rerender(<I18nextProvider i18n={i18n}><MarkdownContent content={`${fenced}\n\nAnd one more sentence.`} /></I18nextProvider>);
  await screen.findByText("And one more sentence.");
  assert.ok(document.querySelector(".markdown-code-block") === block, "the block element survives the re-render");
  assert.ok(screen.getByRole("button", { name: copyLabel }).querySelector("[aria-label=check]"), "its copied feedback survives too");
});

test("a block reports whether its closing fence has arrived", () => {
  const closedOf = (content: string) => {
    const seen: boolean[] = [];
    cleanup();
    renderMarkdown(content, { codeBlockActions: (block) => { seen.push(block.closed); return null; } });
    return seen.at(-1);
  };
  assert.equal(closedOf("```html\n<p>streaming"), false, "no closing fence yet");
  assert.equal(closedOf("```html\n"), false, "just the opening fence");
  assert.equal(closedOf("```html\n<p>x</p>\n```"), true);
  assert.equal(closedOf("```html\n<p>x</p>\n`````\nafter"), true, "a longer closing fence closes");
  assert.equal(closedOf("````html\n<p>x</p>\n```\nstill code"), false, "a shorter fence is content");
  assert.equal(closedOf("~~~html\n<p>x</p>\n~~~"), true);
  assert.equal(closedOf("Lead **  loose  ** strong.\n\n```html\n<p>x</p>\n```\n"), true, "offsets match the normalized source");
  assert.equal(closedOf("    indented code\n"), true, "an indented block has no fence");
});
