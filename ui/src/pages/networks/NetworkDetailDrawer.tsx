import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Descriptions, Popconfirm, Space, Spin, Table, Tag, Typography, message } from "antd";
import type { ColumnsType } from "antd/es/table";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { api } from "../../api";
import type { NetworkAttachment, NetworkDetail, NetworkNode } from "../../networkTypes";
import DetailDrawer, { DetailExtra, detailExpandable } from "../../components/detail/DetailDrawer";
import DetailSection from "../../components/detail/DetailSection";
import { formatTime } from "../../i18n/datetime";
import AttachNetworkModal from "./AttachNetworkModal";
import NetworkFormModal from "./NetworkFormModal";
import NetworkTestPanel from "./NetworkTestPanel";
import NetworkMonitorPanel from "./NetworkMonitorPanel";
import ManagementNodesTable from "./ManagementNodesTable";
import { Endpoint, RoleTags, TypeTag, WarningMark, isAcceleratorRDMA, usesRails, sourceText } from "./networkDisplay";
import { BootstrapCell, RailsCell } from "./RDMAEndpoints";
import { HelpTip } from "../../ui-kit/HelpTip";

const tabKeys = ["overview", "monitor", "test"] as const;

type Props = {
  // The network whose detail is open, if any (the list page's ?network=).
  id: number | null;
  tab?: string;
  onTab: (key: string) => void;
  onClose: () => void;
  // Something the list shows changed (renamed, deleted).
  onChanged: () => void;
};

// antd's drawer slides out in 0.3 s (motionDurationSlow).
const SLIDE_OUT_MS = 400;

/** One network in a drawer over the list: where it applies and which nodes it resolved to, its test, its traffic. */
export default function NetworkDetailDrawer(props: Props) {
  // Reading lives with a shown drawer only. A closing drawer keeps the last
  // network while it slides out, then unmounts and reads nothing.
  const [shown, setShown] = useState<number | null>(props.id);
  if (props.id != null && props.id !== shown) setShown(props.id);
  useEffect(() => {
    if (props.id != null) return;
    const timer = setTimeout(() => setShown(null), SLIDE_OUT_MS);
    return () => clearTimeout(timer);
  }, [props.id]);
  const id = props.id ?? shown;
  if (id == null) return <DetailDrawer open={false} onClose={props.onClose} title="" />;
  return <OpenNetworkDetail {...props} id={id} open={props.id != null} />;
}

function OpenNetworkDetail({ id, open, tab: requested, onTab, onClose, onChanged }: Omit<Props, "id"> & { id: number; open: boolean }) {
  const { t } = useTranslation("networks");
  const [detail, setDetail] = useState<NetworkDetail | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [attachOpen, setAttachOpen] = useState(false);
  const [editingAttachment, setEditingAttachment] = useState<NetworkAttachment | null>(null);
  const [formOpen, setFormOpen] = useState(false);
  const tab = tabKeys.find((key) => key === requested) ?? "overview";

  const load = useCallback(() => {
    api.network(id).then((d) => { setDetail(d); setError(null); }).catch((e: Error) => setError(e));
  }, [id]);
  useEffect(load, [load]);
  // A running test finishes on the server; poll until its result arrives.
  const testRunning = detail?.latest_test?.status === "running";
  useEffect(() => {
    if (!testRunning) return;
    const timer = setInterval(load, 2000);
    return () => clearInterval(timer);
  }, [testRunning, load]);

  const current = detail?.id === id ? detail : null;
  if (!current) {
    return (
      <DetailDrawer open={open} onClose={onClose} title={t("detail.title")}>
        {error ? <Alert type="error" showIcon message={error.message} action={<Button size="small" onClick={load}>{t("detail.retry")}</Button>} /> : <Spin />}
      </DetailDrawer>
    );
  }
  const name = current.builtin ? t("management.name") : current.name;
  // A change made in the drawer can change what the list shows (a network's
  // attachments, its resolved nodes): the list reads again with it.
  const updated = (next: NetworkDetail) => { setDetail(next); onChanged(); };
  const testTab = { key: "test", label: t("tabs.test"), children: <NetworkTestPanel network={current} onStarted={load} onChanged={current.builtin ? () => load() : setDetail} /> };
  const monitorTab = { key: "monitor", label: t("tabs.monitor"), children: <NetworkMonitorPanel network={current} /> };

  const remove = async () => {
    try {
      await api.deleteNetwork(current.id);
      message.success(t("message.deleted"));
      onChanged();
      onClose();
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    }
  };

  return (
    <DetailDrawer
      open={open}
      onClose={onClose}
      title={name}
      subtitle={<Space size={4} wrap><RoleTags roles={current.roles} /><TypeTag type={current.type} /></Space>}
      // The management network cannot be edited or deleted; what changes is
      // which roles it carries on each node (its node table).
      extra={
        current.builtin ? undefined : (
          <Space>
            <Button size="small" onClick={() => setFormOpen(true)}>{t("actions.edit")}</Button>
            <Popconfirm title={t("actions.confirmDelete", { name: current.name })} onConfirm={remove} okText={t("actions.delete")} cancelText={t("actions.cancel")}>
              <Button size="small" danger>{t("actions.delete")}</Button>
            </Popconfirm>
          </Space>
        )
      }
      activeTab={tab}
      onTabChange={onTab}
      tabs={[
        {
          key: "overview",
          label: t("tabs.overview"),
          children: current.builtin ? (
            <>
              {/* What the network is, point by point; what its switches do is said beside them (ManagementNodesTable). */}
              <Alert
                type="info"
                showIcon
                style={{ marginBottom: 16 }}
                message={t("management.intro.title")}
                description={<ul className="detail-points">{(["source", "fixed", "roles"] as const).map((key) => <li key={key}>{t(`management.intro.${key}`)}</li>)}</ul>}
              />
              <DetailSection title={t("detail.resolved", { count: current.nodes.length })}>
                <ManagementNodesTable nodes={current.nodes} onChanged={load} />
              </DetailSection>
            </>
          ) : (
            <NetworkOverview
              detail={current}
              description={current.description}
              onDetail={updated}
              onAttach={(attachment) => { setEditingAttachment(attachment); setAttachOpen(true); }}
            />
          ),
        },
        monitorTab,
        testTab,
      ]}
    >
      {!current.builtin && (
        <>
          <AttachNetworkModal
            open={attachOpen}
            network={current}
            attachment={editingAttachment}
            onClose={() => setAttachOpen(false)}
            onSaved={(d) => { setAttachOpen(false); updated(d); }}
          />
          <NetworkFormModal
            open={formOpen}
            network={current}
            roleOptions={current.role_options}
            typeOptions={current.type_options}
            typeRoles={current.type_roles}
            onClose={() => setFormOpen(false)}
            onSaved={(d) => { setFormOpen(false); updated(d); }}
          />
        </>
      )}
    </DetailDrawer>
  );
}

/** A defined network's overview: how it picks addresses, what it is attached to, and the nodes it resolved to. */
function NetworkOverview({ detail, description, onDetail, onAttach }: {
  detail: NetworkDetail;
  description?: string;
  onDetail: (detail: NetworkDetail) => void;
  onAttach: (attachment: NetworkAttachment | null) => void;
}) {
  const { t } = useTranslation("networks");
  const rails = usesRails(detail.type);
  const detach = async (attachment: NetworkAttachment) => {
    try {
      onDetail(await api.detachNetwork(detail.id, attachment.id));
    } catch (e) {
      message.error(t("message.failed", { detail: (e as Error).message }));
    }
  };
  const overrideText = (a: NetworkAttachment) => [a.interface, a.ip, a.hcas.join(",")].filter(Boolean).join(" · ");

  // The row keeps the node, the roles it carries and the address it uses;
  // where that came from, and an RDMA network's bootstrap and HCAs (still
  // editable), sit one step down so the row fits the 720px detail drawer.
  const nodeColumns: ColumnsType<NetworkNode> = [
    {
      title: t("nodeTable.node"),
      dataIndex: "node_name",
      render: (_, n) => <span><Link to="/nodes" state={{ nodeUUID: n.node_uuid }} className="detail-name-cap" title={n.node_name} onClick={(event) => event.stopPropagation()}>{n.node_name}</Link><WarningMark warnings={n.warnings} /></span>,
    },
    { title: t("nodeTable.roles"), dataIndex: "roles", render: (_, n) => <RoleTags roles={n.roles} /> },
    rails
      ? { title: t("nodeTable.source"), dataIndex: "source", render: (_, n) => sourceText(t, n) }
      : { title: t("nodeTable.endpoint"), key: "endpoint", render: (_, n) => <Endpoint iface={n.interface} ip={n.ip} /> },
  ];
  const extraColumns = rails
    ? [
        { key: "bootstrap", title: t("nodeTable.bootstrap"), wide: true, render: (_: unknown, n: NetworkNode) => <BootstrapCell network={detail} node={n} onChanged={onDetail} /> },
        {
          key: "hcas",
          wide: true,
          title: <span>{t("nodeTable.hcaIPs")}{detail.nodes[0]?.link_layer && <Tag style={{ marginLeft: 6 }}>{t(`linkLayer.${detail.nodes[0].link_layer}`)}</Tag>}</span>,
          render: (_: unknown, n: NetworkNode) => <RailsCell network={detail} node={n} onChanged={onDetail} />,
        },
      ]
    : [{ key: "source", title: t("nodeTable.source"), render: (_: unknown, n: NetworkNode) => sourceText(t, n) }];

  return (
    <>
      {description && <Typography.Paragraph type="secondary">{description}</Typography.Paragraph>}
      <DetailSection title={t("detail.selector")}>
        {rails ? (
          <>
            <Descriptions bordered size="small" column={1}>
              <Descriptions.Item label={t("selector.bootstrapCidr")}>{detail.cidr || t("selector.inferredShort")}</Descriptions.Item>
            </Descriptions>
            <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: "8px 0 0" }} className="flex items-center gap-1">
              {t(isAcceleratorRDMA(detail.type) ? "detail.acceleratorEditHint" : "detail.rdmaEditHint")}
              <HelpTip content={t(isAcceleratorRDMA(detail.type) ? "detail.acceleratorInferredHelp" : "detail.rdmaInferredHelp")} />
            </Typography.Paragraph>
          </>
        ) : (
          <Descriptions bordered size="small" column={1}>
            <Descriptions.Item label={t("selector.cidr")}>{detail.cidr || "—"}</Descriptions.Item>
            <Descriptions.Item label={t("selector.interface")}>{detail.interface || "—"}</Descriptions.Item>
          </Descriptions>
        )}
      </DetailSection>
      <DetailSection title={t("detail.attachments")} extra={<Button size="small" onClick={() => onAttach(null)}>{t("actions.attach")}</Button>}>
        {detail.attachments.length === 0 ? (
          <Typography.Text type="secondary">{t("detail.noAttachments")}</Typography.Text>
        ) : (
          <Table<NetworkAttachment>
            rowKey="id"
            size="small"
            pagination={false}
            showHeader={false}
            dataSource={detail.attachments}
            scroll={{ x: "max-content" }}
            columns={[
              {
                key: "target",
                render: (_, a) => (
                  <Space direction="vertical" size={0}>
                    <span>
                      <Tag>{t(`source.${a.target}`)}</Tag>
                      {a.target === "node" ? (a.node_name || a.node_uuid) : <Typography.Text code>{`${a.label_key}=${a.label_value ?? ""}`}</Typography.Text>}
                    </span>
                    {overrideText(a) && <Typography.Text type="secondary">{t("detail.overrides", { value: overrideText(a) })}</Typography.Text>}
                  </Space>
                ),
              },
              { key: "time", render: (_, a) => <Typography.Text type="secondary">{t("detail.attachedAt", { time: formatTime(a.created_at) })}</Typography.Text> },
              {
                key: "actions",
                align: "right",
                render: (_, a) => (
                  <Space size={0}>
                    {a.target === "node" && <Button type="link" size="small" onClick={() => onAttach(a)}>{t("actions.edit")}</Button>}
                    <Popconfirm title={t("actions.confirmDetach")} onConfirm={() => detach(a)} okText={t("actions.detach")} cancelText={t("actions.cancel")}>
                      <Button type="link" size="small" danger>{t("actions.detach")}</Button>
                    </Popconfirm>
                  </Space>
                ),
              },
            ]}
          />
        )}
      </DetailSection>
      <DetailSection title={t("detail.resolved", { count: detail.nodes.length })}>
        <Table<NetworkNode>
          className="detail-expandable"
          rowKey="node_uuid"
          size="small"
          pagination={{ defaultPageSize: 20, hideOnSinglePage: true, showSizeChanger: true, pageSizeOptions: [10, 20, 50, 100] }}
          columns={nodeColumns}
          dataSource={detail.nodes}
          locale={{ emptyText: t("detail.noNodes") }}
          scroll={{ x: "max-content" }}
          expandable={detailExpandable<NetworkNode>((n) => <DetailExtra columns={extraColumns} record={n} />, { expand: t("detail.expand"), collapse: t("detail.collapse") })}
        />
      </DetailSection>
    </>
  );
}
