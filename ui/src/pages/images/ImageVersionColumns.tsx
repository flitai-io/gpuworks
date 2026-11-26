import { Space, Switch, Tag, Tooltip, Typography } from "antd";
import type { ColumnsType } from "antd/es/table";
import type { ImageCatalogEntry } from "../../api";
import { bytes, rollingTag, upstreamGoneTag } from "./ImagePageHelpers";
import type { TFunction } from "i18next";

const { Text } = Typography;

type Props = {
  seriesEnabled: boolean;
  onSelect: (row: ImageCatalogEntry) => void;
  onToggle: (row: ImageCatalogEntry, enabled: boolean) => void;
};

// Catalog metadata is read-only here: a system image changes only through a
// manifest, and removing a community or custom version is an action of the
// detail drawer, not of every row.
export function buildVersionColumns({
  seriesEnabled,
  onSelect,
  onToggle,
}: Props, t: TFunction): ColumnsType<ImageCatalogEntry> {
  return [
    {
      title: t("versionTable.column.tag"),
      dataIndex: "image_ref",
      // The tag tells a series' versions apart; the repository they share
      // goes under it. The link keeps the full reference as its name.
      render: (value: string, row) => {
        const { repository, tag } = splitImageRef(value);
        return (
          <div className="image-version-ref">
            <Space size={4}>
              <a
                href="#image-version"
                aria-label={value}
                title={value}
                onClick={(event) => {
                  event.preventDefault();
                  event.stopPropagation();
                  onSelect(row);
                }}
              >
                <Text code>{tag}</Text>
              </a>
              {rollingTag(t, row)}
              {upstreamGoneTag(t, row)}
            </Space>
            {repository && <Text type="secondary">{repository}</Text>}
          </div>
        );
      },
    },
    {
      title: t("versionTable.column.distributed"),
      key: "coverage",
      render: (_value, row) =>
        `${row.present_nodes ?? 0} / ${row.total_nodes ?? 0}`,
    },
    {
      title: t("versionTable.column.status"),
      key: "enabled",
      render: (_value, row) =>
        row.deprecated ? (
          <Tooltip
            title={row.deprecated_since ? t("versionTable.deprecatedSince", { version: row.deprecated_since }) : undefined}
          >
            <Tag color="orange">
              {row.reference_count
                ? t("versionTable.deprecatedRefs", { refs: row.reference_count })
                : t("versionTable.olderVersion")}
            </Tag>
          </Tooltip>
        ) : (
          <Space size={6}>
            <Switch
              size="small"
              aria-label={t("versionTable.enabledLabel", { ref: row.image_ref })}
              checked={row.effective_enabled ?? row.enabled}
              disabled={!seriesEnabled}
              title={!seriesEnabled ? t("versionTable.disabledBySeries") : undefined}
              onClick={(_, event) => event.stopPropagation()}
              onChange={(checked) => onToggle(row, checked)}
            />
            {!seriesEnabled && <Tag>{t("versionTable.disabledBySeries")}</Tag>}
          </Space>
        ),
    },
  ];
}

// splitImageRef parts a reference into the repository and what tells its
// versions apart: the tag, or the digest of a pinned reference.
export function splitImageRef(ref: string): { repository: string; tag: string } {
  const at = ref.indexOf("@");
  if (at > 0) return { repository: ref.slice(0, at), tag: ref.slice(at + 1) };
  const colon = ref.lastIndexOf(":");
  if (colon > ref.lastIndexOf("/")) return { repository: ref.slice(0, colon), tag: ref.slice(colon + 1) };
  return { repository: "", tag: ref };
}

// VersionExtra is a version row's expanded detail: what it runs and how big
// it is, beside the tag, coverage and state the row keeps.
export function VersionExtra({ row, t }: { row: ImageCatalogEntry; t: TFunction }) {
  return (
    <dl className="detail-extra">
      <div>
        <dt>{t("versionTable.column.engineVersion")}</dt>
        <dd>{row.engine_version || "—"}</dd>
      </div>
      <div>
        <dt>{t("versionTable.column.frameworkVersion")}</dt>
        <dd>{`${row.framework || "—"} ${row.framework_version || ""}`}</dd>
      </div>
      <div>
        <dt>
          <Tooltip title={t("versionTable.sizeHelp")}>{t("versionTable.column.size")}</Tooltip>
        </dt>
        <dd>{bytes(row.download_size_bytes ?? row.size_bytes)}</dd>
      </div>
      <div>
        <dt>{t("versionTable.column.arch")}</dt>
        <dd>{row.arch || "multi"}</dd>
      </div>
      <div style={{ gridColumn: "1 / -1" }}>
        <dt>{t("versionTable.column.ref")}</dt>
        <dd><Text code copyable>{row.image_ref}</Text></dd>
      </div>
    </dl>
  );
}
