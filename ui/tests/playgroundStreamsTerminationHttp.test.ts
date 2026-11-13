/** Phase4 probes: actual Streams/history/metrics; native HTTP, no product edits. */
import assert from 'node:assert/strict';
import { createServer, type ServerResponse } from 'node:http';
import type { Socket } from 'node:net';
import { once } from 'node:events';
import test from 'node:test';
import { Streams, type StreamHost } from '../src/pages/playground/conversations/streams';
import { historyMessages } from '../src/pages/playground/conversations/history';
import type { ConversationColumn } from '../src/pages/playground/conversations/model';
import type { ChatMessage } from '../src/pages/playground/types';

const PATH = '/api/v1/playground/v1/chat/completions';
const PARTIAL = 'partial answer';
const ERROR = 'local upstream stream failure';
const USAGE = { prompt_tokens: 16, completion_tokens: 8, total_tokens: 24 };
const frame = (value: unknown) => `data: ${JSON.stringify(value)}\n\n`;
const PARAMETERS = { enabled: {}, values: {}, systemPrompt: '', extraJSON: '' };

for (const terminal of ['done', 'error-frame', 'premature-eof'] as const) {
  test(`actual Streams native HTTP: ${terminal}`, { timeout: 2500, concurrency: false }, async (t) => {
    const firstPatched = Promise.withResolvers<void>();
    const column: ConversationColumn = { id: 'column-local', model: 'local-model', parameters: PARAMETERS,
      messages: [
        { id: 'question-local', role: 'user', content: 'local termination probe' },
        { id: 'reply-local', role: 'assistant', content: '', streaming: true, owner: 'page-local' },
      ] };
    const getReply = () => column.messages.find((message) => message.id === 'reply-local')!;
    const saved: { save: string; message: ChatMessage }[] = [];
    const counts: number[] = [];
    let observedPartialWhileOpen = false;
    let sawPartial = false;
    let sentEnd = false;
    const host: StreamHost = {
      clock: () => performance.now(),
      patch(conversationID, messageID, update, save) {
        assert.equal(conversationID, 'conversation-local');
        assert.equal(messageID, 'reply-local');
        const before = getReply();
        const after = update(before); // Only the production callback determines state.
        column.messages = column.messages.map((message) => message.id === messageID ? after : message);
        saved.push({ save, message: structuredClone(after) });
        // Only the first partial patch says whether the body was still open: the
        // 250ms metrics tick patches the same content with streaming still true, and
        // on a loaded machine it can land after the server has ended the response.
        if (!sawPartial && after.content === PARTIAL && after.streaming === true) {
          sawPartial = true;
          observedPartialWhileOpen = !sentEnd;
          firstPatched.resolve();
        }
      },
      changed: (running) => { counts.push(running); },
    };
    const streams = new Streams(host);
    const sockets = new Set<Socket>();
    const responses = new Set<ServerResponse>();
    const handlers = new Set<Promise<void>>();
    const serverErrors: unknown[] = [];
    const requests: unknown[] = [];
    let active = 0;
    let stopping = false;
    const interrupt = () => { stopping = true; firstPatched.resolve(); streams.clear(); };
    t.signal.addEventListener('abort', interrupt, { once: true });
    const server = createServer((request, response) => {
      active += 1;
      responses.add(response);
      const task = (async () => {
        assert.equal(request.method, 'POST');
        assert.equal(request.url, PATH);
        assert.equal(request.headers['content-type'], 'application/json');
        const chunks: Buffer[] = [];
        for await (const chunk of request) chunks.push(Buffer.from(chunk));
        const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
        requests.push(body);
        assert.deepEqual(body, { model: 'local-model', messages: [
          { role: 'user', content: 'local termination probe' },
        ], stream: true });
        response.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' });
        response.write(frame({ id: 'local-stream', choices: [{ index: 0, delta: { content: PARTIAL }, finish_reason: null }] }));
        await firstPatched.promise;
        if (stopping) return;
        if (terminal === 'done') {
          response.write(frame({ id: 'local-stream', choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] }));
          response.write(frame({ choices: [], usage: USAGE }));
          response.write('data: [DONE]\n\n');
        } else if (terminal === 'error-frame') {
          response.write(frame({ error: { message: ERROR } }));
        }
        sentEnd = true;
        response.end();
      })().catch((error) => {
        serverErrors.push(error);
        response.destroy(error as Error);
      }).finally(() => { active -= 1; responses.delete(response); });
      handlers.add(task);
      void task.finally(() => handlers.delete(task));
    });
    server.on('connection', (socket) => {
      sockets.add(socket);
      socket.on('close', () => sockets.delete(socket));
    });
    const nativeFetch = globalThis.fetch;
    try {
      server.listen(0, '127.0.0.1');
      await once(server, 'listening');
      const address = server.address();
      assert.ok(address && typeof address !== 'string');
      const base = `http://127.0.0.1:${address.port}`;
      globalThis.fetch = (input, init) => {
        assert.equal(input, PATH);
        return nativeFetch(new URL(input as string, base), init);
      };
      await streams.start('conversation-local', column, getReply(), []);
      await Promise.all(handlers);
      assert.deepEqual(serverErrors, []);
      assert.equal(requests.length, 1);
      assert.equal(observedPartialWhileOpen, true);
      assert.equal(streams.size, 0);
      assert.equal(counts[0], 1);
      assert.equal(counts.at(-1), 0);
      const reply = getReply();
      assert.equal(reply.content, PARTIAL);
      assert.equal(reply.streaming, false);
      assert.equal(reply.owner, undefined);
      assert.equal(saved.at(-1)?.save, 'now');
      assert.ok(reply.metrics && reply.metrics.durationMS >= 0);
      // One content chunk has no observable decode interval: do not invent TPS/TPOT.
      assert.equal(reply.metrics.tpotMS, undefined);
      assert.equal(reply.metrics.tps, undefined);
      const history = historyMessages(column);
      t.diagnostic(JSON.stringify({ terminal, message: reply, history,
        patchModes: saved.map((record) => record.save), counts,
        partialPatchedWhileBodyOpen: observedPartialWhileOpen, activeHandlers: active }));
      if (terminal === 'done') {
        assert.equal(reply.error, undefined);
        assert.equal(reply.interrupted, undefined);
        assert.deepEqual(reply.usage, USAGE);
        assert.equal(reply.metrics.outputTokens, 8);
        assert.equal(reply.metrics.approximate, undefined);
        assert.deepEqual(history, [
          { role: 'user', content: 'local termination probe' },
          { role: 'assistant', content: PARTIAL },
        ]);
      } else {
        assert.equal(reply.usage, undefined);
        assert.equal(reply.metrics.outputTokens, 1); // Existing tracker: one chunk, approximate.
        assert.equal(reply.metrics.approximate, true);
        if (terminal === 'error-frame') assert.equal(reply.error, ERROR);
        assert.ok(reply.error || reply.interrupted,
          'premature EOF must not leave actual Streams assistant error/interrupted absent');
        if (terminal === 'premature-eof') assert.equal(reply.error, '__playground_stream_interrupted__');
        assert.deepEqual(history, [{ role: 'user', content: 'local termination probe' }],
          'failed partial assistant must not become context for the next request');
      }
    } finally {
      globalThis.fetch = nativeFetch;
      t.signal.removeEventListener('abort', interrupt);
      interrupt();
      for (const response of responses) response.destroy();
      const closed = new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
      server.closeAllConnections();
      for (const socket of sockets) socket.destroy();
      await Promise.all(handlers);
      await closed;
      assert.equal(active, 0);
      assert.equal(streams.size, 0);
      assert.equal(server.listening, false);
      t.diagnostic(JSON.stringify({ cleanup: true, activeHandlers: active, activeStreams: streams.size,
        listening: server.listening, remainingOpenSockets: [...sockets].filter((socket) => !socket.destroyed).length }));
    }
  });
}
