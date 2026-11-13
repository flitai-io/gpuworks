import { useEffect, useRef, useState } from "react";
import { CopyOutlined } from "@ant-design/icons";
import { Alert, Button, Collapse, Drawer, Form, Input, Modal, Radio, Space, Switch, Table, Tag, Typography, message } from "antd";
import type { ColumnsType } from "antd/es/table";
import { api, type CheckResult, type SSHInstallReport, type SSHPrecheckReport } from "../api";
import { APIError, apiErrorText } from "../apiError";
import { buildInstallCommand } from "../nodeInstallCommand";
import { DEFAULT_AGENT_LISTEN } from "../agentListen";
import SSHCredentialFields, { type SSHAuthMode } from "./SSHCredentialFields";
import TemporaryAddressHint from "./TemporaryAddressHint";
import { useCopy } from "./CopyButton";
import { useTranslation } from "react-i18next";

const { Paragraph, Text } = Typography;

// Install answers these when the host already has a gpuworks agent.
const REPLACE_AGENT_CODES = new Set(["NODE_INSTALL_HOST_ALREADY_NODE", "NODE_INSTALL_EXISTING_AGENT", "NODE_INSTALL_EXISTING_AGENT_UNKNOWN", "NODE_INSTALL_EXISTING_AGENT_SELF", "NODE_INSTALL_EXISTING_AGENT_SAME_DIR"]);

// The offline package a node the server dials installs from when it has no
// SSH: it cannot download from the server (offlinepkg on the server).
const OFFLINE_PACKAGE = "/dl/gpuworks-agent.run";

export default function AddNodeDrawer({
  open,
  onClose,
  onInstalled,
}: {
  open: boolean;
  onClose: () => void;
  onInstalled: () => void;
}) {
  const { t } = useTranslation("nodes");
  const { t: tCommon } = useTranslation("common");
  const [copy, copyHolder] = useCopy();
  const origin = window.location.origin;
  const [form] = Form.useForm();
  const [sshSupported, setSshSupported] = useState<"yes" | "no">("yes");
  const [connectMode, setConnectMode] = useState<"agent_dial" | "server_dial">("agent_dial");
  const [serverURL, setServerURL] = useState(origin);
  // A node the server dials never contacts the server, so it is given no
  // server address at all.
  const effectiveServerURL = connectMode === "agent_dial" ? serverURL.trim() || origin : undefined;
  const [hostAddress, setHostAddress] = useState("");
  const [installDir, setInstallDir] = useState("");
  const [serverDialListen, setServerDialListen] = useState(DEFAULT_AGENT_LISTEN);
  const [offlineChecksum, setOfflineChecksum] = useState("");
  const [useSystemd, setUseSystemd] = useState(true);
  const [nodeName, setNodeName] = useState("");
  const [pendingCommand, setPendingCommand] = useState("");
  const [pendingNodeUUID, setPendingNodeUUID] = useState("");
  const requestVersion = useRef(0);
  const [creatingPending, setCreatingPending] = useState(false);
  const [authMode, setAuthMode] = useState<SSHAuthMode>("platform_key");
  const [report, setReport] = useState<SSHPrecheckReport | null>(null);
  const [installReport, setInstallReport] = useState<SSHInstallReport | null>(null);
  const [checking, setChecking] = useState(false);
  const [installing, setInstalling] = useState(false);
  const installInFlight = useRef(false);
  const precheckSummary = summarizeChecks(report?.checks ?? []);
  const installSummary = summarizeChecks(installReport?.checks ?? []);

  const cmd = pendingCommand || t("add.commandPlaceholder");

  const resetResults = () => {
    requestVersion.current++;
    setPendingNodeUUID("");
    setPendingCommand("");
    setChecking(false);
    setCreatingPending(false);
    setReport(null);
    setInstallReport(null);
  };

  const resetForm = () => {
    form.resetFields();
    resetResults();
    setSshSupported("yes");
    setConnectMode("agent_dial");
    setServerURL(origin);
    setHostAddress("");
    setInstallDir("");
    setServerDialListen(DEFAULT_AGENT_LISTEN);
    setUseSystemd(true);
    setNodeName("");
    setPendingCommand("");
    setAuthMode("platform_key");
  };

  const handleClose = () => { resetForm(); onClose(); };

  const previouslyOpen = useRef(open);
  useEffect(() => {
    // A never-opened drawer has no mounted Form to reset. Initial state is
    // already empty; reset only when an open drawer is closed externally.
    if (previouslyOpen.current && !open) resetForm();
    previouslyOpen.current = open;
  }, [open]);

  useEffect(() => {
    if (!open || !pendingNodeUUID) return;
    const version = requestVersion.current;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    const checkConnection = async () => {
      try {
        const node = await api.node(pendingNodeUUID);
        if (!active || version !== requestVersion.current) return;
        if (node.uuid === pendingNodeUUID && node.state === "online") {
          message.success(t("add.message.connected"));
          onInstalled();
          handleClose();
          return;
        }
      } catch {
        // Installation can complete before registration; a transient read
        // failure must not report success or discard the installation report.
      }
      if (active && version === requestVersion.current) timer = setTimeout(checkConnection, 3000);
    };
    void checkConnection();
    return () => { active = false; clearTimeout(timer); };
  }, [open, pendingNodeUUID]);

  const createPendingNode = async () => {
    if (connectMode === "server_dial" && !hostAddress.trim()) {
      message.error(t("add.message.hostAddressRequired"));
      return
    }
    setCreatingPending(true);
    const version = requestVersion.current;
    try {
      const created = await api.createNode({
        name: nodeName.trim() || undefined,
        connect_mode: connectMode,
        server_dial_listen: connectMode === "server_dial" ? serverDialListen.trim() || DEFAULT_AGENT_LISTEN : undefined,
        host_address: connectMode === "server_dial" ? hostAddress || undefined : undefined,
        server_url: effectiveServerURL,
      });
      if (version !== requestVersion.current) return;
      setPendingNodeUUID(created.node.uuid);
      setPendingCommand(buildInstallCommand({
        baseCommand: created.install_command,
        token: created.token,
        connectMode,
        serverURL: effectiveServerURL ?? "",
        origin,
        installDir,
        noSystemd: !useSystemd,
      }));
      message.success(t("add.message.created"));
      onInstalled();
    } catch (e) {
      if (version !== requestVersion.current) return;
      message.error(t("add.message.createFailed", { detail: (e as Error).message }));
    } finally {
      if (version === requestVersion.current) setCreatingPending(false);
    }
  };

  const copyCommand = () => copy(cmd, { success: t("add.message.copied") });

  const buildAuthPayload = (values: any) => ({
    target: values.target,
    name: nodeName.trim() || undefined,
    auth_mode: authMode,
    password: authMode === "password" || authMode === "keyboard_interactive" ? values.password : undefined,
    private_key_pem: authMode === "private_key" ? values.private_key_pem?.trim() || undefined : undefined,
    server_url: effectiveServerURL,
    install_dir: installDir || undefined,
    connect_mode: connectMode,
    server_dial_listen: connectMode === "server_dial" ? serverDialListen.trim() || DEFAULT_AGENT_LISTEN : undefined,
    host_address: connectMode === "server_dial" ? hostAddress.trim() : undefined,
    no_systemd: useSystemd ? undefined : true,
  });

  // The package's checksum, shown beside its download once there is a
  // command to run it with.
  useEffect(() => {
    if (!pendingCommand || connectMode !== "server_dial" || sshSupported !== "no" || typeof fetch !== "function") return;
    let active = true;
    fetch(`${OFFLINE_PACKAGE}.sha256`)
      .then((res) => (res.ok ? res.text() : ""))
      .then((text) => { if (active) setOfflineChecksum(text.trim().split(/\s+/)[0] ?? ""); })
      .catch(() => undefined);
    return () => { active = false; };
  }, [pendingCommand, connectMode, sshSupported]);

  const requireHostAddress = () => {
    if (connectMode === "server_dial" && !hostAddress.trim()) {
      message.error(t("add.message.hostAddressRequired"));
      return false;
    }
    return true;
  };

  const runPrecheck = async () => {
    const version = requestVersion.current;
    const values = await form.validateFields().catch(() => null);
    if (!values || version !== requestVersion.current) return; // the form shows the field errors
    if (!requireHostAddress()) return;
    setChecking(true);
    try {
      const next = await api.precheckNode(buildAuthPayload(values));
      if (version !== requestVersion.current) return;
      setReport(next);
      setInstallReport(null);
      const summary = summarizeChecks(next.checks ?? []);
      if (summary.errors > 0) {
        message.error(t("add.message.precheckWithErrors"));
      } else if (summary.warns > 0) {
        message.warning(t("add.message.precheckWithIssues"));
      } else {
        message.success(t("add.message.precheckComplete"));
      }
    } catch (e) {
      if (version !== requestVersion.current) return;
      message.error(t("add.message.precheckFailed", { detail: apiErrorText(tCommon, e) }));
    } finally {
      if (version === requestVersion.current) setChecking(false);
    }
  };

  const runInstall = async (replaceExistingAgent = false) => {
    if (installInFlight.current) return;
    const version = requestVersion.current;
    const values = await form.validateFields().catch(() => null);
    if (!values || version !== requestVersion.current || installInFlight.current) return;
    if (!requireHostAddress()) return;
    // Invalidating form results does not cancel the remote operation. Keep
    // this guard until it settles, including across drawer close/reopen.
    installInFlight.current = true;
    setInstalling(true);
    try {
      const next = await api.installNode({ ...buildAuthPayload(values), replace_existing_agent: replaceExistingAgent || undefined });
      if (version !== requestVersion.current) return;
      setInstallReport(next);
      // Failed installation rejects the request. A successful report may
      // still contain runtime capability errors that do not prevent joining.
      setPendingNodeUUID(next.node_uuid);
      message.success(t("add.message.installExecuted"));
      onInstalled();
    } catch (e) {
      if (version !== requestVersion.current) return;
      if (!replaceExistingAgent && e instanceof APIError && REPLACE_AGENT_CODES.has(e.code)) {
        // The host already has an agent: installing replaces it, so ask first.
        Modal.confirm({
          title: t("add.replaceAgent.title"),
          // A nohup install replaces only the agent in its own directory,
          // which the error already says; the others speak of the service.
          content: e.code === "NODE_INSTALL_EXISTING_AGENT_SAME_DIR" ? apiErrorText(tCommon, e) : `${apiErrorText(tCommon, e)} ${t("add.replaceAgent.content")}`,
          okText: t("add.replaceAgent.confirm"),
          okButtonProps: { danger: true },
          cancelText: t("add.replaceAgent.cancel"),
          onOk: () => version === requestVersion.current ? runInstall(true) : undefined,
        });
      } else {
        message.error(t("add.message.installFailed", { detail: apiErrorText(tCommon, e) }));
      }
    } finally {
      installInFlight.current = false;
      setInstalling(false);
    }
  };

  // Install directory and systemd apply whether the agent is installed over
  // SSH or by hand, so both branches render the same advanced section.
  const advanced = (
    <Collapse
      ghost
      style={{ marginBottom: 16 }}
      items={[
        {
          key: "advanced",
          label: t("add.advanced"),
          children: (
            <>
              <Form.Item label={t("add.installDir")} tooltip={t("add.installDirTooltip")} extra={t("add.installDirHelp")} style={{ marginBottom: 12 }}>
                <Input value={installDir} onChange={(e) => { resetResults(); setInstallDir(e.target.value); }} placeholder={t("add.installDirPlaceholder")} />
              </Form.Item>
              <Form.Item label={t("add.useSystemd")} tooltip={t("add.useSystemdTooltip")} extra={useSystemd ? undefined : t("add.useSystemdOffHelp")} style={{ marginBottom: 0 }}>
                <Switch checked={useSystemd} onChange={(checked) => { resetResults(); setUseSystemd(checked); }} aria-label={t("add.useSystemd")} />
              </Form.Item>
            </>
          ),
        },
      ]}
    />
  );

  return (
    <Drawer title={t("action.add")} open={open} onClose={handleClose} width={640} destroyOnHidden>
      {copyHolder}
      {pendingNodeUUID && <Alert type="info" showIcon message={t("add.waitingForConnection")} description={t("add.waitingForConnectionHelp")} style={{ marginBottom: 16 }} />}
      <Space direction="vertical" size={16} style={{ width: "100%" }}>
        <Form.Item label={t("add.sshSupportLabel")} style={{ marginBottom: 0 }}>
          <Radio.Group value={sshSupported} onChange={(e) => { resetResults(); setSshSupported(e.target.value); }}>
            <Radio.Button value="yes">{t("add.sshSupported")}</Radio.Button>
            <Radio.Button value="no">{t("add.sshUnsupported")}</Radio.Button>
          </Radio.Group>
        </Form.Item>

        <Form.Item label={t("add.nodeNameOptional")} style={{ marginBottom: 0 }}>
          <Input value={nodeName} onChange={(e) => { resetResults(); setNodeName(e.target.value); }} placeholder={t("add.nodeNamePlaceholder")} />
        </Form.Item>

        <Form.Item label={t("add.connectionDirection")} style={{ marginBottom: 0 }}>
          <Radio.Group value={connectMode} onChange={(e) => { resetResults(); setConnectMode(e.target.value); }}>
            <Radio.Button value="agent_dial">{t("connection.agentDial")}</Radio.Button>
            <Radio.Button value="server_dial">{t("connection.serverDial")}</Radio.Button>
          </Radio.Group>
        </Form.Item>

        {connectMode === "agent_dial" ? (
          <Form.Item label={t("detail.serverExternalAddress")} style={{ marginBottom: 0 }}>
            <Input value={serverURL} onChange={(e) => { resetResults(); setServerURL(e.target.value); }} placeholder={origin} />
            {/* The hint belongs on the address the node will dial: it becomes
                server_url and has to keep working after the connector restarts. */}
            <TemporaryAddressHint value={serverURL} />
          </Form.Item>
        ) : (
          <>
            {/* Where the agent listens comes first: the address below may name
                another port when a platform maps it, and reading only that
                one the agent would seem to listen there. */}
            <Form.Item label={t("add.agentListenAddress")} help={t("add.agentListenAddressHelp")} style={{ marginBottom: 0 }}>
              <Input
                aria-label={t("add.agentListenAddress")}
                value={serverDialListen}
                onChange={(e) => { resetResults(); setServerDialListen(e.target.value); }}
                placeholder={DEFAULT_AGENT_LISTEN}
              />
            </Form.Item>
            <Form.Item
              label={t("add.hostAddress")}
              help={t("add.hostAddressHelp")}
              required
              style={{ marginBottom: 0 }}
            >
              <Input
                aria-label={t("add.hostAddress")}
                value={hostAddress}
                onChange={(e) => { resetResults(); setHostAddress(e.target.value); }}
                placeholder={t("add.hostAddressPlaceholder")}
              />
            </Form.Item>
          </>
        )}

        {sshSupported === "yes" ? (
          <>
            <Form
              form={form}
              layout="vertical"
              onValuesChange={() => resetResults()}
            >
              <Form.Item
                label={t("add.target")}
                name="target"
                help={t("add.targetHelp")}
                rules={[{ required: true, whitespace: true, message: t("add.targetRequired") }]}
              >
                <Input placeholder="root@192.0.2.10:22" />
              </Form.Item>
              <SSHCredentialFields authMode={authMode} onAuthModeChange={(mode) => { resetResults(); setAuthMode(mode); }} />

              {advanced}

              <Space>
                <Button type="primary" loading={checking} disabled={installing || !!pendingNodeUUID} onClick={runPrecheck}>
                  {t("add.startPrecheck")}
                </Button>
                <Button loading={installing} disabled={installing || checking || !!pendingNodeUUID} onClick={() => void runInstall()}>
                  {t("add.installAgent")}
                </Button>
                {report?.host && (
                  <Text type="secondary">
                    {report.user}@{report.host}:{report.port}
                  </Text>
                )}
              </Space>
            </Form>
            {report && <SummaryStrip title={t("add.precheckSummary")} summary={precheckSummary} tone="info" />}
            {report && <ReportTable title={t("add.precheckResults")} checks={report.checks} />}
            {installReport && (
              <>
                <SummaryStrip
                  title={t("add.installSummary")}
                  summary={installSummary}
                  tone={installSummary.errors > 0 ? "error" : installSummary.warns > 0 ? "warning" : "success"}
                />
                <ReportTable
                  title={t("add.installResults", { binary: installReport.binary_name, installDir: installReport.install_dir, address: installReport.server_dial_address ? ` · ${installReport.server_dial_address}` : "" })}
                  checks={installReport.checks}
                />
              </>
            )}
          </>
        ) : (
          <>
            {advanced}
            <div>
              <Paragraph>{t(connectMode === "server_dial" ? "add.offlineInstructions" : "add.manualInstructions")}</Paragraph>
              {connectMode === "server_dial" && (
                <Paragraph>
                  <a href={OFFLINE_PACKAGE} download>{t("add.offlineDownload")}</a>
                  {offlineChecksum && <Text type="secondary" style={{ display: "block" }}>{t("add.offlineChecksum", { sum: offlineChecksum })}</Text>}
                </Paragraph>
              )}
              {!pendingCommand && <Button type="primary" loading={creatingPending} onClick={createPendingNode}>{t("add.createCommand")}</Button>}
              {pendingCommand && <Text type="secondary">{t("add.tokenSaved")}</Text>}
              <pre
                style={{
                  background: "#0d1117",
                  color: "#e6edf3",
                  padding: 16,
                  borderRadius: 2,
                  overflowX: "auto",
                }}
              >
                {cmd}
              </pre>
              <Space>
                <Button icon={<CopyOutlined />} disabled={!pendingCommand} onClick={() => void copyCommand()}>
                  {t("add.copyCommand")}
                </Button>
              </Space>
            </div>
          </>
        )}
      </Space>
    </Drawer>
  );
}

function SummaryStrip({
  title,
  summary,
  tone,
}: {
  title: string;
  summary: { ok: number; warns: number; errors: number };
  tone: "info" | "success" | "warning" | "error";
}) {
  const { t } = useTranslation("nodes");
  const color =
    tone === "success" ? "green" : tone === "warning" ? "gold" : tone === "error" ? "red" : "blue";
  return (
    <Alert
      type={tone}
      showIcon
      style={{ marginTop: 18, marginBottom: 12 }}
      message={title}
      description={
        <Space wrap size={8}>
          <Tag color="green">{t("add.check.okCount", { count: summary.ok })}</Tag>
          <Tag color="gold">{t("add.check.warnCount", { count: summary.warns })}</Tag>
          <Tag color={color}>{t("add.check.errorCount", { count: summary.errors })}</Tag>
        </Space>
      }
    />
  );
}

function ReportTable({ title, checks }: { title: string; checks: CheckResult[] }) {
  const { t } = useTranslation("nodes");
  const columns: ColumnsType<CheckResult> = [
    { title: t("add.check.item"), dataIndex: "name", width: 150 },
    {
      title: t("add.check.status"),
      dataIndex: "status",
      width: 90,
      render: (status: CheckResult["status"]) => <Tag color={statusColor[status]}>{t(`add.check.${status}`)}</Tag>,
    },
    { title: t("add.check.detail"), dataIndex: "detail", render: (detail: string | undefined) => detail || "—" },
  ];
  return (
    <>
      <Typography.Title level={5} style={{ marginTop: 18 }}>
        {title}
      </Typography.Title>
      <Table<CheckResult>
        rowKey="name"
        size="small"
        pagination={false}
        dataSource={checks}
        columns={columns}
      />
    </>
  );
}

const statusColor: Record<CheckResult["status"], string> = {
  ok: "green",
  warn: "gold",
  error: "red",
};

function summarizeChecks(checks: CheckResult[]) {
  return checks.reduce(
    (acc, check) => {
      if (check.status === "ok") acc.ok += 1;
      else if (check.status === "warn") acc.warns += 1;
      else acc.errors += 1;
      return acc;
    },
    { ok: 0, warns: 0, errors: 0 },
  );
}
