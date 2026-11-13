import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Card, Popconfirm, Space, Table, Tag, Tooltip, Typography, message } from "antd";
import type { ColumnsType } from "antd/es/table";
import { useTranslation } from "react-i18next";
import { api, type AIConfig } from "../../api";
import AIConfigModal, { presetLabel } from "./AIConfigModal";

const { Text } = Typography;

/**
 * externalHost is the host an AI endpoint sends data to, or "" when the
 * endpoint is this platform itself (a model served here, reached through its
 * own gateway) - then nothing leaves the platform.
 */
export function externalHost(baseURL: string, origin: string = window.location.origin): string {
  try {
    const url = new URL(baseURL, origin);
    return url.origin === new URL(origin).origin ? "" : url.host;
  } catch {
    return baseURL;
  }
}

/**
 * AICard lists the AI services the assistant can use and edits them in place:
 * every field of a stored service is editable, and the key is replaced only
 * when a new one is typed.
 */
export default function AICard() {
  const { t } = useTranslation("settings");
  const [configs, setConfigs] = useState<AIConfig[]>([]);
  const [addOpen, setAddOpen] = useState(false);
  const [editing, setEditing] = useState<AIConfig | null>(null);

  const load = useCallback(
    () => api.aiConfigs().then((list) => setConfigs(list.items)).catch(() => {}),
    [],
  );
  // The assistant uses the stored default, and the server guarantees there is
  // exactly one while any service is stored: a create, an edit and a delete all
  // keep that rule, and the database refuses a second mark. So the notice names
  // the row the traffic goes to, and there is nothing to guess at - the
  // fallback this once had picked the first row of the list, which is the newest
  // one, while the server's own reader fell back to the oldest; the notice could
  // name a host the assistant never called.
  const active = configs.find((c) => c.is_default);
  const egressHost = active ? externalHost(active.base_url) : "";
  useEffect(() => {
    load();
  }, [load]);

  const del = async (id: number) => {
    try {
      const answer = await api.deleteAIConfig(id);
      // The service the operator deleted may have been the one in use; the
      // server names the one that took over, so the console tells them rather
      // than letting the egress notice above change host without a word.
      const promoted = configs.find((c) => c.id === answer?.promoted_id);
      message.success(promoted ? t("ai.deletedPromoted", { name: promoted.name }) : t("ai.deleted"));
      load();
    } catch (e) {
      message.error(t("error.delete", { detail: (e as Error).message }));
    }
  };

  const close = () => {
    setAddOpen(false);
    setEditing(null);
  };
  const done = () => {
    close();
    load();
  };

  const columns: ColumnsType<AIConfig> = [
    { title: t("ai.name"), dataIndex: "name" },
    { title: t("ai.preset"), dataIndex: "preset", width: 180, render: (preset: string) => presetLabel(t, preset) },
    {
      title: t("ai.baseUrl"),
      dataIndex: "base_url",
      render: (baseURL: string) =>
        baseURL ? (
          <Tooltip title={baseURL}>
            <Text ellipsis style={{ maxWidth: 260, display: "inline-block" }}>
              {baseURL}
            </Text>
          </Tooltip>
        ) : (
          "—"
        ),
    },
    { title: t("ai.model"), dataIndex: "model", render: (model: string) => model || "—" },
    {
      title: t("ai.default"),
      dataIndex: "is_default",
      width: 80,
      render: (isDefault: boolean) => (isDefault ? <Tag color="green">{t("ai.default")}</Tag> : "—"),
    },
    {
      title: "",
      key: "action",
      width: 160,
      render: (_: unknown, c: AIConfig) => (
        <Space size={0}>
          <Button type="link" size="small" onClick={() => setEditing(c)}>
            {t("common.edit")}
          </Button>
          <Popconfirm title={t("ai.deleteConfirm", { name: c.name })} onConfirm={() => del(c.id)} okText={t("common.delete")} cancelText={t("common.cancel")}>
            <Button type="link" danger size="small">
              {t("common.delete")}
            </Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  return (
    <Card
      title={t("ai.service")}
      extra={
        <Button type="primary" onClick={() => setAddOpen(true)}>
          {t("common.add")}
        </Button>
      }
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message={t("ai.notice")}
      />
      {egressHost && <Alert type="warning" showIcon style={{ marginBottom: 16 }} message={t("ai.egressNotice", { host: egressHost })} />}
      <Table<AIConfig>
        rowKey="id"
        columns={columns}
        dataSource={configs}
        pagination={false}
        locale={{ emptyText: t("ai.empty") }}
      />
      <AIConfigModal open={addOpen} onClose={close} onDone={done} />
      <AIConfigModal open={editing !== null} editing={editing ?? undefined} onClose={close} onDone={done} />
    </Card>
  );
}
