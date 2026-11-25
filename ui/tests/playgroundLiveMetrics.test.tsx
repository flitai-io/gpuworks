import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { chatCatalog, controllableSSE, dom, locale, originalFetch, renderChat, sendPrompt, serveStreams } from "./helpers/playgroundChat.tsx";

const { cleanup, screen, waitFor, within } = await import("@testing-library/react");
const pane = locale.playground.pane as Record<string, string>;
const parameters = locale.playground.parameters as Record<string, string>;

afterEach(() => {
  cleanup();
  dom.window.sessionStorage.clear();
  globalThis.fetch = originalFetch;
});

// metric reads one value of the reply's metrics row by its label.
function metric(label: string) {
  const row = document.querySelector(".playground-metrics");
  if (!row) return undefined;
  const cell = [...row.querySelectorAll("span")].find((span) => span.querySelector("small")?.textContent === label);
  return cell?.querySelector("strong")?.textContent ?? undefined;
}

const delta = (content: string, completion?: number) => ({ choices: [{ delta: { content } }], ...(completion === undefined ? {} : { usage: { prompt_tokens: 12, completion_tokens: completion, total_tokens: 12 + completion } }) });

test("metrics appear and update while the reply streams, from continuous usage", async () => {
  const stream = controllableSSE();
  const bodies = serveStreams([stream]);
  const user = await renderChat();
  await sendPrompt(user, "count");
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0].stream_options, { include_usage: true, continuous_usage_stats: true });
  stream.frame({ choices: [{ delta: { role: "assistant", content: "" } }], usage: { prompt_tokens: 12, completion_tokens: 0, total_tokens: 12 } });
  stream.frame(delta("one", 1));
  stream.frame(delta(" two", 2));
  stream.frame(delta(" three", 3));
  // Before the reply ends the row already shows exact running counts.
  await waitFor(() => assert.equal(metric("Token"), "3"));
  assert.notEqual(metric("TTFT"), "—");
  assert.notEqual(metric("Prefill"), "—");
  stream.frame(delta(" four", 4));
  await waitFor(() => assert.equal(metric("Token"), "4"));
  stream.frame({ choices: [], usage: { prompt_tokens: 12, completion_tokens: 4, total_tokens: 16 } });
  stream.end();
  await screen.findByRole("button", { name: pane.copyReply });
  assert.equal(metric("Token"), "4");
});

test("without usage on every chunk the running count is marked as an estimate", async () => {
  const stream = controllableSSE();
  serveStreams([stream]);
  const user = await renderChat();
  await sendPrompt(user, "count");
  stream.push("one");
  stream.push(" two");
  await waitFor(() => assert.equal(metric("Token"), "≈2"));
  // The final usage replaces the estimate.
  stream.frame({ choices: [], usage: { prompt_tokens: 12, completion_tokens: 5, total_tokens: 17 } });
  stream.end();
  await screen.findByRole("button", { name: pane.copyReply });
  assert.equal(metric("Token"), "5");
});

test("stream options show in the parameters panel and can be switched off", async () => {
  const first = controllableSSE();
  const second = controllableSSE();
  const bodies = serveStreams([first, second]);
  const user = await renderChat(chatCatalog());
  await user.click(screen.getByRole("button", { name: new RegExp(pane.parameters) }));
  const section = screen.getByText(parameters.streamOptions).closest(".playground-field") as HTMLElement;
  const usage = within(section).getByRole("checkbox", { name: /include_usage/ }) as HTMLInputElement;
  const continuous = within(section).getByRole("checkbox", { name: /continuous_usage_stats/ }) as HTMLInputElement;
  assert.equal(usage.checked, true);
  assert.equal(continuous.checked, true);

  await user.click(continuous);
  await sendPrompt(user, "first");
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0].stream_options, { include_usage: true });
  first.push("ok");
  first.end();
  await screen.findByRole("button", { name: pane.copyReply });

  // Without include_usage, continuous_usage_stats has no effect: it is shown
  // off and disabled, and the request carries no stream_options at all.
  await user.click(continuous);
  await user.click(usage);
  assert.equal(continuous.checked, false);
  assert.equal(continuous.disabled, true);
  await sendPrompt(user, "second");
  await waitFor(() => assert.equal(bodies.length, 2));
  assert.equal("stream_options" in bodies[1], false);
  second.end();
});

test("a conversation saved before stream options existed sends the defaults", async () => {
  dom.window.sessionStorage.setItem("gpuworks.playground.single.0", JSON.stringify({ version: 1, modelID: "chat-model", parameters: { enabled: {}, values: {}, systemPrompt: "", extraJSON: "" }, conversations: { "chat-model": [{ id: "u1", role: "user", content: "earlier" }, { id: "a1", role: "assistant", content: "reply" }] } }));
  const stream = controllableSSE();
  const bodies = serveStreams([stream]);
  const user = await renderChat();
  await sendPrompt(user, "hi");
  await waitFor(() => assert.equal(bodies.length, 1));
  assert.deepEqual(bodies[0].stream_options, { include_usage: true, continuous_usage_stats: true });
  assert.deepEqual(bodies[0].messages, [{ role: "user", content: "earlier" }, { role: "assistant", content: "reply" }, { role: "user", content: "hi" }]);
  stream.end();
});
