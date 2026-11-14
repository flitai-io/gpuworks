import { useTranslation } from "react-i18next";

import type { EvaluateResponse } from "../../api";
import CommandFixes, { type CommandFix } from "../../components/CommandFixes";

// The error evaluate attributes to placement.pd when a node of the placement
// hangs UCX (NIXL) and the PD mode moves its KV over Ethernet.
const UCX_NETLINK_HANG = "PD_UCX_NETLINK_HANG";

/** fixesOf reads the server's fixes out of the error's params, ignoring a malformed entry. */
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
 * PdUCXNetlinkFixes shows the commands the server generated to end a node's
 * UCX hang, beside the PD section's refusal text, exactly as received and with
 * a copy button for each.
 */
export default function PdUCXNetlinkFixes({ result }: { result: EvaluateResponse | null }) {
  const { t } = useTranslation("deploy");
  const error = result?.errors?.find((item) => item.code === UCX_NETLINK_HANG);
  const fixes = fixesOf(error?.params);
  if (!error || fixes.length === 0) return null;
  return (
    <div className="flex flex-col gap-2" data-testid="pd-ucx-netlink-fixes" style={{ marginBottom: 12 }}>
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
