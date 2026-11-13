import { CloudServerOutlined, LockOutlined, UserOutlined } from "@ant-design/icons";
import { Alert, Button, Form, Input, Typography } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import LanguageSwitcher from "../components/LanguageSwitcher";
import ForgotPasswordHint from "../components/ForgotPasswordHint";
import { BrandMark, useBranding, useDocumentBranding } from "../branding";

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
          <LoginForm onLogin={onLogin} />
        </section>
      </div>
    </main>
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
        throw new Error(Number.isSafeInteger(seconds) && seconds > 0
          ? t("throttledRetry", { seconds })
          : t("apiError.LOGIN_THROTTLED", { ns: "common" }));
      }
      if (!res.ok) throw new Error(t(res.status === 401 ? "invalidCredentials" : "failed"));
      onLogin();
    } catch (err) {
      setError(err instanceof Error ? err.message : t("failed"));
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
