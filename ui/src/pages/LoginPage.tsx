import { CloudServerOutlined, LockOutlined, UserOutlined } from "@ant-design/icons";
import { Alert, Button, Form, Input, Typography } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import type { SignInOptions } from "../api";
import LanguageSwitcher from "../components/LanguageSwitcher";
import ForgotPasswordHint from "../components/ForgotPasswordHint";
import { BrandMark, useBranding, useDocumentBranding } from "../branding";
import { Button as ZButton } from "../components/ui/button";

const { Title } = Typography;

export default function LoginPage({ onLogin }: { onLogin: () => void }) {
  const { t } = useTranslation("auth");
  const branding = useBranding();
  useDocumentBranding(branding);
  // Start the server's update check while the administrator signs in; the
  // header reads the result afterwards. Nothing here depends on it.
  useEffect(() => {
    api.prefetchUpdates().catch(() => {});
  }, []);

  return (
    <main className="auth-page">
      <div className="auth-language-switcher"><LanguageSwitcher /></div>
      <div className="auth-shell">
        <section className="auth-brand-panel" aria-label={t("productInfo", { product: branding.productName })}>
          <div className="auth-brand-content">
            <BrandMark logo={branding.logo} label={branding.productName} />
            {/* The built-in wordmark already spells the default name. */}
            {branding.productNameSet && <div className="auth-brand-name">{branding.productName}</div>}
            <div className="auth-brand-copy">{branding.loginDescription}</div>
          </div>
        </section>

        <section className="auth-form-panel">
          <CloudServerOutlined style={{ color: "#005bd4", fontSize: 24, marginBottom: 16 }} aria-hidden="true" />
          <Title level={2} className="auth-title">{t("title")}</Title>
          <SignInMethods onLogin={onLogin} />
        </section>
      </div>
    </main>
  );
}

// The codes /api/v1/auth/complete sends the browser back with (?code=) when
// a sign-in through an identity source does not go through.
const SIGN_IN_REFUSALS = new Set(["IDENTITY_TICKET_INVALID", "IDENTITY_SOURCE_UNAVAILABLE", "USER_DISABLED", "SERVICE_UNAVAILABLE"]);

function refusalFromLocation(): string {
  const code = new URLSearchParams(window.location.search).get("code") ?? "";
  return SIGN_IN_REFUSALS.has(code) ? code : "";
}

/**
 * The password form and the identity sources the server offers, as
 * GET /api/v1/auth/sign-in-options says. Until it answers, or if it fails,
 * the password form alone is shown: the server decides who may use it.
 */
function SignInMethods({ onLogin }: { onLogin: () => void }) {
  const { t } = useTranslation("auth");
  const [options, setOptions] = useState<SignInOptions | null>(null);
  const [showPassword, setShowPassword] = useState(false);
  const [refusal] = useState(refusalFromLocation);
  useEffect(() => {
    let alive = true;
    api.signInOptions().then((o) => { if (alive) setOptions(o); }).catch(() => {});
    return () => { alive = false; };
  }, []);

  const entries = options?.entries ?? [];
  // Only local administrators use a password here; their form stays one
  // click away, since they must always be able to sign in.
  const passwordFolded = options?.local_password === "local_admin" && entries.length > 0 && !showPassword;
  return (
    <>
      {refusal && <p role="alert" className="mb-3 text-body text-fg-danger">{t(`apiError.${refusal}`, { ns: "common" })}</p>}
      {passwordFolded ? null : <LoginForm onLogin={onLogin} />}
      {entries.length > 0 && (
        <nav aria-label={t("signInWithOthers")} className="mt-4 flex flex-col gap-2">
          {!passwordFolded && <p className="text-label text-fg-muted text-center">{t("signInWithOthers")}</p>}
          {entries.map((e) => (
            <ZButton key={e.source} asChild variant="tertiary" size="lg">
              <a href={e.url}>{e.label}</a>
            </ZButton>
          ))}
        </nav>
      )}
      {passwordFolded && (
        <ZButton variant="link" size="md" className="mt-3 w-full" onClick={() => setShowPassword(true)}>{t("localAdminSignIn")}</ZButton>
      )}
    </>
  );
}

export function LoginForm({ onLogin }: { onLogin: () => void }) {
  const { t } = useTranslation("auth");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [form] = Form.useForm<{ username: string; password: string }>();

  async function submit(values: { username: string; password: string }) {
    setLoading(true);
    setError("");
    try {
      const res = await fetch("/api/v1/auth/login", {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(values),
      });
      if (res.status === 429) {
        const retryAfter = res.headers.get("Retry-After") ?? "";
        const seconds = /^\d+$/.test(retryAfter) ? Number(retryAfter) : 0;
        setError(Number.isSafeInteger(seconds) && seconds > 0
          ? t("throttledRetry", { seconds })
          : t("apiError.LOGIN_THROTTLED", { ns: "common" }));
        return;
      }
      if (!res.ok) {
        setError(t(res.status === 401 ? "invalidCredentials" : "failed"));
        return;
      }
      onLogin();
    } catch {
      setError(t("failed"));
    } finally {
      setLoading(false);
    }
  }

  return (
    <Form form={form} layout="vertical" onFinish={submit} requiredMark={false}>
      <Form.Item label={t("username")} name="username" rules={[{ required: true, whitespace: true, message: t("usernameRequired") }]}>
        <Input size="large" prefix={<UserOutlined />} placeholder={t("username")} autoComplete="username" onChange={() => error && setError("")} />
      </Form.Item>
      <Form.Item label={t("password")} name="password" rules={[{ required: true, whitespace: true, message: t("passwordRequired") }]}>
        <Input.Password size="large" prefix={<LockOutlined />} placeholder={t("password")} autoComplete="current-password" onChange={() => error && setError("")} />
      </Form.Item>
      <div className="auth-error-slot">
        {error && <Alert type="error" showIcon message={error} role="alert" />}
      </div>
      <Button className="auth-submit" type="primary" htmlType="submit" loading={loading}>
        {t("submit")}
      </Button>
      <ForgotPasswordHint />
    </Form>
  );
}
