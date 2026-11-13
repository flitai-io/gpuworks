import { Button, Dropdown, Space, Tooltip } from "antd";
import { Badge } from "../../components/ui/badge";
import { Tooltip as ZeronTooltip } from "../../components/ui/tooltip";
import { CodeOutlined, DeleteOutlined, MoreOutlined } from "@ant-design/icons";
import type { TFunction } from "i18next";
import type { GPU, Node, NodeSourceOption } from "../../api";
import { NODE_SOURCE_MANUAL } from "../../nodeSources";
import type { ResourceColumn } from "../../components/resourceTable/ResourceTable";
import CellTags from "../../components/resourceTable/CellTags";
import { NodeSchedulingTag } from "../../components/NodeSchedulingModal";
import { NodeConnectionBadge } from "../../components/NodeConnectionBadge";
import { NODE_CONNECTION_STATES } from "../../nodes/connectionState";
import { systemLabelName, systemLabelValue } from "../../components/SystemLabelTags";
import NodeSourceCell from "./NodeSourceCell";
import { formatTime } from "../../i18n/datetime";
import type { UpgradePhase } from "../../agentUpgrade";

export const NODE_TABLE_ID = "nodes";

export function summarizeGPUs(gpus: GPU[]): string[] {
  const counts = new Map<string, number>();
  for (const g of gpus) {
    const name = g.name.replace(/^NVIDIA\s+/i, "");
    counts.set(name, (counts.get(name) ?? 0) + 1);
  }
  return [...counts.entries()].map(([name, n]) => `${n}× ${name}`);
}

export function formatGPUDriver(node: Node): string {
  const driver = node.gpu_driver || node.nvidia_driver;
  if (!driver) return "—";
  return node.gpu_vendor ? `${node.gpu_vendor} · ${driver}` : driver;
}

export function formatFramework(node: Node): string {
  if (!node.framework) return "—";
  return node.framework_version
    ? `${node.framework} · ${node.framework_version}`
    : node.framework;
}

export interface NodeColumnContext {
  t: TFunction<"nodes">;
  /** The state filter the list was asked for. */
  states: string[];
  showCheckReason?: boolean;
  /** The source filter the list was asked for, and every source the fleet has to choose from. */
  sources: string[];
  sourceOptions: NodeSourceOption[];
  upgradeErrors?: Record<string, string> | null;
  upgradePhase: (uuid: string) => UpgradePhase | undefined;
  onOpen: (node: Node) => void;
  onEditLabels: (node: Node) => void;
  onScheduling: (node: Node) => void;
  onDelete: (node: Node) => void;
}

/**
 * The node list's columns. Keys are the server's sort keys where the column
 * sorts, so a header click asks GET /nodes for that order.
 */
export function nodeColumns({ t, states, showCheckReason, sources, sourceOptions, upgradeErrors, upgradePhase, onOpen, onEditLabels, onScheduling, onDelete }: NodeColumnContext): ResourceColumn<Node>[] {
  return [
    {
      key: "name",
      role: "name",
      title: t("column.name"),
      width: 220,
      serverSort: true,
      render: (_: unknown, n: Node) => (
        <>
          <Button type="link" onClick={() => onOpen(n)}>{n.name || n.hostname || n.uuid.slice(0, 12)}</Button>
          <span className="resource-table-sub">{n.hostname || t("waitingFirstConnection")}</span>
          {showCheckReason && n.agent_eligibility && <span className="resource-table-sub">{t(`batchUpgrade.reason.${n.agent_eligibility}`)}</span>}
        </>
      ),
    },
    {
      key: "state",
      title: t("column.status"),
      width: 130,
      serverSort: true,
      filters: NODE_CONNECTION_STATES.map((value) => ({ value, text: t(`state.${value}`) })),
      filteredValue: states,
      // The stale-reading warning is not here: it is about the cards, not
      // the node, and on a fleet list it crowds the column that answers
      // "is this node reachable". The node's own page carries it, beside
      // the cards it is about.
      render: (_: unknown, node: Node) => (
        <Space size={4}>
          <NodeConnectionBadge node={node} />
          <NodeSchedulingTag node={node} />
        </Space>
      ),
    },
    {
      key: "gpu_count",
      title: "GPU",
      label: "GPU",
      width: 200,
      serverSort: true,
      render: (_: unknown, n: Node) => (
        <CellTags tags={summarizeGPUs(n.gpus).map((g) => ({ key: g, text: g, color: "blue" }))} empty={t("noGPU")} />
      ),
    },
    {
      key: "agent_version",
      title: t("detail.agentVersion"),
      label: t("detail.agentVersion"),
      width: 180,
      serverSort: true,
      render: (_: unknown, n: Node) => {
        const offline = n.agent_eligibility === "offline" || n.state === "offline";
        const inBatch = n.agent_eligibility === "busy" || upgradePhase(n.uuid) === "waiting";
        return (
          <div className="flex flex-wrap items-center gap-1">
            {n.agent_version ? <span>{n.agent_version}</span> : <Badge size="sm" status="neutral">{t("fleet.notReported")}</Badge>}
            {offline && n.agent_version && <Badge size="sm" status="neutral">{t("fleet.beforeOffline")}</Badge>}
            {!offline && n.agent_version === "dev" && <Badge size="sm" status="neutral">{t("batchUpgrade.eligibility.unknown_version")}</Badge>}
            {inBatch ? (
              <Badge size="sm" status="info">{t("fleet.inBatch")}</Badge>
            ) : !offline && (upgradeErrors?.[n.uuid] ? (
              <ZeronTooltip content={upgradeErrors[n.uuid]}><Badge size="sm" status="danger" tabIndex={0}>{t("upgrade.failed")}</Badge></ZeronTooltip>
            ) : n.agent_eligibility === "eligible" && <Badge size="sm" status="warning">{t(`fleet.status.${n.agent_version_change ?? "align"}`)}</Badge>)}
          </div>
        );
      },
    },
    {
      // The external platform the node joined through, linked to its row there. Its filter is the server's:
      // the choices are every source the fleet has, and one the address names stays listed even with no node.
      key: "source",
      title: t("column.source"),
      width: 190,
      filters: [
        ...sourceOptions.map((option) => ({ value: option.value, text: sourceOptionText(t, option) })),
        ...sources.filter((value) => !sourceOptions.some((option) => option.value === value)).map((value) => ({ value, text: sourceOptionText(t, { value, nodes: 0 }) })),
      ],
      filterSearch: true,
      filteredValue: sources,
      render: (_: unknown, node: Node) => <NodeSourceCell source={node.source} unknown={node.source_unknown} />,
    },
    {
      key: "system_labels",
      title: t("systemLabels.title"),
      width: 240,
      render: (_: unknown, node: Node) => (
        <CellTags
          tags={(node.system_labels ?? []).map((label) => ({
            key: `${label.key}=${label.value}`,
            text: `${systemLabelName(label.key, t)}: ${systemLabelValue(label, t)}`,
            tooltip: `${label.key}=${label.value}`,
          }))}
          limit={1}
        />
      ),
    },
    {
      key: "labels",
      title: t("labels.title"),
      width: 200,
      render: (_: unknown, node: Node) => (
        <CellTags tags={Object.entries(node.labels ?? {}).map(([key, value]) => ({ key, text: `${key}=${value}` }))} />
      ),
    },
    { key: "cpu_cores", title: "CPU", label: "CPU", width: 100, serverSort: true, render: (_: unknown, n: Node) => t("cpuCores", { count: n.cpu_cores }) },
    { key: "mem_total", title: t("column.memory"), width: 110, serverSort: true, render: (_: unknown, n: Node) => `${n.mem_total_gib.toFixed(0)} GiB` },
    { key: "driver", title: t("column.driver"), width: 150, render: (_: unknown, n: Node) => formatGPUDriver(n) },
    { key: "framework", title: t("column.framework"), width: 150, render: (_: unknown, n: Node) => formatFramework(n) },
    { key: "created_at", title: t("column.createdAt"), width: 180, serverSort: true, defaultHidden: true, render: (_: unknown, n: Node) => n.created_at ? formatTime(n.created_at) : "—" },
    {
      key: "action",
      role: "actions",
      width: 110,
      render: (_: unknown, n: Node) => (
        <Space size={2}>
          <Tooltip title={t(n.state === "online" ? "terminal.openHost" : "terminal.offline")}>
            <Button
              type="text"
              size="small"
              icon={<CodeOutlined />}
              disabled={n.state !== "online"}
              aria-label={t("action.terminal")}
              // Opened in its own tab: a session lives up to the idle timeout, so
              // it must survive navigation here, and operators read the terminal
              // beside the GPU and log pages while troubleshooting.
              onClick={() => window.open(`/nodes/${encodeURIComponent(n.uuid)}/terminal`, "_blank", "noopener")}
            />
          </Tooltip>
          <Dropdown
            trigger={["click"]}
            menu={{
              items: [
                { key: "details", label: t("action.details") },
                { key: "labels", label: t("labels.edit") },
                { key: "scheduling", label: t("scheduling.menu") },
                { type: "divider" },
                { key: "delete", icon: <DeleteOutlined />, danger: true, label: t("action.delete") },
              ],
              onClick: ({ key }) => {
                if (key === "details") onOpen(n);
                if (key === "labels") onEditLabels(n);
                if (key === "scheduling") onScheduling(n);
                if (key === "delete") onDelete(n);
              },
            }}
          >
            <Tooltip title={t("action.more")}>
              <Button type="text" size="small" icon={<MoreOutlined />} aria-label={t("action.more")} />
            </Tooltip>
          </Dropdown>
        </Space>
      ),
    },
  ];
}

/** How a source reads in the filter: an account by its name, a cluster by its name and the account it was connected from, the rest as added by hand. */
export function sourceOptionText(t: TFunction<"nodes">, option: NodeSourceOption): string {
  if (option.value === NODE_SOURCE_MANUAL) return t("source.manual");
  if (option.cluster_name) return option.provider_name ? t("source.clusterOf", { name: option.cluster_name, provider: option.provider_name }) : t("source.cluster", { name: option.cluster_name });
  return option.provider_name || t("source.selected");
}
