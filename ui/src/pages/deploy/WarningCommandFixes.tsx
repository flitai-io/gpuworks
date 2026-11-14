import { useTranslation } from "react-i18next";

import type { ProductWarning } from "../../api";
import CommandFixes, { type CommandFix } from "../../components/CommandFixes";

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
 * WarningCommandFixes lists, under an evaluate warning in the summary's one
 * warning list, the fix commands the server put in its params (a node that
 * hangs UCX), exactly as received and with a copy button for each. A warning
 * without fixes renders nothing.
 */
export default function WarningCommandFixes({ warning }: { warning: ProductWarning }) {
  const { t } = useTranslation("deploy");
  const fixes = UCX_NETLINK_HANG.includes(warning.code) ? fixesOf(warning.params) : [];
  if (fixes.length === 0) return null;
  return (
    <div className="flex flex-col gap-2" data-testid="pd-ucx-netlink-fixes">
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
