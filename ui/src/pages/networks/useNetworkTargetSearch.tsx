import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import { apiErrorText } from "../../apiError";
import { Input } from "../../components/ui/input";
import type { NetworkAttachment, NetworkNode, NetworkNodeFilters } from "../../networkTypes";

export type NetworkSearchTarget = NetworkAttachment | NetworkNode;
export type NetworkSearchKind = "nodes" | "attachments";
type Match = { ids: Set<string>; expires: number };
type Answer<T> = { rows: T[]; key: string; attempt: number; ids?: Set<string>; error?: Error };

/** Match across the full network before paginating or selecting rows. */
export function useNetworkTargetSearch<T extends NetworkSearchTarget>(networkId: number, kind: NetworkSearchKind, data: T[], label: string, filters?: NetworkNodeFilters) {
  const { t: common } = useTranslation("common");
  const [input, setInput] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [answer, setAnswer] = useState<Answer<T> | null>(null);
  const query = input.trim();
  const roles = filters?.roles ?? [];
  const sources = filters?.source ?? [];
  const hasCriteria = query !== "" || roles.length > 0 || sources.length > 0;
  const key = hasCriteria ? JSON.stringify([query, roles, sources]) : "";
  // A new detail snapshot (including an attach/detach) invalidates its matches.
  const recent = useMemo(() => new Map<string, Match>(), [data, networkId, kind]);

  useEffect(() => {
    if (!key) return;
    const cached = recent.get(key);
    if (cached && cached.expires > Date.now()) {
      setAnswer({ rows: data, key, attempt, ids: cached.ids });
      return;
    }
    const controller = new AbortController();
    const timer = setTimeout(async () => {
      try {
        const result = await api.searchNetworkTargets(networkId, kind, query, controller.signal, filters);
        if (controller.signal.aborted) return;
        const ids = new Set(result.ids);
        for (const [cachedKey, entry] of recent) if (entry.expires <= Date.now()) recent.delete(cachedKey);
        recent.set(key, { ids, expires: Date.now() + 10_000 });
        setAnswer({ rows: data, key, attempt, ids });
      } catch (cause) {
        if (!controller.signal.aborted) setAnswer({ rows: data, key, attempt, error: cause instanceof Error ? cause : new Error(String(cause)) });
      }
    }, 250);
    return () => { clearTimeout(timer); controller.abort(); };
  }, [query, key, filters, networkId, kind, data, recent, attempt]);

  const current = answer?.rows === data && answer.key === key && answer.attempt === attempt ? answer : null;
  const visible = useMemo(() => {
    if (!key) return data;
    const ids = current?.ids;
    return ids ? data.filter(row => ids.has("id" in row ? String(row.id) : row.node_uuid)) : [];
  }, [data, current, key]);
  return {
    visible,
    query,
    searchKey: key,
    searchBox: <Input type="search" size="sm" className="w-72 max-w-full" aria-label={label} placeholder={label}
      value={input} onChange={event => {
        if (event.target.value.trim() !== query) setAnswer(null);
        setInput(event.target.value);
      }} />,
    loading: Boolean(key && !current),
    error: current?.error ? new Error(apiErrorText(common, current.error)) : undefined,
    onRefresh: current?.error ? () => setAttempt(value => value + 1) : undefined,
  };
}
