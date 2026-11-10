import { useEffect, useRef, useState } from "react";
import { Button, Input, Modal, Space, message } from "antd";
import { useTranslation } from "react-i18next";
import {
  api,
  type EngineProfile,
  type Node,
  type NodeDoctorReport as NodeDoctorReportData,
  type TopoHighlight,
} from "../../api";
import type { UpgradePhase } from "../../agentUpgrade";
import { useAssistantSubjects } from "../../assistant/AssistantContext";
import { nodeSubjects } from "../../assistant/pageSubjects";
import AgentUpgradeButton from "../../components/AgentUpgradeButton";
import AIAssistant from "../../components/AIAssistant";
import NodeDoctorReport from "../../components/NodeDoctorReport";
import { NodeReconnectButton } from "../../components/NodeReconnectButton";
import { NodeRecoveryStatus } from "../../components/NodeRecoveryControls";
import { nodeDisplayName } from "../../nodeDisplay";
import DetailDrawer from "../../components/detail/DetailDrawer";
import NodeBasicTab from "./NodeBasicTab";
import NodeGpuTab from "./NodeGpuTab";
import NodeHealthTab from "../../components/cardHealth/NodeHealthTab";

export type NodeDetailTab = "basic" | "accelerators" | "health" | "doctor";

// NodeDetailDrawer shows one node in two tabs, an overview and its GPUs, plus
// a diagnosis tab once a diagnosis has run. It opens on the overview unless
// the caller names a tab, as the instance topology link does.
export default function NodeDetailDrawer({
  node,
  engines,
  initialTab,
  highlight,
  upgradePhase,
  onUpgrade,
  onClose,
}: {
  node: Node | null;
  engines?: EngineProfile[] | null;
  initialTab?: NodeDetailTab;
  highlight?: TopoHighlight;
  upgradePhase?: UpgradePhase;
  onUpgrade: (node: Node) => void;
  onClose: () => void;
}) {
  const { t } = useTranslation("nodes");
  const { t: tAI } = useTranslation("ai");
  // Fetch full node detail (carries topology) when the drawer opens.
  const [full, setFull] = useState<Node | null>(null);
  const [doctor, setDoctor] = useState<NodeDoctorReportData>();
  const [doctorLoading, setDoctorLoading] = useState(false);
  const doctorGeneration = useRef(0);
  const [renameLoading, setRenameLoading] = useState(false);
  const [runtimeLoading, setRuntimeLoading] = useState(false);
  const [renameOpen, setRenameOpen] = useState(false);
  const [renameValue, setRenameValue] = useState("");
  const [tab, setTab] = useState<NodeDetailTab>("basic");
  useAssistantSubjects(node ? nodeSubjects(full ?? node, tAI) : null);
  useEffect(() => {
    setFull(null);
    doctorGeneration.current++;
    setDoctorLoading(false);
    setDoctor(undefined);
    setTab(initialTab ?? "basic");
    if (!node) return;
    let alive = true;
    api
      .node(node.uuid)
      .then((n) => alive && setFull(n))
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [node, initialTab]);
  const shown = full ?? node;
  // An update answers with the node without its token; keep the one the
  // detail read carried.
  const merge = (updated: Node) =>
    setFull((current) => (current ? { ...current, ...updated, token: current.token } : updated));

  const runDoctor = async () => {
    if (!node) return;
    const generation = ++doctorGeneration.current;
    setDoctorLoading(true);
    setDoctor(undefined);
    try {
      const report = await api.nodeDoctor(node.uuid);
      if (generation !== doctorGeneration.current) return;
      setDoctor(report);
      setTab("doctor");
    } catch (e) {
      if (generation === doctorGeneration.current) message.error(t("message.doctorFailed", { detail: (e as Error).message }));
    } finally {
      if (generation === doctorGeneration.current) setDoctorLoading(false);
    }
  };

  const rename = async () => {
    if (!node) return;
    setRenameLoading(true);
    try {
      merge(await api.renameNode(node.uuid, renameValue.trim()));
      message.success(t("rename.updated"));
      setRenameOpen(false);
    } catch (e) {
      message.error(t("rename.failed", { detail: (e as Error).message }));
    } finally {
      setRenameLoading(false);
    }
  };

  const switchRuntime = async (value: string) => {
    if (!node || !["auto", "docker", "nerdctl", "udocker"].includes(value)) return;
    setRuntimeLoading(true);
    try {
      merge(await api.updateNodeRuntime(node.uuid, value as "auto" | "docker" | "nerdctl" | "udocker"));
      message.success(t("runtime.updated"));
    } catch (e) {
      message.error(t("runtime.failed", { detail: (e as Error).message }));
    } finally {
      setRuntimeLoading(false);
    }
  };

  return (
    <DetailDrawer
      className="node-detail-drawer"
      open={!!node}
      onClose={onClose}
      // The name leads; the hostname follows when the node has a name of its own.
      title={node ? shown?.name || node.hostname : t("detail.title")}
      subtitle={node && shown?.name && shown.name !== node.hostname ? node.hostname : undefined}
      extra={
        node && (
          <Space wrap>
            <NodeReconnectButton node={shown ?? node} onUpdated={setFull} />
            <Button size="small" aria-label={doctor ? t("doctorChecks.rerun") : t("action.doctor")} loading={doctorLoading} onClick={runDoctor}>
              {doctor ? t("doctorChecks.rerun") : t("action.doctor")}
            </Button>
            <AIAssistant task="node.health" subject={{ kind: "node", uuid: node.uuid, label: nodeDisplayName(node) }} buttonSize="small" />
            <AgentUpgradeButton online={node.state === "online"} phase={upgradePhase} onUpgrade={() => onUpgrade(node)} />
          </Space>
        )
      }
      banner={shown && <NodeRecoveryStatus node={shown} onUpdated={setFull} />}
      activeTab={tab}
      onTabChange={(key) => setTab(key as NodeDetailTab)}
      tabs={
        shown
          ? [
              {
                key: "basic",
                label: t("detail.tab.basic"),
                children: (
                  <NodeBasicTab
                    node={shown}
                    engines={engines}
                    upgradePhase={upgradePhase}
                    runtimeLoading={runtimeLoading}
                    onRename={() => {
                      setRenameValue(shown.name || "");
                      setRenameOpen(true);
                    }}
                    onRuntime={switchRuntime}
                    onUpdated={setFull}
                  />
                ),
              },
              {
                key: "accelerators",
                label: t("detail.tab.accelerators"),
                count: shown.gpus.length,
                countTitle: t("detail.tab.count", { count: shown.gpus.length }),
                children: <NodeGpuTab node={shown} topology={full?.topology} highlight={highlight} onUpdated={merge} />,
              },
              ...(shown.gpus.length
                ? [{ key: "health", label: t("cardHealth.tab"), children: <NodeHealthTab nodeUUID={shown.uuid} /> }]
                : []),
              ...(doctor ? [{ key: "doctor", label: t("detail.tab.doctor"), children: <NodeDoctorReport report={doctor} /> }] : []),
            ]
          : undefined
      }
    >
      <Modal
        title={t("rename.title")}
        open={renameOpen}
        onCancel={() => setRenameOpen(false)}
        onOk={rename}
        confirmLoading={renameLoading}
        okText={t("action.save")}
        cancelText={t("action.cancel")}
      >
        <Input
          value={renameValue}
          maxLength={255}
          onChange={(e) => setRenameValue(e.target.value)}
          placeholder={t("rename.placeholder")}
        />
      </Modal>
    </DetailDrawer>
  );
}
