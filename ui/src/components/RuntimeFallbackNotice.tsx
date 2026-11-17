import { CopyOutlined, ExclamationCircleFilled, InfoCircleFilled } from "@ant-design/icons";
import { Button, Space, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { Node, RuntimeSocketDenial } from "../api";
import { useCopy } from "./CopyButton";
import { runtimePluginInstallCommand } from "./RuntimePluginStatus";

type RuntimeFallbackNode = Pick<Node, "gpu_vendor" | "runtime_fallback_from" | "runtime_fallback_reason" | "runtime_fallback_plugin" | "runtime_fallback_socket">;

/**
 * The reasons the agent passes a runtime over for runveil
 * (Node.runtime_fallback_reason). daemon_not_ready is a daemon that did not
 * answer while the agent started; socket_permission_denied is a socket the
 * agent's account may not open, which needs the agent restarted once fixed;
 * gpu_ready is the passed-over runtime being able to serve again; the agent
 * switches back once the node is idle.
 */
const fallbackCodes = ["gpu_plugin_missing", "gpu_runtime_unconfigured", "daemon_not_ready", "socket_permission_denied", "gpu_ready"] as const;

function known(code: string): code is (typeof fallbackCodes)[number] {
  return (fallbackCodes as readonly string[]).includes(code);
}

/** The command that sets up the passed-over runtime, or "" when there is none to offer. */
export function runtimeFallbackCommand(node: RuntimeFallbackNode): string {
  const from = node.runtime_fallback_from ?? "";
  switch (node.runtime_fallback_reason) {
    case "gpu_plugin_missing":
      return runtimePluginInstallCommand({ gpu_vendor: node.gpu_vendor, runtime_driver: from });
    case "socket_permission_denied":
      // The server builds it from what the agent found (account, group, how it restarts).
      return node.runtime_fallback_socket?.command ?? "";
    case "gpu_runtime_unconfigured":
      if (node.gpu_vendor?.trim().toLowerCase() === "nvidia" && from === "docker") {
        return "sudo nvidia-ctk runtime configure --runtime=docker\nsudo systemctl restart docker";
      }
      return "";
    default:
      return "";
  }
}

/**
 * Why the node runs on runveil although it has Docker (or nerdctl): the
 * agent passed that runtime over because it cannot give containers the
 * cards. Shown beside the runtime selector, with the command that fixes it.
 */
export default function RuntimeFallbackNotice({ node }: { node: RuntimeFallbackNode }) {
  const { t } = useTranslation("nodes");
  const [copy, copyHolder] = useCopy();
  const from = node.runtime_fallback_from ?? "";
  const code = node.runtime_fallback_reason ?? "";
  if (!from || !code) return null;
  const socket = code === "socket_permission_denied" ? node.runtime_fallback_socket : undefined;
  const text = socket
    ? socketText(t, from, socket)
    : known(code) && code !== "socket_permission_denied"
      ? t(`runtime.fallback.${code}`, { from, plugin: node.runtime_fallback_plugin ?? "" })
      : `${from}: ${code}`;
  const command = runtimeFallbackCommand(node);
  const ready = code === "gpu_ready";
  const install = code === "gpu_plugin_missing";
  const copyCommand = () =>
    copy(command, socket
      ? { success: t("runtime.socketCommandCopied"), manualTitle: t("runtime.socketCommandTitle") }
      : {
          success: t(install ? "runtime.pluginInstallCopied" : "runtime.configureCopied"),
          manualTitle: t("runtime.manualCopyTitle"),
        });

  return (
    <>
      <Space size={4} align="start" role={ready ? "status" : "alert"}>
        {ready ? <InfoCircleFilled style={{ color: "#1677ff" }} /> : <ExclamationCircleFilled style={{ color: "#fa8c16" }} />}
        <Typography.Text type={ready ? undefined : "warning"} style={ready ? undefined : { color: "#fa8c16" }}>
          {text}
        </Typography.Text>
      </Space>
      {command && (
        <div>
          <Button size="small" icon={<CopyOutlined />} onClick={() => void copyCommand()}>
            {t(socket ? "runtime.copySocketCommand" : install ? "runtime.copyInstallCommand" : "runtime.copyConfigureCommand")}
          </Button>
        </div>
      )}
      {copyHolder}
    </>
  );
}

/** The notice's opening for each cause the server reads from the agent's facts. */
const socketCauseText = {
  not_in_group: "runtime.fallback.socket_permission_denied",
  restart_pending: "runtime.fallback.socket_restart_pending",
  group_held: "runtime.fallback.socket_group_held",
  root_group: "runtime.fallback.socket_root_group",
  unknown: "runtime.fallback.socket_unknown",
} as const;

/** Causes whose way out is restarting the agent. */
const socketRestartCauses = new Set(["not_in_group", "restart_pending", "root_group"]);

/**
 * The notice for a socket the agent's account may not open, from what the
 * server sent: why (its cause), a directory-service group of the same name
 * when there is one, how to restart the agent when the server's command
 * could not include it (no supervisor known), what a restart does to the
 * node's instances, and how to stop the notice when the account is not meant
 * to use the runtime.
 */
function socketText(t: TFunction<"nodes">, from: string, socket: RuntimeSocketDenial): string {
  const account = socket.account || t("runtime.fallback.socketAccountUnknown");
  const group = socket.group
    ? t("runtime.fallback.socketGroup", { group: socket.group, gid: socket.gid })
    : t("runtime.fallback.socketGid", { gid: socket.gid });
  const key = socketCauseText[socket.cause as keyof typeof socketCauseText];
  if (!key) return `${from}: socket_permission_denied (${socket.cause})`;
  const parts = [t(key, { from, account, group, socket: socket.socket })];
  if (socket.same_name_gid && socket.group && (socket.cause === "not_in_group" || socket.cause === "restart_pending")) {
    parts.push(t("runtime.fallback.socketSameName", { account, name: socket.group, sameGid: socket.same_name_gid, gid: socket.gid }));
  }
  if (socketRestartCauses.has(socket.cause)) {
    if (!socket.supervisor) parts.push(t("runtime.fallback.socketRestartHow", { account }));
    parts.push(t("runtime.fallback.socketRestartEffect", { from }));
  }
  parts.push(t("runtime.fallback.socketOptOut", { account, from }));
  return parts.join(" ");
}
