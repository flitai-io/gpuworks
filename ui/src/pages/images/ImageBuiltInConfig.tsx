import { Card, Space, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { ImageCatalogEntry, ImageInspection } from "../../api";
import { HelpTip } from "../../ui-kit/HelpTip";

const { Text } = Typography;

type Props = {
  version: ImageCatalogEntry;
  // The last node inspection of this version, if any: read on request and
  // shown as the image's own configuration, never stored (ADR 0031).
  inspection: { nodeUUID: string; result: ImageInspection } | null;
  nodeName: (nodeUUID: string) => string;
};

export default function ImageBuiltInConfig({ version, inspection, nodeName }: Props) {
  const { t } = useTranslation("images");
  const inspected = inspection?.result;
  return (
    <Card size="small" title={t("detail.builtInTitle")}>
      <Space direction="vertical" size={4} style={{ width: "100%" }}>
        {version.driver_requirement && (
          <Text>{t("detail.driverRequirement", { value: version.driver_requirement })}</Text>
        )}
        {inspection && inspected ? (
          <>
            <Text type="secondary">{t("detail.builtInFrom", { node: nodeName(inspection.nodeUUID) })}</Text>
            <Text>
              ENTRYPOINT: <Text code>{inspected.entrypoint?.length ? JSON.stringify(inspected.entrypoint) : t("detail.builtInNone")}</Text>
            </Text>
            <Text>
              CMD: <Text code>{inspected.cmd?.length ? JSON.stringify(inspected.cmd) : t("detail.builtInNone")}</Text>
            </Text>
            {Object.keys(inspected.env || {}).length > 0 && (
              <Text>
                ENV:{" "}
                {Object.entries(inspected.env || {}).map(([key, value]) => (
                  <Tag key={key} style={{ wordBreak: "break-all" }}>{key}={value}</Tag>
                ))}
              </Text>
            )}
            {(inspected.volumes?.length || 0) > 0 && (
              <Text>
                VOLUME: <Text code>{(inspected.volumes || []).join(" ")}</Text>
              </Text>
            )}
          </>
        ) : (
          <Text type="secondary">{t("detail.builtInHint")} <HelpTip content={t("detail.builtInHelp")} /></Text>
        )}
      </Space>
    </Card>
  );
}
