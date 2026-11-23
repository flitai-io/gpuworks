import SettingsSaveButton from "../SettingsSaveButton";
import { useState } from "react";
import { Alert, Button, Checkbox, Drawer, Form, Input, Space } from "antd";
import { useTranslation } from "react-i18next";
import { api, type NodeRequirementsMap, type PullProxy, type PullProxyRequest } from "../../../api";
import NodeRequirementsSelect from "./NodeRequirementsSelect";
import { pullErrorField, pullErrorText } from "./pullFormat";

const FORM_FIELDS = ["http_proxy", "https_proxy", "no_proxy", "username", "password", "requirements", "note", "control_plane"];

/**
 * PullProxyDrawer creates or edits one proxy entry. The stored password is
 * never shown: on edit an empty password field keeps it, and the clear
 * checkbox removes it. What the server refuses comes back as a product error
 * and is shown at the field it belongs to.
 */
export default function PullProxyDrawer({
  proxy,
  onClose,
  onSaved,
}: {
  proxy: PullProxy | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [httpProxy, setHTTPProxy] = useState(proxy?.http_proxy ?? "");
  const [httpsProxy, setHTTPSProxy] = useState(proxy?.https_proxy ?? "");
  const [noProxy, setNoProxy] = useState(proxy?.no_proxy ?? "");
  const [username, setUsername] = useState(proxy?.username ?? "");
  const [password, setPassword] = useState("");
  const [clearPassword, setClearPassword] = useState(false);
  const [requirements, setRequirements] = useState<NodeRequirementsMap>(proxy?.node_requirements ?? {});
  // Untouched, a new entry without node requirements also serves the management node.
  const [controlTouched, setControlTouched] = useState<boolean | null>(proxy ? proxy.control_plane : null);
  const [note, setNote] = useState(proxy?.note ?? "");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<{ field: string; text: string } | null>(null);

  const controlPlane = controlTouched ?? Object.keys(requirements).length === 0;
  const stored = Boolean(proxy?.has_password);

  const save = async () => {
    setSaving(true);
    setError(null);
    const body: PullProxyRequest = {
      http_proxy: httpProxy.trim(),
      https_proxy: httpsProxy.trim(),
      no_proxy: noProxy.trim(),
      username: username.trim(),
      control_plane: controlPlane,
      note: note.trim(),
      node_requirements: requirements,
    };
    // Omitted keeps the stored password; "" removes it.
    if (clearPassword) body.password = "";
    else if (password !== "") body.password = password;
    try {
      if (proxy) await api.updatePullProxy(proxy.id, body);
      else await api.createPullProxy(body);
      onSaved();
    } catch (err) {
      setError({ field: pullErrorField(err), text: pullErrorText(t, tCommon, err) });
    } finally {
      setSaving(false);
    }
  };
  const fieldHelp = (field: string) => (error?.field === field ? error.text : undefined);
  const fieldStatus = (field: string) => (error?.field === field ? "error" : undefined);

  return (
    <Drawer
      open
      width={560}
      title={proxy ? t("pull.proxies.editTitle") : t("pull.proxies.newTitle")}
      onClose={onClose}
      maskClosable={!saving}
      footer={
        <Space style={{ display: "flex", justifyContent: "flex-end" }}>
          <Button onClick={onClose} disabled={saving}>{t("pull.action.cancel")}</Button>
          <SettingsSaveButton loading={saving} onSave={save} />
        </Space>
      }
    >
      <Form layout="vertical">
        <Form.Item label={t("pull.proxies.httpProxy")} htmlFor="pull-proxy-http" extra={t("pull.proxies.httpProxyHelp")} validateStatus={fieldStatus("http_proxy")} help={fieldHelp("http_proxy")}>
          <Input id="pull-proxy-http" autoFocus autoComplete="off" value={httpProxy} placeholder="http://proxy.corp.example:3128" onChange={(event) => setHTTPProxy(event.target.value)} />
        </Form.Item>
        <Form.Item label={t("pull.proxies.httpsProxy")} htmlFor="pull-proxy-https" extra={t("pull.proxies.httpsProxyHelp")} validateStatus={fieldStatus("https_proxy")} help={fieldHelp("https_proxy")}>
          <Input id="pull-proxy-https" autoComplete="off" value={httpsProxy} placeholder="http://proxy.corp.example:3128" onChange={(event) => setHTTPSProxy(event.target.value)} />
        </Form.Item>
        <Form.Item label={t("pull.proxies.noProxy")} htmlFor="pull-proxy-no" extra={t("pull.proxies.noProxyHelp")} validateStatus={fieldStatus("no_proxy")} help={fieldHelp("no_proxy")}>
          <Input id="pull-proxy-no" autoComplete="off" value={noProxy} placeholder="localhost,10.0.0.0/8,.corp" onChange={(event) => setNoProxy(event.target.value)} />
        </Form.Item>
        <Form.Item label={t("pull.proxies.username")} htmlFor="pull-proxy-user" validateStatus={fieldStatus("username")} help={fieldHelp("username")}>
          <Input id="pull-proxy-user" autoComplete="off" value={username} onChange={(event) => setUsername(event.target.value)} />
        </Form.Item>
        <Form.Item
          label={t("pull.proxies.password")}
          htmlFor="pull-proxy-password"
          extra={stored ? (clearPassword ? t("pull.proxies.passwordClearedHelp") : t("pull.proxies.passwordKeepHelp")) : undefined}
          validateStatus={fieldStatus("password")}
          help={fieldHelp("password")}
        >
          <Input.Password
            id="pull-proxy-password"
            autoComplete="new-password"
            value={password}
            disabled={clearPassword}
            onChange={(event) => setPassword(event.target.value)}
          />
          {stored && (
            <Checkbox
              style={{ marginTop: 8 }}
              checked={clearPassword}
              onChange={(event) => { setClearPassword(event.target.checked); if (event.target.checked) setPassword(""); }}
            >
              {t("pull.proxies.clearPassword")}
            </Checkbox>
          )}
        </Form.Item>

        <Form.Item label={t("pull.rules.nodes")} extra={t("pull.proxies.nodesHelp")} validateStatus={fieldStatus("requirements")} help={fieldHelp("requirements")}>
          <NodeRequirementsSelect ariaLabel={t("pull.rules.nodes")} value={requirements} onChange={setRequirements} />
        </Form.Item>

        <Form.Item extra={t("pull.proxies.controlPlaneHelp")} validateStatus={fieldStatus("control_plane")} help={fieldHelp("control_plane")}>
          <Checkbox checked={controlPlane} onChange={(event) => setControlTouched(event.target.checked)}>{t("pull.proxies.controlPlaneLabel")}</Checkbox>
        </Form.Item>

        <Form.Item label={t("pull.proxies.note")} htmlFor="pull-proxy-note" validateStatus={fieldStatus("note")} help={fieldHelp("note")}>
          <Input.TextArea id="pull-proxy-note" rows={2} value={note} placeholder={t("pull.proxies.notePlaceholder")} onChange={(event) => setNote(event.target.value)} />
        </Form.Item>

        {error && !FORM_FIELDS.includes(error.field) && <Alert role="alert" type="error" showIcon message={error.text} />}
      </Form>
    </Drawer>
  );
}
