import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { api, type AIAskRequest, type AIFinding, type AIHistoryMessage, type AIImageLimit, type AIModelFit, type AIStreamEvent } from "../api";
import { APIError, apiErrorText } from "../apiError";
import type { LiveStep } from "../components/AIAssistantParts";
import { keepNewestImages } from "./images";
import { memoryThreadDB, newestFirst, newThreadID, openThreadDB, overflow, titleFrom, type StoredTurn, type Thread, type ThreadDB } from "./threads";

/**
 * What the assistant is asked to do: a kind of subject and an intent. Each
 * task has its own seed and prompt on the server, so a user starts one with
 * a click; "ask" is a free question, about a subject when one is chosen.
 */
export type AssistantTask = "instance.diagnose" | "model.fit" | "deployment.unscheduled" | "node.health" | "ask";

/** The resource a task is about, by reference, with the name to show. */
export type AssistantSubject =
  | { kind: "instance"; uuid: string; label: string }
  | { kind: "model"; id: number; label: string }
  | { kind: "deployment"; id: number; label: string }
  | { kind: "node"; uuid: string; label: string };

/** A resource the current page shows, the tasks it offers, and what about it needs attention. */
export interface PageSubject {
  subject: AssistantSubject;
  tasks: AssistantTask[];
  attention?: string;
}

export type Facts = { findings: AIFinding[] } | { fit: AIModelFit };

export type FactsState =
  | { status: "none" | "loading" }
  | { status: "done"; value: Facts }
  | { status: "error"; message: string };

export type DoneInfo = Extract<AIStreamEvent, { type: "done" }>;

/** What has streamed in so far. */
export type Live = { text: string; reasoning: string; steps: LiveStep[] };

export type AnswerState =
  | { status: "unconfigured" }
  | { status: "streaming" | "stopped"; live: Live }
  | { status: "done"; live: Live; done: DoneInfo }
  | { status: "error"; live: Live; message: string };

/** One question and its answer in the thread. */
export interface Turn {
  id: number;
  task: AssistantTask;
  subject?: AssistantSubject;
  question?: string;
  /** data: URLs sent with the question. */
  images?: string[];
  facts: FactsState;
  answer: AnswerState;
}

/** toStored is a finished turn as the browser keeps it; a streaming one is not stored. */
function toStored(turn: Turn): StoredTurn | null {
  const a = turn.answer;
  if (a.status === "streaming") return null;
  const live = "live" in a ? a.live : emptyLive;
  return {
    task: turn.task,
    subject: turn.subject,
    question: turn.question,
    images: turn.images,
    facts: turn.facts.status === "done" ? turn.facts.value : undefined,
    status: a.status,
    text: live.text,
    reasoning: live.reasoning,
    steps: live.steps,
    done: a.status === "done" ? a.done : undefined,
    message: a.status === "error" ? a.message : undefined,
  };
}

/** fromStored is a kept turn shown again. */
function fromStored(stored: StoredTurn, id: number): Turn {
  const live: Live = { text: stored.text, reasoning: stored.reasoning, steps: stored.steps };
  const answer: AnswerState =
    stored.status === "done" && stored.done ? { status: "done", live, done: stored.done }
      : stored.status === "error" ? { status: "error", live, message: stored.message ?? "" }
        : stored.status === "unconfigured" ? { status: "unconfigured" }
          : { status: "stopped", live };
  return {
    id,
    task: stored.task as AssistantTask,
    subject: stored.subject as AssistantSubject | undefined,
    question: stored.question,
    images: stored.images,
    facts: stored.facts ? { status: "done", value: stored.facts } : { status: "none" },
    answer,
  };
}

const emptyLive: Live = { text: "", reasoning: "", steps: [] };

/** apply folds one stream event into what is shown. */
export function applyEvent(live: Live, ev: AIStreamEvent): Live {
  switch (ev.type) {
    case "text":
      return { ...live, text: live.text + ev.delta };
    case "reasoning":
      return { ...live, reasoning: live.reasoning + ev.delta };
    case "step": {
      // Text the model wrote before calling tools ("let me check the logs")
      // ends a paragraph; the answer that follows starts a new one.
      const text = live.text && !live.text.endsWith("\n\n") ? live.text + "\n\n" : live.text;
      const step: LiveStep = { id: ev.id, tool: ev.tool, args: ev.args, status: ev.status, ms: ev.ms, error: ev.error };
      const at = live.steps.findIndex((s) => s.id === ev.id);
      const steps = at < 0 ? [...live.steps, step] : live.steps.map((s, i) => (i === at ? step : s));
      return { ...live, text, steps };
    }
    default:
      return live;
  }
}

function subjectRefs(subject?: AssistantSubject): Partial<AIAskRequest> {
  switch (subject?.kind) {
    case "instance":
      return { instance_uuid: subject.uuid };
    case "model":
      return { model_id: subject.id };
    case "deployment":
      return { deployment_id: subject.id };
    case "node":
      return { node_uuid: subject.uuid };
    default:
      return {};
  }
}

export function sameSubject(a?: AssistantSubject, b?: AssistantSubject): boolean {
  return !!a && !!b && a.kind === b.kind && JSON.stringify(subjectRefs(a)) === JSON.stringify(subjectRefs(b));
}

function factsFor(task: AssistantTask, subject: AssistantSubject | undefined, signal: AbortSignal): Promise<Facts> | null {
  if (task === "instance.diagnose" && subject?.kind === "instance") return api.aiInstanceFindings(subject.uuid, signal).then((list) => ({ findings: list.items }));
  if (task === "model.fit" && subject?.kind === "model") return api.aiModelFit(subject.id, signal).then((fit) => ({ fit }));
  return null;
}

interface AssistantValue {
  open: boolean;
  setOpen: (open: boolean) => void;
  turns: Turn[];
  pageSubjects: PageSubject[];
  /** start opens the panel on a new thread running task about subject. */
  start: (task: AssistantTask, subject?: AssistantSubject) => void;
  /** ask continues the thread with a question and its images, or starts a free one about subject. */
  ask: (question: string, subject?: AssistantSubject, images?: string[]) => void;
  /** imageLimit is the image limits of the AI service in use, or null when it does not take images. */
  imageLimit: AIImageLimit | null;
  /** threads are this account's kept threads, newest first; threadID is the one shown, null for a new one. */
  threads: Thread[];
  threadID: string | null;
  openThread: (id: string) => void;
  deleteThread: (id: string) => void;
  stop: () => void;
  retry: (turnId: number) => void;
  reset: () => void;
  register: (key: string, subjects: PageSubject[]) => void;
  unregister: (key: string) => void;
}

const AssistantContext = createContext<AssistantValue | null>(null);

// AssistantProvider holds the assistant across pages: the thread, the panel's
// visibility and what the current page offers. It lives in the app shell, so
// navigating keeps the conversation.
//
// Finished threads are kept in this browser per account (threads.ts): the
// panel lists them, and opening one continues it. threadDB lets a test hand
// in a store of its own.
export function AssistantProvider({ children, threadDB }: { children: ReactNode; threadDB?: ThreadDB }) {
  const { t, i18n } = useTranslation("ai");
  const { t: tc } = useTranslation("common");
  const [imageLimit, setImageLimit] = useState<AIImageLimit | null>(null);
  const [threads, setThreads] = useState<Thread[]>([]);
  const [threadID, setThreadID] = useState<string | null>(null);
  const store = useRef<{ db: ThreadDB; userID: number } | null>(null);
  const threadIDRef = useRef<string | null>(null);
  threadIDRef.current = threadID;
  // justOpened skips saving a thread that was only opened, not changed.
  const justOpened = useRef(false);
  const [open, setOpen] = useState(false);
  const [turns, setTurns] = useState<Turn[]>([]);
  const [registry, setRegistry] = useState<Record<string, PageSubject[]>>({});
  const nextId = useRef(1);
  const active = useRef<{ id: number; ac: AbortController } | null>(null);
  const turnsRef = useRef<Turn[]>([]);
  turnsRef.current = turns;

  useEffect(() => () => active.current?.ac.abort(), []);

  // The account's threads load once: who is signed in, then the browser's
  // store (memory only where IndexedDB is refused).
  useEffect(() => {
    let alive = true;
    (async () => {
      const userID = await api.me().then((user) => user.id, () => 0);
      const db = threadDB ?? await openThreadDB().catch(() => memoryThreadDB());
      const list = await db.list(userID).catch(() => [] as Thread[]);
      if (!alive) return;
      store.current = { db, userID };
      setThreads(list.sort(newestFirst));
    })();
    return () => { alive = false; };
  }, [threadDB]);

  // Whether the service in use takes images is asked each time the panel
  // opens: the setting may have changed since.
  useEffect(() => {
    if (!open) return;
    let alive = true;
    api.aiAvailable().then(
      (a) => alive && setImageLimit(a.image_input && a.images ? a.images : null),
      () => alive && setImageLimit(null),
    );
    return () => { alive = false; };
  }, [open]);

  // A thread is saved whenever none of its turns is streaming: a finished
  // answer, a stop or an error. Past MAX_THREADS the oldest are dropped.
  useEffect(() => {
    const kept = store.current;
    if (justOpened.current) {
      justOpened.current = false;
      return;
    }
    if (!kept || turns.length === 0 || turns.some((turn) => turn.answer.status === "streaming")) return;
    const stored = turns.map(toStored).filter((x): x is StoredTurn => !!x);
    const first = turns[0];
    const id = threadIDRef.current ?? newThreadID();
    const now = Date.now();
    setThreads((all) => {
      const previous = all.find((thread) => thread.id === id);
      const thread: Thread = {
        id, userID: kept.userID, createdAt: previous?.createdAt ?? now, updatedAt: now, turns: stored,
        title: titleFrom(first.question || t(`task.${first.task}.title`) + (first.subject ? ` · ${first.subject.label}` : "")),
      };
      const next = [thread, ...all.filter((x) => x.id !== id)];
      const dropped = overflow(next);
      void kept.db.put(thread).then(() => kept.db.remove(dropped)).catch(() => {});
      return next.filter((x) => !dropped.includes(x.id));
    });
    if (!threadIDRef.current) setThreadID(id);
  }, [turns, t]);
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "i") {
        event.preventDefault();
        setOpen((v) => !v);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const update = useCallback((id: number, change: (turn: Turn) => Turn) => {
    setTurns((all) => all.map((turn) => (turn.id === id ? change(turn) : turn)));
  }, []);

  // run streams one turn's answer, and its facts when the task has any.
  const run = useCallback((turn: Turn, history: AIHistoryMessage[]) => {
    active.current?.ac.abort();
    const ac = new AbortController();
    active.current = { id: turn.id, ac };
    const locale = i18n.resolvedLanguage === "zh" ? "zh" : "en";
    const facts = history.length === 0 ? factsFor(turn.task, turn.subject, ac.signal) : null;
    update(turn.id, (x) => ({ ...x, facts: { status: facts ? "loading" : "none" }, answer: { status: "streaming", live: emptyLive } }));
    facts?.then(
      (value) => !ac.signal.aborted && update(turn.id, (x) => ({ ...x, facts: { status: "done", value } })),
      (e) => !ac.signal.aborted && update(turn.id, (x) => ({ ...x, facts: { status: "error", message: (e as Error).message } })),
    );
    const body: AIAskRequest = { task: turn.task, ...subjectRefs(turn.subject), locale };
    if (turn.question) body.question = turn.question;
    if (turn.images?.length) body.images = turn.images;
    if (history.length) body.history = history;
    const liveOf = (x: Turn) => ("live" in x.answer ? x.answer.live : emptyLive);
    let ended = false;
    api.aiAskStream(body, (ev) => {
      if (ac.signal.aborted) return;
      if (ev.type === "done" || ev.type === "error") ended = true;
      update(turn.id, (x) => {
        const live = liveOf(x);
        if (ev.type === "done") return { ...x, answer: { status: "done", live, done: ev } };
        if (ev.type === "error") return { ...x, answer: { status: "error", live, message: ev.code ? tc(`apiError.${ev.code}`, { defaultValue: ev.error }) : ev.error } };
        return { ...x, answer: { status: "streaming", live: applyEvent(live, ev) } };
      });
    }, ac.signal).then(
      () => {
        // A stream that closed without done or error is not an answer.
        if (!ac.signal.aborted && !ended) update(turn.id, (x) => ({ ...x, answer: { status: "error", live: liveOf(x), message: t("streamCut") } }));
      },
      (e) => {
        if (ac.signal.aborted) return;
        if (e instanceof APIError && e.code === "AI_ENDPOINT_NOT_CONFIGURED") update(turn.id, (x) => ({ ...x, answer: { status: "unconfigured" } }));
        else update(turn.id, (x) => ({ ...x, answer: { status: "error", live: liveOf(x), message: apiErrorText(tc, e) || t("requestFailed") } }));
      },
    );
  }, [i18n, t, tc, update]);

  // historyBefore is the thread up to a turn: what each earlier turn asked
  // (the task's own wording when it was a click), with its images, and what
  // came back. Images go along only while the service takes them, the newest
  // kept within the request's limit.
  const historyBefore = useCallback((all: Turn[], id: number) => {
    const earlier = all.slice(0, Math.max(0, all.findIndex((turn) => turn.id === id)));
    const current = all.find((turn) => turn.id === id);
    const images = imageLimit
      ? keepNewestImages(earlier.map((turn) => turn.images ?? []), current?.images?.length ?? 0, imageLimit.per_request)
      : earlier.map(() => []);
    const out: AIHistoryMessage[] = [];
    earlier.forEach((turn, i) => {
      const user: AIHistoryMessage = { role: "user", content: turn.question || (turn.images?.length ? "" : t(`task.${turn.task}.title`)) };
      if (images[i].length) user.images = images[i];
      out.push(user);
      if ("live" in turn.answer && turn.answer.live.text) out.push({ role: "assistant", content: turn.answer.live.text });
    });
    return out;
  }, [imageLimit, t]);

  const start = useCallback((task: AssistantTask, subject?: AssistantSubject) => {
    const turn: Turn = { id: nextId.current++, task, subject, facts: { status: "none" }, answer: { status: "streaming", live: emptyLive } };
    setThreadID(null);
    threadIDRef.current = null;
    setTurns([turn]);
    setOpen(true);
    run(turn, []);
  }, [run]);

  const ask = useCallback((question: string, subject?: AssistantSubject, images?: string[]) => {
    const q = question.trim();
    const attached = images?.length ? images : undefined;
    if (!q && !attached) return;
    const thread = turnsRef.current;
    const first = thread[0];
    // A follow-up keeps the thread's task and subject; a first question is
    // free, about the subject the user picked.
    const turn: Turn = first
      ? { id: nextId.current++, task: first.task, subject: first.subject, question: q, images: attached, facts: { status: "none" }, answer: { status: "streaming", live: emptyLive } }
      : { id: nextId.current++, task: "ask", subject, question: q, images: attached, facts: { status: "none" }, answer: { status: "streaming", live: emptyLive } };
    const all = [...thread, turn];
    setTurns(all);
    setOpen(true);
    run(turn, historyBefore(all, turn.id));
  }, [historyBefore, run]);

  const stop = useCallback(() => {
    const current = active.current;
    if (!current) return;
    current.ac.abort();
    update(current.id, (x) => ({
      ...x,
      facts: x.facts.status === "loading" ? { status: "none" } : x.facts,
      answer: { status: "stopped", live: "live" in x.answer ? x.answer.live : emptyLive },
    }));
  }, [update]);

  const retry = useCallback((id: number) => {
    const all = turnsRef.current;
    const turn = all.find((x) => x.id === id);
    if (turn) run(turn, historyBefore(all, id));
  }, [historyBefore, run]);

  const reset = useCallback(() => {
    active.current?.ac.abort();
    active.current = null;
    setThreadID(null);
    threadIDRef.current = null;
    setTurns([]);
  }, []);

  // openThread shows a kept thread; asking continues it.
  const openThread = useCallback((id: string) => {
    const thread = threads.find((x) => x.id === id);
    if (!thread) return;
    active.current?.ac.abort();
    active.current = null;
    setThreadID(id);
    threadIDRef.current = id;
    justOpened.current = true;
    setTurns(thread.turns.map((stored) => fromStored(stored, nextId.current++)));
  }, [threads]);

  const deleteThread = useCallback((id: string) => {
    void store.current?.db.remove([id]).catch(() => {});
    setThreads((all) => all.filter((x) => x.id !== id));
    if (threadIDRef.current === id) reset();
  }, [reset]);

  const register = useCallback((key: string, subjects: PageSubject[]) => setRegistry((r) => ({ ...r, [key]: subjects })), []);
  const unregister = useCallback((key: string) => setRegistry((r) => {
    const next = { ...r };
    delete next[key];
    return next;
  }), []);

  const pageSubjects = useMemo(() => Object.values(registry).flat(), [registry]);
  const value = useMemo<AssistantValue>(
    () => ({ open, setOpen, turns, pageSubjects, start, ask, stop, retry, reset, register, unregister, imageLimit, threads, threadID, openThread, deleteThread }),
    [open, turns, pageSubjects, start, ask, stop, retry, reset, register, unregister, imageLimit, threads, threadID, openThread, deleteThread],
  );
  return <AssistantContext.Provider value={value}>{children}</AssistantContext.Provider>;
}

/** useOptionalAssistant is the assistant when the app shell provides one, else null. */
export function useOptionalAssistant(): AssistantValue | null {
  return useContext(AssistantContext);
}

export function useAssistant(): AssistantValue {
  const value = useContext(AssistantContext);
  if (!value) throw new Error("useAssistant needs an AssistantProvider");
  return value;
}

let registrations = 0;

/**
 * useAssistantSubjects tells the assistant what the calling page shows, so
 * its panel offers those tasks and points at what needs attention. Pass
 * null while nothing is shown.
 */
export function useAssistantSubjects(subjects: PageSubject[] | null) {
  const value = useContext(AssistantContext);
  const key = useRef("");
  if (!key.current) key.current = `page-${++registrations}`;
  const signature = JSON.stringify(subjects);
  useEffect(() => {
    if (!value) return;
    const id = key.current;
    if (subjects && subjects.length) value.register(id, subjects);
    else value.unregister(id);
    return () => value.unregister(id);
    // The signature carries the subjects' content; the register functions are stable.
  }, [signature, value?.register, value?.unregister]);
}
