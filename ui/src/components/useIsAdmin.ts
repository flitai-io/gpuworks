import { useEffect, useState } from "react";
import { api } from "../api";

/** Whether the signed-in user is an admin; false until known and when the lookup fails. */
export function useIsAdmin(enabled = true): boolean {
  const [isAdmin, setIsAdmin] = useState(false);
  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    api.me().then((user) => { if (alive) setIsAdmin(user.role === "admin"); }).catch(() => {});
    return () => { alive = false; };
  }, [enabled]);
  return enabled && isAdmin;
}
