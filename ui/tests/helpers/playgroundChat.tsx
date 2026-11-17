// Shared harness for playground chat tests: jsdom + i18n, a one-model
// catalog, hand-fed SSE streams and a fetch stub that serves them in order.
// Import it first (statically); it installs the globals antd needs.
import React from "react";
import { afterEach } from "node:test";
import { IDBFactory } from "fake-indexeddb";
import { I18nextProvider } from "react-i18next";
import { createI18n, dom } from "./dom.ts";
import type { PlaygroundCatalog } from "../../src/pages/playground/types.ts";
import { openConversationDB } from "../../src/pages/playground/conversations/db.ts";
import { ConversationStore, type StoreOptions } from "../../src/pages/playground/conversations/store.ts";
import type { PeerChannel, SyncMessage } from "../../src/pages/playground/conversations/sync.ts";
import { ConversationStoreContext } from "../../src/pages/playground/conversations/useConversations.ts";

Object.defineProperty(globalThis, "sessionStorage", { value: dom.window.sessionStorage, configurable: true });
Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
Object.defineProperty(dom.window, "requestAnimationFrame", { configurable: true, value: (callback: FrameRequestCallback) => dom.window.setTimeout(() => callback(performance.now()), 0) });
Object.defineProperty(dom.window, "cancelAnimationFrame", { configurable: true, value: (id: number) => dom.window.clearTimeout(id) });
Object.defineProperty(globalThis, "requestAnimationFrame", { configurable: true, value: dom.window.requestAnimationFrame });
Object.defineProperty(globalThis, "cancelAnimationFrame", { configurable: true, value: dom.window.cancelAnimationFrame });

export { dom };
export const { i18n, locale } = await createI18n(["playground"]);
const { MemoryRouter, Route, Routes, useLocation } = await import("react-router-dom");
const { render, screen } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const { default: ChatPage } = await import("../../src/pages/playground/ChatPage.tsx");
export const originalFetch = globalThis.fetch;

/**
 * peerHub stands in for BroadcastChannel between the pages of one browser:
 * a message reaches every other page on the hub, asynchronously, never the
 * sender.
 */
export function peerHub() {
  const handlers = new Map<object, (message: SyncMessage) => void>();
  const sent: SyncMessage[] = [];
  return {
    sent,
    channel(): PeerChannel {
      const self = {};
      return {
        post(message) {
          sent.push(message);
          for (const [peer, handler] of handlers) if (peer !== self) setTimeout(() => handler(structuredClone(message)), 0);
        },
        listen(handler) { handlers.set(self, handler); },
        close() { handlers.delete(self); },
      };
    },
  };
}

const stores: ConversationStore[] = [];

/**
 * testStore is one page's conversation store over a browser's IndexedDB
 * (fake-indexeddb, fresh unless one is passed) and pages hub. Probes and
 * throttled saves are shortened; tests that time them use mock timers.
 */
export function testStore(options: StoreOptions & { idb?: IDBFactory; hub?: ReturnType<typeof peerHub> } = {}) {
  const idb = options.idb ?? new IDBFactory();
  const hub = options.hub ?? peerHub();
  const store = new ConversationStore({
    openDB: () => openConversationDB(idb),
    peers: () => hub.channel(),
    session: dom.window.sessionStorage,
    probeWaitMS: 20,
    saveIntervalMS: 10,
    ...options,
  });
  stores.push(store);
  return { store, idb, hub };
}

afterEach(() => {
  for (const store of stores.splice(0)) store.dispose();
});

export function chatCatalog(overrides: Partial<PlaygroundCatalog> = {}): PlaygroundCatalog {
  return {
    models: [{
      id: "chat-model", label: "chat-model", capability: "chat", sleeping: false, public_data_plane: true,
      operations: [{ type: "openai.chat", endpoint: "/v1/chat/completions", renderer: "chat", response: "sse", mode: "stream", request_encodings: ["json"] }],
    }],
    parameters: [],
    // As GET /api/v1/playground/models serves them.
    stream_options: [
      { name: "include_usage", label: { en: "Report usage (include_usage)", zh: "返回用量（include_usage）" }, description: { en: "Return prompt and completion token counts in the last chunk.", zh: "在流的最后一个块返回 token 数。" }, default: true },
      { name: "continuous_usage_stats", label: { en: "Usage on every chunk (continuous_usage_stats)", zh: "逐 chunk 返回用量（continuous_usage_stats）" }, description: { en: "Running token counts on every chunk.", zh: "每个 chunk 都带累计 token 数。" }, default: true, requires: "include_usage" },
    ],
    html_preview: { allow_network: false },
    ...overrides,
  };
}

/**
 * controllableSSE is a chat completion stream the test feeds by hand, so it
 * can look at the page while a reply is still being generated.
 */
export function controllableSSE() {
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({ start(c) { controller = c; } });
  return {
    response: new Response(body, { headers: { "Content-Type": "text/event-stream" } }),
    push(content: string) { controller.enqueue(encoder.encode(`data: ${JSON.stringify({ choices: [{ delta: { content } }] })}\n\n`)); },
    // frame sends one raw chunk, e.g. a delta with continuous usage or the
    // final usage-only chunk.
    frame(value: Record<string, unknown>) { controller.enqueue(encoder.encode(`data: ${JSON.stringify(value)}\n\n`)); },
    end() { controller.enqueue(encoder.encode("data: [DONE]\n\n")); controller.close(); },
    // abort is what fetch does to the body when the request's signal fires.
    abort() { try { controller.error(new DOMException("The operation was aborted.", "AbortError")); } catch { /* already closed */ } },
  };
}

/**
 * serveStreams answers chat completions with the given streams in order and
 * /api/v1/auth/me with a user of the given role.
 */
export function serveStreams(streams: ReturnType<typeof controllableSSE>[], { role = "admin" }: { role?: string } = {}) {
  let next = 0;
  const bodies: Record<string, unknown>[] = [];
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url.endsWith("/api/v1/auth/me")) return new Response(JSON.stringify({ id: 1, username: role, role }), { headers: { "Content-Type": "application/json" } });
    if (!url.endsWith("/v1/chat/completions")) throw new Error(`unexpected request ${url}`);
    bodies.push(JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>);
    const stream = streams[next++];
    if (init?.signal?.aborted) throw new DOMException("The operation was aborted.", "AbortError");
    init?.signal?.addEventListener("abort", () => stream.abort(), { once: true });
    return stream.response;
  }) as typeof fetch;
  return bodies;
}

function Where() {
  const location = useLocation();
  return <span data-testid="where">{location.pathname + location.hash}</span>;
}

export function chatTree(catalog: PlaygroundCatalog, store: ConversationStore, path = "/playground/chat") {
  return (
    <ConversationStoreContext.Provider value={store}>
      <I18nextProvider i18n={i18n}>
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path="/playground/chat" element={<ChatPage catalog={catalog} onCatalogChanged={() => undefined} />} />
            <Route path="*" element={<Where />} />
          </Routes>
        </MemoryRouter>
      </I18nextProvider>
    </ConversationStoreContext.Provider>
  );
}

/** renderChat shows the chat page on a page store of its own (or the given one) once its conversations are loaded. */
export async function renderChat(catalog: PlaygroundCatalog = chatCatalog(), { store = testStore().store, path }: { store?: ConversationStore; path?: string } = {}) {
  const user = userEvent.setup();
  const view = render(chatTree(catalog, store, path));
  await screen.findByRole("textbox", { name: locale.playground.chat.inputAria });
  return Object.assign(user, { view });
}

export async function sendPrompt(user: ReturnType<typeof userEvent.setup>, text: string) {
  await user.type(await screen.findByRole("textbox", { name: locale.playground.chat.inputAria }), text);
  await user.click(screen.getByRole("button", { name: new RegExp(locale.playground.chat.send) }));
}
