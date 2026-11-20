import type { ReactNode } from "react";
import { Alert, Tag, Typography } from "antd";
import { useTranslation } from "react-i18next";
import type { EvaluateResponse, Node } from "../../api";
import { nodeDisplayName } from "../../nodeDisplay";

// AutoGroupPreview lists the nodes evaluate would put an automatic multi-node
// group on now, rank by rank. It is a preview: the create and every later
// start pick again, preferring the nodes the group ran on last.
export default function AutoGroupPreview({
  result,
  loading,
  ready,
  knownNodes,
}: {
  result: EvaluateResponse | null;
  loading: boolean;
  ready: boolean;
  knownNodes?: Map<string, Node>;
}) {
  const { t } = useTranslation("deploy");
  const ranks = result?.rank_commands ?? [];
  const placementError = result?.errors?.find((error) => error.field === "placement_mode")?.code;
  const placementBlocked = result?.preflight?.checks?.some((check) => check.id === "automatic_placement" && check.status === "fail");
  let body: ReactNode;
  if (loading) {
    body = <Typography.Text type="secondary">{t("workbench.node.evaluating")}</Typography.Text>;
  } else if (ranks.length > 0) {
    body = (
      <div className="deploy-multi-node-list" data-testid="auto-group-preview">
        {ranks.map((rank, index) => {
          const node = knownNodes?.get(rank.node_uuid);
          return (
            <div className="deploy-node-item" key={`${rank.node_uuid}-${index}`}>
              <Tag color={rank.role === "follower" ? "default" : "blue"}>
                {rank.role === "follower" ? "Worker" : "Master"}
              </Tag>
              <strong>{rank.node_name || (node ? nodeDisplayName(node) : rank.node_uuid.slice(0, 8))}</strong>
              <span>{result?.parallelism?.gpus_per_node ?? node?.gpu_count ?? 0} GPU</span>
            </div>
          );
        })}
      </div>
    );
  } else {
    let message = t("workbench.node.autoGroupUnavailable");
    if (!ready) {
      message = t("workbench.node.chooseModelFirst");
    } else if (placementError === "AUTO_PLACEMENT_TP_REQUIRED") {
      message = t("workbench.node.autoGroupRequiresTP");
    } else if (placementError === "UNIT_PLACEMENT_NONE") {
      message = t("workbench.node.autoGroupNoPreview");
    }
    body = <Alert type="info" showIcon message={message} />;
  }
  return (
    <>
      <div className="deploy-node-card-head">
        <strong>{t("workbench.node.previewTitle")}</strong>
        {result?.preflight && !loading && !placementBlocked && (
          <Tag color={result.preflight.ok ? "success" : "error"}>
            {result.preflight.ok ? t("workbench.node.preflightPassed") : t("workbench.node.preflightFailed")}
          </Tag>
        )}
      </div>
      {body}
    </>
  );
}
