import assert from "node:assert/strict";
import { createServer, type ServerResponse } from "node:http";
import type { Socket } from "node:net";
import { once } from "node:events";
import { afterEach, test } from "node:test";
import { chatCatalog, chatTree, dom, i18n, originalFetch, testStore } from "./helpers/playgroundChat.tsx";
import fs from "node:fs";

const { cleanup, render, screen, waitFor } = await import("@testing-library/react");
const userEvent = (await import("@testing-library/user-event")).default;
const PATH = "/api/v1/playground/v1/chat/completions";
const PARTIAL = "partial answer from native HTTP";
const REFUSAL = "The requested model is unavailable";
const frame = (value: unknown) => `data: ${JSON.stringify(value)}\n\n`;
afterEach(() => { cleanup(); globalThis.fetch = originalFetch; dom.window.sessionStorage.clear(); });

for (const terminal of ["socket-reset", "done", "premature-eof", "error-frame", "http-refusal", "cancel"] as const) {
  for (const language of terminal === "socket-reset" ? ["en", "zh"] as const : ["en"] as const) {
    test(`chat page native HTTP ${terminal} (${language})`, { timeout: 60000 }, async (t) => {
      const translations = JSON.parse(fs.readFileSync(new URL(`../src/i18n/locales/${language}/playground.json`, import.meta.url), "utf8"));
      i18n.addResourceBundle(language, "playground", translations);
      await i18n.changeLanguage(language);
      const sockets = new Set<Socket>();
      const serverErrors: unknown[] = [];
      const handlers = new Set<Promise<void>>();
      let response: ServerResponse | undefined;
      let requestBody: unknown;
      const server = createServer((request, reply) => {
        const task = (async () => {
          assert.equal(request.method, "POST");
          assert.equal(request.url, PATH);
          const chunks: Buffer[] = [];
          for await (const chunk of request) chunks.push(Buffer.from(chunk));
          requestBody = JSON.parse(Buffer.concat(chunks).toString());
          response = reply;
          if (terminal === "http-refusal") {
            reply.writeHead(503, { "Content-Type": "application/json" });
            reply.end(JSON.stringify({ error: { message: REFUSAL } }));
            return;
          }
          reply.writeHead(200, { "Content-Type": "text/event-stream" });
          reply.write(frame({ choices: [{ index: 0, delta: { content: PARTIAL }, finish_reason: null }] }));
        })().catch((error) => { serverErrors.push(error); reply.destroy(); });
        handlers.add(task);
        void task.finally(() => handlers.delete(task));
      });
      server.on("connection", (socket) => { sockets.add(socket); socket.on("close", () => sockets.delete(socket)); });
      server.listen(0, "127.0.0.1");
      await once(server, "listening");
      const address = server.address();
      assert.ok(address && typeof address !== "string");
      globalThis.fetch = (input, init) => String(input) === "/api/v1/auth/me"
        ? Promise.resolve(Response.json({ id: 1, username: "admin", role: "admin" }))
        : String(input) === "/api/v1/priority-classes"
          ? Promise.resolve(Response.json({ items: [], min_value: -100, max_value: 100, policies: [], default_policy: "", ceiling_policies: [] }))
        : originalFetch(new URL(String(input), `http://127.0.0.1:${address.port}`), init);
      try {
        const user = userEvent.setup({ delay: null });
        render(chatTree(chatCatalog(), testStore().store));
        await user.type(await screen.findByRole("textbox", { name: translations.chat.inputAria }), "exercise native stream termination");
        await user.click(screen.getByRole("button", { name: new RegExp(translations.chat.send) }));
        if (terminal !== "http-refusal") {
          await screen.findByText(PARTIAL);
          assert.ok(response && !response.writableEnded, "partial answer arrived before body ended");
          if (terminal === "socket-reset") response.destroy();
          else if (terminal === "cancel") await user.click(screen.getByRole("button", { name: new RegExp(translations.pane.stop) }));
          else {
            if (terminal === "done") response.write("data: [DONE]\n\n");
            if (terminal === "error-frame") response.write(frame({ error: { message: REFUSAL } }));
            response.end();
          }
        }
        if (terminal === "done") {
          await screen.findByRole("button", { name: translations.pane.copyReply });
          assert.equal(screen.queryByRole("alert"), null);
        } else {
          const expected = terminal === "cancel" ? translations.pane.generationStopped
            : ["error-frame", "http-refusal"].includes(terminal) ? REFUSAL : translations.pane.streamInterrupted;
          await waitFor(() => assert.equal(screen.getByRole("alert").textContent, expected));
          if (terminal !== "http-refusal") assert.ok(screen.getByText(PARTIAL));
          assert.equal(document.body.textContent?.includes("terminated"), false);
          assert.equal(document.body.textContent?.includes("network error"), false);
        }
        assert.equal((requestBody as { model: string }).model, "chat-model");
        assert.deepEqual(serverErrors, []);
        t.diagnostic(JSON.stringify({ terminal, language, partialVisible: terminal !== "http-refusal", nativeHTTP: true }));
      } finally {
        cleanup();
        response?.destroy();
        const closed = new Promise<void>((resolve) => server.close(() => resolve()));
        server.closeAllConnections();
        for (const socket of sockets) socket.destroy();
        await Promise.all(handlers);
        await closed;
        globalThis.fetch = originalFetch;
        await i18n.changeLanguage("en");
        assert.equal(server.listening, false);
      }
    });
  }
}
