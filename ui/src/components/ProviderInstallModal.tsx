import { useEffect, useState } from "react";
import TemporaryAddressHint from "./TemporaryAddressHint";
import { Alert, Button, Collapse, Input, Modal, Radio, Select, Space, Switch, Table, Tag, Typography } from "antd";
import { APIError, apiErrorText } from "../apiError";
import {
  api,
  type CheckResult,
  type ProviderInstallResponse,
  type ProviderInstance,
} from "../api";
import { buildProviderInstallRequest } from "../providerInstallRequest";
import { isCommandChannel } from "../providerConnections";
import { installVerdict, type InstallOutcome } from "../providerInstallOutcome";
import { useTranslation } from "react-i18next";
import { localizedText } from "../i18n/localizedText";
import { DEFAULT_AGENT_LISTEN } from "../agentListen";

// AutoDL's port documentation: its Pro instances open HTTP ports only, which
// a server-dialed agent cannot use, so the one-click install never offers it.
export const AUTODL_PORT_DOCS = "https://www.autodl.com/docs/port/";

export default function ProviderInstallModal({
  providerId,
  providerKind,
  defaultServerDialListen,
  serverDialSupported = true,
  serverDialAutoPort = false,
  inst,
  onClose,
  onDone,
}: {
  providerId: number;
  providerKind?: string;
  defaultServerDialListen?: string;
  // The provider's instances can be dialed by the server (the server's
  // provider dictionary says).
  serverDialSupported?: boolean;
  // The server picks the agent's port and address from the provider's port
  // mappings when both are left empty.
  serverDialAutoPort?: boolean;
  inst: ProviderInstance | null;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t, i18n } = useTranslation("providers");
  const { t: tCommon } = useTranslation("common");
  const language = i18n.resolvedLanguage || i18n.language;
  const [key, setKey] = useState("");
  const [password, setPassword] = useState("");
  const [connectMode, setConnectMode] = useState<"agent_dial" | "server_dial">("agent_dial");
  const [listen, setListen] = useState("");
  const [serverURL, setServerURL] = useState(window.location.origin);
  const [sshConnectionID, setSSHConnectionID] = useState("auto");
  const [serverDialAddress, setServerDialAddress] = useState("");
  const [installDir, setInstallDir] = useState("");
  const [useSystemd, setUseSystemd] = useState(true);

  const [result, setResult] = useState<ProviderInstallResponse | null>(null);
  // The request itself failed (the server answered with an error): nothing
  // came back to show as check rows, so the reason is the whole result.
  const [requestError, setRequestError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const verdict = installVerdict(result, requestError);
  // The agent was put on the instance. Installing again would replace it and
  // create another node, so the form gives way to the result and a Done button.
  const installed = !!verdict && verdict.outcome !== "failed";

  useEffect(() => {
    if (inst) {
      setKey("");
      setPassword("");
      setConnectMode("agent_dial");
      setListen(serverDialAutoPort ? "" : defaultServerDialListen || DEFAULT_AGENT_LISTEN);
      setServerURL(window.location.origin);
      setSSHConnectionID("auto");
      setServerDialAddress(serverDialAutoPort ? "" : inst.public_ip || "");
      setInstallDir("");
      setUseSystemd(true);
      setResult(null);
      setRequestError(null);
    }
  }, [defaultServerDialListen, inst, serverDialAutoPort]);

  const submit = async (allowInstanceRestart = false) => {
    if (!inst) return;
    setSubmitting(true);
    setResult(null);
    setRequestError(null);
    try {
      const next = await api.installAgent(providerId, buildProviderInstallRequest({
        externalID: inst.external_id,
        serverURL: serverURL.trim() || window.location.origin,
        sshKeyPEM: key,
        password,
        sshConnectionID,
        connectMode,
        serverDialListen: listen,
        serverDialAddress,
        allowInstanceRestart,
        installDir,
        noSystemd: !useSystemd,
      }));
      setResult(next);
    } catch (error) {
      if (!allowInstanceRestart && error instanceof APIError && error.code === "INSTANCE_RESTART_REQUIRED") {
        // Opening a port restarts the instance (RunPod): nothing changed yet,
        // so ask, and install again only when the operator agrees.
        setSubmitting(false);
        Modal.confirm({
          title: t("install.restartConfirm.title"),
          content: t(restartConfirmContent(error.params?.need)),
          okText: t("install.restartConfirm.ok"),
          okButtonProps: { danger: true },
          cancelText: t("install.cancel"),
          onOk: () => submit(true),
        });
        return;
      }
      setResult(null);
      // A coded refusal (an invalid install directory, say) reads in the
      // operator's language; other failures carry the server's reason.
      setRequestError(apiErrorText(tCommon, error));
    } finally {
      setSubmitting(false);
    }
  };

  // Closing after an install refreshes the instance list, so it shows as onboarded.
  const close = installed ? onDone : onClose;

  return (
    <Modal
      className="provider-install-modal"
      width={720}
      centered
      title={t("install.title", { name: inst?.name ?? "" })}
      open={!!inst}
      onCancel={close}
      footer={installed ? (
        <Button type="primary" onClick={close}>{t("install.done")}</Button>
      ) : (
        <>
          <Button onClick={close}>{t("install.cancel")}</Button>
          <Button type="primary" loading={submitting} onClick={() => void submit()}>
            {verdict ? t("install.retry") : t("install.install")}
          </Button>
        </>
      )}
      destroyOnHidden
    >
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        {submitting && <Alert type="info" showIcon message={t("install.running")} />}
        {verdict && <InstallVerdictBanner outcome={verdict.outcome} detail={verdict.detail} />}
        {!installed && (
          <>
            <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>
              {t(autoAuthHelpKey(providerKind))}
            </Typography.Paragraph>
            <Typography.Text strong>{t("install.connection")}</Typography.Text>
            <Select
              style={{ width: "100%" }}
              aria-label={t("install.connection")}
              value={sshConnectionID}
              onChange={setSSHConnectionID}
              options={[
                { value: "auto", label: t("install.autoConnection") },
                ...(inst?.ssh_connections || []).map((connection) => ({
                  value: connection.id,
                  disabled: !connection.install_supported,
                  label: isCommandChannel(connection)
                    ? localizedText(connection.label, language) || connection.type
                    : `${localizedText(connection.label, language) || t(connection.type === "direct" ? "install.direct" : "install.proxy")} · ${connection.user || "root"}@${connection.host}:${connection.port}${connection.install_supported ? "" : t("install.terminalOnly")}`,
                })),
              ]}
            />
            {serverDialSupported ? (
              <Radio.Group value={connectMode} onChange={(event) => setConnectMode(event.target.value)}>
                <Radio.Button value="agent_dial">{t("install.agentDial")}</Radio.Button>
                <Radio.Button value="server_dial">{t("install.serverDial")}</Radio.Button>
              </Radio.Group>
            ) : providerKind === "autodl" ? (
              <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>
                {t("install.autodlAgentDialOnly")}{" "}
                <a href={AUTODL_PORT_DOCS} target="_blank" rel="noreferrer">{t("install.autodlPortDocs")}</a>
              </Typography.Paragraph>
            ) : null}
            {connectMode === "agent_dial" ? (
              <>
                <Typography.Text strong>{t("install.serverURL")}</Typography.Text>
                <Input value={serverURL} onChange={(event) => setServerURL(event.target.value)} placeholder={window.location.origin} />
                {/* What the node will be told to dial: a quick-tunnel address here
                    stops working as soon as the connector restarts. */}
                <TemporaryAddressHint value={serverURL} />
              </>
            ) : (
              <>
                {/* Where the agent listens comes first: the address below may
                    name another port when a platform maps it. */}
                <Typography.Text strong>{t("install.listenAddress")}</Typography.Text>
                <Input aria-label={t("install.listenAddress")} value={listen} onChange={(event) => setListen(event.target.value)} placeholder={serverDialAutoPort ? t("install.automatic") : DEFAULT_AGENT_LISTEN} />
                <Typography.Text type="secondary">{t("install.listenAddressHelp")}</Typography.Text>
                <Typography.Text strong>{t("install.agentAddress")}</Typography.Text>
                <Input
                  aria-label={t("install.agentAddress")}
                  value={serverDialAddress}
                  onChange={(event) => setServerDialAddress(event.target.value)}
                  placeholder={serverDialAutoPort ? t("install.automatic") : t("install.addressPlaceholder")}
                />
                <Typography.Text type="secondary">{t(serverDialAutoPort ? "install.autoPortHelp" : "install.agentAddressHelp")}</Typography.Text>
              </>
            )}
            <Collapse
              ghost
              items={[{
                key: "advanced",
                label: t("install.advanced"),
                children: (
                  <Space direction="vertical" size={10} style={{ width: "100%" }}>
                    <Typography.Text strong>{t("install.installDir")}</Typography.Text>
                    <Input aria-label={t("install.installDir")} value={installDir} onChange={(event) => setInstallDir(event.target.value)} placeholder="/opt/gpuworks" />
                    <Typography.Text type="secondary">{t("install.installDirHelp")}</Typography.Text>
                    <Space size={8}>
                      <Switch checked={useSystemd} onChange={setUseSystemd} aria-label={t("install.useSystemd")} />
                      <Typography.Text strong>{t("install.useSystemd")}</Typography.Text>
                    </Space>
                    <Typography.Text type="secondary">{t(useSystemd ? "install.useSystemdHelp" : "install.useSystemdOffHelp")}</Typography.Text>
                    <Typography.Text strong>{t("install.credentialOverride")}</Typography.Text>
                    <Typography.Text type="secondary">{t("install.credentialOverrideHelp")}</Typography.Text>
                    <Input.TextArea
                      rows={6}
                      value={key}
                      onChange={(event) => setKey(event.target.value)}
                      placeholder={"-----BEGIN OPENSSH PRIVATE KEY-----\n..."}
                      aria-label={t("install.privateKey")}
                    />
                    <Input.Password
                      value={password}
                      onChange={(event) => setPassword(event.target.value)}
                      placeholder={t("install.passwordPlaceholder")}
                      aria-label={t("install.password")}
                      autoComplete="off"
                    />
                  </Space>
                ),
              }]}
            />
          </>
        )}
        {result?.server_url && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {t("install.serverURLUsed", { url: result.server_url })}
          </Typography.Text>
        )}
        {result?.ssh_connection && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {isCommandChannel(result.ssh_connection)
              ? t("install.connectionUsedChannel", { label: localizedText(result.ssh_connection.label, language) || result.ssh_connection.type })
              : t("install.connectionUsed", { label: localizedText(result.ssh_connection.label, language) || "SSH", user: result.ssh_connection.user || "root", host: result.ssh_connection.host, port: result.ssh_connection.port })}
          </Typography.Text>
        )}
        {result?.ssh_attempts?.filter((attempt) => attempt.status === "failed").map((attempt) => (
          <Alert key={`${attempt.connection_id}:${attempt.target}`} type="warning" showIcon message={t("install.attemptFailed", { target: attempt.target })} description={attempt.detail} />
        ))}
        {result?.checks && <InstallChecks checks={result.checks} />}
        {(!!result?.precheck_details?.length || !!result?.install_details?.length) && (
          <Collapse
            ghost
            items={[
              ...(result?.precheck_details?.length ? [{ key: "precheck", label: t("install.precheckDetails"), children: <InstallChecks checks={result.precheck_details} /> }] : []),
              ...(result?.install_details?.length ? [{ key: "details", label: t("install.details"), children: <InstallChecks checks={result.install_details} /> }] : []),
            ]}
          />
        )}
      </Space>
    </Modal>
  );
}

// The answer to "did it work": a banner at the top of the dialog that stays
// until the dialog is closed, where a toast would have been gone in seconds.
function InstallVerdictBanner({ outcome, detail }: { outcome: InstallOutcome; detail?: string }) {
  const { t } = useTranslation("providers");
  const type = outcome === "installed" ? "success" : outcome === "failed" ? "error" : "warning";
  return (
    <Alert
      type={type}
      showIcon
      message={t(`install.outcome.${outcome}.title`)}
      description={outcome === "failed" && detail ? detail : t(`install.outcome.${outcome}.description`)}
    />
  );
}

function InstallChecks({ checks }: { checks: CheckResult[] }) {
  const { t } = useTranslation("providers");
  return (
    <Table<CheckResult>
      rowKey="name"
      size="small"
      pagination={false}
      dataSource={checks}
      columns={[
        { title: t("install.checks.item"), dataIndex: "name", width: 130 },
        {
          title: t("install.checks.status"),
          dataIndex: "status",
          width: 80,
          render: (status: CheckResult["status"]) => (
            <Tag color={status === "ok" ? "green" : status === "warn" ? "gold" : "red"}>
              {t(status === "ok" ? "install.checks.passed" : status === "warn" ? "install.checks.warning" : "install.checks.failed")}
            </Tag>
          ),
        },
        { title: t("install.checks.detail"), dataIndex: "detail", render: (detail: string | undefined) => detail || "—" },
      ]}
    />
  );
}

// restartConfirmContent words the restart question by what the restart is
// for: a port, the platform SSH key, or both (INSTANCE_RESTART_REQUIRED's
// need).
function restartConfirmContent(need: unknown): string {
  switch (need) {
    case "ssh_key":
      return "install.restartConfirm.contentSSHKey";
    case "ssh_key_port":
      return "install.restartConfirm.contentSSHKeyPort";
    default:
      return "install.restartConfirm.content";
  }
}

// Each provider gets the platform key onto an instance its own way; the help
// line says which, so an operator knows what the server does on their behalf.
function autoAuthHelpKey(providerKind?: string): string {
  switch (providerKind) {
    case "aliyun":
    case "digitalocean":
    case "runpod":
    case "vast":
    case "compshare":
    case "autodl":
      return `install.autoAuth.${providerKind}`;
  }
  return "install.autoAuth.other";
}
