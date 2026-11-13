import { Badge, Space, Tooltip } from "antd";
import { ExclamationCircleFilled } from "@ant-design/icons";
import { useTranslation } from "react-i18next";

import type { Node } from "../api";
import { nodeConnection } from "../nodes/connectionState";
import { colors } from "../theme/tokens";
import { formatTime } from "../i18n/datetime";

type ConnectionNode = Pick<Node, "state" | "recovery_phase" | "last_recovery_error" | "last_seen_at" | "last_probe_at" | "recovery_attempts">;

/**
 * A node's connection, shown the same way wherever it appears. Recovery is
 * a warning mark on the state rather than a status beside it: an operator
 * reads "connecting" and hovers only if they care why.
 */
export function NodeConnectionBadge({ node }: { node: ConnectionNode }) {
  const { t } = useTranslation("nodes");
  const connection = nodeConnection(node);
  const note = connection.noteKey ? t(connection.noteKey) : "";
  const detail = [note, node.last_recovery_error].filter(Boolean).join("\n");
  return (
    <Tooltip trigger={["hover", "focus"]} title={<ConnectionHistory node={node} detail={detail} />}>
      <span tabIndex={0} aria-label={t(connection.labelKey) + " · " + (detail || t("connectionDiagnostics.historyTitle"))}>
        <Space size={4}>
          <Badge status={connection.badge} text={t(connection.labelKey)} />
          {detail && (
            <ExclamationCircleFilled
              role="img"
              aria-label={detail}
              style={{ color: colors.warning, cursor: "help" }}
            />
          )}
        </Space>
      </span>
    </Tooltip>
  );
}

// Tooltip mounts its content on demand; unopened rows do not format dates or
// build history DOM, and the data already arrived with the node list.
function ConnectionHistory({ node, detail }: { node: ConnectionNode; detail: string }) {
  const { t } = useTranslation("nodes");
  const unknown = t("connectionDiagnostics.unknown");
  return (
    <div>
      {detail && <div style={{ whiteSpace: "pre-line" }}>{detail}</div>}
      <div>{t("connectionDiagnostics.lastSeen")}: {formatTime(node.last_seen_at, unknown)}</div>
      <div>{t("connectionDiagnostics.lastRecovery")}: {formatTime(node.last_probe_at, unknown)}</div>
      <div>{t("connectionDiagnostics.attempts", { count: node.recovery_attempts ?? 0 })}</div>
    </div>
  );
}
