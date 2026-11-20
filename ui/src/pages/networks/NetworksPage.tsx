import { useState } from "react";
import { Alert, Button, Card, Popconfirm, Space, Tag, Tooltip, Typography, message } from "antd";
import { DeleteOutlined, EditOutlined, ProfileOutlined } from "@ant-design/icons";
import { Link } from "react-router-dom";
import { numericDetailID, useDetailParam } from "../../components/detail/useDetailParam";
import NetworkDetailDrawer from "./NetworkDetailDrawer";
import { networkDetailPath } from "./networkLinks";
import { useTranslation } from "react-i18next";
import { api, useEventPayload, useEvents, usePolling } from "../../api";
import type { Network, NetworkList } from "../../networkTypes";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import PageHeader from "../../components/PageHeader";
import NetworkFormModal from "./NetworkFormModal";
import { Button as GuideButton } from "../../components/ui/button";
import { useIcon } from "../../lib/icon-context";
import { RoleTags, SelectorSummary, TypeTag, WarningMark, networkName } from "./networkDisplay";

export function AttachmentTags({ network }: { network: Network }) {
  const { t } = useTranslation("networks");
  if (network.builtin) return <Tag>{t("management.attachedTo")}</Tag>;
  if (network.attachments.length === 0) return <span>—</span>;
  if (network.attachments.length > 5) {
    const nodes = network.attachments.filter(a => a.target === "node").length;
    const labels = network.attachments.length - nodes;
    const summary = [
      nodes > 0 && t("table.attachmentNodes", { count: nodes }),
      labels > 0 && t("table.attachmentLabels", { count: labels }),
    ].filter(Boolean).join(" · ");
    return <Link to={networkDetailPath(network.id)}>{summary}</Link>;
  }
  return (
    <Space size={4} wrap>
      {network.attachments.map((a) => (
        <Tag key={a.id}>
          {a.target === "node" ? (a.node_name || a.node_uuid) : `${a.label_key}=${a.label_value ?? ""}`}
          {a.target === "label" && <span style={{ marginLeft: 4, opacity: 0.6 }}>{t("source.label")}</span>}
        </Tag>
      ))}
    </Space>
  );
}

// The node event reason of an agent's routine status report
// (store.NodeChangeStatus on the server).
const NODE_STATUS_REPORT = "status";

export default function NetworksPage() {
  const { t } = useTranslation("networks");
  const GuideIcon = useIcon("doc-introduction");
  const detail = useDetailParam("network");
  // The list changes when a network or a node it resolves against does, so
  // it reads again on those events instead of on a timer. A node's routine
  // status report, which every agent sends once a minute, is left out: the
  // list would otherwise be read again every few seconds on a large fleet.
  const { data, refetch, refresh, loading, error, updatedAt } = usePolling<NetworkList>(api.networks, 0);
  useEvents("network", refresh);
  useEventPayload("node", (event) => {
    if (event.reason !== NODE_STATUS_REPORT) void refresh();
  });
  const [editing, setEditing] = useState<Network | null>(null);
  const [formOpen, setFormOpen] = useState(false);

  const remove = async (network: Network) => {
    try {
      await api.deleteNetwork(network.id);
      message.success(t("message.deleted"));
      refetch();
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    }
  };

  const columns: ResourceColumn<Network>[] = [
    { key: "name", role: "name", title: t("table.name"), width: 220, sortValue: (n) => networkName(t, n), render: (_, n) => <Link to={networkDetailPath(n.id)}>{networkName(t, n)}</Link> },
    { key: "roles", title: t("table.roles"), width: 200, render: (_, n) => <RoleTags roles={n.roles} /> },
    {
      key: "type",
      title: t("table.type"),
      width: 130,
      sortValue: (n) => n.type,
      filters: [...new Set((data?.items ?? []).map((n) => n.type))].map((type) => ({ value: type, text: type })),
      onFilter: (value, n) => n.type === value,
      render: (_, n) => <TypeTag type={n.type} />,
    },
    { key: "selector", title: t("table.selector"), width: 220, render: (_, n) => n.builtin ? <Typography.Text type="secondary">{t("management.selector")}</Typography.Text> : <SelectorSummary network={n} /> },
    { key: "attachments", title: t("table.attachedTo"), width: 220, render: (_, n) => <AttachmentTags network={n} /> },
    {
      key: "nodes",
      title: t("table.nodes"),
      width: 110,
      align: "right",
      sortValue: (n) => n.resolved_node_count,
      render: (_, n) => (
        <span>
          {n.resolved_node_count}
          {n.warning_node_count > 0 && <WarningMark summary={t("warning.summary", { count: n.warning_node_count })} />}
        </span>
      ),
    },
    {
      key: "actions",
      role: "actions",
      title: t("table.actions"),
      width: 110,
      render: (_, n) => (
        <Space size={2}>
          <Tooltip title={t("actions.detail")}><Button type="text" size="small" icon={<ProfileOutlined />} aria-label={t("actions.detail")} onClick={() => detail.open(n.id)} /></Tooltip>
          {/* The management network is built in: it is neither edited nor deleted. */}
          {!n.builtin && (
            <>
              <Tooltip title={t("actions.edit")}><Button type="text" size="small" icon={<EditOutlined />} aria-label={t("actions.edit")} onClick={() => { setEditing(n); setFormOpen(true); }} /></Tooltip>
              <Popconfirm
                title={t("actions.confirmDelete", { name: n.name })}
                onConfirm={() => remove(n)}
                okText={t("actions.delete")}
                cancelText={t("actions.cancel")}
                okButtonProps={{ danger: true }}
              >
                <Tooltip title={t("actions.delete")}><Button type="text" size="small" danger icon={<DeleteOutlined />} aria-label={t("actions.delete")} /></Tooltip>
              </Popconfirm>
            </>
          )}
        </Space>
      ),
    },
  ];

  return (
    <>
      <PageHeader
        title={t("title")}
        description={t("description")}
        extra={
          <>
            <GuideButton asChild variant="secondary" leadingIcon={GuideIcon}>
              <Link to="/networks/guide">{t("topology.guide")}</Link>
            </GuideButton>
            <Button type="primary" onClick={() => { setEditing(null); setFormOpen(true); }}>{t("actions.create")}</Button>
          </>
        }
      />
      <Card>
        <ResourceTable<Network>
          tableId="networks"
          rowKey="id"
          columns={columns}
          data={data?.items ?? []}
          loading={loading}
          error={error}
          onRefresh={refetch}
          updatedAt={updatedAt}
          emptyDescription={t("empty")}
        />
        <Alert style={{ marginTop: 16 }} type="info" showIcon message={t("fallbackNote")} />
      </Card>
      <NetworkFormModal
        open={formOpen}
        network={editing}
        roleOptions={data?.roles ?? []}
        typeOptions={data?.types ?? []}
        typeRoles={data?.type_roles}
        onClose={() => setFormOpen(false)}
        onSaved={(saved) => {
          setFormOpen(false);
          refetch();
          if (!editing) detail.open(saved.id);
        }}
      />
      <NetworkDetailDrawer
        id={numericDetailID(detail.id)}
        tab={detail.tab}
        onTab={detail.setTab}
        onClose={() => detail.open(null)}
        onChanged={refetch}
      />
    </>
  );
}
