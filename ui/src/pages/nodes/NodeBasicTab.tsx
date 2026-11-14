import { Button, Descriptions, Select, Space, Tag } from "antd";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { EngineProfile, Node } from "../../api";
import type { UpgradePhase } from "../../agentUpgrade";
import KVDiskKernelWarning from "../../components/KVDiskKernelWarning";
import { GPUProbeFailedTag } from "../../components/NodeAcceleratorWarnings";
import GPUIsolationNotice, { GPUIsolationTag } from "../../components/NodeGPUIsolation";
import UCXNetlinkHangNotice, { UCXNetlinkHangTag } from "../../components/NodeUCXNetlinkHang";
import { NodeConnectionBadge } from "../../components/NodeConnectionBadge";
import NodeNetworksCard from "../../components/NodeNetworksCard";
import NodePullProxy from "../../components/NodePullProxy";
import { NodeSSHAccess } from "../../components/NodeRecoveryControls";
import RuntimeFallbackNotice from "../../components/RuntimeFallbackNotice";
import RuntimePluginStatus from "../../components/RuntimePluginStatus";
import { SystemLabelTags } from "../../components/SystemLabelTags";
import { formatTime } from "../../i18n/datetime";
import { runtimeLabel } from "../../nodeDisplay";
import DetailSection from "../../components/detail/DetailSection";
import NodeDiskUsage from "./NodeDiskUsage";
import NodeConnectionDiagnostics from "../../components/NodeConnectionDiagnostics";

// NodeBasicTab is the node detail's overview tab: the node itself, its
// container runtime, its storage and how it is reached.
export default function NodeBasicTab({
  node,
  engines,
  upgradePhase,
  runtimeLoading,
  onRename,
  onRuntime,
  onUpdated,
}: {
  node: Node;
  engines?: EngineProfile[] | null;
  upgradePhase?: UpgradePhase;
  runtimeLoading: boolean;
  onRename: () => void;
  onRuntime: (value: string) => void;
  onUpdated: (node: Node) => void;
}) {
  const { t } = useTranslation("nodes");
  const labels = Object.entries(node.labels ?? {});
  return (
    <>
      <DetailSection title={t("detail.section.node")}>
        <Descriptions bordered size="small" column={1}>
          <Descriptions.Item label={t("detail.name")}>
            <Space>
              <span>{node.name || "—"}</span>
              <Button size="small" onClick={onRename}>
                {t("action.edit")}
              </Button>
            </Space>
          </Descriptions.Item>
          <Descriptions.Item label={t("column.status")}>
            <Space size={4} wrap>
              <NodeConnectionBadge node={node} />
              <GPUProbeFailedTag node={node} />
              <GPUIsolationTag node={node} />
              <UCXNetlinkHangTag node={node} />
            </Space>
            <div>
              {t("connectionDiagnostics.lastSeen")}: {formatTime(node.last_seen_at, t("connectionDiagnostics.unknown"))}
            </div>
          </Descriptions.Item>
          <Descriptions.Item label="UUID">{node.uuid}</Descriptions.Item>
          <Descriptions.Item label={t("detail.agentVersion")}>
            {node.agent_version || "—"}
            {upgradePhase === "waiting" && (
              <Tag color="processing" style={{ marginLeft: 8 }}>
                {t("upgrade.inProgress")}
              </Tag>
            )}
          </Descriptions.Item>
          <Descriptions.Item label={t("detail.os")}>
            {node.os} / {node.arch} · {t("detail.kernel")} {node.kernel || "—"}
            <KVDiskKernelWarning kernel={node.kernel} engines={engines} />
          </Descriptions.Item>
          <Descriptions.Item label="CPU">
            {node.cpu_model || "—"} ({t("cpuCores", { count: node.cpu_cores })})
          </Descriptions.Item>
          <Descriptions.Item label={t("column.memory")}>{node.mem_total_gib.toFixed(1)} GiB</Descriptions.Item>
          {node.last_reject_reason && (
            <Descriptions.Item label={t("column.lastReject")}>
              {node.last_reject_reason}
              {node.last_reject_at ? ` · ${formatTime(node.last_reject_at)}` : ""}
            </Descriptions.Item>
          )}
          <Descriptions.Item label={t("systemLabels.title")}>
            <SystemLabelTags labels={node.system_labels} />
          </Descriptions.Item>
          <Descriptions.Item label={t("labels.title")}>
            {labels.length > 0 ? (
              <Space size={[4, 4]} wrap>
                {labels.map(([key, value]) => (
                  <Tag key={key}>
                    {key}={value}
                  </Tag>
                ))}
              </Space>
            ) : (
              "—"
            )}
          </Descriptions.Item>
        </Descriptions>
      </DetailSection>

      <DetailSection title={t("detail.section.runtime")}>
        <Descriptions bordered size="small" column={1}>
          <Descriptions.Item label={t("detail.runtime")}>
            <Space direction="vertical" size={4} style={{ width: "100%" }}>
              {node.runtime_choices?.length ? (
                <Select
                  size="small"
                  style={{ minWidth: 200 }}
                  value={node.runtime_preference || node.runtime_driver || undefined}
                  loading={runtimeLoading}
                  placeholder={t("runtime.select")}
                  onChange={onRuntime}
                  options={node.runtime_choices.map((value) => ({
                    value,
                    label:
                      value === "auto" && node.runtime_driver
                        ? t("runtime.autoCurrent", { runtime: runtimeLabel(node.runtime_driver) })
                        : runtimeLabel(value),
                  }))}
                />
              ) : (
                <span>{node.runtime_driver ? runtimeLabel(node.runtime_driver) : "—"}</span>
              )}
              <RuntimeFallbackNotice node={node} />
              <GPUIsolationNotice node={node} />
              <UCXNetlinkHangNotice node={node} />
            </Space>
          </Descriptions.Item>
          <Descriptions.Item label={t("detail.runtimePlugin")}>
            <RuntimePluginStatus node={node} />
          </Descriptions.Item>
        </Descriptions>
      </DetailSection>

      <DetailSection title={t("detail.section.storage")}>
        <Descriptions bordered size="small" column={1}>
          <Descriptions.Item label={t("detail.disk")}>{node.disk_total_gib.toFixed(1)} GiB</Descriptions.Item>
        </Descriptions>
        {/* The usage table runs the drawer's full width; inside a value cell its paths wrapped. */}
        <div className="detail-block" aria-label={t("diskUsage.title")}>
          <NodeDiskUsage node={node} />
        </div>
      </DetailSection>

      <DetailSection title={t("detail.section.network")}>
        <Descriptions bordered size="small" column={1}>
          <Descriptions.Item label={t("detail.connectionMode")}>{formatConnection(node, t)}</Descriptions.Item>
          <Descriptions.Item label={t("detail.sshChannel")}>
            <NodeSSHAccess node={node} onUpdated={onUpdated} />
          </Descriptions.Item>
          <Descriptions.Item label={t("detail.managementAddress")}>{node.management_address || "—"}</Descriptions.Item>
          <Descriptions.Item label={t("detail.sourceAddress")}>{node.last_remote_addr || "—"}</Descriptions.Item>
          <Descriptions.Item label={t("detail.serverExternalAddress")}>{node.server_url || "—"}</Descriptions.Item>
          <Descriptions.Item label={t("detail.agentExternalAddress")}>{node.server_dial_address || "—"}</Descriptions.Item>
          <Descriptions.Item label={t("detail.agentListenAddress")}>
            {node.server_dial_listen ||
              (node.server_dial_address ? `:${node.server_dial_address.split(":").pop()}` : "—")}
          </Descriptions.Item>
        </Descriptions>
        <div className="detail-cards">
          <NodeConnectionDiagnostics nodeUUID={node.uuid} />
          <NodeNetworksCard nodeUUID={node.uuid} />
          <NodePullProxy nodeUUID={node.uuid} />
        </div>
      </DetailSection>
    </>
  );
}

function formatConnection(node: Node, t: TFunction<"nodes">): string {
  if (node.connect_mode === "server_dial") {
    return node.server_dial_address
      ? t("connection.serverDialAddress", { address: node.server_dial_address })
      : t("connection.serverDial");
  }
  return t("connection.agentDial");
}
