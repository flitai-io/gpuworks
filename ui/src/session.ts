export type SessionChange = "expired" | "authenticated";

const storageKey = "gpuworks.session";
const listeners = new Set<(change: SessionChange) => void>();
let expired = false;

function receive(change: SessionChange) {
  if (change === "expired" && expired) return;
  expired = change === "expired";
  for (const listener of listeners) listener(change);
}

function publish(change: SessionChange) {
  if (change === "expired" && expired) return;
  receive(change);
  try {
    window.localStorage.setItem(storageKey, JSON.stringify({ type: change, nonce: crypto.randomUUID() }));
  } catch {
    // Cookie authentication remains available when browser storage is disabled.
  }
}

function onStorage(event: StorageEvent) {
  if (event.key !== storageKey || !event.newValue) return;
  try {
    const value = JSON.parse(event.newValue);
    if (value.type === "expired" || value.type === "authenticated") receive(value.type);
  } catch {
    // Ignore unrelated or malformed browser storage events.
  }
}

export function subscribeSessionChanges(listener: (change: SessionChange) => void): () => void {
  if (listeners.size === 0 && typeof window !== "undefined") window.addEventListener("storage", onStorage);
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0 && typeof window !== "undefined") window.removeEventListener("storage", onStorage);
  };
}

export function lastSessionChange(): string | null {
  try { return window.localStorage.getItem(storageKey); } catch { return null; }
}

export function notifySessionExpired() { publish("expired"); }
export function notifySessionAuthenticated() { publish("authenticated"); }
export function isSessionExpired() { return expired; }

export function startSessionActivity(): () => void {
  let lastAttempt = -Infinity;
  let pending = false;
  const controller = new AbortController();
  const onActivity = () => {
    if (expired || document.visibilityState !== "visible" || pending || Date.now() - lastAttempt < 60_000) return;
    lastAttempt = Date.now();
    pending = true;
    void fetch("/api/v1/auth/activity", { method: "POST", credentials: "include", signal: controller.signal })
      .then(async (response) => {
        if (response.status !== 401) return;
        const payload = await response.json();
        if (payload?.code === "AUTHENTICATION_REQUIRED") notifySessionExpired();
      })
      .catch(() => { /* Network errors do not establish that the session expired. */ })
      .finally(() => { pending = false; });
  };
  const events = ["pointerdown", "keydown", "input", "wheel", "touchmove"] as const;
  for (const event of events) document.addEventListener(event, onActivity, { capture: true, passive: true });
  return () => {
    controller.abort();
    for (const event of events) document.removeEventListener(event, onActivity, true);
  };
}
