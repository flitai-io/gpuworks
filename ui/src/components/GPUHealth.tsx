import { InfoCircleOutlined } from "@ant-design/icons";
import { Button, Popover, Space, Tag, Tooltip, Typography } from "antd";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { GPU, GPUHealthField, GPUHealthGuide } from "../api";
import CopyButton from "./CopyButton";

/**
 * The facts of one card the health column reads, as both the node view
 * (GPU) and the GPU list (PhysicalGPU) carry them. `schedulable` is the
 * server's verdict on whether work may go on the card; the rest says why.
 */
export type GPUHealthFacts = Pick<
  GPU,
  "schedulable" | "foreign_processes" | "ownership" | "health_status" | "health_code" | "health_message" | "health_evidence" | "health_guide"
>;

const knownCodes = new Set([
  "driver_init_failed",
  "vendor_cli_unanswered",
  "inventory_identity_invalid",
  "inventory_incomplete",
  "telemetry_unavailable",
  "runtime_unusable",
  "memory_repair_pending",
  "pcie_link_degraded",
  "pcie_errors",
  "pcie_fatal_errors",
  "nvlink_down",
  "memory_uncorrectable_errors",
  "memory_remap_failed",
  "reset_required",
  "reboot_required",
  "power_cycle_required",
  "vendor_health_warning",
  "vendor_health_fault",
  "memory_error_history",
]);

/**
 * Whether this node owns the card, and so whether its health means
 * anything. Counting cards this node does not own as faults would send an
 * operator looking for a problem that is not there.
 */
export function gpuOwned(gpu: Pick<GPUHealthFacts, "ownership">): boolean {
  return !gpu.ownership;
}

/**
 * The card's level, as the server stored it. The server normalizes it on
 * the way in (accelerator.CardHealthLevel), so there is nothing to
 * interpret here — this reads a value rather than re-deriving a rule that
 * also lives in Go and in SQL.
 *
 * It explains a card; it does not decide whether work may go on it. That
 * is `gpu.schedulable`, which the server decided.
 */
export function gpuHealthStatus(gpu: Pick<GPUHealthFacts, "health_status" | "ownership">): "OK" | "Warning" | "Error" {
  switch (gpu.health_status?.trim().toLowerCase()) {
    case "warning":
    case "warn":
      return "Warning";
    case "ok":
      return "OK";
    case undefined:
    case "":
      // The server always states a level for a card it owns; a card this
      // node does not own has none, and is not faulty for lacking one.
      return "OK";
    default:
      return "Error";
  }
}

/**
 * Why a card is not what its level alone would suggest — the label of its
 * finding if it has one, else whoever holds it — or "" when there is
 * nothing to say. The one wording a card that cannot be picked shows, in
 * the deploy selectors and wherever else the same question is asked.
 */
export function gpuStateReason(t: TFunction, gpu: GPUHealthFacts & Partial<Pick<GPU, "index">>): string {
  const status = gpuHealthStatus(gpu);
  if (status !== "OK") return gpuHealthLabel(t, status, gpu.health_code);
  if (gpu.ownership) return t(`gpuHealth.ownership.${gpu.ownership}`);
  if (gpu.foreign_processes?.length) return t("gpuHealth.foreignUse.label");
  // Nothing wrong with the card, and still nothing to put on it.
  if ((gpu.index ?? 0) < 0) return t("gpuHealth.noDeviceIndex");
  if (gpu.schedulable === false) return t("gpuHealth.schedulable.stopped");
  return "";
}

export function gpuHealthLabel(t: TFunction, status: string, code?: string): string {
  if (code && knownCodes.has(code)) return t(`gpuHealth.code.${code}.label`);
  return t(`gpuHealth.status.${status}`);
}

const tagColor = { OK: "green", Warning: "orange", Error: "red" } as const;

/**
 * How much of a command is shown before it is collapsed. A runtime check's
 * command carries the script it runs, which is dozens of lines: shown in full
 * it buries the evidence underneath it, and the reader copies it rather than
 * reads it. Short commands — the vendor tool queries — are unaffected.
 */
const COMMAND_PREVIEW_LINES = 4;

/**
 * A command a person runs, with a copy button. A long one is collapsed to its
 * first lines, and what is copied is always the whole command: a truncated
 * command that looked complete would be pasted and fail.
 */
export function HealthCommand({ command }: { command: string }) {
  const { t } = useTranslation("common");
  const [expanded, setExpanded] = useState(false);
  const lines = command.split("\n");
  const long = lines.length > COMMAND_PREVIEW_LINES;
  const shown = long && !expanded ? lines.slice(0, COMMAND_PREVIEW_LINES).join("\n") : command;
  return (
    <div className="gpu-health-command">
      <Typography.Text code style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>
        {shown}
        {long && !expanded && "\n…"}
      </Typography.Text>
      <Space direction="vertical" size={0} align="start">
        <CopyButton size="small" text={command} label={t("gpuHealth.copyCommand")} success={t("gpuHealth.copied")} />
        {long && (
          <Button type="link" size="small" style={{ paddingInline: 0 }} onClick={() => setExpanded(!expanded)}>
            {expanded ? t("gpuHealth.collapseCommand") : t("gpuHealth.expandCommand", { count: lines.length })}
          </Button>
        )}
      </Space>
    </div>
  );
}

/**
 * abnormalField marks the evidence values that triggered or contributed to a
 * finding: a "Yes" flag, a positive counter, or a recovery event that is
 * present. "[N/A]" means the card does not support the field.
 */
function abnormalField(field: GPUHealthField): boolean {
  const value = field.value.trim().toLowerCase();
  if (value === "yes") return true;
  if (/^\d+$/.test(value)) return Number(value) > 0 && !field.name.startsWith("NPU") && field.name !== "Chip";
  return field.name.startsWith("xid.") && value !== "" && value !== "[n/a]" && value !== "n/a" && value !== "no";
}

export function HealthEvidence({ fields }: { fields?: GPUHealthField[] }) {
  const { t } = useTranslation("common");
  if (!fields?.length) return null;
  return (
    <div className="gpu-health-evidence" role="list">
      {fields.map((field, i) => field.name
        ? (
          <div key={`${field.name}-${i}`} role="listitem">
            <Typography.Text code>{field.name}</Typography.Text>{" "}
            <Typography.Text strong={abnormalField(field)}>{field.value}</Typography.Text>
            {abnormalField(field) && <Typography.Text type="warning">{` (${t("gpuHealth.abnormal")})`}</Typography.Text>}
          </div>
        )
        : (
          <div key={`legacy-${i}`} role="listitem">
            <Typography.Text type="secondary">{t("gpuHealth.legacyEvidence")}</Typography.Text>{" "}
            <Typography.Text>{field.value}</Typography.Text>
          </div>
        ))}
    </div>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="gpu-health-section">
      <Typography.Text type="secondary" strong>{title}</Typography.Text>
      <div>{children}</div>
    </div>
  );
}

/**
 * GPUHealthDetail explains a finding: what it is, what it does to scheduling,
 * what to do, where the evidence came from and how to recheck it. Shared by
 * the GPU list popover and the diagnostic report.
 */
export function GPUHealthDetail({ status, code, takesWork, evidence, guide, extraAction, commandsOnly = false }: {
  status: "OK" | "Warning" | "Error";
  code?: string;
  /** The server's verdict on whether the card takes work, when known. */
  takesWork?: boolean;
  evidence?: GPUHealthField[];
  guide?: GPUHealthGuide;
  extraAction?: ReactNode;
  /** Leave out the explanation, for a card listed under a finding that already gives it. */
  commandsOnly?: boolean;
}) {
  const { t } = useTranslation("common");
  const known = !!code && knownCodes.has(code);
  const action = known ? t(`gpuHealth.code.${code}.action`) : "";
  return (
    <Space direction="vertical" size={8} style={{ maxWidth: commandsOnly ? undefined : 520 }}>
      {!commandsOnly && <GPUHealthSummary status={status} code={code} takesWork={takesWork} />}
      {((action && !commandsOnly) || !!guide?.actions?.length || !!extraAction) && (
        <Section title={t("gpuHealth.action")}>
          {action && !commandsOnly && <Typography.Text>{action}</Typography.Text>}
          {extraAction}
          {guide?.actions?.map((command) => <HealthCommand key={command} command={command} />)}
        </Section>
      )}
      {(guide?.probe || !!evidence?.length) && (
        <Section title={t("gpuHealth.evidence")}>
          {guide?.probe && <HealthCommand command={guide.probe} />}
          <HealthEvidence fields={evidence} />
        </Section>
      )}
      {guide?.recheck && (
        <Section title={t("gpuHealth.recheck")}>
          <HealthCommand command={guide.recheck} />
        </Section>
      )}
    </Space>
  );
}

/** The explanation of a finding and what it does to scheduling. */
export function GPUHealthSummary({ status, code, takesWork, withAction = false }: { status: "OK" | "Warning" | "Error"; code?: string; takesWork?: boolean; withAction?: boolean }) {
  const { t } = useTranslation("common");
  const known = !!code && knownCodes.has(code);
  // The server decided whether the card takes work; the level only says
  // how it is preferred among the cards that do. Undefined means there is
  // no card to ask — a doctor finding, not a stored card — and the level
  // is then all there is to go on.
  const impact = takesWork === false || status === "Error" ? "stopped" : status === "Warning" ? "degraded" : "ok";
  const action = known ? t(`gpuHealth.code.${code}.action`) : "";
  return (
    <>
      <Typography.Text>{known ? t(`gpuHealth.code.${code}.summary`) : t("gpuHealth.unknownSummary")}</Typography.Text>
      <Section title={t("gpuHealth.impact")}>
        <Typography.Text>{t(`gpuHealth.schedulable.${impact}`)}</Typography.Text>
      </Section>
      {withAction && action && (
        <Section title={t("gpuHealth.action")}>
          <Typography.Text>{action}</Typography.Text>
        </Section>
      )}
    </>
  );
}

/** GPUHealthTag is the health column of a GPU table: the level, and an info icon with the explanation when there is a finding. */
export default function GPUHealthTag({ gpu }: { gpu: GPUHealthFacts }) {
  const { t } = useTranslation("common");
  // A card this node does not own has no health to show. The column asks
  // "can this card be used", and for such a card the answer is whose it is
  // instead — reading it as a fault would be wrong and alarming.
  if (!gpuOwned(gpu)) {
    return (
      <Tag color="default" className="status-tag" title={t(`gpuHealth.ownership.${gpu.ownership}`)} style={{ marginInlineEnd: 0 }}>
        {t(`gpuHealth.ownership.${gpu.ownership}`)}
      </Tag>
    );
  }
  // Somebody outside the platform is using the card. It is not faulty and
  // its health is whatever it was; what an operator needs to know is that
  // the platform is keeping off it, and who is on it.
  if (gpu.foreign_processes?.length) {
    const who = gpu.foreign_processes
      .map((p) => `${p.name} (pid ${p.pid}${p.memory_bytes ? `, ${Math.round(p.memory_bytes / 2 ** 30)} GiB` : ""})`)
      .join("\n");
    const detail = t("gpuHealth.foreignUse.detail", { processes: who });
    return (
      <Tooltip trigger={["hover", "focus"]} title={<span style={{ whiteSpace: "pre-line" }}>{detail}</span>}>
        <Tag color="orange" className="status-tag" tabIndex={0} aria-label={detail} style={{ marginInlineEnd: 0, cursor: "help" }}>
          {t("gpuHealth.foreignUse.label", { count: gpu.foreign_processes.length })}
        </Tag>
      </Tooltip>
    );
  }
  const status = gpuHealthStatus(gpu);
  const label = gpuHealthLabel(t, status, gpu.health_code);
  const evidence = gpu.health_evidence ?? (gpu.health_message ? [{ name: "", value: gpu.health_message }] : undefined);
  const hasDetail = status !== "OK" || !!gpu.health_code;
  return (
    <Space size={4}>
      {/* A long label (English especially) stops at the tag's maximum width; its colour carries the level. */}
      <Tag color={tagColor[status]} className="status-tag" title={label} style={{ marginInlineEnd: 0 }}>{label}</Tag>
      {hasDetail && (
        <Popover
          trigger={["hover", "click"]}
          title={label}
          content={<GPUHealthDetail status={status} code={gpu.health_code} takesWork={gpu.schedulable} evidence={evidence} guide={gpu.health_guide} />}
        >
          <Button type="text" size="small" icon={<InfoCircleOutlined />} aria-label={t("gpuHealth.detailsLabel")} />
        </Popover>
      )}
    </Space>
  );
}
