import { Alert, Button, Form, InputNumber, Modal, Select, Space, Typography } from "antd";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import type { ImageCatalogEntry } from "../../api";

const { Text } = Typography;

export type PeerSyncSource = { uuid: string; label: string };

type Props = {
  open: boolean;
  version: ImageCatalogEntry | null;
  /** Human label of the target node, for the dialog title. */
  nodeLabel: string;
  /** Nodes that hold this image and have a live agent session. */
  sources: PeerSyncSource[];
  /** The source of the failed attempt being retried, when it is still usable. */
  initialSource?: string;
  /** The failed attempt's limit in KiB/s, prefilled for a retry only. */
  initialRateKiB?: number;
  onCancel: () => void;
  /** Rejects with the server's reason when the source cannot reach the target. */
  onProbe: (sourceNode: string) => Promise<unknown>;
  /** Rejects with the server's reason; the dialog stays open to show it. */
  onStart: (sourceNode: string, rateLimitKiB: number) => Promise<void>;
};

/**
 * ImagePeerSyncDialog copies an image from the chosen source. The start
 * button stays locked until a real direct or relayed transport probe passes.
 */
export default function ImagePeerSyncDialog({
  open,
  version,
  nodeLabel,
  sources,
  initialSource,
  initialRateKiB,
  onCancel,
  onProbe,
  onStart,
}: Props) {
  const { t } = useTranslation("images");
  const [source, setSource] = useState<string>();
  const [rateMiB, setRateMiB] = useState<number | null>(null);
  const [probe, setProbe] = useState<{ state: "idle" | "running" | "ok" | "failed"; error?: string; transport?: "direct" | "relay" }>({ state: "idle" });
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    const usable = sources.some((item) => item.uuid === initialSource);
    setSource(usable ? initialSource : sources.length === 1 ? sources[0].uuid : undefined);
    setRateMiB(initialRateKiB && initialRateKiB > 0 ? initialRateKiB / 1024 : null);
    setProbe({ state: "idle" });
    setStarting(false);
    setStartError(null);
  }, [open, initialSource, initialRateKiB]);

  const runProbe = async () => {
    if (!source) return;
    setProbe({ state: "running" });
    setStartError(null);
    try {
      const result = await onProbe(source);
      const transport = result && typeof result === "object" && "transport" in result
        && (result.transport === "direct" || result.transport === "relay") ? result.transport : undefined;
      setProbe({ state: "ok", transport });
    } catch (e) {
      setProbe({ state: "failed", error: e instanceof Error ? e.message : String(e) });
    }
  };

  const submit = async () => {
    if (!source || probe.state !== "ok") return;
    setStarting(true);
    setStartError(null);
    try {
      await onStart(source, rateMiB && rateMiB > 0 ? Math.round(rateMiB * 1024) : 0);
    } catch (e) {
      setStartError(e instanceof Error ? e.message : t("action.pullFailed"));
    } finally {
      setStarting(false);
    }
  };

  return (
    <Modal
      open={open}
      title={t("nodeTable.peerDialog.title", { ref: version?.image_ref ?? "", node: nodeLabel })}
      onCancel={onCancel}
      okText={t("nodeTable.peerDialog.start")}
      cancelText={t("common.cancel")}
      confirmLoading={starting}
      okButtonProps={{ disabled: !source || probe.state !== "ok" }}
      onOk={() => void submit()}
      destroyOnHidden
    >
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        <Text type="secondary">{t("nodeTable.peerDialog.description")}</Text>
        <Form layout="vertical" component="div">
          <Form.Item label={t("nodeTable.peerDialog.source")} htmlFor="peer-sync-source" style={{ marginBottom: 12 }}>
            <Select
              id="peer-sync-source"
              aria-label={t("nodeTable.peerDialog.source")}
              placeholder={t("nodeTable.peerDialog.sourcePlaceholder")}
              value={source}
              onChange={(value) => {
                setSource(value);
                setProbe({ state: "idle" });
                setStartError(null);
              }}
              options={sources.map((item) => ({
                value: item.uuid,
                label: item.label,
              }))}
            />
          </Form.Item>
          <Form.Item
            label={t("nodeTable.rateLimit")}
            htmlFor="peer-sync-rate"
            extra={t("nodeTable.peerDialog.rateHelp")}
            style={{ marginBottom: 0 }}
          >
            <InputNumber
              id="peer-sync-rate"
              aria-label={t("nodeTable.rateLimit")}
              min={0}
              step={10}
              style={{ width: 200 }}
              placeholder={t("nodeTable.unlimited")}
              suffix="MiB/s"
              value={rateMiB}
              onChange={(value) => setRateMiB(value == null ? null : Number(value))}
            />
          </Form.Item>
        </Form>
        <Space align="center" wrap>
          <Button disabled={!source} loading={probe.state === "running"} onClick={() => void runProbe()}>
            {t("nodeTable.testTransport")}
          </Button>
          {probe.state === "idle" ? <Text type="secondary">{t("nodeTable.peerDialog.probeHint")}</Text> : null}
        </Space>
        {probe.state === "ok" ? <Alert type="success" showIcon message={t(probe.transport === "relay" ? "nodeTable.peerDialog.probeRelay" : probe.transport === "direct" ? "nodeTable.peerDialog.probeDirect" : "nodeTable.peerDialog.probeOk")} /> : null}
        {probe.state === "failed" ? (
          <Alert type="error" showIcon message={t("nodeTable.peerDialog.probeFailed")} description={probe.error} />
        ) : null}
        {startError ? <Alert type="error" showIcon message={startError} /> : null}
      </Space>
    </Modal>
  );
}
