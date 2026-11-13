import { PeerTransferDetails } from "../../components/peerTransfer/PeerTransferDetails";
import { productMessageText } from "../../apiError";
import { Button, Flex, Popconfirm, Progress, Space, Tag, Tooltip, Typography, message } from "antd";
import { CheckCircleFilled, CloudDownloadOutlined, ReloadOutlined, StopOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { api, type Model, type ModelNodePresence } from "../../api";
import ResourceTable, { type ResourceColumn } from "../../components/resourceTable/ResourceTable";
import { detailExpandable } from "../../components/detail/DetailDrawer";
import { formatTime } from "../../i18n/datetime";
import { useIsAdmin } from "../../components/useIsAdmin";
import { nodeRowName } from "../../nodeDisplay";
import { storageErrorText, storageTypeLabel } from "../../storageDisplay";
import { formatBytes } from "../../format";
import { storageTypeInfo, type ModelStoragePresence } from "../../storageTypes";
import { ModelNodeFileDelete } from "./ModelNodeFileDelete";
import { ModelStorageFileDelete } from "./ModelStorageFileDelete";
import { acquisitionLabel, modelPresenceBytes, presenceVisual, rateLabel, transferOrigin } from "./modelPresenceDisplay";

type ModelFile = ModelNodePresence | ModelStoragePresence;

function StateTag({ state }: { state: string }) {
  const { t } = useTranslation("models");
  const visual = presenceVisual[state];
  const label = visual ? t(visual.labelKey) : state;
  return <Tooltip title={label} trigger={["hover", "focus"]}>
    <Tag className="model-file-state-tag" color={visual?.color} tabIndex={0}>{label}</Tag>
  </Tooltip>;
}

function SourceText({ row }: { row: Parameters<typeof transferOrigin>[0] }) {
  const { t } = useTranslation("models");
  const value = row.acquisition;
  const label = value ? (acquisitionLabel[value] ? t(acquisitionLabel[value]) : value) : "—";
  const origin = transferOrigin(row);
  if (!origin) return <>{label}</>;
  if ("node" in origin) return <>{label} · {t("files.fromNode", { node: origin.node })}</>;
  const host = t("files.fromHost", { host: origin.host });
  // After a source failed the agent says why it moved on; the reason is long,
  // so it stays behind the host.
  return <>{label} · {origin.switched ? <Tooltip title={t("files.switchedSource", { reason: origin.switched })}><span style={{ textDecoration: "underline dotted" }}>{host}</span></Tooltip> : host}</>;
}

const FILE_PAGE_SIZE = 10;

/** A transfer's progress, bytes and, while it moves, its rate. */
function TransferProgress({ row }: { row: ModelFile }) {
  const { t } = useTranslation("models");
  const moving = row.state === "downloading" || row.state === "syncing";
  const complete = row.state === "present" && ["download", "auto", "peer", "copy"].includes(row.acquisition);
  if (!("node_uuid" in row) && !moving && !complete) return <>—</>;
  const rate = moving ? rateLabel(row.rate_bps) : "";
  const details = [modelPresenceBytes(row), rate].filter(Boolean).join(" · ");
  return <Space direction="vertical" size={0} className="model-file-progress">
    <Progress percent={complete ? 100 : Math.round((row.progress || 0) * 100)} size="small" status={moving ? "normal" : undefined}
      format={complete ? () => <Space size={4}><CheckCircleFilled />{t("files.completed")}</Space> : undefined} />
    <Typography.Text type="secondary" title={details}>{details}</Typography.Text>
  </Space>;
}

function CancelDownload({ modelId, storageId, nodeUuid, onChanged }: { modelId: number; storageId?: number; nodeUuid?: string; onChanged: () => void }) {
  const { t } = useTranslation("models");
  const cancel = async () => {
    try {
      await api.cancelModelDownload(modelId, { storage_id: storageId, node_uuid: nodeUuid });
      message.success(t("presenceMessage.downloadCanceled"));
      onChanged();
    } catch (e) {
      message.error(storageErrorText(t, e));
    }
  };
  const label = t("presenceAction.cancelDownload");
  return <Popconfirm title={t("presenceAction.cancelDownloadConfirm")} onConfirm={cancel} okText={label} cancelText={t("action.cancel")}>
    <Tooltip title={label} trigger={["hover", "focus"]}>
      <Button type="text" size="small" danger icon={<StopOutlined />} aria-label={label} />
    </Tooltip>
  </Popconfirm>;
}

function NodeFileActions({ model, row, isAdmin, onChanged }: { model: Model; row: ModelNodePresence; isAdmin: boolean; onChanged: () => void }) {
  const { t } = useTranslation("models");
  const run = async (action: () => Promise<unknown>, success: string) => {
    try {
      await action();
      message.success(success);
      onChanged();
    } catch (e) {
      message.error(storageErrorText(t, e));
    }
  };
  // The legacy sync endpoint resolves the target by node; only offer a new
  // disk retry when that resolution cannot select a different local copy.
  const soleLocalCopy = (model.nodes ?? []).filter((copy) => copy.node_uuid === row.node_uuid && !copy.cache && (!copy.storage_type || copy.storage_type === "local")).length === 1;
  const diskRetry = row.state === "insufficient_disk_space" && !row.cache &&
    (row.acquisition === "download" || (row.acquisition === "peer" && (!row.storage_type || row.storage_type === "local") && Boolean(row.source_node_uuid?.trim()) && soleLocalCopy));
  const retry = () => {
    if (row.acquisition === "download" && !row.cache) {
      return run(() => row.storage_id !== undefined
        ? api.downloadModelToStorage(model.id, { storage_id: row.storage_id, node_uuids: [row.node_uuid] })
        : api.downloadModel(model.id, row.node_uuid), t("presenceMessage.downloadStarted"));
    }
    // Disk-blocked transfers retain their current limit; its omitted zero
    // means unlimited, not permission to restore an older archived limit.
    const rateLimit = row.state === "insufficient_disk_space" ? row.rate_limit_kib_s ?? 0 : row.last_rate_limit_kib_s ?? 0;
    return run(() => api.syncModel(model.id, row.node_uuid, row.source_node_uuid, rateLimit), t("presenceMessage.syncStarted"));
  };
  return <Flex wrap>
    {row.state === "download_required" && <Tooltip title={t("action.internetDownload")} trigger={["hover", "focus"]}>
      <Button type="text" size="small" icon={<CloudDownloadOutlined />} aria-label={t("action.internetDownload")} onClick={() => run(() => api.downloadModel(model.id, row.node_uuid), t("presenceMessage.downloadStarted"))} />
    </Tooltip>}
    {(row.state === "failed" || row.state === "download_required" || row.state === "canceled" || diskRetry) && <Tooltip title={t("action.retry")} trigger={["hover", "focus"]}>
      <Button type="text" size="small" icon={<ReloadOutlined />} aria-label={t("action.retry")} onClick={retry} />
    </Tooltip>}
    {row.acquisition === "download" && (row.state === "pending" || row.state === "downloading") && <CancelDownload modelId={model.id} storageId={row.storage_id} nodeUuid={row.node_uuid} onChanged={onChanged} />}
    {row.state === "syncing" && <Popconfirm title={t("presenceAction.cancelConfirm")} onConfirm={() => run(() => api.cancelModelSync(model.id, row.node_uuid), t("presenceMessage.syncCanceled"))} okText={t("presenceAction.cancelSync")} cancelText={t("presenceAction.continueSync")}>
      <Tooltip title={t("presenceAction.cancelSync")} trigger={["hover", "focus"]}>
        <Button type="text" size="small" danger icon={<StopOutlined />} aria-label={t("presenceAction.cancelSync")} />
      </Tooltip>
    </Popconfirm>}
    {isAdmin && <ModelNodeFileDelete
      modelId={model.id}
      nodeUuid={row.node_uuid}
      nodeName={row.node_name}
      storageId={row.storage_id}
      storageName={row.storage_name}
      cache={row.cache}
      writable={row.deletable === true}
      onDone={onChanged}
    />}
  </Flex>;
}

function FileDetails({ row }: { row: ModelFile }) {
  const { t } = useTranslation("models");
  const { t: common } = useTranslation("common");
  const node = "node_uuid" in row;
  const tagTextColors: Record<string, string> = { red: "var(--fg-danger)", orange: "var(--fg-warning)" };
  const codedMessage = "transfer_error_code" in row && row.transfer_error_code
    ? productMessageText(common, { code: row.transfer_error_code, message: row.message ?? "" }) : undefined;
  const statusNote = codedMessage || (row.state === "missing" && !node ? t("files.missingHint")
    : ["failed", "download_required", "canceled", "insufficient_disk_space"].includes(row.state) ? row.message : undefined);
  return <><dl className="detail-extra">
    <div style={{ gridColumn: node ? "1 / -1" : "1 / -2" }}>
      <dt>{t(node ? "files.path" : "files.relativePath")}</dt>
      <dd>{row.path ? <Typography.Text code>{row.path}</Typography.Text> : "—"}</dd>
    </div>
    <div><dt>{t("column.source")}</dt><dd><SourceText row={row} /></dd></div>
    {node ? (
      <>
        <div><dt>{t("detail.nodePinnedRevision")}</dt><dd>{row.revision ? <Typography.Text code>{row.revision}</Typography.Text> : t("detail.noResolvedRevision")}</dd></div>
        <div><dt>{t("files.lastUsed")}</dt><dd>{row.last_used_at ? formatTime(row.last_used_at) : t("files.neverUsed")}</dd></div>
      </>
    ) : (
      <>
        <div><dt>{t("files.readyNodes")}</dt><dd>{row.ready_nodes}</dd></div>
        {storageTypeInfo(row.storage_type).cache && <div><dt>{t("files.cachedNodes")}</dt><dd>{row.cached_nodes}</dd></div>}
        {row.writer_node && <div><dt>{t("files.writerNodeLabel")}</dt><dd>{row.writer_node}</dd></div>}
      </>
    )}
    {statusNote && <div style={{ gridColumn: "1 / -1" }}>
      <dt>{t("files.statusNote")}</dt>
      <dd style={{ color: tagTextColors[presenceVisual[row.state]?.color] ?? "var(--fg-muted)" }}>{statusNote}</dd>
    </div>}
  </dl>{node && row.transfer_id && <PeerTransferDetails transferId={row.transfer_id} transferErrorCode={row.transfer_error_code} />}</>;
}

function FileLocation({ row }: { row: ModelFile }) {
  const { t } = useTranslation("models");
  const node = "node_uuid" in row;
  const name = node ? nodeRowName(row) : row.storage_name;
  const storageType = row.storage_type && storageTypeLabel(t, row.storage_type);
  const description = [node ? row.storage_name : t("files.sharedTitle"), storageType].filter(Boolean).join(" · ");
  return <>
    <Flex align="center" gap={6}>
      <Tooltip title={node && row.node_uuid !== name ? `${name} · ${row.node_uuid}` : name}>
        <Typography.Text strong={!node || !!row.node_name} code={node && !row.node_name} ellipsis>{name}</Typography.Text>
      </Tooltip>
      {node && row.cache && <Tag style={{ flex: "none", marginInlineEnd: 0 }}>{t("files.cache")}</Tag>}
    </Flex>
    {description && <span className="resource-table-sub" title={description}>{description}</span>}
  </>;
}

function fileKey(row: ModelFile) {
  return "node_uuid" in row
    ? `node:${row.node_uuid}:${row.storage_id ?? 0}:${row.cache ? "cache" : "local"}`
    : `shared:${row.storage_id}`;
}

export function ModelFilesSection({ model, onChanged }: { model: Model; onChanged: () => void }) {
  const { t } = useTranslation("models");
  const { t: tStorage } = useTranslation("storage");
  const isAdmin = useIsAdmin();
  const nodes = model.nodes ?? [];
  const rows: ModelFile[] = [
    ...nodes.filter((row) => !row.cache),
    ...(model.storages ?? []),
    ...nodes.filter((row) => row.cache),
  ];
  const columns: ResourceColumn<ModelFile>[] = [
    { key: "location", title: tStorage("files.column.place"), width: 132, render: (_, row) => <FileLocation row={row} /> },
    { key: "state", title: t("detail.status"), width: 88, ellipsis: false, render: (_, row) => <StateTag state={row.state} /> },
    { key: "progress", title: t("detail.progress"), width: 218, minWidth: 218, ellipsis: false, render: (_, row) => <TransferProgress row={row} /> },
    { key: "size", title: t("detail.size"), width: 78, align: "right", render: (_, row) => row.size_bytes ? formatBytes(row.size_bytes) : "—" },
    { key: "actions", role: "actions", title: t("column.actions"), width: 100, render: (_, row) => (
      <div onClick={(event) => event.stopPropagation()}>
        {"node_uuid" in row ? <NodeFileActions model={model} row={row} isAdmin={isAdmin} onChanged={onChanged} /> : <Space size={4} wrap>
          {row.acquisition === "download" && (row.state === "pending" || row.state === "downloading") &&
            <CancelDownload modelId={model.id} storageId={row.storage_id} onChanged={onChanged} />}
          {isAdmin && <ModelStorageFileDelete modelId={model.id} storageId={row.storage_id} storageName={row.storage_name} storageType={row.storage_type} writable={row.writable} cachedNodes={row.cached_nodes} onDone={onChanged} />}
        </Space>}
      </div>
    ) },
  ];

  return <div id="model-files" style={{ marginTop: 16, scrollMarginTop: 16 }}>
    <Typography.Text strong>{t("files.title")}</Typography.Text>
    <ResourceTable<ModelFile>
      key={model.id}
      tableId="model-file-locations"
      rowKey={fileKey}
      columns={columns}
      data={rows}
      columnSettings={false}
      pagination={rows.length > FILE_PAGE_SIZE ? { pageSize: FILE_PAGE_SIZE, size: "small", showSizeChanger: false } : false}
      emptyDescription={t("files.empty")}
      expandable={detailExpandable((row) => <FileDetails row={row} />, {
        expand: tStorage("detail.expand"), collapse: tStorage("detail.collapse"),
      })}
    />
  </div>;
}
