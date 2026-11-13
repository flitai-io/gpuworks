import { Alert, Button, Card, Descriptions, Input, Space, Tag, Typography } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type ImageArchivePreview, type ImageCatalogEntry } from "../../api";
import { APIError } from "../../apiError";
import { OutboundHostAccess, useOutboundHostAccess } from "../../components/OutboundHostAccess";
import ArchiveURLResolveAlert from "./ArchiveURLResolveAlert";
import { bytes } from "./ImagePageHelpers";

const { Text } = Typography;

type Props = {
  /** Every catalog version currently listed, searched for a matching tag. */
  versions: ImageCatalogEntry[];
  /**
   * Called when the archive's reference already exists in the catalog. The
   * caller opens that version and records the URL on it.
   */
  onMatched: (version: ImageCatalogEntry, preview: ImageArchivePreview) => void;
  /**
   * Called when nothing matches. The caller prefills the custom image form
   * with the reference the archive carries and the address it came from.
   */
  onPrefill: (preview: ImageArchivePreview) => void;
};

/**
 * ImageArchiveURLImport turns a pasted `docker save` archive address into a
 * catalog entry. The server reads the archive's real references out of its
 * tail, so the operator never has to guess them from the file name.
 */
export default function ImageArchiveURLImport({ versions, onMatched, onPrefill }: Props) {
  const { t } = useTranslation("images");
  const [url, setUrl] = useState("");
  const access = useOutboundHostAccess([url]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [preview, setPreview] = useState<ImageArchivePreview | null>(null);

  const matchedVersion = (tags: string[]) =>
    versions.find((version) => tags.includes(version.image_ref)) || null;

  const resolve = async () => {
    const address = url.trim();
    if (!address) {
      setError(new Error(t("import.url.required")));
      return;
    }
    setLoading(true);
    setError(null);
    setPreview(null);
    try {
      await access.authorize();
      const result = await api.previewImageArchiveURL(address);
      setPreview(result);
      const match = matchedVersion(result.repo_tags || []);
      if (match) {
        onMatched(match, result);
        return;
      }
      if (result.repo_tags?.length) onPrefill(result);
    } catch (e) {
      setError(e instanceof APIError ? e : new Error(t("import.url.failed")));
    } finally {
      setLoading(false);
    }
  };

  const tags = preview?.repo_tags || [];
  const match = preview ? matchedVersion(tags) : null;
  return (
    <Card size="small" title={t("import.url.title")}>
      <Space direction="vertical" size={10} style={{ width: "100%" }}>
        <Text type="secondary">{t("import.url.description")}</Text>
        <Space.Compact style={{ width: "100%" }}>
          <Input
            id="image-archive-url"
            name="image_archive_url"
            value={url}
            aria-label={t("import.url.label")}
            placeholder="https://containerimages.example/runner.tar"
            onChange={(event) => setUrl(event.target.value)}
            onPressEnter={() => void resolve()}
          />
          <Button type="primary" loading={loading} onClick={() => void resolve()}>
            {t("import.url.resolve")}
          </Button>
        </Space.Compact>
        <OutboundHostAccess access={access} />
        {error ? <ArchiveURLResolveAlert error={error} fallback={t("import.url.failed")} /> : null}
        {preview ? (
          <Descriptions size="small" column={1} bordered>
            <Descriptions.Item label={t("import.url.size")}>{bytes(preview.size_bytes)}</Descriptions.Item>
            <Descriptions.Item label={t("import.url.tags")}>
              {tags.length ? (
                <Space wrap>
                  {tags.map((tag) => (
                    <Tag key={tag} color="blue">
                      {tag}
                    </Tag>
                  ))}
                </Space>
              ) : (
                <Text type="secondary">{t("import.url.tagsUnknown")}</Text>
              )}
            </Descriptions.Item>
          </Descriptions>
        ) : null}
        {preview && !preview.previewable ? (
          <Alert type="info" showIcon message={t("import.url.notPreviewable")} />
        ) : null}
        {match ? (
          <Alert type="success" showIcon message={t("import.url.matched", { ref: match.image_ref })} />
        ) : null}
        {preview && !match && tags.length ? (
          <Alert type="info" showIcon message={t("import.url.prefilled", { ref: tags[0] })} />
        ) : null}
      </Space>
    </Card>
  );
}
