import type { LiveStep } from "../components/AIAssistantParts";
import type { AIStreamEvent, AIFinding, AIModelFit } from "../api";

/** MAX_THREADS is how many threads a browser keeps per account; the oldest goes first. */
export const MAX_THREADS = 30;
/** TITLE_LENGTH bounds a thread's title, taken from its first question. */
export const TITLE_LENGTH = 40;

type DoneInfo = Extract<AIStreamEvent, { type: "done" }>;

/**
 * StoredTurn is one finished turn as the browser keeps it: what was asked
 * (the task, its subject, the question and its images), the platform checks
 * shown, and the answer as it ended. A turn still streaming is never stored:
 * the thread is saved when it ends.
 */
export interface StoredTurn {
  task: string;
  subject?: { kind: string; uuid?: string; id?: number; label: string };
  question?: string;
  images?: string[];
  facts?: { findings: AIFinding[] } | { fit: AIModelFit };
  status: "done" | "stopped" | "error" | "unconfigured";
  text: string;
  reasoning: string;
  steps: LiveStep[];
  done?: DoneInfo;
  message?: string;
}

/** Thread is one conversation with the assistant, kept in this browser for one account. */
export interface Thread {
  id: string;
  userID: number;
  title: string;
  createdAt: number;
  updatedAt: number;
  turns: StoredTurn[];
}

/** ThreadDB is where the browser keeps an account's threads. */
export interface ThreadDB {
  list(userID: number): Promise<Thread[]>;
  put(thread: Thread): Promise<void>;
  remove(ids: string[]): Promise<void>;
}

export function newThreadID(): string {
  return `t-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
}

/** titleFrom is a thread's title: its first question's first line, shortened. */
export function titleFrom(text: string): string {
  const line = text.split("\n").map((part) => part.trim()).find(Boolean) ?? "";
  return line.length > TITLE_LENGTH ? `${line.slice(0, TITLE_LENGTH - 1)}…` : line;
}

/** newestFirst orders threads by their last change. */
export function newestFirst(a: Thread, b: Thread): number {
  return b.updatedAt - a.updatedAt || (a.id < b.id ? 1 : -1);
}

/** overflow is the ids to drop so at most limit threads remain, the newest kept. */
export function overflow(threads: Thread[], limit = MAX_THREADS): string[] {
  return [...threads].sort(newestFirst).slice(limit).map((thread) => thread.id);
}

/** memoryThreadDB keeps threads for this page only: where IndexedDB is missing or refused. */
export function memoryThreadDB(): ThreadDB {
  const rows = new Map<string, Thread>();
  return {
    async list(userID) {
      return [...rows.values()].filter((thread) => thread.userID === userID);
    },
    async put(thread) {
      rows.set(thread.id, thread);
    },
    async remove(ids) {
      for (const id of ids) rows.delete(id);
    },
  };
}

const DB_NAME = "gpuworks-assistant";
const STORE = "threads";
const BY_USER = "userID";

function request<T>(req: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function done(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error ?? new Error("IndexedDB transaction aborted"));
  });
}

/**
 * openThreadDB opens the browser's thread store: one record per thread, keyed
 * by id and indexed by account. It rejects where IndexedDB is missing or
 * refused (some private windows); the caller then keeps threads in memory.
 */
export async function openThreadDB(factory: IDBFactory | undefined = globalThis.indexedDB): Promise<ThreadDB> {
  if (!factory) throw new Error("IndexedDB is not available");
  const open = factory.open(DB_NAME, 1);
  open.onupgradeneeded = () => {
    const store = open.result.createObjectStore(STORE, { keyPath: "id" });
    store.createIndex(BY_USER, BY_USER);
  };
  const db = await request(open);
  // Another tab upgrading the schema must not be blocked by this one.
  db.onversionchange = () => db.close();
  return {
    async list(userID) {
      return request(db.transaction(STORE).objectStore(STORE).index(BY_USER).getAll(userID)) as Promise<Thread[]>;
    },
    async put(thread) {
      const tx = db.transaction(STORE, "readwrite");
      tx.objectStore(STORE).put(thread);
      await done(tx);
    },
    async remove(ids) {
      if (!ids.length) return;
      const tx = db.transaction(STORE, "readwrite");
      for (const id of ids) tx.objectStore(STORE).delete(id);
      await done(tx);
    },
  };
}
