import { useRef, useState } from "react";
import {
  Alert,
  Button,
  Card,
  Form,
  Input,
  Modal,
  Popconfirm,
  Space,
  Switch,
  Tag,
  Tooltip,
  Typography,
  message,
} from "antd";
import { EditOutlined, StopOutlined } from "@ant-design/icons";
import { Link } from "react-router-dom";
import { api, usePolling, type ApiKey, type List } from "../api";
import { analyticsApi, type KeyUsageList, type UsageCounts } from "../analyticsApi";
import ResourceTable, { type ResourceColumn } from "../components/resourceTable/ResourceTable";
import PageHeader from "../components/PageHeader";
import { formatTime } from "../i18n/datetime";
import { useTranslation } from "react-i18next";
import { fmtCompact, fmtInt } from "./analytics/format";
import PlatformTokensCard from "./apikeys/PlatformTokensCard";
import { ALL_DEPLOYMENTS, KeyScopeCell, KeyScopeInput, KeyScopeModal, scopeRequest, scopeRule } from "./apikeys/KeyScope";
import SecretModal from "./apikeys/SecretModal";

const { Paragraph, Text } = Typography;

export default function APIKeysPage() {
  const { t, i18n } = useTranslation("apikeys");
  const language = i18n.resolvedLanguage ?? "en";
  const { data, refetch, loading, error, updatedAt } = usePolling<List<ApiKey>>(api.apiKeys, 15000);
  const [createOpen, setCreateOpen] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);
  const [showRevoked, setShowRevoked] = useState(false);
  const [editing, setEditing] = useState<ApiKey | null>(null);
  // Token usage is a convenience beside the keys: while it loads, or if it
  // fails, its columns read as a dash and the table is unaffected.
  const usage = usePolling<KeyUsageList>(() => analyticsApi.keyUsage(), 60000);
  const usageByPrefix = new Map((usage.data?.items ?? []).map((u) => [u.prefix, u]));
  const keys = data?.items ?? [];
  const revokedCount = keys.filter((k) => k.revoked_at).length;
  const shown = showRevoked ? keys : keys.filter((k) => !k.revoked_at);

  // tokensCell is a key's prompt + completion tokens over a period, linking to
  // that period's usage in Usage analytics; no record at all is a dash.
  const tokensCell = (k: ApiKey, counts: UsageCounts | undefined, range: "today" | "7d") => {
    if (!counts) return "—";
    const total = counts.prompt_tokens + counts.completion_tokens;
    const text = fmtCompact(total, language);
    if (counts.requests === 0) return text;
    return (
      <Tooltip title={t("tokens.tooltip", { input: fmtInt(counts.prompt_tokens), cached: fmtInt(counts.cached_tokens), output: fmtInt(counts.completion_tokens) })}>
        <Link to={`/analytics/usage?keyPrefix=${encodeURIComponent(k.prefix)}&range=${range}`} aria-label={t("tokens.open", { tokens: text, name: k.name })}>{text}</Link>
      </Tooltip>
    );
  };
  const tokensOf = (counts: UsageCounts | undefined) => (counts ? counts.prompt_tokens + counts.completion_tokens : null);

  // A token has no detail of its own, so its name is plain text rather than a link.
  const columns: ResourceColumn<ApiKey>[] = [
    { key: "name", role: "name", title: t("table.name"), width: 220, sortValue: (k) => k.name, render: (_: unknown, k: ApiKey) => k.name || "—" },
    { key: "prefix", title: t("table.prefix"), width: 180, render: (_: unknown, k: ApiKey) => <Text code>{k.prefix}</Text> },
    {
      key: "status",
      title: t("table.status"),
      width: 160,
      sortValue: (k) => (k.revoked_at ? 1 : 0),
      render: (_: unknown, k: ApiKey) =>
        k.revoked_at ? (
          <Tooltip title={t("status.revokedAt", { time: formatTime(k.revoked_at) })}>
            <Tag>{t("status.revoked")}</Tag>
          </Tooltip>
        ) : (
          <Tag color="green">{t("status.active")}</Tag>
        ),
    },
    { key: "scope", title: t("table.scope"), width: 240, render: (_: unknown, k: ApiKey) => <KeyScopeCell apiKey={k} /> },
    {
      key: "tokens_today",
      title: t("table.tokensToday"),
      width: 130,
      sortValue: (k) => tokensOf(usageByPrefix.get(k.prefix)?.today),
      render: (_: unknown, k: ApiKey) => tokensCell(k, usageByPrefix.get(k.prefix)?.today, "today"),
    },
    {
      key: "tokens_7d",
      title: t("table.tokens7d"),
      width: 150,
      sortValue: (k) => tokensOf(usageByPrefix.get(k.prefix)?.last_7_days),
      render: (_: unknown, k: ApiKey) => tokensCell(k, usageByPrefix.get(k.prefix)?.last_7_days, "7d"),
    },
    { key: "last_used", title: t("table.lastUsed"), width: 180, sortValue: (k) => k.last_used, render: (_: unknown, k: ApiKey) => formatTime(k.last_used, t("never")) },
    { key: "created_at", title: t("table.createdAt"), width: 180, sortValue: (k) => k.created_at, render: (_: unknown, k: ApiKey) => formatTime(k.created_at) },
    {
      key: "action",
      role: "actions",
      width: 110,
      render: (_: unknown, k: ApiKey) =>
        k.revoked_at ? null : (
          <Space size={0}>
            <Tooltip title={t("scope.edit")}>
              <Button type="text" size="small" icon={<EditOutlined />} aria-label={t("scope.editAria", { name: k.name })} onClick={() => setEditing(k)} />
            </Tooltip>
            <Popconfirm
              title={t("revoke.confirm", { name: k.name })}
              onConfirm={async () => {
                try {
                  await api.revokeApiKey(k.prefix);
                  message.success(t("revoke.success"));
                  refetch();
                } catch (e) {
                  message.error(t("revoke.failed", { detail: (e as Error).message }));
                }
              }}
              okText={t("actions.revoke")}
              cancelText={t("actions.cancel")}
              okButtonProps={{ danger: true }}
            >
              <Tooltip title={t("actions.revoke")}>
                <Button type="text" size="small" danger icon={<StopOutlined />} aria-label={t("actions.revoke")} />
              </Tooltip>
            </Popconfirm>
          </Space>
        ),
    },
  ];

  // Integration examples use the address this page was opened from.
  const gateway = window.location.origin;

  return (
    <>
      <PageHeader
        title={t("title")}
        description={t("description")}
      />
      <Card
        title={t("gateway.title")}
        style={{ marginBottom: 16 }}
        extra={
          <Space size={16}>
            {revokedCount > 0 && (
              <Space size={6}>
                <Switch size="small" checked={showRevoked} onChange={setShowRevoked} aria-label={t("showRevoked", { count: revokedCount })} />
                <Text type="secondary">{t("showRevoked", { count: revokedCount })}</Text>
              </Space>
            )}
            <Button type="primary" onClick={() => setCreateOpen(true)}>{t("create.button")}</Button>
          </Space>
        }
      >
      <Paragraph type="secondary">{t("gateway.description")}</Paragraph>
      <ResourceTable<ApiKey>
        tableId="api-keys"
        rowKey="prefix"
        columns={columns}
        data={shown}
        loading={loading}
        error={error}
        onRefresh={refetch}
        updatedAt={updatedAt}
        emptyKind="data"
        emptyDescription={t("empty")}
      />

      <Alert
        style={{ marginTop: 16 }}
        type="info"
        message={t("usage")}
        description={
          <Paragraph style={{ margin: 0 }}>
            <pre className="code-block">
              {`curl ${gateway}/v1/chat/completions \\
  -H "Content-Type: application/json" \\
  -H "Authorization: Bearer <key>" \\
  -d '{"model":"<served_name>","messages":[{"role":"user","content":"hi"}]}'`}
            </pre>
            {t("usageBatches")} <Link to="/batches">{t("usageBatchesLink")}</Link>
          </Paragraph>
        }
      />

      <CreateModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={(key) => {
          setCreateOpen(false);
          setSecret(key);
          refetch();
        }}
      />
      <SecretModal secret={secret} onClose={() => setSecret(null)} />
      <KeyScopeModal
        apiKey={editing}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null);
          refetch();
        }}
      />
      </Card>
      <PlatformTokensCard />
    </>
  );
}

function CreateModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: (key: string) => void;
}) {
  const { t } = useTranslation("apikeys");
  const [form] = Form.useForm();
  const [submitting, setSubmitting] = useState(false);
  const submittingRef = useRef(false);

  const submit = async () => {
    if (submittingRef.current) return;
    submittingRef.current = true;
    try {
      let v;
      try {
        v = await form.validateFields();
      } catch {
        // Form.Item presents validation errors beside the corresponding field.
        return;
      }
      setSubmitting(true);
      const res = await api.createApiKey(v.name, scopeRequest(v.scope));
      form.resetFields();
      onCreated(res.key);
    } catch (e) {
      message.error(t("create.failed", { detail: (e as Error).message }));
    } finally {
      submittingRef.current = false;
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title={t("create.title")}
      open={open}
      onCancel={onClose}
      onOk={submit}
      confirmLoading={submitting}
      okText={t("actions.create")}
      cancelText={t("actions.cancel")}
      destroyOnHidden
    >
      <Form form={form} layout="vertical">
        <Form.Item label={t("table.name")} name="name" rules={[{ required: true, whitespace: true, message: t("create.nameRequired") }]}>
          <Input placeholder={t("create.namePlaceholder")} onPressEnter={() => void submit()} />
        </Form.Item>
        <Form.Item label={t("scope.label")} name="scope" initialValue={ALL_DEPLOYMENTS} rules={[scopeRule(t)]}>
          <KeyScopeInput />
        </Form.Item>
      </Form>
    </Modal>
  );
}
