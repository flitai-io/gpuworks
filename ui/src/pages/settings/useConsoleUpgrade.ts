import { useCallback, useEffect, useState } from "react";
import { api, usePolling, type ServerUpgradeStarted } from "../../api";
import { useSessionUser } from "../../components/sessionUser";
import { lastSessionChange, subscribeSessionChanges } from "../../session";

type Attempt = Pick<ServerUpgradeStarted, "from" | "to"> & {
  id?: string;
  backup?: string;
  phase: "download" | "restarting" | "unknown" | "rollback" | "restored" | "failed";
  deadline: number;
};
type LastCheck = { at: number; reason: "offline" | "unconfirmed"; version?: string };

const refreshKey = "gpuworks.console-upgrade";
const restartWindow = 5 * 60_000;
const noticeWindow = 30 * 60_000;

function readAttempt(user: number | undefined): Attempt | null {
  try {
    const saved = JSON.parse(window.sessionStorage.getItem(refreshKey) ?? "null");
    const a = saved?.attempt;
    if (!a || saved.user !== user || saved.session !== lastSessionChange() ||
        typeof a.to !== "string" || typeof a.from !== "string" || !Number.isFinite(a.deadline)) return null;
    if (a.phase === "restored" && a.deadline <= Date.now()) return null;
    return a;
  } catch { return null; }
}

export function useConsoleUpgrade(onResult: (release: string, recovered: boolean) => void) {
  const user = useSessionUser()?.id;
  const [attempt, setAttempt] = useState<Attempt | null>(() => readAttempt(user));
  const [revision, setRevision] = useState(0);
  const [lastCheck, setLastCheck] = useState<LastCheck>();
  const [checkingResult, setCheckingResult] = useState(false);
  const dismiss = useCallback(() => { setAttempt(null); setLastCheck(undefined); }, []);
  const probeVersion = () => setRevision(value => value + 1);

  useEffect(() => {
    try {
      if (attempt) window.sessionStorage.setItem(refreshKey, JSON.stringify({ user, session: lastSessionChange(), attempt }));
      else window.sessionStorage.removeItem(refreshKey);
    } catch { /* Storage may be unavailable in a private window. */ }
    return subscribeSessionChanges(dismiss);
  }, [attempt, user, dismiss]);

  useEffect(() => {
    if (!attempt || attempt.phase === "rollback" || attempt.phase === "failed") return;
    if (attempt.phase === "restored") {
      const timeout = setTimeout(dismiss, Math.max(0, attempt.deadline - Date.now()));
      return () => clearTimeout(timeout);
    }
    const controller = new AbortController();
    const { signal } = controller;
    let retry: ReturnType<typeof setTimeout>;
    const deadline = attempt.phase === "unknown" ? undefined : setTimeout(() => {
      controller.abort();
      setAttempt({ ...attempt, phase: "unknown" });
      onResult(attempt.to, false);
    }, Math.max(0, attempt.deadline - Date.now()));
    const probe = async () => {
      if (signal.aborted) return;
      setCheckingResult(true);
      let running: string | undefined;
      const bounded = AbortSignal.any([signal, AbortSignal.timeout(8000)]);
      try {
        const res = await fetch("/healthz", { cache: "no-store", signal: bounded });
        if (res.ok) running = (await res.json()).version;
        if (signal.aborted) return;
        if (running === attempt.to) {
          setAttempt({ ...attempt, phase: "restored", deadline: Date.now() + noticeWindow });
          onResult(attempt.to, true);
          return;
        }
        if (running && (attempt.id || attempt.backup)) {
          const p = await api.serverUpgradeResult(attempt.id || "", attempt.backup || "", bounded);
          if (signal.aborted) return;
          if (p.phase === "rollback" && running === attempt.from && p.to === attempt.to && p.from === running) {
            setAttempt({ ...attempt, phase: "rollback" });
            onResult(running, false);
            return;
          }
          if (p.phase === "failed" && p.to === attempt.to) {
            setAttempt({ ...attempt, phase: "failed" });
            return;
          }
          if (attempt.phase === "download" && p.phase === "restart") {
            setAttempt({ ...attempt, phase: "restarting", deadline: Date.now() + restartWindow });
            return;
          }
        }
      } catch { /* A restart can interrupt either request. Continue observing this attempt. */ }
      finally { if (!signal.aborted) setCheckingResult(false); }
      if (signal.aborted) return;
      setLastCheck({ at: Date.now(), reason: running ? "unconfirmed" : "offline", version: running });
      retry = setTimeout(probe, attempt.phase === "unknown" ? 15_000 : 2000);
    };
    void probe();
    return () => { controller.abort(); clearTimeout(retry); clearTimeout(deadline); };
  }, [attempt, revision, dismiss, onResult]);

  const restored = attempt?.phase === "restored" ? attempt : null;
  const fleet = usePolling(async () => restored ? { attempt: restored, summary: await api.agentFleetSummary() } : null,
    restored ? 8000 : 0, [restored]);
  const data = restored && fleet.data?.attempt === restored && !fleet.error ? fleet.data.summary : null;
  const sameTarget = !!data && data.target === restored?.to && data.target !== "dev";
  const visible = sameTarget && (data.align > 0 || data.check > 0);
  useEffect(() => {
    // Agents may reconnect after the new server first answers its health probe.
    if (data && (!sameTarget || (!visible && !data.offline))) dismiss();
  }, [data, sameTarget, visible, dismiss]);

  return {
    trackDownload: (id: string, from: string, to: string) => {
      setLastCheck(undefined);
      setAttempt({ id, from, to, phase: "download", deadline: Date.now() + 10 * 60_000 + restartWindow });
    },
    begin: ({ from, to, backup }: ServerUpgradeStarted, id?: string) =>
      setAttempt(current => {
        if (id && (current?.id !== id || current.phase === "restored" || current.phase === "rollback")) return current;
        return { id, from, to, backup, phase: "restarting", deadline: Date.now() + restartWindow };
      }),
    attempt, checkingResult, probeVersion, lastCheck,
    summary: visible ? data : null,
    dismiss,
  };
}
