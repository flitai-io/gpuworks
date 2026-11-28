export interface AuthUser {
  id: number;
  username: string;
  role: string;
  /** The length a new password must have; /auth/me only. */
  password_policy?: { min_length: number; max_length: number };
  /** The effective local administrator floor; /auth/me only, display only. */
  local_admin?: boolean;
}

/** GET /api/v1/auth/sign-in-options: how this server signs people in. */
export interface SignInOptions {
  /** "all", or "local_admin" when only local administrators use a password. */
  local_password: "all" | "local_admin";
  /** Identity sources a user can sign in through now; empty for none. */
  entries: SignInEntry[];
}

/** One "sign in with ..." button: url is the core's sign-in start. */
export interface SignInEntry {
  source: string;
  label: string;
  icon?: string;
  url: string;
}
