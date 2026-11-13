import {
  Alert,
  Button,
  Checkbox,
  Descriptions,
  Input,
  Modal,
  Space,
  Tag,
  Tooltip,
  Typography,
} from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type ImageArchivePreview, type ImageCatalogEntry } from "../../api";
import { APIError, apiErrorText } from "../../apiError";
import { OutboundHostAccess, useOutboundHostAccess } from "../../components/OutboundHostAccess";
import ArchiveURLResolveAlert from "./ArchiveURLResolveAlert";
import { bytes } from "./ImagePageHelpers";

const { Text } = Typography;

type Props = {
  open: boolean;
  /** The catalog version this node would end up holding. */
  version: ImageCatalogEntry | null;
  /** Human label of the target node, for the dialog title. */
  nodeLabel: string;
  /** Prefilled when a declared address was chosen; empty for a new address. */
  initialURL: string;
  onCancel: () => void;
  /**
   * Records the address on the image and starts the transfer. It rejects when
   * the server refuses either step, so the reason stays on screen.
   */
  onStart: (url: string, relay: boolean) => Promise<void>;
};

/**
 * ImageArchiveURLDialog asks one node to fetch one image from a `docker save`
 * archive published over HTTP.
 *
 * The address is resolved before anything is downloaded, so the operator sees
 * the references the archive actually carries. When the archive turns out to
 * carry some other image, the transfer is refused here: the operator asked for
 * this specific image, and the address they pasted belongs to a different one.
 * An archive that cannot be inspected (compressed, or a mirror without Range
 * support) is a different case and stays allowed — the node loads it and
 * reports the reference it really carried.
 */
export default function ImageArchiveURLDialog({
  open,
  version,
  nodeLabel,
  initialURL,
  onCancel,
  onStart,
}: Props) {
  const { t } = useTranslation("images");
  const { t: tCommon } = useTranslation("common");
  const [url, setUrl] = useState(initialURL);
  const access = useOutboundHostAccess([url], open);
  const [relay, setRelay] = useState(false);
  const [resolving, setResolving] = useState(false);
  const [starting, setStarting] = useState(false);
  const [preview, setPreview] = useState<ImageArchivePreview | null>(null);
  const [resolveError, setResolveError] = useState<Error | null>(null);
  const [startError, setStartError] = useState<Error | null>(null);

  const resolve = async (address: string, authorize = true) => {
    const trimmed = address.trim();
    if (!trimmed) {
      setResolveError(new Error(t("import.url.required")));
      setPreview(null);
      return;
    }
    setResolving(true);
    setResolveError(null);
    try {
      if (authorize) await access.authorize();
      setPreview(await api.previewImageArchiveURL(trimmed));
    } catch (e) {
      setPreview(null);
      setResolveError(e instanceof APIError ? e : new Error(t("import.url.failed")));
    } finally {
      setResolving(false);
    }
  };

  // Opening the dialog on an address the image already declares resolves it
  // straight away: the operator came here to fetch that image, not to wait for
  // a button they have already pressed once.
  useEffect(() => {
    if (!open) return;
    setUrl(initialURL);
    setRelay(false);
    setPreview(null);
    setResolveError(null);
    setStartError(null);
    setResolving(false);
    setStarting(false);
    if (initialURL.trim()) void resolve(initialURL, false);
  }, [open, initialURL]);

  const errorMessage = (error: Error, fallback: string) => error instanceof APIError
    ? apiErrorText((key, options) => tCommon(key, { ...options, defaultValue: fallback }), error)
    : error.message;

  const tags = preview?.repo_tags ?? [];
  // A mismatch is only known once the archive named its references. An archive
  // that named none is unverifiable, not wrong.
  const mismatch =
    Boolean(version) &&
    tags.length > 0 &&
    !tags.includes(String(version?.image_ref));
  const declared = Boolean(
    version && (version.registry_sources ?? []).includes(url.trim()),
  );

  const submit = async () => {
    if (mismatch) return;
    setStarting(true);
    setStartError(null);
    try {
      if (relay) await access.authorize();
      await onStart(url.trim(), relay);
    } catch (e) {
      setStartError(e instanceof APIError ? e : new Error(t("action.pullFailed")));
    } finally {
      setStarting(false);
    }
  };

  return (
    <Modal
      open={open}
      title={t("nodeTable.archiveDialog.title", {
        ref: version?.image_ref ?? "",
        node: nodeLabel,
      })}
      onCancel={onCancel}
      okText={t("nodeTable.pull")}
      cancelText={t("common.cancel")}
      confirmLoading={starting}
      okButtonProps={{ disabled: !url.trim() || resolving || mismatch }}
      onOk={() => void submit()}
      destroyOnHidden
    >
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        <Text type="secondary">{t("nodeTable.archiveDialog.description")}</Text>
        <Space.Compact style={{ width: "100%" }}>
          <Input
            id="node-archive-url"
            name="node_archive_url"
            aria-label={t("import.url.label")}
            placeholder="https://containerimages.example/runner.tar"
            value={url}
            onChange={(event) => {
              setUrl(event.target.value);
              setPreview(null);
              setResolveError(null);
              setStartError(null);
            }}
            onPressEnter={() => void resolve(url)}
          />
          <Button loading={resolving} onClick={() => void resolve(url)}>
            {t("import.url.resolve")}
          </Button>
        </Space.Compact>
        <OutboundHostAccess access={access} />
        {!declared && url.trim() ? (
          <Text type="secondary">
            {t("nodeTable.archiveDialog.recordNote", { ref: version?.image_ref ?? "" })}
          </Text>
        ) : null}
        {preview ? (
          <Descriptions size="small" column={1} bordered>
            <Descriptions.Item label={t("import.url.size")}>
              {bytes(preview.size_bytes)}
            </Descriptions.Item>
            <Descriptions.Item label={t("import.url.tags")}>
              {tags.length ? (
                <Space wrap>
                  {tags.map((tag) => (
                    <Tag key={tag} color={tag === version?.image_ref ? "green" : "blue"}>
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
        {mismatch ? (
          <Alert
            type="error"
            showIcon
            message={t("nodeTable.archiveDialog.mismatch")}
            description={t("nodeTable.archiveDialog.mismatchDetail", {
              ref: version?.image_ref ?? "",
              tags: tags.join("、"),
            })}
          />
        ) : null}
        {!mismatch && preview && !preview.previewable ? (
          <Alert
            type="warning"
            showIcon
            message={t("nodeTable.archiveDialog.unverified")}
            description={t("import.url.notPreviewable")}
          />
        ) : null}
        {!mismatch && !preview && resolveError ? (
          <ArchiveURLResolveAlert error={resolveError} fallback={t("import.url.failed")} />
        ) : null}
        {startError ? <Alert type="error" showIcon message={errorMessage(startError, t("action.pullFailed"))}
          description={startError instanceof APIError ? `HTTP ${startError.status} · ${startError.code}` : undefined} /> : null}
        <Tooltip title={t("nodeTable.relayHelp")}>
          <Checkbox checked={relay} onChange={(event) => setRelay(event.target.checked)}>
            {t("nodeTable.relay")}
          </Checkbox>
        </Tooltip>
      </Space>
    </Modal>
  );
}
