import { useState } from "react";
import { Alert, Button, Drawer, Form, Input, Radio, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import { api, type Credential } from "../../../api";
import { OutboundHostAccess, useOutboundHostAccess } from "../../../components/OutboundHostAccess";
import { useLoginTests } from "./loginTest";
import { pullErrorField, pullErrorText } from "./pullFormat";

/** isSiteToken: a credential without a user name is a model site's token. */
export function isSiteToken(credential: { username: string }): boolean {
  return credential.username === "";
}

/** Match the stored credential normalization and the registry login API alias. */
export function credentialLoginHost(host: string, site: boolean): string {
  const entered = host.trim().toLowerCase().replace(/^https:\/\//, "").replace(/^http:\/\//, "").replace(/\/$/, "");
  const normalized = ["index.docker.io", "registry-1.docker.io", "registry.hub.docker.com"].includes(entered) ? "docker.io" : entered;
  return !site && normalized === "docker.io" ? "registry-1.docker.io" : normalized;
}

type CredentialKind = "registry" | "site";

/**
 * CredentialDrawer creates or edits a credential: a registry login (user name
 * and password) or a model site's token (secret only). A new credential first
 * chooses its type; an edit derives it from the stored user name and keeps it. The password is
 * write-only: the server never returns it, so an edit starts with the field
 * empty and an empty value keeps the stored one. "Save and test" stores the
 * login and then tries it against the registry from the server.
 */
export default function CredentialDrawer({
  credential,
  onClose,
  onChanged,
}: {
  credential: Credential | null;
  onClose: () => void;
  onChanged: () => void;
}) {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [savedId, setSavedId] = useState<number | null>(credential?.id ?? null);
  const [hadSecret, setHadSecret] = useState(credential?.has_secret ?? false);
  const [kind, setKind] = useState<CredentialKind>(credential && isSiteToken(credential) ? "site" : "registry");
  const [host, setHost] = useState(credential?.host ?? "");
  const loginHost = credentialLoginHost(host, kind === "site");
  const outboundAccess = useOutboundHostAccess([loginHost]);
  const [username, setUsername] = useState(credential?.username ?? "");
  const [secret, setSecret] = useState("");
  const [revealed, setRevealed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ field: string; text: string } | null>(null);
  const { results, test } = useLoginTests();
  const testResult = savedId != null ? results[savedId] : undefined;

  // persist stores the login (a create the first time, a replace afterwards)
  // and forgets the typed password; it returns the id, or null after showing why not.
  const persist = async (): Promise<number | null> => {
    setError(null);
    if (kind === "registry" && !username.trim()) {
      setError({ field: "username", text: t("pull.credentials.usernameRequired") });
      return null;
    }
    const body = { host: host.trim(), username: kind === "site" ? "" : username.trim(), secret };
    try {
      await outboundAccess.authorize();
      const saved = savedId != null ? await api.updateCredential(savedId, body) : await api.createCredential(body);
      setSavedId(saved.id);
      setHadSecret(saved.has_secret);
      setSecret("");
      setRevealed(false);
      onChanged();
      return saved.id;
    } catch (err) {
      setError({ field: pullErrorField(err), text: pullErrorText(t, tCommon, err) });
      return null;
    }
  };
  const save = async () => {
    setBusy(true);
    try {
      if ((await persist()) != null) onClose();
    } finally {
      setBusy(false);
    }
  };
  const saveAndTest = async () => {
    setBusy(true);
    try {
      const id = await persist();
      if (id != null) await test(id);
    } finally {
      setBusy(false);
    }
  };
  const fieldHelp = (field: string) => (error?.field === field ? error.text : undefined);
  const fieldStatus = (field: string) => (error?.field === field ? "error" : undefined);
  const testing = testResult?.state === "testing";
  const site = kind === "site";

  return (
    <Drawer
      open
      width={480}
      title={credential ? t("pull.credentials.editTitle") : t("pull.credentials.newTitle")}
      onClose={onClose}
      maskClosable={!busy}
      footer={
        <Space style={{ display: "flex", justifyContent: "flex-end" }}>
          <Button onClick={onClose} disabled={busy}>{t("pull.action.cancel")}</Button>
          <Button loading={busy && testing} disabled={busy} onClick={() => void saveAndTest()}>{t("pull.credentials.saveAndTest")}</Button>
          <Button type="primary" loading={busy && !testing} disabled={busy} onClick={() => void save()}>{t("pull.action.save")}</Button>
        </Space>
      }
    >
      <Form layout="vertical">
        <Form.Item label={t("pull.credentials.type")} extra={t(site ? "pull.credentials.typeSiteHelp" : "pull.credentials.typeRegistryHelp")}>
          {credential ? (
            <Typography.Text>{t(site ? "pull.credentials.typeSite" : "pull.credentials.typeRegistry")}</Typography.Text>
          ) : (
            <Radio.Group
              value={kind}
              onChange={(event) => setKind(event.target.value as CredentialKind)}
              options={[
                { value: "registry", label: t("pull.credentials.typeRegistry") },
                { value: "site", label: t("pull.credentials.typeSite") },
              ]}
            />
          )}
        </Form.Item>
        <Form.Item
          label={t("pull.credentials.host")}
          htmlFor="pull-credential-host"
          extra={t("pull.credentials.hostHelp")}
          validateStatus={fieldStatus("host")}
          help={fieldHelp("host")}
        >
          <Input
            id="pull-credential-host"
            autoFocus
            autoComplete="off"
            value={host}
            placeholder={t(site ? "pull.credentials.hostPlaceholderSite" : "pull.credentials.hostPlaceholderRegistry")}
            onChange={(event) => setHost(event.target.value)}
            onPressEnter={() => void save()}
          />
        </Form.Item>
        <OutboundHostAccess access={outboundAccess} />
        {!site && (
          <Form.Item label={t("pull.credentials.username")} htmlFor="pull-credential-username" validateStatus={fieldStatus("username")} help={fieldHelp("username")}>
            <Input
              id="pull-credential-username"
              autoComplete="off"
              value={username}
              onChange={(event) => setUsername(event.target.value)}
              onPressEnter={() => void save()}
            />
          </Form.Item>
        )}
        <Form.Item
          label={t(site ? "pull.credentials.token" : "pull.credentials.secret")}
          htmlFor="pull-credential-secret"
          extra={hadSecret ? t(site ? "pull.credentials.tokenKeepHelp" : "pull.credentials.secretKeepHelp") : t(site ? "pull.credentials.tokenHelp" : "pull.credentials.secretHelp")}
          validateStatus={fieldStatus("secret")}
          help={fieldHelp("secret")}
        >
          <Space.Compact style={{ width: "100%" }}>
            <Input
              id="pull-credential-secret"
              type={revealed ? "text" : "password"}
              autoComplete="new-password"
              value={secret}
              placeholder={hadSecret ? t("pull.credentials.secretStored") : undefined}
              onChange={(event) => setSecret(event.target.value)}
              onPressEnter={() => void save()}
            />
            <Button aria-pressed={revealed} onClick={() => setRevealed((current) => !current)}>
              {revealed ? t("pull.credentials.hideSecret") : t("pull.credentials.showSecret")}
            </Button>
          </Space.Compact>
        </Form.Item>
        <div aria-live="polite">
          {testResult?.state === "ok" && <Alert type="success" showIcon message={t(site ? "pull.credentials.testOkToken" : "pull.credentials.testOk")} />}
          {testResult?.state === "failed" && <Alert type="error" showIcon message={testResult.text} />}
          {testing && <Typography.Text type="secondary">{t("pull.credentials.testing")}</Typography.Text>}
        </div>
        {error && !["host", "username", "secret"].includes(error.field) && <Alert role="alert" type="error" showIcon message={error.text} />}
      </Form>
    </Drawer>
  );
}
