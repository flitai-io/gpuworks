import { createContext, useContext } from "react";
import type { AuthUser } from "../api";

/**
 * SessionUserContext carries the user SessionBoundary read from /auth/me when
 * it confirmed the session, so the shell needs no second request for it.
 * Undefined until known, and when the answer could not be read.
 */
export const SessionUserContext = createContext<AuthUser | undefined>(undefined);

export function useSessionUser(): AuthUser | undefined {
  return useContext(SessionUserContext);
}
