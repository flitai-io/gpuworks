import { useEffect, useState } from "react";
import {
  Alert,
  Button,
  Popconfirm,
  Select,
  Table,
  Typography,
} from "antd";
import { DeleteOutlined } from "@ant-design/icons";
import { Link } from "react-router-dom";
import type { ColumnsType } from "antd/es/table";
import type {
  ImageCatalogEntry,
  ImageInspection,
  ImageSeries,
  ImageNode,
} from "../../api";
import DetailDrawer, { detailExpandable } from "../../components/detail/DetailDrawer";
import DetailSection from "../../components/detail/DetailSection";
import ImageNodeTable from "./ImageNodeTable";
import { VersionExtra } from "./ImageVersionColumns";
import ImageBuiltInConfig from "./ImageBuiltInConfig";
import { digestMismatchNodes } from "./ImagePageHelpers";
import { useTranslation } from "react-i18next";

const { Text } = Typography;
type Props = {
  detail: ImageSeries | null;
  onClose: () => void;
  /**
   * The series' versions. Until GET /images/{series_id} answers these are the
   * catalog list's rows, which carry counts but no node rows.
   */
  detailVersions: ImageCatalogEntry[];
  /** Set while the series detail, the only source of node rows, is being read. */
  nodesLoading: boolean;
  versionColumns: ColumnsType<ImageCatalogEntry>;
  selectedVersion: ImageCatalogEntry | null;
  setSelectedVersion: (version: ImageCatalogEntry) => void;
  // The last node inspection of the selected version, if any: read on
  // request and shown as the image's own configuration, never stored.
  inspection: { nodeUUID: string; result: ImageInspection } | null;
  nodeName: (nodeUUID: string) => string;
  inspecting: string | null;
  availablePeer: (version: ImageCatalogEntry, target: ImageNode) => boolean;
  modeFor: (
    version: ImageCatalogEntry,
    target: ImageNode,
  ) => string;
  setMode: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    mode: string,
  ) => void;
  pullVersion: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    retry?: boolean,
  ) => void;
  probeImage: (version: ImageCatalogEntry, nodeUUID: string, sourceNode: string) => Promise<unknown>;
  syncFromPeer: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    sourceNode: string,
    rateLimitKiB: number,
  ) => Promise<void>;
  pullFromURL: (
    version: ImageCatalogEntry,
    nodeUUID: string,
    url: string,
    relay: boolean,
  ) => Promise<void>;
  importFromPath: (version: ImageCatalogEntry, nodeUUID: string, path: string) => Promise<void>;
  inspectVersion: (version: ImageCatalogEntry, nodeUUID: string) => void;
  cancelTransfer: (version: ImageCatalogEntry, nodeUUID: string) => void;
  removeFromNode: (version: ImageCatalogEntry, nodeUUID: string) => void;
  repullVersion: (version: ImageCatalogEntry, nodeUUID: string) => void;
  /** Removes a community or custom series; system series have no delete. */
  deleteSeries: (series: ImageSeries) => void;
  /** Removes a community or custom version; system versions have no delete. */
  deleteVersion: (version: ImageCatalogEntry) => void;
};

export default function ImageDetailDrawer({
  detail,
  onClose,
  detailVersions,
  nodesLoading,
  versionColumns,
  selectedVersion,
  setSelectedVersion,
  inspection,
  nodeName,
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
  deleteSeries,
  deleteVersion,
}: Props) {
  const { t } = useTranslation("images");
  const [tab, setTab] = useState("versions");
  // Picking a version, by its row or its link, is asking where it is: the
  // node tab opens on it. A series opened on a version (after an import)
  // opens there too.
  useEffect(() => setTab(selectedVersion ? "nodes" : "versions"), [detail?.id]);
  useEffect(() => {
    if (selectedVersion) setTab("nodes");
  }, [selectedVersion?.id]);
  // System rows are maintained by the catalog manifest: no delete here.
  const editable = Boolean(detail && detail.source !== "official");
  const mismatched = detailVersions
    .map((version) => ({ version, nodes: digestMismatchNodes(version) }))
    .filter((item) => item.nodes > 0);
  const openVersion = (version: ImageCatalogEntry) => {
    setSelectedVersion(version);
    setTab("nodes");
  };
  return (
    <DetailDrawer
      open={!!detail}
      onClose={onClose}
      title={detail ? detail.name : t("detail.fallbackTitle")}
      subtitle={detail ? [detail.engine || "-", detail.framework].filter(Boolean).join(" · ") : undefined}
      extra={
        editable &&
        detail && (
          <Popconfirm
            title={t("seriesTable.deleteTitle", { name: detail.name })}
            description={t("seriesTable.deleteDescription")}
            okText={t("common.delete")}
            cancelText={t("common.cancel")}
            okButtonProps={{ danger: true }}
            onConfirm={() => deleteSeries(detail)}
          >
            <Button danger size="small" icon={<DeleteOutlined />}>{t("detail.deleteSeries")}</Button>
          </Popconfirm>
        )
      }
      activeTab={tab}
      onTabChange={setTab}
      tabs={
        detail
          ? [
              {
                key: "versions",
                label: t("detail.tab.versions"),
                count: detailVersions.length,
                // The table is the tab's only block: a section title would repeat the tab's.
                children: (
                  <>
                    {mismatched.map(({ version, nodes }) => (
                      <Alert
                        key={version.id}
                        type="warning"
                        showIcon
                        style={{ marginBottom: 10 }}
                        message={t("detail.digestMismatch", { ref: version.image_ref, nodes })}
                      />
                    ))}
                    <Typography.Paragraph type="secondary" style={{ fontSize: 12, margin: "0 0 10px" }}>
                      {t("detail.versionHint")}
                    </Typography.Paragraph>
                    <Table
                      className="detail-expandable"
                      rowKey="id"
                      size="small"
                      dataSource={detailVersions}
                      columns={versionColumns}
                      scroll={{ x: "max-content" }}
                      // A row's click opens its node distribution; its chevron expands it.
                      expandable={detailExpandable<ImageCatalogEntry>(
                        (row) => <VersionExtra row={row} t={t} />,
                        { expand: t("nodeTable.expand"), collapse: t("nodeTable.collapse") },
                        { rowClick: false },
                      )}
                      pagination={{ defaultPageSize: 8, hideOnSinglePage: true }}
                      rowClassName={(row) =>
                        [row.effective_enabled === false ? "image-version-disabled" : "", row.id === selectedVersion?.id ? "image-version-selected" : ""]
                          .filter(Boolean)
                          .join(" ")
                      }
                      onRow={(row) => ({ onClick: () => openVersion(row) })}
                    />
                  </>
                ),
              },
              {
                key: "nodes",
                label: t("detail.tab.nodes"),
                children: (
                  <>
                    <DetailSection
                      title={t("detail.nodeDistribution")}
                      extra={
                        editable && selectedVersion ? (
                          <Popconfirm
                            title={t("versionTable.deleteTitle", { ref: selectedVersion.image_ref })}
                            description={t("versionTable.deleteDescription")}
                            okText={t("common.delete")}
                            cancelText={t("common.cancel")}
                            okButtonProps={{ danger: true }}
                            onConfirm={() => deleteVersion(selectedVersion)}
                          >
                            <Button danger size="small" icon={<DeleteOutlined />}>{t("detail.deleteVersion")}</Button>
                          </Popconfirm>
                        ) : null
                      }
                    >
                      <div className="image-version-pick">
                        <Text type="secondary">{t("detail.version")}</Text>
                        <Select
                          size="small"
                          placeholder={t("detail.selectVersion")}
                          aria-label={t("detail.version")}
                          value={selectedVersion?.id}
                          onChange={(id: number) => {
                            const version = detailVersions.find((item) => item.id === id);
                            if (version) setSelectedVersion(version);
                          }}
                          options={detailVersions.map((version) => ({ value: version.id, label: version.image_ref }))}
                        />
                      </div>
                      {/* With no version chosen the picker's placeholder is the prompt. */}
                      {selectedVersion && (
                        <>
                          <ImageNodeTable
                            selectedVersion={selectedVersion}
                            loading={nodesLoading}
                            inspecting={inspecting}
                            availablePeer={availablePeer}
                            modeFor={modeFor}
                            setMode={setMode}
                            pullVersion={pullVersion}
                            probeImage={probeImage}
                            syncFromPeer={syncFromPeer}
                            pullFromURL={pullFromURL}
                            importFromPath={importFromPath}
                            inspectVersion={inspectVersion}
                            cancelTransfer={cancelTransfer}
                            removeFromNode={removeFromNode}
                            repullVersion={repullVersion}
                          />
                          <Text type="secondary" style={{ display: "block", marginTop: 8, fontSize: 12 }}>
                            {t("nodeTable.autoNote")} <Link to="/settings">{t("nodeTable.autoNoteLink")}</Link>
                          </Text>
                        </>
                      )}
                    </DetailSection>
                    {/* The card carries its own title; a section around it would repeat it. */}
                    {selectedVersion && (
                      <div className="detail-cards">
                        <ImageBuiltInConfig version={selectedVersion} inspection={inspection} nodeName={nodeName} />
                      </div>
                    )}
                  </>
                ),
              },
            ]
          : undefined
      }
    />
  );
}
