import { useEffect, useRef, useState } from "react";
import { Alert, Button, Popconfirm, Space, Table, Tag, Typography, message } from "antd";
import type { ColumnsType } from "antd/es/table";
import { PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, usePolling, type Credential } from "../../../api";
import CredentialDrawer, { credentialLoginHost, isSiteToken } from "./CredentialDrawer";
import CredentialLoginAccess from "./CredentialLoginAccess";
import { outboundPolicyApi } from "../../../outboundPolicyApi";
import { outboundHostName } from "../../../components/OutboundHostAccess";
import { useLoginTests } from "./loginTest";
import { pullErrorText } from "./pullFormat";

/**
 * Credentials lists the stored credentials: registry logins and model-site
 * tokens (a token has no user name). A secret is never shown - only whether
 * one is stored. A credential is handed to a node for the length of a pull and
 * then removed again; "Test login" tries it from the server.
 */
export default function Credentials() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const credentialsQuery = usePolling(() => api.credentials(), 0, []);
  const [editing, setEditing] = useState<Credential | "new" | null>(null);
  const [actionError, setActionError] = useState("");
  const [checkingId, setCheckingId] = useState<number | null>(null);
  const [authorizing, setAuthorizing] = useState<Credential | null>(null);
  const { results, test } = useLoginTests();
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  const testLogin = async (credential: Credential) => {
    setCheckingId(credential.id);
    setActionError("");
    try {
      // A policy read must not turn an existing login test into a credential edit.
      // The test endpoint always enforces the current server-side policy itself.
      const policy = await outboundPolicyApi.get().catch(() => null);
      if (!mounted.current) return;
      if (policy?.restrict_private_hosts) {
        const host = outboundHostName(credentialLoginHost(credential.host, isSiteToken(credential)));
        try {
          const checks = await outboundPolicyApi.checkHosts([host]);
          if (!mounted.current) return;
          if (checks.length === 1 && checks[0].status === "allowed") {
            await test(credential.id);
            return;
          }
        } catch {
          // The selected target's dialog can retry an unavailable host check.
        }
        if (!mounted.current) return;
        setAuthorizing(credential);
        return;
      }
      await test(credential.id);
    } finally {
      if (mounted.current) setCheckingId(null);
    }
  };

  const remove = async (credential: Credential) => {
    setActionError("");
    try {
      await api.deleteCredential(credential.id);
      message.success(t("pull.credentials.deleted"));
      await credentialsQuery.refetch();
    } catch (err) {
      setActionError(pullErrorText(t, tCommon, err));
    }
  };

  const columns: ColumnsType<Credential> = [
    {
      title: t("pull.credentials.type"),
      key: "type",
      render: (_: unknown, credential) => (isSiteToken(credential) ? t("pull.credentials.typeSite") : t("pull.credentials.typeRegistry")),
    },
    { title: t("pull.credentials.host"), dataIndex: "host", render: (host: string) => <Typography.Text code>{host}</Typography.Text> },
    { title: t("pull.credentials.username"), dataIndex: "username", render: (username: string) => username || "—" },
    {
      title: t("pull.credentials.secret"),
      dataIndex: "has_secret",
      render: (has: boolean) => (has ? <Tag color="green">{t("pull.credentials.secretStored")}</Tag> : <Tag color="orange">{t("pull.credentials.secretMissing")}</Tag>),
    },
    {
      title: t("pull.credentials.lastTest"),
      key: "test",
      render: (_: unknown, credential) => {
        const result = results[credential.id];
        if (!result) return <Typography.Text type="secondary">—</Typography.Text>;
        if (result.state === "testing") return <Typography.Text type="secondary">{t("pull.credentials.testing")}</Typography.Text>;
        if (result.state === "ok") return <Tag color="green">{t(isSiteToken(credential) ? "pull.credentials.testOkToken" : "pull.credentials.testOk")}</Tag>;
        return <Typography.Text type="danger" role="alert">{result.text}</Typography.Text>;
      },
    },
    {
      title: t("pull.rules.actions"),
      key: "actions",
      fixed: "right",
      width: 220,
      render: (_: unknown, credential) => (
        <Space size={4}>
          <Button
            size="small"
            type="link"
            aria-label={t("pull.credentials.testFor", { host: credential.host })}
            disabled={!credential.has_secret || checkingId !== null || authorizing !== null || results[credential.id]?.state === "testing"}
            onClick={() => void testLogin(credential)}
          >
            {t("pull.credentials.test")}
          </Button>
          <Button size="small" type="link" onClick={() => setEditing(credential)}>{t("pull.action.edit")}</Button>
          <Popconfirm
            title={t("pull.credentials.deleteConfirm", { host: credential.host })}
            description={t("pull.credentials.deleteConfirmBody")}
            okText={t("pull.action.delete")}
            cancelText={t("pull.action.cancel")}
            okButtonProps={{ danger: true }}
            onConfirm={() => remove(credential)}
          >
            <Button size="small" type="link" danger aria-label={t("pull.credentials.deleteFor", { host: credential.host })}>{t("pull.action.delete")}</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  return (
    <Space direction="vertical" size={12} style={{ width: "100%" }}>
      <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>{t("pull.credentials.intro")}</Typography.Paragraph>
      <div style={{ display: "flex", justifyContent: "flex-end" }}>
        <Button type="primary" icon={<PlusOutlined aria-hidden />} onClick={() => setEditing("new")}>{t("pull.credentials.new")}</Button>
      </div>
      {credentialsQuery.error && (
        <Alert
          type="error"
          showIcon
          message={t("pull.loadFailed", { detail: credentialsQuery.error.message })}
          action={<Button size="small" onClick={() => void credentialsQuery.refetch()}>{t("pull.action.retry")}</Button>}
        />
      )}
      {actionError && <Alert type="error" showIcon role="alert" message={actionError} closable onClose={() => setActionError("")} />}
      <div role="region" aria-label={t("pull.credentials.tableLabel")}>
        <Table<Credential>
          rowKey="id"
          size="small"
          pagination={false}
          loading={credentialsQuery.loading && !credentialsQuery.data}
          columns={columns}
          dataSource={credentialsQuery.data?.items ?? []}
          scroll={{ x: "max-content" }}
          locale={{ emptyText: credentialsQuery.error ? " " : t("pull.credentials.empty") }}
        />
      </div>
      {editing && (
        <CredentialDrawer
          credential={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onChanged={() => void credentialsQuery.refetch()}
        />
      )}
      {authorizing && <CredentialLoginAccess
        credential={authorizing}
        onClose={() => setAuthorizing(null)}
        onTest={async () => { await test(authorizing.id); }}
      />}
    </Space>
  );
}
