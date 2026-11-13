import { useEffect, useState } from "react";
import { Alert, Form, Input, Radio, Spin, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api, type PlatformSSHKey, type SSHAuthMode } from "../api";

export type { SSHAuthMode };

export default function SSHCredentialFields({
  authMode,
  onAuthModeChange,
}: {
  authMode: SSHAuthMode;
  onAuthModeChange: (mode: SSHAuthMode) => void;
}) {
  const { t } = useTranslation("nodes");
  return (
    <>
      <Form.Item label={t("ssh.auth.label")}>
        <Radio.Group value={authMode} onChange={(event) => onAuthModeChange(event.target.value)}>
          <Radio.Button value="platform_key">{t("ssh.auth.platformKey")}</Radio.Button>
          <Radio.Button value="password">{t("ssh.auth.password")}</Radio.Button>
          <Radio.Button value="keyboard_interactive">{t("ssh.auth.keyboardInteractive")}</Radio.Button>
          <Radio.Button value="private_key">{t("ssh.auth.key")}</Radio.Button>
        </Radio.Group>
      </Form.Item>
      {authMode === "platform_key" && <PlatformKeyAuthorization />}
      {(authMode === "password" || authMode === "keyboard_interactive") && (
        <Form.Item label={t("ssh.password")} name="password" extra={authMode === "keyboard_interactive" ? `${t("ssh.keyboardInteractiveHelp")} ${t("ssh.authorizesPlatformKey")}` : t("ssh.authorizesPlatformKey")} rules={[{ required: true, whitespace: true, message: t("ssh.passwordRequired") }]}>
          <Input.Password autoComplete="off" />
        </Form.Item>
      )}
      {authMode === "private_key" && (
        <Form.Item label={t("ssh.privateKey")} name="private_key_pem" extra={t("ssh.authorizesPlatformKey")} rules={[{ required: true, whitespace: true, message: t("ssh.privateKeyRequired") }]}>
          <Input.TextArea rows={6} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" />
        </Form.Item>
      )}
    </>
  );
}

// PlatformKeyAuthorization shows the command that authorizes the server's own
// key on a target, which is all the platform_key mode asks of the operator.
export function PlatformKeyAuthorization() {
  const { t } = useTranslation("nodes");
  const [key, setKey] = useState<PlatformSSHKey | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let live = true;
    api.platformSSHKey()
      .then((k) => live && setKey(k))
      .catch((e) => live && setError((e as Error).message));
    return () => {
      live = false;
    };
  }, []);
  if (error) {
    return <Alert type="error" showIcon style={{ marginBottom: 16 }} message={t("ssh.platformKey.unavailable", { detail: error })} />;
  }
  if (!key) {
    return <Spin size="small" style={{ display: "block", marginBottom: 16 }} />;
  }
  return (
    <Form.Item label={t("ssh.platformKey.authorizeLabel")} extra={t("ssh.platformKey.authorizeHelp", { fingerprint: key.fingerprint })}>
      <Typography.Paragraph copyable={{ text: key.authorize_command }} code style={{ marginBottom: 0, wordBreak: "break-all" }}>
        {key.authorize_command}
      </Typography.Paragraph>
    </Form.Item>
  );
}
