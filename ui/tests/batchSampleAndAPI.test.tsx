import assert from "node:assert/strict";
import { after, afterEach, beforeEach, test } from "node:test";
import React from "react";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./helpers/dom.ts";
import { trackMessageChannels } from "./helpers/trackedMessageChannels.ts";
import { batch, dictionary, model, page } from "./helpers/batchFixtures.ts";

// The sample requests file (downloaded or used as is), the .jsonl-only file
// picker, and the panel that tells the reader batches are an API.

const closeChannels = trackMessageChannels();
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
class Events { addEventListener() {} removeEventListener() {} close() {} }
Object.defineProperty(globalThis, "EventSource", { value: Events, configurable: true });
const { i18n, locale } = await createI18n(["batches"]);
const { cleanup, render, screen, waitFor, within } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { FeedbackHost, message } = await import("../src/ui-kit/feedback/index.tsx");
const { ConfigProvider } = await import("antd");
const { MemoryRouter, Route, Routes } = await import("react-router-dom");
const { api } = await import("../src/api.ts");
const { batchApi } = await import("../src/batchApi.ts");
const { default: BatchesPage } = await import("../src/pages/BatchesPage.tsx");
const { batchSampleJSONL, embeddingInputs, samplePrompts, sampleFileName, sampleLines, SAMPLE_EMBEDDING_LINES, SAMPLE_LINES, SAMPLE_MAX_TOKENS, SAMPLE_TASK_TYPES } = await import("../src/pages/batches/batchSample.ts");
const { batchAPISteps } = await import("../src/pages/batches/BatchAPIUsage.tsx");

const b = locale.batches;
const original = { list: batchApi.list, create: batchApi.create, dictionary: batchApi.dictionary, me: api.me };

beforeEach(() => {
  localStorage.clear();
  api.me = (async () => ({ role: "admin" })) as never;
  batchApi.list = (async () => page([])) as never;
  batchApi.dictionary = async () => dictionary([model("qwen-chat", "running"), model("embedder", "running", ["/v1/embeddings"])]);
});
afterEach(() => {
  message.destroy();
  cleanup();
  Object.assign(batchApi, { list: original.list, create: original.create, dictionary: original.dictionary });
  api.me = original.me;
});
after(() => closeChannels());

function mountPage() {
  render(
    <I18nextProvider i18n={i18n}><FeedbackHost />
      <ConfigProvider theme={{ token: { motion: false } }}>
        <MemoryRouter initialEntries={["/batches"]}>
          <Routes><Route path="/batches" element={<BatchesPage />} /></Routes>
        </MemoryRouter>
      </ConfigProvider>
    </I18nextProvider>,
  );
  return userEvent.setup({ document: dom.window.document });
}

async function openCreate(user: ReturnType<typeof userEvent.setup>) {
  const create = () => screen.getAllByRole("button", { name: b.action.create })[0] as HTMLButtonElement;
  await waitFor(() => assert.equal(create().disabled, false));
  await user.click(create());
  const dialog = await screen.findByRole("dialog", { name: b.create.title });
  await waitFor(() => assert.ok(dialog.contains(document.activeElement), "initial dialog focus is settled"));
  return dialog;
}

test("a sample is a thousand valid batch requests, ten thousand for embeddings", () => {
  assert.equal(SAMPLE_LINES, 1000);
  assert.equal(SAMPLE_EMBEDDING_LINES, 10000);
  for (const [endpoint, count, width] of [["/v1/chat/completions", 1000, 4], ["/v1/completions", 1000, 4], ["/v1/embeddings", 10000, 5]] as const) {
    assert.equal(sampleLines(endpoint), count);
    const text = batchSampleJSONL(endpoint, "m");
    assert.ok(text.endsWith("}\n"));
    const lines = text.trimEnd().split("\n").map((line) => JSON.parse(line));
    assert.equal(lines.length, count);
    assert.deepEqual(lines.map((line) => line.custom_id), Array.from({ length: count }, (_, n) => `req-${String(n + 1).padStart(width, "0")}`));
    assert.equal(new Set(lines.map((line) => line.custom_id)).size, count, "custom_id is unique");
    for (const line of lines) {
      // The server's per-line rules: an object with a non-empty string
      // custom_id, method POST, a known url and a body naming the model.
      assert.deepEqual(Object.keys(line).sort(), ["body", "custom_id", "method", "url"]);
      assert.equal(line.method, "POST");
      assert.equal(line.url, endpoint);
      assert.equal(line.body.model, "m");
      assert.equal(line.body.stream, undefined, "no stream: batches refuse streaming requests");
      if (endpoint === "/v1/chat/completions") {
        assert.deepEqual(line.body.messages.map((m: { role: string }) => m.role), ["user"]);
        assert.equal(line.body.max_tokens, SAMPLE_MAX_TOKENS);
      } else if (endpoint === "/v1/completions") {
        assert.equal(typeof line.body.prompt, "string");
        assert.equal(line.body.max_tokens, SAMPLE_MAX_TOKENS);
      } else {
        assert.equal(typeof line.body.input, "string");
      }
    }
    assert.ok(Buffer.byteLength(text) < 8 << 20, "the file stays well under the 200 MB upload limit");
    assert.match(sampleFileName(endpoint), /^batch-sample-[a-z-]+\.jsonl$/);
  }
});

test("an embeddings sample embeds ten thousand distinct texts, each two prompts", () => {
  const texts = embeddingInputs();
  assert.equal(texts.length, SAMPLE_EMBEDDING_LINES);
  assert.equal(new Set(texts).size, SAMPLE_EMBEDDING_LINES, "no two texts are the same");
  assert.deepEqual(embeddingInputs(50), embeddingInputs(50), "the same seed gives the same texts");
  const prompts = samplePrompts(SAMPLE_EMBEDDING_LINES);
  for (const [n, text] of texts.slice(0, 50).entries()) {
    const [first, second] = text.split("\n");
    assert.equal(first, prompts[n]);
    assert.ok(prompts.includes(second), "the second half is a prompt of the sample too");
  }
});

test("a sample is deterministic and applies the chosen model", () => {
  assert.equal(batchSampleJSONL("/v1/chat/completions", "qwen"), batchSampleJSONL("/v1/chat/completions", "qwen"));
  const a = JSON.parse(batchSampleJSONL("/v1/chat/completions", "qwen").split("\n")[0]);
  const b = JSON.parse(batchSampleJSONL("/v1/chat/completions", "llama").split("\n")[0]);
  assert.equal(a.body.model, "qwen");
  assert.equal(b.body.model, "llama");
  assert.deepEqual(a.body.messages, b.body.messages, "the model changes, the prompts do not");
  assert.notDeepEqual(samplePrompts(20, 1), samplePrompts(20, 2), "the seed drives the prompts");
});

test("a sample mixes distinct, long-answer prompts in English and Chinese", () => {
  const prompts = samplePrompts();
  assert.equal(prompts.length, SAMPLE_LINES);
  assert.equal(new Set(prompts).size, SAMPLE_LINES, "no two prompts are the same");
  assert.ok(SAMPLE_TASK_TYPES >= 8, "at least eight kinds of task");
  // Request n uses kind n mod SAMPLE_TASK_TYPES, so the first round shows every kind.
  assert.equal(new Set(prompts.slice(0, SAMPLE_TASK_TYPES).map((p) => p.slice(0, 12))).size, SAMPLE_TASK_TYPES);
  const cjk = prompts.filter((p) => /\p{Script=Han}/u.test(p)).length;
  assert.ok(cjk >= 400 && cjk <= 600, `about half the prompts are Chinese (got ${cjk})`);
  for (const p of prompts) assert.ok(p.length >= 30 && p.length <= 400, `a prompt of reasonable length: ${p}`);
});

test("the sample generator ships small: at most 20 KB minified and gzipped", async () => {
  const { transformSync } = await import("esbuild");
  const { readFileSync } = await import("node:fs");
  const { gzipSync } = await import("node:zlib");
  const source = readFileSync(new URL("../src/pages/batches/batchSample.ts", import.meta.url), "utf8");
  const { code } = transformSync(source, { loader: "ts", minify: true, format: "esm" });
  const gzipped = gzipSync(code, { level: 9 }).length;
  assert.ok(gzipped <= 20 * 1024, `the generator is ${gzipped} bytes gzipped`);
});

test("the file picker takes only .jsonl", async () => {
  const user = mountPage();
  const dialog = await openCreate(user);
  const input = dialog.querySelector("input[type=file]") as HTMLInputElement;
  assert.equal(input.accept, ".jsonl");
  assert.ok(dialog.textContent?.includes(b.create.fileHelp));
});

test("use the sample: it names an active deployment and goes up as the request file", async () => {
  let sent: { file: File; endpoint?: string } | null = null;
  batchApi.create = (async (file: File, options?: { endpoint?: string }) => {
    sent = { file, endpoint: options?.endpoint };
    return batch();
  }) as never;
  const user = mountPage();
  const dialog = await openCreate(user);
  // The model list holds the active deployments' served names, once each.
  await waitFor(() => assert.ok(dialog.textContent?.includes("qwen-chat")));
  assert.ok(!dialog.textContent?.includes("stopped-one"));
  const submit = within(dialog).getByRole("button", { name: b.create.submit }) as HTMLButtonElement;
  assert.equal(submit.disabled, true, "nothing to create until a file is chosen");
  await user.click(screen.getByRole("button", { name: b.create.sample.use }));
  await waitFor(() => assert.equal(submit.disabled, false));
  await user.click(submit);
  await waitFor(() => assert.ok(sent));
  const got = sent as unknown as { file: File; endpoint?: string };
  assert.equal(got.endpoint, undefined, "the server reads the endpoint from the file");
  assert.equal(got.file.name, "batch-sample-chat-completions.jsonl");
  const text = await got.file.text();
  assert.equal(JSON.parse(text.split("\n")[0]).body.model, "qwen-chat");
});

test("the sample type picks the endpoint of the sample, and the file goes up without one", async () => {
  let sent: { file: File; endpoint?: string } | null = null;
  batchApi.create = (async (file: File, options?: { endpoint?: string }) => {
    sent = { file, endpoint: options?.endpoint };
    return batch();
  }) as never;
  const user = mountPage();
  const dialog = await openCreate(user);
  await waitFor(() => assert.ok(dialog.textContent?.includes("qwen-chat")));
  within(dialog).getByRole("combobox", { name: b.create.sample.endpoint }).focus();
  await user.keyboard("{Enter}");
  for (const key of ["chat", "completions"] as const) {
    assert.ok(await screen.findByRole("option", { name: b.create.sample.endpoints[key].replace("{{endpoint}}", key === "chat" ? "/v1/chat/completions" : "/v1/completions") }));
  }
  await user.click(await screen.findByRole("option", { name: b.create.sample.endpoints.embeddings.replace("{{endpoint}}", "/v1/embeddings") }));
  await user.click(screen.getByRole("button", { name: b.create.sample.use }));
  await user.click(within(dialog).getByRole("button", { name: b.create.submit }));
  await waitFor(() => assert.ok(sent));
  const got = sent as unknown as { file: File; endpoint?: string };
  assert.equal(got.endpoint, undefined);
  assert.equal(got.file.name, "batch-sample-embeddings.jsonl");
  const first = JSON.parse((await got.file.text()).split("\n")[0]);
  assert.equal(first.url, "/v1/embeddings");
  assert.equal(typeof first.body.input, "string");
  assert.equal(first.body.model, "embedder", "the embeddings sample names the model that serves embeddings");
});

test("the sample offers only the models serving its endpoint, in the server's order", async () => {
  // The server lists an embedding model first (it runs; the chat model only
  // starts): a chat sample still names the chat model, never the embedder.
  batchApi.dictionary = async () => dictionary([model("BAAI/bge-m3", "running", ["/v1/embeddings"]), model("glm", "starting"), model("deepseek", "stopped")]);
  const user = mountPage();
  const dialog = await openCreate(user);
  await user.click(within(dialog).getByRole("checkbox", { name: b.create.model.showWaiting }));
  const sampleModel = await within(dialog).findByRole("combobox", { name: b.create.sample.model });
  await waitFor(() => assert.ok(within(dialog).getByText(`glm · ${b.create.model.state.starting}`, { selector: '[data-slot="select-value"]' })));
  sampleModel.focus();
  await user.keyboard("{Enter}");
  const offered = (await screen.findAllByRole("option")).map((option) => option.getAttribute("title") ?? option.textContent);
  assert.ok(!offered.some((label) => label?.includes("BAAI/bge-m3")), `the embedder is not offered for chat: ${offered}`);
  assert.ok(await screen.findByRole("option", { name: `deepseek · ${b.create.model.state.stopped}` }));
});

test("download the sample hands the browser a .jsonl file", async () => {
  const created: Blob[] = [];
  let clicked = "";
  const urlApi = dom.window.URL as unknown as { createObjectURL: (b: Blob) => string; revokeObjectURL: (u: string) => void };
  const before = { create: urlApi.createObjectURL, revoke: urlApi.revokeObjectURL };
  urlApi.createObjectURL = (blob: Blob) => { created.push(blob); return "blob:sample"; };
  urlApi.revokeObjectURL = () => {};
  Object.defineProperty(globalThis, "URL", { value: dom.window.URL, configurable: true });
  const anchorClick = dom.window.HTMLAnchorElement.prototype.click;
  dom.window.HTMLAnchorElement.prototype.click = function (this: HTMLAnchorElement) { clicked = this.download; };
  try {
    const user = mountPage();
    const dialog = await openCreate(user);
    await waitFor(() => assert.ok(dialog.textContent?.includes("qwen-chat")));
    await user.click(screen.getByRole("button", { name: b.create.sample.download }));
    assert.equal(clicked, "batch-sample-chat-completions.jsonl");
    assert.equal(created.length, 1);
    assert.equal((await created[0].text()).trimEnd().split("\n").length, SAMPLE_LINES);
  } finally {
    dom.window.HTMLAnchorElement.prototype.click = anchorClick;
    urlApi.createObjectURL = before.create;
    urlApi.revokeObjectURL = before.revoke;
  }
});

test("the page says batches are an OpenAI-compatible API and shows the calls", async () => {
  const user = mountPage();
  const header = await screen.findByRole("button", { name: new RegExp(b.api.title.replace(/[()/]/g, ".")) });
  await user.click(header);
  await waitFor(() => assert.ok(screen.getByText(b.api.steps.upload)));
  const link = screen.getByRole("link", { name: b.api.keys }) as HTMLAnchorElement;
  assert.equal(link.getAttribute("href"), "/apikeys");
  const steps = batchAPISteps("http://gw.example");
  assert.deepEqual(steps.map((s) => s.key), ["upload", "create", "poll", "download", "sdk"]);
  assert.ok(steps[0].code.includes("http://gw.example/v1/files") && steps[0].code.includes("purpose=batch"));
  assert.ok(steps[1].code.includes("/v1/batches") && steps[1].code.includes('"completion_window":"24h"'));
  assert.ok(steps[4].code.includes('base_url="http://gw.example/v1"'));
});
