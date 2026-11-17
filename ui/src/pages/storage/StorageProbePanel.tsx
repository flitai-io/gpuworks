import { Alert, Descriptions, Typography } from "antd";
import dayjs from "dayjs";
import { useTranslation } from "react-i18next";
import type { StorageProbeResult } from "../../storageTypes";
import { formatBytes } from "../../format";
import { nodeRowName } from "../../nodeDisplay";
import { HelpTip } from "../../ui-kit/HelpTip";

/** A node's trial mount result, with the target and result-receipt time retained from that check. */
export default function StorageProbePanel({ result }: { result: StorageProbeResult }) {
  const { t } = useTranslation("storage");
  const yes = t("probe.yes");
  const no = t("probe.no");
  const at = result.checked_at ? dayjs(result.checked_at) : null;
  return (
    <Alert
      type={result.ok ? (result.writable ? "success" : "warning") : "error"}
      showIcon
      message={<>{t(result.ok ? "probe.ok" : "probe.failed", { node: nodeRowName(result) })} <HelpTip content={t("probe.scopeHelp")} /></>}
      description={
        <div>
          <Descriptions size="small" column={1}>
            <Descriptions.Item label={t("probe.target")}>{result.target || t("probe.unknown")}</Descriptions.Item>
            <Descriptions.Item label={t("probe.receivedAt")}>{at?.isValid() ? at.format("YYYY-MM-DD HH:mm:ss.SSS") : t("probe.unknown")}</Descriptions.Item>
          </Descriptions>
          {!result.ok ? <>
            {(result.message || result.reason) && <div>{result.message || result.reason}</div>}
            {result.output && <Typography.Paragraph code style={{ whiteSpace: "pre-wrap", marginTop: 8, marginBottom: 0 }}>{result.output}</Typography.Paragraph>}
          </> : <>
            <Descriptions size="small" column={2}>
              {result.version && <Descriptions.Item label={t("probe.version")}>{result.version}</Descriptions.Item>}
              {result.options && <Descriptions.Item label={t("probe.options")}>{result.options}</Descriptions.Item>}
              <Descriptions.Item label={t("probe.readable")}>{result.readable ? yes : no}</Descriptions.Item>
              <Descriptions.Item label={t("probe.writable")}>{result.writable ? yes : no}</Descriptions.Item>
              {result.total_bytes ? <Descriptions.Item label={t("probe.capacity")}>{t("probe.capacityValue", { available: formatBytes(result.available_bytes ?? 0), total: formatBytes(result.total_bytes) })}</Descriptions.Item> : null}
            </Descriptions>
            {!result.writable && <Typography.Text type="warning">{t("probe.readOnlyNotice")}</Typography.Text>}
          </>}
        </div>
      }
    />
  );
}
