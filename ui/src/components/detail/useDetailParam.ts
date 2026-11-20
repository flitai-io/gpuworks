import { useCallback } from "react";
import { useSearchParams } from "react-router-dom";

// useDetailParam keeps which resource's detail is open, and its tab, in the
// list page's URL (?storage=3&tab=files): a link opens the list with the
// drawer on it, and closing the drawer drops both.
export function useDetailParam(param: string) {
  const [params, setParams] = useSearchParams();
  // An empty value (?network=) opens nothing, like an absent one.
  const id = params.get(param) || null;
  const tab = params.get("tab") ?? undefined;
  const open = useCallback(
    (next: string | number | null, nextTab?: string) =>
      setParams(
        (current) => {
          const updated = new URLSearchParams(current);
          if (next == null || next === "") {
            updated.delete(param);
            updated.delete("tab");
          } else {
            updated.set(param, String(next));
            if (nextTab) updated.set("tab", nextTab);
            else updated.delete("tab");
          }
          return updated;
        },
        { replace: true },
      ),
    [param, setParams],
  );
  const setTab = useCallback(
    (nextTab: string) =>
      setParams(
        (current) => {
          const updated = new URLSearchParams(current);
          updated.set("tab", nextTab);
          return updated;
        },
        { replace: true },
      ),
    [setParams],
  );
  return { id, tab, open, setTab };
}

// numericDetailID reads a numeric resource id from the URL: an absent, empty
// or non-numeric value opens nothing, while "0" stays 0 (the management
// network's id).
export function numericDetailID(id: string | null): number | null {
  if (id == null || !/^\d+$/.test(id)) return null;
  const value = Number(id);
  return Number.isSafeInteger(value) ? value : null;
}
