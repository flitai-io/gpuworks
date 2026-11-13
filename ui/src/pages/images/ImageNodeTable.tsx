import { PeerTransferDetails } from "../../components/peerTransfer/PeerTransferDetails";
import { productMessageText } from "../../apiError";
import { Button, Popconfirm, Progress, Select, Space, Table, Tag, Tooltip, Typography } from "antd";
import { useState } from "react";
import type { ImageCatalogEntry, ImageNode } from "../../api";
import { useTranslation } from "react-i18next";
import { agoText } from "../../format";
import { formatTime } from "../../i18n/datetime";
import {
  archiveURLDialogValue,
  bytes,
  imageSourceOptions,
  isArchiveURL,
  peerSources,
  registryName,
  shortDigest,
  stateTag,
} from "./ImagePageHelpers";
import { detailExpandable } from "../../components/detail/DetailDrawer";
import ImageArchiveURLDialog from "./ImageArchiveURLDialog";
import ImagePeerSyncDialog from "./ImagePeerSyncDialog";
import ImageArchivePathDialog from "./ImageArchivePathDialog";
import { Badge } from "../../components/ui/badge";

const { Text } = Typography;
type FetchMode = "peer" | string;

type Props = {
  /** A version read from the series detail: only it carries node rows. */
  selectedVersion: ImageCatalogEntry;
  /** Set while the series detail is still being read. */
  loading?: boolean;
  inspecting: string | null;
  availablePeer: (version: ImageCatalogEntry, target: ImageNode) => boolean;
  modeFor: (version: ImageCatalogEntry, target: ImageNode) => FetchMode;
  setMode: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    mode: FetchMode,
  ) => void;
  pullVersion: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    retry?: boolean,
  ) => void;
  /** Verifies a direct or relayed peer connection; rejects with the reason. */
  probeImage: (version: ImageCatalogEntry, nodeUUID: string, sourceNode: string) => Promise<unknown>;
  /**
   * Starts (or retries) a peer copy from sourceNode. Rejecting leaves the
   * dialog open with the reason on screen.
   */
  syncFromPeer: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    sourceNode: string,
    rateLimitKiB: number,
  ) => Promise<void>;
  /**
   * Records an archive address on the image and asks this node to fetch it.
   * Rejecting leaves the dialog open with the reason on screen.
   */
  pullFromURL: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    url: string,
    relay: boolean,
  ) => Promise<void>;
  importFromPath: (version: ImageCatalogEntry, nodeUUID: string, path: string) => Promise<void>;
  inspectVersion: (version: ImageCatalogEntry, nodeUUID: string) => void;
  /** Stops a transfer that is still running on that node. */
  cancelTransfer: (version: ImageCatalogEntry, nodeUUID: string) => void;
  /** Deletes the image from that node and marks the row absent again. */
  removeFromNode: (version: ImageCatalogEntry, nodeUUID: string) => void;
  /**
   * Pulls a rolling tag again on a node that already holds it, so the node
   * picks up whatever the tag points at upstream now.
   */
  repullVersion: (version: ImageCatalogEntry, nodeUUID: string) => void;
};

export default function ImageNodeTable({
  selectedVersion,
  loading = false,
  inspecting,
  availablePeer,
  modeFor,
  setMode,
  pullVersion,
  probeImage,
  syncFromPeer,
  pullFromURL,
  importFromPath,
  inspectVersion,
  cancelTransfer,
  removeFromNode,
  repullVersion,
}: Props) {
  const { t } = useTranslation("images");
  const { t: common } = useTranslation("common");
  // The archive dialog is the one place an address is entered, resolved and
  // confirmed, so it also owns the per-node relay choice: one node can reach
  // the mirror while its neighbour cannot.
  const [dialog, setDialog] = useState<{
    version: ImageCatalogEntry;
    row: ImageNode;
    url: string;
  } | null>(null);
  // Peer sync is chosen, probed and rate limited in its own dialog, like the
  // URL fetch: the choices belong to one transfer, not to the table row.
  const [peerDialog, setPeerDialog] = useState<{ version: ImageCatalogEntry; row: ImageNode } | null>(null);
  const [pathDialog, setPathDialog] = useState<{ version: ImageCatalogEntry; row: ImageNode } | null>(null);
  const declaredArchiveURL = (version: ImageCatalogEntry) =>
    (version.registry_sources ?? []).find((entry) => isArchiveURL(entry)) || "";
  // Each row names its node; the page lists no nodes of its own.
  const nodeLabel = (row: ImageNode) => row.node_name || row.node_uuid.slice(0, 12);
  // Automatic first (the default), then a copy from another node when one
  // holds the image, then the archive addresses and the URL action.
  const sourceOptions = (row: ImageNode) => {
    const [automatic, ...archives] = imageSourceOptions(selectedVersion.registry_sources, t);
    return [
      automatic,
      ...(availablePeer(selectedVersion, row)
        ? [{ value: "peer", label: t("nodeTable.peerOption") }]
        : []),
      ...archives,
      { value: "local", label: t("nodeTable.localOption") },
    ];
  };
  // The row's own facts beyond its state, and the actions on an image the node
  // already holds: one step down from the row, so the row keeps to what the
  // table is read for and the actions that move bytes (detailDrawer.css).
  const renderExtra = (row: ImageNode) => {
    const displayMessage = row.transfer_error_code
      ? productMessageText(common, { code: row.transfer_error_code, message: row.message ?? "" }) : row.message;
    const at = row.updated_at ? Date.parse(row.updated_at) : NaN;
    return (
      <>
        <dl className="detail-extra">
          <div>
            <dt>{t("nodeTable.column.digest")}</dt>
            <dd>{row.image_digest_id ? <Text code copyable={{ text: row.image_digest_id }}>{shortDigest(row.image_digest_id)}</Text> : "—"}</dd>
          </div>
          <div>
            <dt>{t("nodeTable.column.updated")}</dt>
            <dd>
              {Number.isNaN(at) ? "—" : (
                <Tooltip title={formatTime(row.updated_at)}>
                  <span>{agoText(t, Math.max(0, (Date.now() - at) / 1000))}</span>
                </Tooltip>
              )}
            </dd>
          </div>
          <div>
            <dt>UUID</dt>
            <dd><Text code copyable>{row.node_uuid}</Text></dd>
          </div>
          <div style={{ gridColumn: "1 / -1" }}>
            <dt>{t("nodeTable.column.message")}</dt>
            <dd>{displayMessage ? <Text type="warning">{displayMessage}</Text> : "—"}</dd>
          </div>
        </dl>
        {row.transfer_id && <PeerTransferDetails transferId={row.transfer_id} transferErrorCode={row.transfer_error_code} />}
        <Space size={8} wrap className="detail-extra-actions">
          <Tooltip title={t("nodeTable.inspectHelp")}>
          <Button
            size="small"
            disabled={row.state !== "present"}
            loading={inspecting === `${selectedVersion.id}:${row.node_uuid}`}
            onClick={(event) => {
              event.stopPropagation();
              inspectVersion(selectedVersion, row.node_uuid);
            }}
          >
            {t("nodeTable.refreshInspect")}
          </Button>
          </Tooltip>
          {row.state === "present" && selectedVersion.rolling_tag ? (
            <Tooltip title={t("nodeTable.repullHelp")}>
              <Button
                size="small"
                onClick={(event) => {
                  event.stopPropagation();
                  repullVersion(selectedVersion, row.node_uuid);
                }}
              >
                {t("nodeTable.repull")}
              </Button>
            </Tooltip>
          ) : null}
          {row.state === "present" ? (
            <Popconfirm
              title={t("nodeTable.removeConfirm", { ref: selectedVersion.image_ref, node: nodeLabel(row) })}
              description={t("nodeTable.removeImpact")}
              okText={t("nodeTable.removeConfirmOk")}
              cancelText={t("common.cancel")}
              onConfirm={() => removeFromNode(selectedVersion, row.node_uuid)}
            >
              <Button
                size="small"
                danger
                onClick={(event) => event.stopPropagation()}
              >
                {t("nodeTable.remove")}
              </Button>
            </Popconfirm>
          ) : null}
        </Space>
      </>
    );
  };
  const nodeColumns = [
    {
      title: t("nodeTable.column.node"),
      dataIndex: "node_uuid",
      // Keep the hostname readable when the source and action columns grow.
      width: 150,
      render: (value: string, row: ImageNode) => {
        return (
          <Space direction="vertical" size={0}>
            <Text>{nodeLabel(row)}</Text>
            <Text type="secondary" code>
              {value.slice(0, 12)}
            </Text>
          </Space>
        );
      },
    },
    {
      title: t("nodeTable.column.status"),
      dataIndex: "state",
      render: (value: string) => stateTag(t, value),
    },
    {
      title: t("nodeTable.column.progress"),
      key: "progress",
      render: (_: unknown, row: ImageNode) => {
        if (row.state !== "pulling" && row.state !== "syncing") {
          return <Text type="secondary">—</Text>;
        }
        if (row.source === "local") return <span className="text-fg-muted">{t("nodeTable.localLoading")}</span>;
        const expected = row.size_bytes || selectedVersion.size_bytes || 0;
        const done = row.transferred_bytes || 0;
        return (
          <Space direction="vertical" style={{ width: 108 }} size={2}>
            <Progress
              percent={
                expected
                  ? Math.min(100, Math.round((done / expected) * 100))
                  : 0
              }
              size="small"
            />
            <Text type="secondary">
              {bytes(done)}{expected > 0 ? ` / ${bytes(expected)}` : ""}
              {row.rate_bps ? ` · ${bytes(row.rate_bps)}/s` : ""}
            </Text>
          </Space>
        );
      },
    },
    {
      title: t("nodeTable.column.source"),
      key: "source",
      render: (_: unknown, row: ImageNode) =>
        row.state === "present" ? (
          row.source_node_uuid ? (
            <Tag color="cyan">{t("nodeTable.peerFrom", {
              node: row.source_node_name || row.source_node_uuid.slice(0, 8),
            })}</Tag>
          ) : row.source === "quay" ? (
            <Tag>Quay</Tag>
          ) : row.source === "huaweicloud" ? (
            <Tag>Huawei Cloud</Tag>
          ) : row.source === "mirror" ? (
            <Tag>{t("nodeTable.sourceMirror")}</Tag>
          ) : row.source === "local" ? (
            <Badge size="sm" status="neutral">{t("nodeTable.sourceLocal")}</Badge>
          ) : row.source === "offline" ? (
            <Tag>{t("nodeTable.offline")}</Tag>
          ) : row.source === "url" ? (
            <Tag>{t("nodeTable.sourceURL")}</Tag>
          ) : row.source === "url-relay" ? (
            <Tag>{t("nodeTable.sourceURLRelay")}</Tag>
          ) : row.source && /[./]/.test(row.source) ? (
            // The node reports the reference it pulled; its registry is the source.
            <Tag title={row.source}>{registryName(row.source)}</Tag>
          ) : row.source ? (
            <Tag>{registryName(selectedVersion.image_ref)}</Tag>
          ) : (
            // Present without a recorded transfer: a replica pulled it at
            // launch, or someone pulled it on the node by hand.
            <Tooltip title={t("nodeTable.sourceUnknownHelp")}>
              <Text type="secondary">{t("nodeTable.sourceUnknown")}</Text>
            </Tooltip>
          )
        ) : row.state === "pulling" || row.state === "syncing" ? (
          // A transfer in flight: where the bytes come from right now - the
          // node copying it over, or the registry of the reference the node
          // is pulling (it reports each candidate as it tries it).
          row.source_node_uuid ? (
            <Tag color="cyan">{t("nodeTable.peerFrom", {
              node: row.source_node_name || row.source_node_uuid.slice(0, 8),
            })}</Tag>
          ) : row.source === "local" ? (
            <Badge size="sm" status="info">{t("nodeTable.sourceLocal")}</Badge>
          ) : row.source && /[./]/.test(row.source) ? (
            <Tag color="processing" title={row.source}>{t("nodeTable.pullingFrom", { source: registryName(row.source) })}</Tag>
          ) : (
            <Text type="secondary">{t("nodeTable.pullingPending")}</Text>
          )
        ) : (
          // A click in the picker must not also expand the row.
          <span onClick={(event) => event.stopPropagation()}>
            <Select
              size="small"
              style={{ minWidth: 156 }}
              aria-label={t("nodeTable.column.source")}
              value={modeFor(selectedVersion, row)}
              onChange={(value) => setMode(selectedVersion, row.node_uuid, value)}
              options={sourceOptions(row)}
            />
          </span>
        ),
    },
    {
      title: t("nodeTable.column.actions"),
      key: "action",
      render: (_: unknown, row: ImageNode) => (
        <Space>
          {row.state === "pulling" || row.state === "syncing" ? (
            <Button
              size="small"
              danger
              onClick={(event) => {
                event.stopPropagation();
                cancelTransfer(selectedVersion, row.node_uuid);
              }}
            >
              {t("nodeTable.cancel")}
            </Button>
          ) : null}
          {row.state === "present" ? null : (
            <Button
              size="small"
              loading={row.state === "pulling" || row.state === "syncing"}
              onClick={() => {
                const mode = String(modeFor(selectedVersion, row));
                const chosen = imageSourceOptions(selectedVersion.registry_sources, t)
                  .find((option) => option.value === mode);
                // A URL is fetched through the dialog: it asks for the
                // address, resolves it and refuses an archive that carries a
                // different image. A failed URL transfer reopens it on the
                // address this image already declares.
                const retriedURL =
                  row.state === "failed" && /^url(-relay)?$/.test(row.source || "")
                    ? declaredArchiveURL(selectedVersion)
                    : "";
                if (mode === "local") {
                  setPathDialog({ version: selectedVersion, row });
                  return;
                }
                if (mode === "peer") {
                  setPeerDialog({ version: selectedVersion, row });
                  return;
                }
                if (isArchiveURL(mode) || chosen?.opensDialog || retriedURL) {
                  setDialog({
                    version: selectedVersion,
                    row,
                    url: isArchiveURL(mode) ? mode : retriedURL,
                  });
                  return;
                }
                pullVersion(selectedVersion, row.node_uuid, row.state === "failed");
              }}
            >
              {/* The label names what the click does, not what happened last
                  time: a URL fetch opens the dialog again, so it stays the URL
                  action even after a failure. */}
              {isArchiveURL(String(modeFor(selectedVersion, row))) ||
              modeFor(selectedVersion, row) === archiveURLDialogValue
                ? t("nodeTable.pullFromURL")
                : modeFor(selectedVersion, row) === "peer"
                  ? t("nodeTable.peerAction")
                  : modeFor(selectedVersion, row) === "local"
                    ? t("nodeTable.localAction")
                    : t("nodeTable.pull")}
            </Button>
          )}
        </Space>
      ),
    },
  ];
  return (
    <>
      <Table
        rowKey="node_uuid"
        size="small"
        dataSource={selectedVersion.nodes ?? []}
        loading={loading}
        columns={nodeColumns}
        className="detail-expandable"
        scroll={{ x: "max-content" }}
        expandable={detailExpandable<ImageNode>(renderExtra, { expand: t("nodeTable.expand"), collapse: t("nodeTable.collapse") })}
        // An image reaches every node of the fleet; the drawer pages them.
        pagination={{ defaultPageSize: 20, hideOnSinglePage: true, showSizeChanger: true, pageSizeOptions: [10, 20, 50, 100] }}
      />
      {pathDialog && (
        <ImageArchivePathDialog
          imageRef={pathDialog.version.image_ref}
          nodeLabel={nodeLabel(pathDialog.row)}
          onCancel={() => setPathDialog(null)}
          onStart={async (path) => {
            await importFromPath(pathDialog.version, pathDialog.row.node_uuid, path);
            setPathDialog(null);
          }}
        />
      )}
      <ImageArchiveURLDialog
        open={Boolean(dialog)}
        version={dialog?.version ?? null}
        nodeLabel={dialog ? nodeLabel(dialog.row) : ""}
        initialURL={dialog?.url ?? ""}
        onCancel={() => setDialog(null)}
        onStart={async (url, relay) => {
          if (!dialog) return;
          await pullFromURL(dialog.version, dialog.row.node_uuid, url, relay);
          setDialog(null);
        }}
      />
      <ImagePeerSyncDialog
        open={Boolean(peerDialog)}
        version={peerDialog?.version ?? null}
        nodeLabel={peerDialog ? nodeLabel(peerDialog.row) : ""}
        sources={peerDialog ? peerSources(peerDialog.version, peerDialog.row).map((peer) => ({
          uuid: peer.node_uuid,
          label: nodeLabel(peer),
        })) : []}
        // A retry offers the failed attempt's source and limit again; the
        // operator can still change either before probing.
        initialSource={peerDialog?.row.state === "failed" ? peerDialog.row.source_node_uuid : undefined}
        initialRateKiB={peerDialog?.row.state === "failed" ? peerDialog.row.last_rate_limit_kib_s : undefined}
        onCancel={() => setPeerDialog(null)}
        onProbe={(sourceNode) => (peerDialog ? probeImage(peerDialog.version, peerDialog.row.node_uuid, sourceNode) : Promise.resolve())}
        onStart={async (sourceNode, rateLimitKiB) => {
          if (!peerDialog) return;
          await syncFromPeer(peerDialog.version, peerDialog.row.node_uuid, sourceNode, rateLimitKiB);
          setPeerDialog(null);
        }}
      />
    </>
  );
}
