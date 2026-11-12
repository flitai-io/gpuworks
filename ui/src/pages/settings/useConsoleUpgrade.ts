import { useCallback, useEffect, useState } from "react";
import { api, usePolling, type ServerUpgradeStarted } from "../../api";
import { useSessionUser } from "../../components/sessionUser";
import { lastSessionChange, subscribeSessionChanges } from "../../session";

type Attempt = Pick<ServerUpgradeStarted, "from" | "to"> & {
  phase: "restarting" | "restored";
  deadline: number;
};

const refreshKey = "gpuworks.console-upgrade";
const restartWindow = 5 * 60_000;
const noticeWindow = 30 * 60_000;

function fromRefresh(user: number | undefined): Attempt | null {
  try {
    const nav = window.performance.getEntriesByType("navigation")[0] as PerformanceNavigationTiming | undefined;
    if (nav?.type !== "reload" || new URL(nav.name).pathname !== window.location.pathname) return null;
    const saved = JSON.parse(window.sessionStorage.getItem(refreshKey) ?? "null");
    const attempt = saved?.attempt;
    if (saved?.url === window.location.href && saved?.user === user && saved.session === lastSessionChange() &&
        typeof attempt?.to === "string" && typeof attempt.from === "string" &&
        (attempt.phase === "restarting" || attempt.phase === "restored") && attempt.deadline > Date.now()) return attempt;
  } catch { /* The notice still works in the current document when storage is unavailable. */ }
  return null;
}

export function useConsoleUpgrade(onResult: (release: string, recovered: boolean) => void) {
  const user = useSessionUser()?.id;
  const [attempt, setAttempt] = useState<Attempt | null>(() => fromRefresh(user));
  const dismiss = useCallback(() => setAttempt(null), []);

  useEffect(() => {
    // Transfer only a page being unloaded. SPA remounts and ordinary visits start clean.
    try { window.sessionStorage.removeItem(refreshKey); } catch { /* Storage is optional. */ }
    const save = () => {
      if (!attempt) return;
      try {
        window.sessionStorage.setItem(refreshKey, JSON.stringify({ url: window.location.href, user, session: lastSessionChange(), attempt }));
      } catch { /* Storage is optional. */ }
    };
    const onShow = (event: PageTransitionEvent) => { if (event.persisted) dismiss(); };
    window.addEventListener("pagehide", save);
    window.addEventListener("pageshow", onShow);
    const unsubscribe = subscribeSessionChanges(dismiss);
    return () => {
      window.removeEventListener("pagehide", save);
      window.removeEventListener("pageshow", onShow);
      unsubscribe();
    };
  }, [attempt, dismiss, user]);

  useEffect(() => {
    if (!attempt) return;
    const controller = new AbortController();
    let retry: ReturnType<typeof setTimeout>;
    const timeout = setTimeout(() => {
      controller.abort();
      dismiss();
      if (attempt.phase === "restarting") onResult(attempt.to, false);
    }, Math.max(0, attempt.deadline - Date.now()));
    const probe = async () => {
      try {
        const res = await fetch("/healthz", { cache: "no-store", signal: controller.signal });
        if (res.ok && (await res.json()).version === attempt.to && !controller.signal.aborted && Date.now() < attempt.deadline) {
          setAttempt({ ...attempt, phase: "restored", deadline: Date.now() + noticeWindow });
          onResult(attempt.to, true);
          return;
        }
      } catch { /* A restarting server can temporarily refuse connections. */ }
      if (!controller.signal.aborted) retry = setTimeout(probe, 2000);
    };
    if (attempt.phase === "restarting") void probe();
    return () => { controller.abort(); clearTimeout(retry); clearTimeout(timeout); };
  }, [attempt, dismiss, onResult]);

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
    begin: ({ from, to }: ServerUpgradeStarted) => setAttempt({ from, to, phase: "restarting", deadline: Date.now() + restartWindow }),
    upgrading: attempt?.phase === "restarting" ? attempt : null,
    summary: visible ? data : null,
    dismiss,
  };
}
