import { lastSessionChange, subscribeSessionChanges } from "../session";

type ChangeStream = {
  addEventListener(topic: string, listener: EventListener): void;
  removeEventListener(topic: string, listener: EventListener): void;
  close(): void;
};

/** Fall back to one native stream per page when the browser cannot run a shared worker. */
export function openChangeStream(onError: () => void): ChangeStream {
  function native() {
    const source = new EventSource("/api/v1/events");
    source.onerror = onError;
    return source;
  }
  if (typeof SharedWorker === "undefined") return native();

  const listeners = new Map<string, Set<EventListener>>();
  const events = new EventTarget();
  let fallback: EventSource | null = null;
  let port: MessagePort | null = null;
  let closed = false;
  function disconnect() {
    if (port) { port.postMessage({ close: true }); port.close(); port = null; }
    fallback?.close();
    fallback = null;
  }
  function useNative() {
    disconnect();
    if (closed) return;
    fallback = native();
    for (const [topic, handlers] of listeners) for (const handler of handlers) fallback.addEventListener(topic, handler);
  }
  function connect() {
    if (closed || port || fallback) return;
    try {
      const worker = new SharedWorker(new URL("./sharedWorker.ts", import.meta.url), {
        name: "gpuworks-changes:" + (lastSessionChange() ?? ""),
      });
      port = worker.port;
      worker.onerror = () => { if (port === worker.port) useNative(); };
      port.onmessage = ({ data }) => {
        if (port !== worker.port) return;
        if (data.error) { onError(); return; }
        const event = new MessageEvent<string>(data.topic, { data: data.data });
        events.dispatchEvent(event);
      };
      if (listeners.size) port.postMessage({ topics: [...listeners.keys()] });
    } catch { useNative(); }
  }
  connect();
  const unsubscribe = subscribeSessionChanges(change => {
    if (change === "authenticated") { disconnect(); connect(); }
  });
  window.addEventListener("pagehide", disconnect);
  window.addEventListener("pageshow", connect);
  return {
    addEventListener(topic: string, handler: EventListener) {
      const existing = listeners.get(topic);
      if (!existing) listeners.set(topic, new Set([handler]));
      else existing.add(handler);
      if (!existing) port?.postMessage({ topics: [...listeners.keys()] });
      events.addEventListener(topic, handler);
      fallback?.addEventListener(topic, handler);
    },
    removeEventListener(topic: string, handler: EventListener) {
      const existing = listeners.get(topic);
      existing?.delete(handler);
      if (existing?.size === 0) {
        listeners.delete(topic);
        port?.postMessage({ topics: [...listeners.keys()] });
      }
      events.removeEventListener(topic, handler);
      fallback?.removeEventListener(topic, handler);
    },
    close() {
      closed = true;
      unsubscribe();
      disconnect();
      window.removeEventListener("pagehide", disconnect);
      window.removeEventListener("pageshow", connect);
      listeners.clear();
    },
  };
}
