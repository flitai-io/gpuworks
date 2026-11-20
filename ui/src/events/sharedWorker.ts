// One native, cookie-authenticated EventSource per origin / session generation.
// Subscribe to topic names supplied by pages, so new server events need no registry here.
const pages = new Map<MessagePort, Set<string>>();
const handlers = new Map<string, EventListener>();
let source: EventSource | null = null;

function syncTopics() {
  const topics = new Set([...pages.values()].flatMap(names => [...names]));
  if (!topics.size) {
    source?.close();
    source = null;
    handlers.clear();
    return;
  }
  if (!source) {
    source = new EventSource("/api/v1/events");
    source.onerror = () => {
      for (const port of pages.keys()) port.postMessage({ error: true });
    };
  }
  for (const [topic, handler] of handlers) {
    if (topics.has(topic)) continue;
    source.removeEventListener(topic, handler);
    handlers.delete(topic);
  }
  for (const topic of topics) {
    if (handlers.has(topic)) continue;
    const handler = (event: Event) => {
      for (const [port, interests] of pages) {
        if (interests.has(topic)) port.postMessage({ topic, data: (event as MessageEvent<string>).data });
      }
    };
    handlers.set(topic, handler);
    source.addEventListener(topic, handler);
  }
}

self.addEventListener("connect", (event: Event) => {
  const port = (event as MessageEvent).ports[0];
  const names = new Set<string>();
  pages.set(port, names);
  const close = () => { pages.delete(port); port.close(); syncTopics(); };
  port.addEventListener("close", close);
  port.onmessage = ({ data }) => {
    if (data.close) { close(); return; }
    const wasOpen = source?.readyState === EventSource.OPEN;
    const hadNode = names.has("node");
    names.clear();
    for (const topic of data.topics) names.add(topic);
    syncTopics();
    // A new page joining an already open stream also gets the server's initial node refresh.
    if (!hadNode && wasOpen && names.has("node")) port.postMessage({ topic: "node", data: '{"reason":"connected"}' });
  };
});
export {};
