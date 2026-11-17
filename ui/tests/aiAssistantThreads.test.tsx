import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";

Object.defineProperty(globalThis, "FileReader", { value: dom.window.FileReader, configurable: true });
const { i18n, locale } = await createI18n(["ai", "common"], "en");
const { cleanup, fireEvent, render, screen, waitFor, within } = await import("@testing-library/react");
const { default: AssistantPanel } = await import("../src/assistant/AssistantPanel.tsx");
const { AssistantProvider } = await import("../src/assistant/AssistantContext.tsx");
const { memoryThreadDB } = await import("../src/assistant/threads.ts");
type ThreadDB = import("../src/assistant/threads.ts").ThreadDB;

const panel = locale.ai.panel as Record<string, string>;
const apiError = locale.common.apiError as Record<string, string>;
const originalFetch = globalThis.fetch;
afterEach(() => {
  cleanup();
  globalThis.fetch = originalFetch;
});

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
const frame = (event: string, data: unknown) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
const sse = (...frames: string[]) => new Response(frames.join(""), { status: 200, headers: { "Content-Type": "text/event-stream" } });
const done = { calls: 1, usage: { prompt_tokens: 10, completion_tokens: 2 }, first_text_ms: 5, total_ms: 9 };
const limits = { per_message: 4, per_request: 8, max_bytes: 2 << 20, max_side: 1568 };

// fakeServer answers the endpoints the panel calls the way the server does:
// who is signed in, whether the service takes images, and each ask.
function fakeServer(imageInput: boolean, answer: (body: Record<string, unknown>, n: number) => Response) {
  const asked: Record<string, unknown>[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url === "/api/v1/auth/me") return json({ id: 7, username: "alice", role: "admin" });
    if (url === "/api/v1/ai/available") return json(imageInput ? { available: true, image_input: true, images: limits } : { available: true, image_input: false });
    if (url === "/api/v1/ai/ask") {
      const body = JSON.parse(String(init?.body));
      asked.push(body);
      return answer(body, asked.length);
    }
    return json({ error: `unexpected ${url}` }, 500);
  }) as typeof fetch;
  return asked;
}

function shell(threadDB: ThreadDB = memoryThreadDB()) {
  render(<I18nextProvider i18n={i18n}><AssistantProvider threadDB={threadDB}><AssistantPanel /></AssistantProvider></I18nextProvider>);
  fireEvent.keyDown(window, { key: "i", ctrlKey: true });
  return threadDB;
}

const question = () => screen.getByRole("textbox", { name: panel.input });
const pngFile = (name = "shot.png") => new dom.window.File([new Uint8Array([0x89, 0x50, 0x4e, 0x47, 1, 2, 3])], name, { type: "image/png" });

async function ask(text: string) {
  fireEvent.change(question(), { target: { value: text } });
  fireEvent.click(screen.getByRole("button", { name: panel.send }));
}

test("a pasted screenshot is shown, sent with the question, and kept on the question", async () => {
  const asked = fakeServer(true, () => sse(frame("text", { delta: "That is the deploy page." }), frame("done", done)));
  shell();
  const attach = await screen.findByRole("button", { name: panel.attachImage });
  assert.ok(attach);
  fireEvent.paste(question(), { clipboardData: { files: [pngFile()] } });
  const attached = await screen.findByLabelText(panel.attached);
  assert.equal(within(attached).getAllByAltText(/^Image \d/).length, 1);
  // An image alone can be sent.
  assert.equal((screen.getByRole("button", { name: panel.send }) as HTMLButtonElement).disabled, false);
  await ask("what is wrong here?");
  await waitFor(() => assert.equal(asked.length, 1));
  const images = asked[0].images as string[];
  assert.equal(images.length, 1);
  assert.ok(images[0].startsWith("data:image/png;base64,"));
  assert.equal(asked[0].question, "what is wrong here?");
  await screen.findByText("That is the deploy page.");
  // The draft's images are cleared; the question keeps its own.
  assert.equal(screen.getAllByLabelText(panel.attached).length, 1);
  assert.ok(screen.getByAltText(panel.imageAlt.replace("{{n}}", "1")));
});

test("a follow-up sends the earlier images in the thread, and removing a draft image drops it", async () => {
  const asked = fakeServer(true, (_, n) => sse(frame("text", { delta: `answer ${n}` }), frame("done", done)));
  shell();
  await screen.findByRole("button", { name: panel.attachImage });
  fireEvent.paste(question(), { clipboardData: { files: [pngFile("a.png"), pngFile("b.png")] } });
  await waitFor(() => assert.equal(within(screen.getByLabelText(panel.attached)).getAllByAltText(/^Image \d/).length, 2));
  fireEvent.click(screen.getByRole("button", { name: panel.removeImage.replace("{{n}}", "2") }));
  await ask("first");
  await screen.findByText("answer 1");
  await ask("second");
  await screen.findByText("answer 2");
  assert.equal((asked[0].images as string[]).length, 1);
  assert.equal(asked[1].images, undefined);
  const history = asked[1].history as { role: string; content: string; images?: string[] }[];
  assert.equal(history[0].role, "user");
  assert.equal(history[0].images?.length, 1);
  assert.equal(history[1].images, undefined);
});

test("a service that takes no images offers no attach button and says so when an image is pasted", async () => {
  const asked = fakeServer(false, () => sse(frame("done", done)));
  shell();
  await waitFor(() => assert.ok(question()));
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(screen.queryByRole("button", { name: panel.attachImage }), null);
  fireEvent.paste(question(), { clipboardData: { files: [pngFile()] } });
  assert.ok(await screen.findByText(panel.imagesOff));
  assert.equal(screen.queryByLabelText(panel.attached), null);
  assert.equal(asked.length, 0);
});

test("too many images and unsupported files are refused with a reason", async () => {
  fakeServer(true, () => sse(frame("done", done)));
  shell();
  await screen.findByRole("button", { name: panel.attachImage });
  fireEvent.paste(question(), { clipboardData: { files: [1, 2, 3, 4, 5].map((n) => pngFile(`${n}.png`)) } });
  assert.ok(await screen.findByText(panel.imagesTooMany.replace("{{count}}", "4")));
  await waitFor(() => assert.equal(within(screen.getByLabelText(panel.attached)).getAllByAltText(/^Image \d/).length, 4));
  assert.equal((screen.getByRole("button", { name: panel.attachImage }) as HTMLButtonElement).disabled, true);
});

test("a service that refuses the images mid-answer is explained", async () => {
  fakeServer(true, () => sse(frame("error", { error: "llm http 400: not a multimodal model", code: "AI_IMAGE_REJECTED" })));
  shell();
  await screen.findByRole("button", { name: panel.attachImage });
  fireEvent.paste(question(), { clipboardData: { files: [pngFile()] } });
  await screen.findByLabelText(panel.attached);
  await ask("look");
  assert.ok(await screen.findByText(apiError.AI_IMAGE_REJECTED));
});

test("an image sent to a service that takes none is explained from the product error", async () => {
  fakeServer(true, () => json({ error: "the AI service in use is not marked as accepting images", code: "AI_IMAGE_INPUT_OFF" }, 409));
  shell();
  await screen.findByRole("button", { name: panel.attachImage });
  fireEvent.paste(question(), { clipboardData: { files: [pngFile()] } });
  await screen.findByLabelText(panel.attached);
  await ask("look");
  assert.ok(await screen.findByText(apiError.AI_IMAGE_INPUT_OFF));
});

test("finished threads are kept, listed newest first, reopened and continued", async () => {
  const asked = fakeServer(false, (body) => sse(frame("text", { delta: `re: ${body.question}` }), frame("done", done)));
  const db = shell();
  await ask("why is node-1 offline?");
  await screen.findByText("re: why is node-1 offline?");
  await waitFor(async () => assert.equal((await db.list(7)).length, 1));
  fireEvent.click(screen.getByRole("button", { name: panel.newThread }));
  await ask("cluster status?");
  await screen.findByText("re: cluster status?");
  await waitFor(async () => assert.equal((await db.list(7)).length, 2));

  fireEvent.click(screen.getByRole("button", { name: panel.history }));
  const list = await screen.findByRole("list", { name: panel.history });
  const rows = within(list).getAllByRole("listitem");
  assert.equal(rows.length, 2);
  assert.match(rows[0].textContent ?? "", /cluster status\?/);
  assert.match(rows[1].textContent ?? "", /why is node-1 offline\?/);

  // Opening one shows it; asking continues it with its turns as history.
  fireEvent.click(within(rows[1]).getByRole("button", { name: /^why is node-1 offline\?/ }));
  await screen.findByText("re: why is node-1 offline?");
  assert.equal(screen.queryByText("re: cluster status?"), null);
  await ask("and node-2?");
  await screen.findByText("re: and node-2?");
  const last = asked[asked.length - 1];
  assert.deepEqual(last.history, [{ role: "user", content: "why is node-1 offline?" }, { role: "assistant", content: "re: why is node-1 offline?" }]);
  // Continuing saved into the same thread, not a new one.
  await waitFor(async () => {
    const kept = await db.list(7);
    assert.equal(kept.length, 2);
    assert.equal(kept.find((x) => x.title === "why is node-1 offline?")?.turns.length, 2);
  });

  // Deleting the shown thread clears the panel and the store.
  fireEvent.click(screen.getByRole("button", { name: panel.history }));
  const again = await screen.findByRole("list", { name: panel.history });
  fireEvent.click(within(again).getByRole("button", { name: panel.deleteThread.replace("{{title}}", "why is node-1 offline?") }));
  await waitFor(async () => assert.deepEqual((await db.list(7)).map((x) => x.title), ["cluster status?"]));
  assert.equal(within(again).getAllByRole("listitem").length, 1);
});

test("threads stored before load are listed, and an empty history says so", async () => {
  fakeServer(false, () => sse(frame("done", done)));
  shell();
  fireEvent.click(await screen.findByRole("button", { name: panel.history }));
  assert.ok(await screen.findByText(panel.noThreads));
  cleanup();

  const db = memoryThreadDB();
  await db.put({ id: "old", userID: 7, title: "kept question", createdAt: 1, updatedAt: 1, turns: [
    { task: "ask", question: "kept question", images: ["data:image/png;base64,iVBORw0KGgo="], status: "done", text: "kept answer", reasoning: "", steps: [], done },
  ] });
  await db.put({ id: "other", userID: 8, title: "someone else", createdAt: 2, updatedAt: 2, turns: [] });
  shell(db);
  fireEvent.click(await screen.findByRole("button", { name: panel.history }));
  const list = await screen.findByRole("list", { name: panel.history });
  await waitFor(() => assert.equal(within(list).getAllByRole("listitem").length, 1));
  fireEvent.click(within(list).getByRole("button", { name: /^kept question/ }));
  assert.ok(await screen.findByText("kept answer"));
  assert.ok(screen.getByAltText(panel.imageAlt.replace("{{n}}", "1")));
});
