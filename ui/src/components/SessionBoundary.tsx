import { Alert, Button, Modal, Spin, Typography } from "antd";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { APIError, apiResponseError } from "../apiError";
import LoginPage, { LoginForm } from "../pages/LoginPage";
import type { AuthUser } from "../api";
import { SessionUserContext } from "./sessionUser";
import { notifySessionAuthenticated, notifySessionExpired, startSessionActivity, subscribeSessionChanges } from "../session";

export default function SessionBoundary({ children, onLogout }: { children: (logout: () => Promise<void>) => ReactNode; onLogout?: () => void }) {
  const { t } = useTranslation("auth");
  const [authenticated, setAuthenticated] = useState<boolean | null>(null);
  const [expired, setExpired] = useState(false);
  const [error, setError] = useState("");
  const [checking, setChecking] = useState(false);
  const [user, setUser] = useState<AuthUser>();
  const request = useRef<AbortController>(undefined);

  const checkSession = useCallback(async () => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setChecking(true);
    setError("");
    try {
      const response = await fetch("/api/v1/auth/me", { credentials: "include", signal: controller.signal });
      if (controller.signal.aborted) return;
      if (!response.ok) {
        const failure = await apiResponseError(response, "");
        if (failure instanceof APIError && failure.status === 401 && failure.code === "AUTHENTICATION_REQUIRED") {
          setAuthenticated((current) => current === true ? current : false);
          return;
        }
        throw failure;
      }
      const me = await response.json().catch(() => undefined) as AuthUser | undefined;
      if (controller.signal.aborted) return;
      setUser(me && typeof me.username === "string" ? me : undefined);
      setAuthenticated(true);
      setExpired(false);
    } catch {
      if (!controller.signal.aborted) setError("sessionUnavailable");
    } finally {
      if (request.current === controller) setChecking(false);
    }
  }, []);

  useEffect(() => {
    const unsubscribe = subscribeSessionChanges((change) => {
      if (change === "authenticated") { void checkSession(); return; }
      request.current?.abort();
      setChecking(false);
      setExpired(true);
      setAuthenticated((current) => current === null ? false : current);
    });
    void checkSession();
    return () => { unsubscribe(); request.current?.abort(); };
  }, [checkSession]);

  useEffect(() => {
    if (authenticated && !expired) return startSessionActivity();
  }, [authenticated, expired]);

  async function logout() {
    try {
      const response = await fetch("/api/v1/auth/logout", { method: "POST", credentials: "include" });
      if (!response.ok) throw await apiResponseError(response, "");
      onLogout?.();
      request.current?.abort();
      setAuthenticated(false);
      setError("");
      notifySessionExpired();
    } catch {
      setError("logoutFailed");
    }
  }

  const loggedIn = () => {
    setAuthenticated(true);
    setExpired(false);
    setError("");
    notifySessionAuthenticated();
  };

  if (authenticated === null) return (
    <main className="auth-page">
      {error ? <Alert type="error" showIcon message={t(error)} role="alert" action={<Button onClick={() => void checkSession()} loading={checking}>{t("retry")}</Button>} /> : <Spin tip={t("checkingSession")}><div style={{ minHeight: 80, minWidth: 240 }} /></Spin>}
    </main>
  );
  if (!authenticated) return <LoginPage onLogin={loggedIn} />;

  return <>
    {error && <Alert type="error" showIcon role="alert" message={t(error)} action={<Button onClick={() => void checkSession()} loading={checking}>{t("retry")}</Button>} />}
    <SessionUserContext.Provider value={user}>{children(logout)}</SessionUserContext.Provider>
    <Modal open={expired} title={t("sessionExpiredTitle")} footer={null} closable={false} maskClosable={false} keyboard={false} destroyOnHidden>
      <Typography.Paragraph>{t("sessionExpiredDescription")}</Typography.Paragraph>
      <LoginForm onLogin={loggedIn} />
    </Modal>
  </>;
}
