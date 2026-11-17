import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Card, Collapse, Form, Input, Space, Tag, Typography, message } from "antd";
import { useTranslation } from "react-i18next";
import { api, type ServerListen } from "../../api";
import { apiErrorText } from "../../apiError";
import { HelpTip } from "../../ui-kit/HelpTip";

const { Text } = Typography;

interface FormValues {
  console: string;
  agent: string;
  gateway: string;
}

// Addresses are typed as a comma- or space-separated list; most servers have one.
function splitAddresses(raw: string | undefined): string[] {
  return (raw ?? "").split(/[\s,]+/).map((part) => part.trim()).filter(Boolean);
}

function formOf(listen: ServerListen): FormValues {
  return {
    console: listen.console.join(", "),
    agent: listen.agent.join(", "),
    gateway: listen.gateway.join(", "),
  };
}

// The address the health check can reach while the server restarts is the
// page's own origin, so polling it tells when the new process is up.
async function waitForServer(): Promise<void> {
  const deadline = Date.now() + 60_000;
  await new Promise((resolve) => setTimeout(resolve, 1500));
  while (Date.now() < deadline) {
    try {
      const res = await fetch("/healthz", { cache: "no-store" });
      if (res.ok) return;
    } catch {
      // Still restarting.
    }
    await new Promise((resolve) => setTimeout(resolve, 1000));
  }
}

export default function ServerListenCard() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const [listen, setListen] = useState<ServerListen | null>(null);
  const [loadError, setLoadError] = useState("");
  const [saving, setSaving] = useState(false);
  const [restarting, setRestarting] = useState(false);
  const [form] = Form.useForm<FormValues>();

  const load = useCallback(() => {
    return api
      .serverListen()
      .then((value) => {
        setListen(value);
        setLoadError("");
        form.setFieldsValue(formOf(value));
      })
      .catch((error) => setLoadError(apiErrorText(tCommon, error)));
  }, [form, tCommon]);

  useEffect(() => {
    void load();
  }, [load]);

  const save = async () => {
    let values: FormValues;
    try {
      values = await form.validateFields();
    } catch {
      return;
    }
    setSaving(true);
    try {
      const saved = await api.updateServerListen({
        console: splitAddresses(values.console),
        agent: splitAddresses(values.agent),
        gateway: splitAddresses(values.gateway),
      });
      setListen(saved);
      form.setFieldsValue(formOf(saved));
      message.success(t("listen.saved"));
    } catch (error) {
      message.error(apiErrorText(tCommon, error));
    } finally {
      setSaving(false);
    }
  };

  const restart = async () => {
    setRestarting(true);
    try {
      await api.restartServer();
      message.info(t("listen.restarting"));
      await waitForServer();
      await load();
    } catch (error) {
      message.error(apiErrorText(tCommon, error));
    } finally {
      setRestarting(false);
    }
  };

  const editable = listen?.editable ?? false;

  return (
    <Card
      title={t("listen.title")}
      extra={<Button type="primary" loading={saving} disabled={!editable} onClick={() => void save()}>{t("common.save")}</Button>}
    >
      <Space direction="vertical" size={12} style={{ width: "100%", maxWidth: 860 }}>
        <Text type="secondary">{t("listen.description")} <HelpTip content={t("listen.descriptionHelp")} /></Text>
        {loadError && <Alert type="error" showIcon message={loadError} />}
        {listen && !listen.editable && (
          <Alert type="info" showIcon message={listen.overridden ? t("listen.overridden") : t("listen.noConfig")} />
        )}
        {listen?.restart_pending && (
          <Alert
            type="warning"
            showIcon
            message={t("listen.pending")}
            description={listen.restart_mode === "command" ? t("listen.restartCommand", { command: listen.restart_command }) : undefined}
            action={listen.restart_mode === "self" ? (
              <Button size="small" danger loading={restarting} onClick={() => void restart()}>{t("listen.restartNow")}</Button>
            ) : undefined}
          />
        )}
        <Form form={form} layout="vertical" disabled={!editable}>
          <Form.Item
            label={t("listen.console")}
            name="console"
            extra={t("listen.consoleHelp")}
            rules={[{ required: true, whitespace: true, message: t("listen.consoleRequired") }]}
          >
            <Input placeholder="192.0.2.156:8800" />
          </Form.Item>
          <Collapse
            ghost
            items={[{
              key: "advanced",
              label: t("listen.advanced"),
              children: (
                <>
                  <Form.Item label={t("listen.agent")} name="agent" extra={t("listen.agentHelp")}>
                    <Input placeholder={t("listen.inherit")} />
                  </Form.Item>
                  <Form.Item label={t("listen.gateway")} name="gateway" extra={t("listen.gatewayHelp")}>
                    <Input placeholder={t("listen.inherit")} />
                  </Form.Item>
                </>
              ),
            }]}
          />
        </Form>
        {listen && listen.running.length > 0 && (
          <div>
            <Text type="secondary">{t("listen.running")}</Text>
            <ul style={{ margin: "4px 0 0", paddingLeft: 20 }}>
              {listen.running.map((binding) => (
                <li key={binding.addr}>
                  <Text code>{binding.addr}</Text>{" "}
                  {binding.functions.map((fn) => <Tag key={fn}>{t(`listen.function.${fn}`)}</Tag>)}
                </li>
              ))}
            </ul>
          </div>
        )}
      </Space>
    </Card>
  );
}
