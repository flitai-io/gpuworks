import { useTranslation } from "react-i18next";
import type { ProviderInstance } from "../../api";
import CopyButton from "../../components/CopyButton";
import { Badge } from "../../components/ui/badge";
import { Button } from "../../components/ui/button";
import { TableCell, TableRow } from "../../components/ui/table";
import { localizedText } from "../../i18n/localizedText";
import { isCommandChannel } from "../../providerConnections";
import { ConfirmPopover } from "../../ui-kit/ConfirmPopover";

/** Which of the optional columns the account's instance table has. */
export interface InstanceColumns {
  price: boolean;
  region: boolean;
  type: boolean;
}

// The region has its own column where the account's detail is wide enough, and sits under the instance's name where
// it is not (the detail is the size container).
export const WIDE_ONLY = "hidden @[860px]:table-cell";
const NARROW_ONLY = "block @[860px]:hidden";

const POWER_STATUS: Record<string, "success" | "neutral" | "warning"> = { running: "success", stopped: "neutral", unknown: "warning" };

/** How an instance is reached: each connection the vendor gives, with the ssh command to copy. */
function Connections({ inst }: { inst: ProviderInstance }) {
  const { t, i18n } = useTranslation("providers");
  const language = i18n.resolvedLanguage || i18n.language;
  const connections = inst.ssh_connections?.length
    ? inst.ssh_connections
    : inst.public_ip && inst.ssh_port
      ? [{ id: "legacy", type: "direct", label: "SSH", host: inst.public_ip, port: inst.ssh_port, user: inst.ssh_user || "root", install_supported: true }]
      : [];
  if (!connections.length) return <span className="text-fg-muted">{t("instances.notReturned")}</span>;
  return (
    <div className="flex flex-col gap-1">
      {connections.map((connection) => {
        if (isCommandChannel(connection)) {
          return (
            <span key={connection.id} className="flex items-center gap-1.5">
              <Badge size="sm" color="blue">{localizedText(connection.label, language) || connection.type}</Badge>
              <span className="text-label text-fg-muted">{t("instances.connection.noSSH")}</span>
            </span>
          );
        }
        const user = connection.user || "root";
        const command = `ssh ${user}@${connection.host} -p ${connection.port}`;
        return (
          <span key={connection.id} className="flex items-center gap-1.5">
            <Badge size="sm">{localizedText(connection.label, language) || t(connection.type === "direct" ? "instances.connection.direct" : "instances.connection.proxy")}</Badge>
            <span className="max-w-[110px] truncate @[860px]:max-w-[200px]" title={command}>{user}@{connection.host}:{connection.port}</span>
            <CopyButton size="small" label={t("instances.connection.copy")} text={command} />
            {!connection.install_supported && <span className="text-label text-fg-muted">{t("instances.connection.terminalOnly")}</span>}
          </span>
        );
      })}
    </div>
  );
}

/**
 * One instance of an account: what it is, how it is reached, and what can be
 * done with it. The server says what the instance allows (`actions`); nothing
 * here decides it.
 */
export default function InstanceRow({ inst, index, columns, indent, powering, onPower, onInstall }: {
  inst: ProviderInstance;
  index: number;
  columns: InstanceColumns;
  /** The row sits under a group header: its name starts under the group's. */
  indent: boolean;
  /** The power action in flight on this instance, if any. */
  powering: "on" | "off" | null;
  onPower: (action: "on" | "off") => Promise<void>;
  onInstall: () => void;
}) {
  const { t } = useTranslation("providers");
  const actions = inst.actions ?? [];
  const power = actions.includes("power_off") ? "off" : actions.includes("power_on") ? "on" : null;
  const sub = inst.name ? inst.external_id : "";
  const region = columns.region ? inst.region : "";
  const type = inst.kind === "container" ? t("instances.type.container") : inst.kind === "vm" ? t("instances.type.vm") : inst.kind || "—";
  return (
    <TableRow index={index}>
      <TableCell className={indent ? "ps-11" : undefined}>
        <span className="block max-w-[260px] truncate" title={inst.name || inst.external_id}>{inst.name || inst.external_id || "—"}</span>
        {/* The vendor's id under the name it goes by; where the row is too narrow for a region column, the region too. */}
        {(sub || region) && (
          <span className={`${sub ? "block" : NARROW_ONLY} max-w-[260px] truncate text-label text-fg-subtle`} title={sub || undefined}>
            {sub}{region && <span className={sub ? "@[860px]:hidden" : undefined}>{sub ? " · " : ""}{region}</span>}
          </span>
        )}
      </TableCell>
      <TableCell className="whitespace-nowrap">
        {inst.gpu_model || <span className="text-fg-muted">{t("instances.notReturned")}</span>}
        {inst.gpu_count > 0 && ` ×${inst.gpu_count}`}
      </TableCell>
      <TableCell className="whitespace-nowrap">
        <Badge status={POWER_STATUS[inst.power] ?? "warning"}>{POWER_STATUS[inst.power] ? t(`instances.power.${inst.power}`) : inst.power || t("instances.power.unknown")}</Badge>
      </TableCell>
      {columns.price && <TableCell className="whitespace-nowrap">{t("instances.pricePerHour", { price: inst.price_per_hr.toFixed(2) })}</TableCell>}
      {columns.region && <TableCell className={`whitespace-nowrap ${WIDE_ONLY}`}>{inst.region || "—"}</TableCell>}
      {columns.type && <TableCell className="whitespace-nowrap">{type}</TableCell>}
      <TableCell className="whitespace-nowrap"><Connections inst={inst} /></TableCell>
      <TableCell className="whitespace-nowrap">
        <div className="flex items-center gap-2">
          {power === "off" && (
            // Powering off can be undone, so it is not drawn as a destructive action; it still asks first.
            <ConfirmPopover
              title={t("instances.powerOffConfirm")}
              confirmText={t("instances.powerOff")}
              cancelText={t("instances.cancel")}
              onConfirm={() => onPower("off")}
              trigger={<Button variant="link" size="sm" loading={powering === "off"}>{t("instances.powerOff")}</Button>}
            />
          )}
          {power === "on" && <Button variant="link" size="sm" loading={powering === "on"} onClick={() => void onPower("on")}>{t("instances.powerOn")}</Button>}
          {actions.includes("install")
            ? <Button variant="link" size="sm" onClick={onInstall}>{t("instances.installAgent")}</Button>
            : <Badge status="success">{t("instances.onboarded")}</Badge>}
        </div>
      </TableCell>
    </TableRow>
  );
}
