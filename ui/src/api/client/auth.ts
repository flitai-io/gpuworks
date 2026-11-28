import { getJSON, sendJSON } from "../http";
import type { AuthUser, SignInOptions } from "../auth";

export const authApi = {
  me: () => getJSON<AuthUser>("/api/v1/auth/me"),
  /** Unauthenticated: the sign-in page renders it as is. */
  signInOptions: () => getJSON<SignInOptions>("/api/v1/auth/sign-in-options"),
  /** Replaces the signed-in user's password and ends all of their sessions. */
  changePassword: (currentPassword: string, newPassword: string) =>
    sendJSON<{ status: string }>("POST", "/api/v1/auth/password", { current_password: currentPassword, new_password: newPassword }),
};
