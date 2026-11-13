import { useEffect, useMemo, useState } from "react";
import { Alert, Button, Popconfirm, Space, Table, Tag, Typography, message } from "antd";
import type { ColumnsType } from "antd/es/table";
import { ArrowDownOutlined, ArrowUpOutlined, PlusOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, usePolling, type PullSourceRule } from "../../../api";
import PullRuleDrawer from "./PullRuleDrawer";
import { hostOf, moved, pullErrorText, requirementChips } from "./pullFormat";
import { HelpTip } from "../../../ui-kit/HelpTip";

/**
 * PullSourceRules lists the pull source rules in the order they are tried:
 * which images a rule covers, the alternative addresses a node tries first,
 * whether the image's own registry stays as the last resort, and the nodes it
 * applies to. Whether a registry host has a stored login is shown beside each
 * endpoint; the platform cannot know whether a registry needs one, so it is a
 * neutral fact, not a warning.
 */
export default function PullSourceRules() {
  const { t } = useTranslation("settings");
  const { t: tCommon } = useTranslation("common");
  const rulesQuery = usePolling(() => api.pullSourceRules(), 0, []);
  const credentialsQuery = usePolling(() => api.credentials(), 0, []);
  const [rules, setRules] = useState<PullSourceRule[]>([]);
  const [editing, setEditing] = useState<PullSourceRule | "new" | null>(null);
  const [ordering, setOrdering] = useState(false);
  const [actionError, setActionError] = useState("");

  useEffect(() => {
    if (rulesQuery.data) setRules(rulesQuery.data.items);
  }, [rulesQuery.data]);

  const loginHosts = useMemo(
    () => (credentialsQuery.data ? new Set(credentialsQuery.data.items.map((item) => hostOf(item.host))) : null),
    [credentialsQuery.data],
  );

  const reorder = async (index: number, delta: number) => {
    const next = moved(rules, index, delta);
    if (next === rules) return;
    setOrdering(true);
    setActionError("");
    try {
      const result = await api.orderPullSourceRules(next.map((rule) => rule.id));
      setRules(result.items);
    } catch (err) {
      setActionError(pullErrorText(t, tCommon, err));
    } finally {
      setOrdering(false);
    }
  };

  const remove = async (rule: PullSourceRule) => {
    setActionError("");
    try {
      await api.deletePullSourceRule(rule.id);
      message.success(t("pull.rules.deleted"));
      await rulesQuery.refetch();
    } catch (err) {
      setActionError(pullErrorText(t, tCommon, err));
    }
  };

  const columns: ColumnsType<PullSourceRule> = [
    {
      title: t("pull.rules.order"),
      key: "order",
      width: 120,
      render: (_: unknown, rule, index) => (
        <Space size={4}>
          <span>{index + 1}</span>
          <Button
            size="small"
            type="text"
            aria-label={t("pull.rules.moveUp", { match: rule.match })}
            icon={<ArrowUpOutlined aria-hidden />}
            disabled={ordering || index === 0}
            onClick={() => void reorder(index, -1)}
          />
          <Button
            size="small"
            type="text"
            aria-label={t("pull.rules.moveDown", { match: rule.match })}
            icon={<ArrowDownOutlined aria-hidden />}
            disabled={ordering || index === rules.length - 1}
            onClick={() => void reorder(index, 1)}
          />
        </Space>
      ),
    },
    {
      title: t("pull.rules.type"),
      dataIndex: "kind",
      render: (kind: PullSourceRule["kind"]) => <Tag color={kind === "url" ? "geekblue" : undefined}>{kind === "url" ? t("pull.rules.kindUrl") : t("pull.rules.kindImage")}</Tag>,
    },
    {
      title: t("pull.rules.match"),
      dataIndex: "match",
      render: (match: string, rule) => (
        <Space direction="vertical" size={0}>
          <Typography.Text code>{match}</Typography.Text>
          {rule.kind === "url" && rule.control_plane && <Tag>{t("pull.rules.controlPlaneMark")}</Tag>}
          {rule.note && <Typography.Text type="secondary">{rule.note}</Typography.Text>}
        </Space>
      ),
    },
    {
      title: t("pull.rules.endpoints"),
      dataIndex: "endpoints",
      render: (endpoints: string[]) =>
        endpoints.length === 0 ? (
          <Typography.Text type="secondary">{t("pull.rules.noEndpoints")}</Typography.Text>
        ) : (
          <ol style={{ margin: 0, paddingInlineStart: 20 }}>
            {endpoints.map((endpoint) => (
              <li key={endpoint}><Typography.Text code>{endpoint}</Typography.Text></li>
            ))}
          </ol>
        ),
    },
    {
      title: t("pull.rules.upstream"),
      dataIndex: "allow_upstream",
      render: (allow: boolean) => (allow ? <Tag>{t("pull.rules.upstreamAllowed")}</Tag> : <Tag color="orange">{t("pull.rules.upstreamOff")}</Tag>),
    },
    {
      title: t("pull.rules.nodes"),
      key: "nodes",
      render: (_: unknown, rule) => {
        const chips = requirementChips(rule.node_requirements);
        return (
          <Space direction="vertical" size={4}>
            <Space size={[4, 4]} wrap>
              {chips.length === 0 ? <Tag>{t("pull.rules.allNodes")}</Tag> : chips.map((chip) => <Tag key={chip}>{chip}</Tag>)}
            </Space>
            <Typography.Text type="secondary">{t("pull.rules.matchedNodes", { count: rule.matched_nodes })}</Typography.Text>
          </Space>
        );
      },
    },
    {
      title: t("pull.rules.credentials"),
      key: "credentials",
      render: (_: unknown, rule) => {
        const hosts = [...new Set(rule.endpoints.map(hostOf).filter(Boolean))];
        if (!loginHosts || hosts.length === 0) return <Typography.Text type="secondary">—</Typography.Text>;
        return (
          <Space direction="vertical" size={2}>
            {hosts.map((host) => (
              <span key={host}>
                <Typography.Text type="secondary">{host}: </Typography.Text>
                {loginHosts.has(host) ? <Tag color="blue">{t("pull.rules.credentialsSet")}</Tag> : <Tag>{t("pull.rules.credentialsNone")}</Tag>}
              </span>
            ))}
          </Space>
        );
      },
    },
    {
      title: t("pull.rules.actions"),
      key: "actions",
      fixed: "right",
      width: 140,
      render: (_: unknown, rule) => (
        <Space size={4}>
          <Button size="small" type="link" onClick={() => setEditing(rule)}>{t("pull.action.edit")}</Button>
          <Popconfirm
            title={t("pull.rules.deleteConfirm")}
            description={t("pull.rules.deleteConfirmBody")}
            okText={t("pull.action.delete")}
            cancelText={t("pull.action.cancel")}
            okButtonProps={{ danger: true }}
            onConfirm={() => remove(rule)}
          >
            <Button size="small" type="link" danger aria-label={t("pull.rules.deleteRule", { match: rule.match })}>{t("pull.action.delete")}</Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  return (
    <Space direction="vertical" size={12} style={{ width: "100%" }}>
      <Typography.Paragraph type="secondary" style={{ marginBottom: 0 }}>{t("pull.rules.intro")} <HelpTip content={t("pull.rules.introHelp")} /></Typography.Paragraph>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
        <Typography.Text type="secondary">{t("pull.rules.orderHint")}</Typography.Text>
        <Button type="primary" icon={<PlusOutlined aria-hidden />} onClick={() => setEditing("new")}>{t("pull.rules.new")}</Button>
      </div>
      {rulesQuery.error && (
        <Alert
          type="error"
          showIcon
          message={t("pull.loadFailed", { detail: rulesQuery.error.message })}
          action={<Button size="small" onClick={() => void rulesQuery.refetch()}>{t("pull.action.retry")}</Button>}
        />
      )}
      {actionError && <Alert type="error" showIcon role="alert" message={actionError} closable onClose={() => setActionError("")} />}
      <div role="region" aria-label={t("pull.rules.tableLabel")}>
      <Table<PullSourceRule>
        rowKey="id"
        size="small"
        pagination={false}
        loading={rulesQuery.loading && !rulesQuery.data}
        columns={columns}
        dataSource={rules}
        scroll={{ x: "max-content" }}
        locale={{ emptyText: rulesQuery.error ? " " : t("pull.rules.empty") }}
      />
      </div>
      {editing && (
        <PullRuleDrawer
          rule={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            message.success(t("pull.rules.saved"));
            void rulesQuery.refetch();
          }}
        />
      )}
    </Space>
  );
}
