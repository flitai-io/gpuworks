import { useTranslation } from "react-i18next";

import type { Node } from "../api";
import { Notice } from "../ui-kit/Notice";
import { Badge } from "./ui/badge";
import { Tooltip } from "./ui/tooltip";
import CopyButton from "./CopyButton";

type IsolationNode = Pick<Node, "gpu_isolation">;

/**
 * GPUIsolationTag marks a node whose instances see every GPU device of the
 * node and are held to their own card by CUDA_VISIBLE_DEVICES alone. The
 * server says so (Node.gpu_isolation); the tag only words it.
 */
export function GPUIsolationTag({ node }: { node: IsolationNode }) {
  const { t } = useTranslation("nodes");
  if (!node.gpu_isolation) return null;
  const detail = t("gpuIsolation.summary");
  return (
    <Tooltip content={detail}>
      <Badge status="warning" tabIndex={0} aria-label={`${t("gpuIsolation.tag")}: ${detail}`}>
        {t("gpuIsolation.tag")}
      </Badge>
    </Tooltip>
  );
}

/**
 * GPUIsolationNotice explains why the node is soft-isolated and lists the
 * ways back to strict isolation, each with the commands the server generated.
 * The commands are shown and copied exactly as received.
 */
export default function GPUIsolationNotice({ node }: { node: IsolationNode }) {
  const { t } = useTranslation("nodes");
  const isolation = node.gpu_isolation;
  if (!isolation) return null;
  const reason = isolation.reason
    ? t(`gpuIsolation.reason.${isolation.reason}`, { cards: isolation.cards ?? 0, defaultValue: isolation.reason })
    : "";
  return (
    <div className="flex flex-col gap-2">
      <Notice tone="warning" role="alert" title={t("gpuIsolation.tag")}>
        <p>{t("gpuIsolation.summary")}</p>
        {reason && <p>{reason}</p>}
      </Notice>
      {isolation.fixes.length > 0 && (
        <div className="flex flex-col gap-2">
          <strong>{t("gpuIsolation.fixesTitle")}</strong>
          {isolation.fixes.map((fix) => {
            const commands = fix.commands.join("\n");
            return (
              <div key={fix.code} data-fix={fix.code}>
                <p className="mb-1">{t(`gpuIsolation.fix.${fix.code}`, { defaultValue: fix.code })}</p>
                <div className="flex items-start gap-1">
                  <pre tabIndex={0} aria-label={fix.code} className="m-0 flex-1 overflow-x-auto px-3 py-2">
                    {commands}
                  </pre>
                  <CopyButton
                    text={commands}
                    label={t("gpuIsolation.copy")}
                    success={t("gpuIsolation.copied")}
                    manualTitle={t("gpuIsolation.manualCopyTitle")}
                    size="small"
                  />
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
