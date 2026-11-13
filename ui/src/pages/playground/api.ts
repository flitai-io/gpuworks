import { playgroundErrorText, readErrorBody, safeErrorText } from "./errors";
export { playgroundErrorText } from "./errors";
import { notifySessionExpired } from "../../session";
import type {
  Attachment,
  CompletionMessage,
  ParameterState,
  PlaygroundCatalog,
  PlaygroundOperation,
  StreamOptionDefinition,
  Usage,
} from "./types";

const ROOT = "/api/v1/playground";
export const STREAM_INTERRUPTED_ERROR = "__playground_stream_interrupted__";

export async function getPlaygroundCatalog(signal?: AbortSignal): Promise<PlaygroundCatalog> {
  const response = await fetch(`${ROOT}/models`, { credentials: "include", signal });
  if (!response.ok) throw new Error(await responseError(response));
  return response.json();
}

export function parameterBody(state: ParameterState): Record<string, unknown> {
  const output: Record<string, unknown> = {};
  for (const [name, enabled] of Object.entries(state.enabled)) {
    if (enabled) output[name] = state.values[name];
  }
  if (state.extraJSON.trim()) {
    const parsed = JSON.parse(state.extraJSON) as unknown;
    if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") {
      throw new Error("Extra parameters must be a JSON object");
    }
    Object.assign(output, parsed);
  }
  return output;
}

export function userContent(text: string, attachments: Attachment[]): unknown {
  if (!attachments.length) return text;
  const parts: Record<string, unknown>[] = [];
  if (text.trim()) parts.push({ type: "text", text });
  for (const attachment of attachments) {
    if (attachment.type === "image") {
      parts.push({ type: "image_url", image_url: { url: attachment.dataURL } });
    } else {
      const comma = attachment.dataURL.indexOf(",");
      parts.push({
        type: "input_audio",
        input_audio: {
          data: comma >= 0 ? attachment.dataURL.slice(comma + 1) : attachment.dataURL,
          format: audioFormat(attachment.name, attachment.dataURL),
        },
      });
    }
  }
  return parts;
}

function audioFormat(name: string, dataURL: string): string {
  const extension = name.split(".").pop()?.toLowerCase();
  if (extension && ["wav", "mp3", "m4a", "ogg", "webm"].includes(extension)) return extension;
  if (dataURL.startsWith("data:audio/wav")) return "wav";
  if (dataURL.startsWith("data:audio/mpeg")) return "mp3";
  return "wav";
}

interface StreamCallbacks {
  onContent: (delta: string) => void;
  onReasoning: (delta: string) => void;
  onUsage: (usage: Usage) => void;
  // Called for every chunk that carries content or reasoning text.
  onDelta: () => void;
}

export interface StreamResult {
  usage?: Usage;
}

// streamOptionsBody is the stream_options object of a chat request: every
// option switched on (or on by default) whose required option is on too.
// Nothing switched on leaves stream_options out of the request.
export function streamOptionsBody(parameters: ParameterState, definitions: StreamOptionDefinition[]): Record<string, boolean> | undefined {
  const on = (definition: StreamOptionDefinition) => parameters.streamOptions?.[definition.name] ?? definition.default;
  const byName = new Map(definitions.map((definition) => [definition.name, definition]));
  const output: Record<string, boolean> = {};
  for (const definition of definitions) {
    const required = definition.requires ? byName.get(definition.requires) : undefined;
    if (on(definition) && (!definition.requires || (required !== undefined && on(required)))) output[definition.name] = true;
  }
  return Object.keys(output).length ? output : undefined;
}

export function buildChatBody(
  model: string,
  messages: CompletionMessage[],
  parameters: ParameterState,
  streamOptions: StreamOptionDefinition[],
): Record<string, unknown> {
  const options = streamOptionsBody(parameters, streamOptions);
  return {
    ...parameterBody(parameters),
    model,
    messages,
    stream: true,
    ...(options ? { stream_options: options } : {}),
  };
}

export async function streamChat(
  model: string,
  messages: CompletionMessage[],
  parameters: ParameterState,
  streamOptions: StreamOptionDefinition[],
  callbacks: StreamCallbacks,
  signal: AbortSignal,
): Promise<StreamResult> {
  const requestBody = buildChatBody(model, messages, parameters, streamOptions);
  const response = await fetch(`${ROOT}/v1/chat/completions`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(requestBody),
    signal,
  });
  if (!response.ok) throw new Error(await responseError(response));
  if (!response.body) throw new Error("The browser did not receive a streaming response body");

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let usage: Usage | undefined;

  const consumeEvent = (event: string) => {
    const data = event
      .split("\n")
      .filter((line) => line.startsWith("data:"))
      .map((line) => line.slice(5).trimStart())
      .join("\n");
    if (!data) return false;
    if (data === "[DONE]") return true;
    let frame: Record<string, unknown>;
    try {
      frame = JSON.parse(data) as Record<string, unknown>;
    } catch {
      return false;
    }
    if (frame.error) {
      const error = frame.error as { message?: string };
      throw new Error(safeErrorText(typeof error.message === "string" ? error.message : "The upstream service returned a streaming error"));
    }
    const choice = Array.isArray(frame.choices) ? frame.choices[0] as Record<string, unknown> | undefined : undefined;
    const delta = choice?.delta as Record<string, unknown> | undefined;
    const content = typeof delta?.content === "string" ? delta.content : "";
    const reasoning = reasoningDelta(delta);
    if (content || reasoning) callbacks.onDelta();
    if (content) callbacks.onContent(content);
    if (reasoning) callbacks.onReasoning(reasoning);
    // Usage after the text of the same chunk: with continuous_usage_stats its
    // counts include that chunk's tokens.
    if (frame.usage && typeof frame.usage === "object") {
      usage = frame.usage as Usage;
      callbacks.onUsage(usage);
    }
    return false;
  };

  let failed = false;
  try {
    while (true) {
      const { done, value } = await reader.read();
      // Normalize after joining: CR and LF may arrive in different reads.
      buffer = (buffer + decoder.decode(value, { stream: !done })).replace(/\r\n/g, "\n");
      let boundary = buffer.indexOf("\n\n");
      while (boundary >= 0) {
        const event = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        if (consumeEvent(event)) return { usage };
        boundary = buffer.indexOf("\n\n");
      }
      if (done) {
        if (buffer.trim() && consumeEvent(buffer)) return { usage };
        throw new Error(STREAM_INTERRUPTED_ERROR);
      }
    }
  } catch (error) {
    failed = true;
    throw error;
  } finally {
    let cleanupError: unknown;
    try { await reader.cancel(); } catch (error) { cleanupError = error; }
    try { reader.releaseLock(); } catch (error) { cleanupError ??= error; }
    // A reader cleanup failure must not replace the upstream/abort error.
    if (cleanupError && !failed) throw cleanupError;
  }
}


export interface CompletionStreamResult {
  usage?: Usage;
}

// streamCompletion sends a text completion request and hands each streamed
// piece of text (choices[0].text; a completion stream has no delta) to onText.
export async function streamCompletion(
  operation: PlaygroundOperation,
  body: Record<string, unknown>,
  onText: (text: string) => void,
  signal: AbortSignal,
): Promise<CompletionStreamResult> {
  const response = await fetch(operationURL(operation.endpoint), {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ ...body, stream: true }),
    signal,
  });
  if (!response.ok) throw new Error(await responseError(response));
  if (!response.body) throw new Error("The browser did not receive a streaming response body");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let usage: Usage | undefined;
  const consumeEvent = (event: string) => {
    const data = event.split("\n").filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n");
    if (!data || data === "[DONE]") return;
    let frame: Record<string, unknown>;
    try {
      frame = JSON.parse(data) as Record<string, unknown>;
    } catch {
      return;
    }
    if (frame.error) {
      const error = frame.error as { message?: string };
      throw new Error(safeErrorText(typeof error.message === "string" ? error.message : "The upstream service returned a streaming error"));
    }
    const choice = Array.isArray(frame.choices) ? frame.choices[0] as Record<string, unknown> | undefined : undefined;
    if (typeof choice?.text === "string" && choice.text) onText(choice.text);
    if (frame.usage && typeof frame.usage === "object") usage = frame.usage as Usage;
  };
  while (true) {
    const { done, value } = await reader.read();
    buffer += decoder.decode(value, { stream: !done }).replace(/\r\n/g, "\n");
    let boundary = buffer.indexOf("\n\n");
    while (boundary >= 0) {
      consumeEvent(buffer.slice(0, boundary));
      buffer = buffer.slice(boundary + 2);
      boundary = buffer.indexOf("\n\n");
    }
    if (done) break;
  }
  if (buffer.trim()) consumeEvent(buffer);
  return { usage };
}

export async function invokeJSON(operation: PlaygroundOperation, model: string, input: Record<string, unknown>, signal?: AbortSignal): Promise<unknown> {
  const response = await fetch(operationURL(operation.endpoint), {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ model, ...input }),
    signal,
  });
  if (!response.ok) throw new Error(await responseError(response));
  if (operation.response === "audio" || (operation.response === "video" && (response.headers.get("content-type") || "").startsWith("video/"))) return response.blob();
  return response.json();
}

export async function invokeMultipart(operation: PlaygroundOperation, model: string, fields: Record<string, string>, file?: File, signal?: AbortSignal): Promise<unknown> {
  const body = new FormData();
  body.append("model", model);
  for (const [key, value] of Object.entries(fields)) body.append(key, value);
  if (file) body.append("file", file);
  const response = await fetch(operationURL(operation.endpoint), { method: "POST", credentials: "include", body, signal });
  if (!response.ok) throw new Error(await responseError(response));
  return ["audio", "video"].includes(operation.response) ? response.blob() : response.json();
}

export async function invokeOperation(operation: PlaygroundOperation, model: string, input: Record<string, unknown>, signal?: AbortSignal): Promise<unknown> {
  if (operation.request_encoding !== "multipart") return invokeJSON(operation, model, input, signal);
  const body = new FormData();
  body.append("model", model);
  for (const [key, value] of Object.entries(input)) {
    if (value === undefined || value === null) continue;
    // A list of files - several reference images - is the same field
    // repeated, one part per file, in order.
    if (Array.isArray(value) && value.length > 0 && value.every((item) => typeof item === "string" && item.startsWith("data:"))) {
      for (const item of value as string[]) appendMultipartValue(body, key, item);
      continue;
    }
    appendMultipartValue(body, key, value);
  }
  const response = await fetch(operationURL(operation.endpoint), { method: "POST", credentials: "include", body, signal });
  if (!response.ok) throw new Error(await responseError(response));
  if (["audio", "video"].includes(operation.response)) return response.blob();
  return response.json();
}

function appendMultipartValue(body: FormData, key: string, value: unknown) {
  if (typeof value === "string" && value.startsWith("data:")) {
    const comma = value.indexOf(",");
    if (comma > 0) {
      const header = value.slice(5, comma).split(";")[0] || "application/octet-stream";
      const bytes = Uint8Array.from(atob(value.slice(comma + 1)), (character) => character.charCodeAt(0));
      body.append(key, new Blob([bytes], { type: header }), `${key}.bin`);
      return;
    }
  }
  body.append(key, typeof value === "string" ? value : JSON.stringify(value));
}

export async function retrieveAsync(operation: PlaygroundOperation, model: string, taskID: string, kind: "poll" | "content" | "cancel", signal?: AbortSignal): Promise<unknown> {
  if (!operation.async) throw new Error("The selected operation is not asynchronous");
  const lifecycle = operation.async;
  const template = kind === "poll" ? lifecycle.poll_endpoint : kind === "content" ? lifecycle.content_endpoint : lifecycle.cancel_endpoint;
  const method = kind === "poll" ? lifecycle.poll_method : kind === "content" ? lifecycle.content_method : lifecycle.cancel_method;
  const endpoint = template.split("{endpoint}").join(operation.endpoint).split("{id}").join(encodeURIComponent(taskID));
  const query = new URLSearchParams({ model });
  const response = await fetch(`${operationURL(endpoint)}?${query}`, { method, credentials: "include", signal });
  if (!response.ok) throw new Error(await responseError(response));
  return kind === "content" ? response.blob() : response.json();
}

function operationURL(endpoint: string): string {
  if (!endpoint.startsWith("/v1/")) throw new Error(`Unsupported inference endpoint: ${endpoint}`);
  return `${ROOT}${endpoint}`;
}

async function responseError(response: Response): Promise<string> {
  const raw = await readErrorBody(response);
  if (response.status === 401 && raw !== undefined) {
    try {
      const parsed = JSON.parse(raw) as { code?: string };
      if (parsed.code === "AUTHENTICATION_REQUIRED") notifySessionExpired();
    } catch {
      // A non-JSON 401 still uses the fallback text below.
    }
  }
  return playgroundErrorText(response.status, raw);
}

/**
 * A chunk's reasoning text. Engines name the field differently (SGLang and
 * older vLLM `reasoning_content`, vLLM 0.34 `reasoning`, some gateways
 * `thinking`), and transitional vLLM releases send the same text under two
 * names, so the first non-empty one is taken rather than their sum.
 */
export function reasoningDelta(delta: Record<string, unknown> | undefined): string {
  for (const key of ["reasoning_content", "reasoning", "thinking"]) {
    const value = delta?.[key];
    if (typeof value === "string" && value) return value;
  }
  return "";
}
