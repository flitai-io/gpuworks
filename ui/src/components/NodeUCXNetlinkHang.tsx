import { useTranslation } from "react-i18next";

import type { Node } from "../api";
import { Notice } from "../ui-kit/Notice";
import { Badge } from "./ui/badge";
import { Tooltip } from "./ui/tooltip";
import CommandFixes from "./CommandFixes";

type HangNode = Pick<Node, "ucx_netlink_hang">;

/**
 * UCXNetlinkHangTag marks a node whose kernel hangs UCX, so a PD engine that
 * moves its KV with NIXL never finishes starting there. The server says so
 * (Node.ucx_netlink_hang); the tag only words it.
 */
export function UCXNetlinkHangTag({ node }: { node: HangNode }) {
  const { t } = useTranslation("nodes");
  if (!node.ucx_netlink_hang) return null;
  const detail = t("ucxNetlinkHang.summary");
  return (
    <Tooltip content={detail}>
      <Badge status="warning" tabIndex={0} aria-label={`${t("ucxNetlinkHang.tag")}: ${detail}`}>
        {t("ucxNetlinkHang.tag")}
      </Badge>
    </Tooltip>
  );
}

/**
 * UCXNetlinkHangNotice explains the hang and lists the commands the server
 * generated to end it, shown and copied exactly as received.
 */
export default function UCXNetlinkHangNotice({ node }: { node: HangNode }) {
  const { t } = useTranslation("nodes");
  const hang = node.ucx_netlink_hang;
  if (!hang) return null;
  return (
    <div className="flex flex-col gap-2">
      <Notice tone="warning" role="alert" title={t("ucxNetlinkHang.tag")}>
        <p>{t("ucxNetlinkHang.summary")}</p>
        {hang.kernel && <p>{t("ucxNetlinkHang.kernel", { kernel: hang.kernel })}</p>}
        <p>{t("ucxNetlinkHang.impact")}</p>
      </Notice>
      {hang.fixes.length > 0 && (
        <div className="flex flex-col gap-2">
          <strong>{t("ucxNetlinkHang.fixesTitle")}</strong>
          <CommandFixes
            fixes={hang.fixes}
            describe={(code) => t(`ucxNetlinkHang.fix.${code}`, { defaultValue: code })}
            copyLabel={t("ucxNetlinkHang.copy")}
            copiedLabel={t("ucxNetlinkHang.copied")}
            manualCopyTitle={t("ucxNetlinkHang.manualCopyTitle")}
          />
        </div>
      )}
    </div>
  );
}
