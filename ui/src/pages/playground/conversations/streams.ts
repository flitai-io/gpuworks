import { streamChat } from "../api";
import { newStreamTracker, recordDelta, recordUsage, streamMetrics, type StreamTracker } from "../streamMetrics";
import type { ChatMessage, StreamOptionDefinition } from "../types";
import type { ConversationColumn } from "./model";
import { historyMessages } from "./history";

export const ABORTED_ERROR = "__playground_aborted__";

interface Stream {
  conversationID: string;
  messageID: string;
  controller: AbortController;
  tracker: StreamTracker;
  content: string;
  reasoning: string;
  priorityClass?: string;
}

/** StreamHost is the store side of the replies being generated. */
export interface StreamHost {
  clock(): number;
  patch(conversationID: string, messageID: string, patch: (message: ChatMessage) => ChatMessage, save: "now" | "throttled"): void;
  changed(running: number): void;
}

/**
 * Streams runs the replies this page is generating. Deltas are merged into
 * the conversation once per frame; metrics are republished a few times a
 * second (also while the tab is hidden and frames do not run).
 */
export class Streams {
  private readonly streams = new Map<string, Stream>();
  private ticker?: ReturnType<typeof setInterval>;
  private frame?: unknown;

  constructor(private readonly host: StreamHost) {}

  get size() { return this.streams.size; }

  conversations(): Set<string> {
    return new Set([...this.streams.values()].map((stream) => stream.conversationID));
  }

  countIn(conversationID: string): number {
    return [...this.streams.values()].filter((stream) => stream.conversationID === conversationID).length;
  }

  abortConversations(ids: Set<string>) {
    for (const stream of this.streams.values()) if (ids.has(stream.conversationID)) stream.controller.abort();
  }

  abortColumn(conversationID: string, column: ConversationColumn) {
    for (const stream of this.streams.values()) {
      if (stream.conversationID === conversationID && column.messages.some((message) => message.id === stream.messageID)) stream.controller.abort();
    }
  }

  /** flushAll merges every pending delta into its conversation now. */
  flushAll() {
    for (const stream of this.streams.values()) this.flush(stream);
  }

  /** clear aborts every reply without recording anything (sign-out, disposal). */
  clear() {
    const all = [...this.streams.values()];
    this.streams.clear();
    for (const stream of all) stream.controller.abort();
    this.tick();
  }

  /** start generates `reply` from the column's history (which ends with the new question). */
  async start(conversationID: string, column: ConversationColumn, reply: ChatMessage, streamOptions: StreamOptionDefinition[]) {
    const stream: Stream = {
      conversationID, messageID: reply.id, controller: new AbortController(), tracker: newStreamTracker(this.host.clock()), content: "", reasoning: "",
    };
    this.streams.set(reply.id, stream);
    this.tick();
    try {
      const result = await streamChat(column.model, historyMessages(column), column.parameters, streamOptions, {
        onContent: (delta) => { stream.content += delta; this.scheduleFrame(); },
        onReasoning: (delta) => { stream.reasoning += delta; this.scheduleFrame(); },
        onUsage: (usage) => recordUsage(stream.tracker, usage),
        onDelta: () => recordDelta(stream.tracker, this.host.clock()),
        onPriorityClass: (name) => { stream.priorityClass = name; },
      }, stream.controller.signal);
      if (result.usage) recordUsage(stream.tracker, result.usage);
      this.finish(stream, undefined);
    } catch (error) {
      this.finish(stream, (error as Error).name === "AbortError" ? ABORTED_ERROR : (error as Error).message);
    }
  }

  private finish(stream: Stream, error: string | undefined) {
    if (this.streams.get(stream.messageID) !== stream) return;
    this.streams.delete(stream.messageID);
    this.flush(stream);
    const now = this.host.clock();
    const metrics = streamMetrics(stream.tracker, now, !error);
    this.host.patch(stream.conversationID, stream.messageID, (message) => ({
      ...message,
      streaming: false,
      owner: undefined,
      ...(stream.priorityClass ? { priorityClass: stream.priorityClass } : {}),
      ...(error ? { error, metrics: { ...metrics, durationMS: now - stream.tracker.startedAt } } : { usage: stream.tracker.usage, metrics }),
    }), "now");
    this.tick();
  }

  private tick() {
    if (this.streams.size && this.ticker === undefined) this.ticker = setInterval(() => this.publish(true), 250);
    if (!this.streams.size && this.ticker !== undefined) {
      clearInterval(this.ticker);
      this.ticker = undefined;
    }
    this.host.changed(this.streams.size);
  }

  private scheduleFrame() {
    if (this.frame !== undefined) return;
    const run = () => { this.frame = undefined; this.publish(false); };
    this.frame = typeof requestAnimationFrame === "function" ? requestAnimationFrame(run) : setTimeout(run, 16);
  }

  private publish(withMetrics: boolean) {
    for (const stream of this.streams.values()) {
      this.flush(stream);
      if (withMetrics && stream.tracker.firstDeltaAt !== undefined) {
        const metrics = streamMetrics(stream.tracker, this.host.clock(), false);
        this.host.patch(stream.conversationID, stream.messageID, (message) => ({ ...message, metrics }), "throttled");
      }
    }
  }

  private flush(stream: Stream) {
    const { content, reasoning } = stream;
    if (!content && !reasoning) return;
    stream.content = "";
    stream.reasoning = "";
    this.host.patch(stream.conversationID, stream.messageID, (message) => ({
      ...message,
      content: message.content + content,
      reasoning: reasoning ? (message.reasoning || "") + reasoning : message.reasoning,
    }), "throttled");
  }
}
