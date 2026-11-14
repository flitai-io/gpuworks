import { Alert } from "antd";
import { useTranslation } from "react-i18next";

import type { EvaluateResponse } from "../../api";
import CommandFixes, { type CommandFix } from "../../components/CommandFixes";
import { warningText } from "./deployWarnings";

// The warnings evaluate returns when a node the operator placed a PD instance
// on hangs UCX (NIXL): over Ethernet the engine will likely never finish
// starting, over host RDMA it may. Neither stops the deployment.
const UCX_NETLINK_HANG = ["PD_UCX_NETLINK_HANG", "PD_UCX_NETLINK_HANG_RDMA"];

/** fixesOf reads the server's fixes out of the warning's params, ignoring a malformed entry. */
function fixesOf(params: Record<string, unknown> | undefined): CommandFix[] {
  const raw = params?.fixes;
  if (!Array.isArray(raw)) return [];
  return raw.flatMap((item): CommandFix[] => {
    const fix = item as { code?: unknown; commands?: unknown } | null;
    if (!fix || typeof fix.code !== "string" || !Array.isArray(fix.commands)) return [];
    return [{ code: fix.code, commands: fix.commands.filter((command): command is string => typeof command === "string") }];
  });
}

/**
 * PdUCXNetlinkFixes shows, in the PD section, the server's warning that a
 * chosen node hangs UCX and the commands it generated to end the hang, exactly
 * as received and with a copy button for each. The deployment can still be
 * created.
 */
export default function PdUCXNetlinkFixes({ result }: { result: EvaluateResponse | null }) {
  const { t } = useTranslation("deploy");
  const warning = result?.warnings?.find((item) => UCX_NETLINK_HANG.includes(item.code));
  const fixes = fixesOf(warning?.params);
  if (!warning || fixes.length === 0) return null;
  return (
    <div className="flex flex-col gap-2" data-testid="pd-ucx-netlink-fixes" style={{ marginBottom: 12 }}>
      <Alert type="warning" showIcon message={warningText(t, warning)} />
      <strong>{t("pd.ucxNetlinkHang.fixesTitle")}</strong>
      <CommandFixes
        fixes={fixes}
        describe={(code) => t(`pd.ucxNetlinkHang.fix.${code}`, { defaultValue: code })}
        copyLabel={t("pd.ucxNetlinkHang.copy")}
        copiedLabel={t("pd.ucxNetlinkHang.copied")}
        manualCopyTitle={t("pd.ucxNetlinkHang.manualCopyTitle")}
      />
    </div>
  );
}
