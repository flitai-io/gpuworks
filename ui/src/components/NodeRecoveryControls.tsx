import { useEffect, useState } from "react";
import { Button, Checkbox, Descriptions, Form, Input, InputNumber, Modal, Popconfirm, Space, Tag, Timeline, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, useEventPayload, type Node, type SSHCredentialInfo } from "../api";
import SSHCredentialFields, { type SSHAuthMode } from "./SSHCredentialFields";
import { recoveryLabel } from "../nodes/recoveryPhase";
import { formatTime, formatTimeSec } from "../i18n/datetime";

type Updated = (node: Node) => void;

const authModeLabel: Record<SSHAuthMode, string> = {
  platform_key: "ssh.auth.platformKey",
  password: "ssh.auth.password",
  keyboard_interactive: "ssh.auth.keyboardInteractive",
  private_key: "ssh.auth.key",
};

export function NodeSSHAccess({ node, onUpdated }: { node: Node; onUpdated: Updated }) {
  const { t } = useTranslation("nodes");
  const [form] = Form.useForm<{ host: string; port: number; user: string; password?: string; private_key_pem?: string; probe?: boolean }>();
  const [open, setOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [authMode, setAuthMode] = useState<SSHAuthMode>("platform_key");
  const [info, setInfo] = useState<SSHCredentialInfo | null>(null);

  const openEditor = async () => {
    let existing: SSHCredentialInfo | null = null;
    try {
      existing = await api.nodeSSH(node.uuid);
    } catch {
      // A 404 means this pre-existing node has no stored side channel yet.
    }
    setInfo(existing);
    setAuthMode(existing?.auth_mode ?? "platform_key");
    form.setFieldsValue({
      host: existing?.host || node.hostname || "",
      port: existing?.port || 22,
      user: existing?.user || "root",
      password: undefined,
      private_key_pem: undefined,
      probe: true,
    });
    setOpen(true);
  };

  const save = async () => {
    const values = await form.validateFields();
    setSaving(true);
    try {
      const saved = await api.putNodeSSH(node.uuid, {
        host: values.host.trim(),
        port: values.port,
        user: values.user.trim(),
        probe: values.probe,
        auth_mode: authMode,
        password: authMode === "password" || authMode === "keyboard_interactive" ? values.password : undefined,
        private_key_pem: authMode === "private_key" ? values.private_key_pem?.trim() || undefined : undefined,
      });
      setInfo(saved);
      onUpdated(await api.node(node.uuid));
      setOpen(false);
      if (saved.platform_key?.status === "ok") {
        message.success(t("ssh.message.switchedToPlatformKey"));
      } else if (saved.platform_key) {
        message.warning(t("ssh.message.platformKeyNotAuthorized", { detail: saved.platform_key.detail || "" }));
      }
      if (saved.probe?.foreign_server_url) {
        message.warning(t("ssh.message.savedProbeForeignAgent", { server: saved.probe.foreign_server_url }));
      } else if (saved.probe && !saved.probe.reachable) {
        message.warning(t("ssh.message.savedProbeFailed", { detail: saved.probe.detail || t("recovery.phase.unreachable") }));
      } else {
        message.success(t("ssh.message.saved"));
      }
    } catch (error) {
      message.error(t("ssh.message.saveFailed", { detail: (error as Error).message }));
    } finally {
      setSaving(false);
    }
  };

  const remove = async () => {
    setDeleting(true);
    try {
      await api.deleteNodeSSH(node.uuid);
      setInfo(null);
      onUpdated(await api.node(node.uuid));
      setOpen(false);
      message.success(t("ssh.message.cleared"));
    } catch (error) {
      message.error(t("ssh.message.clearFailed", { detail: (error as Error).message }));
    } finally {
      setDeleting(false);
    }
  };

  return (
    <>
      <Space>
        {node.has_ssh_credential ? <Tag color="green">{t("ssh.configured")}</Tag> : <Typography.Text type="secondary">{t("ssh.notConfigured")}</Typography.Text>}
        <Button size="small" onClick={openEditor}>{t(node.has_ssh_credential ? "ssh.edit" : "ssh.configure")}</Button>
      </Space>
      <Modal
        title={t("ssh.title")}
        open={open}
        onCancel={() => setOpen(false)}
        onOk={save}
        confirmLoading={saving}
        okText={t("ssh.save")}
        cancelText={t("ssh.cancel")}
        footer={(_, { OkBtn, CancelBtn }) => (
          <Space style={{ width: "100%", justifyContent: "space-between" }}>
            <span>{info && <Popconfirm title={t("ssh.clearConfirm")} onConfirm={remove}><Button danger loading={deleting}>{t("ssh.clear")}</Button></Popconfirm>}</span>
            <Space><CancelBtn /><OkBtn /></Space>
          </Space>
        )}
      >
        <Form form={form} layout="vertical">
          <Form.Item label={t("ssh.host")} name="host" rules={[{ required: true, whitespace: true, message: t("ssh.hostRequired") }]}><Input placeholder="gpu.example.com" /></Form.Item>
          <Space align="start" style={{ width: "100%" }}>
            <Form.Item label={t("ssh.port")} name="port" rules={[{ required: true }]}><InputNumber min={1} max={65535} /></Form.Item>
            <Form.Item label={t("ssh.user")} name="user" rules={[{ required: true, whitespace: true, message: t("ssh.userRequired") }]}><Input placeholder="root" /></Form.Item>
          </Space>
          <SSHCredentialFields authMode={authMode} onAuthModeChange={setAuthMode} />
          <Form.Item name="probe" valuePropName="checked"><Checkbox>{t("ssh.probeAfterSave")}</Checkbox></Form.Item>
          {info && <Typography.Text type="secondary">{t("ssh.existingAuth", { method: t(authModeLabel[info.auth_mode]) })}</Typography.Text>}
          {info?.auth_mode === "password" && <Typography.Paragraph type="secondary">{t("ssh.savedPasswordHelp")}</Typography.Paragraph>}
        </Form>
      </Modal>
    </>
  );
}

export function NodeRecoveryStatus({ node, onUpdated }: { node: Node; onUpdated: Updated }) {
  const { t } = useTranslation("nodes");
  const [events, setEvents] = useState<Array<{ at: Date; reason: string }>>([]);

  useEffect(() => setEvents([]), [node.uuid]);
  useEventPayload("node", (event) => {
    if (event.key !== node.uuid || !event.reason?.startsWith("recovery:")) return;
    setEvents((current) => [...current.slice(-9), { at: new Date(), reason: event.reason ?? "recovery" }]);
    api.node(node.uuid).then(onUpdated).catch(() => {});
  });

  if (!node.recovery_phase) return null;

  const label = recoveryLabel[node.recovery_phase];
  return (
    <>
      <Typography.Title level={5} style={{ marginTop: 20 }}>{t("recovery.title")}</Typography.Title>
      <Descriptions column={1} bordered size="small">
        <Descriptions.Item label={t("recovery.currentPhase")}><Tag color={label?.color}>{label ? t(label.key) : node.recovery_phase}</Tag></Descriptions.Item>
        <Descriptions.Item label={t("recovery.attempts")}>{node.recovery_attempts ?? 0}</Descriptions.Item>
        <Descriptions.Item label={t("recovery.lastProbe")}>{formatTime(node.last_probe_at)}</Descriptions.Item>
        {node.last_recovery_error && <Descriptions.Item label={t("recovery.lastError")}><Typography.Text type="danger">{node.last_recovery_error}</Typography.Text></Descriptions.Item>}
      </Descriptions>
      {events.length > 0 && (
        <Timeline
          style={{ marginTop: 16 }}
          items={events.map((event) => { const eventLabel = recoveryLabel[event.reason.slice("recovery:".length)]; return { children: `${formatTimeSec(event.at)} · ${eventLabel ? t(eventLabel.key) : event.reason}` }; })}
        />
      )}
    </>
  );
}
