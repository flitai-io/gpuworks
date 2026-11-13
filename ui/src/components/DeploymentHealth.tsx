import { InfoCircleOutlined } from "@ant-design/icons";
import { Badge, Tooltip } from "antd";
import type { BadgeProps } from "antd";
import { useTranslation } from "react-i18next";
import type { DeploymentHealth, HealthProbe, HealthStatus } from "../api";
import { agoText } from "../format";
import type { InferenceAPIOperation } from "../inferenceApi";
import { formatMS } from "../pages/monitor/requestShape";
import { colors } from "../theme/tokens";
import { detailBadgeTone } from "../pages/state";
import { Badge as StatusBadge } from "./ui/badge";

// The server's health check of a deployment (dto/deployment_health.go): a
// minimal request through the gateway every minute. These pieces show it the
// same way on the deployment list, its detail and the service monitor.

// healthCheckable says whether a deployment serving operations can be
// checked: the check asks chat, else text completion, else embeddings
// (inferenceapi.CheckOperation).
export function healthCheckable(
  operations: Pick<InferenceAPIOperation, "type">[] | undefined,
): boolean {
  return (operations ?? []).some(
    (op) => op.type === "openai.chat" || op.type === "openai.completions" || op.type === "openai.embeddings",
  );
}

const BADGE: Record<HealthStatus, BadgeProps["status"]> = {
  ok: "success",
  timeout: "warning",
  failed: "error",
  off: "default",
  no_replica: "default",
  pending: "processing",
};

export const PROBE_COLOR: Record<HealthProbe["status"], string> = {
  ok: colors.success,
  timeout: colors.warning,
  failed: colors.danger,
};

// HealthCheckHelp is the question mark that says how the check works.
export function HealthCheckHelp() {
  const { t } = useTranslation("common");
  return (
    <Tooltip
      title={<div style={{ maxWidth: 360 }}>{t("healthCheck.help")}</div>}
      trigger={["hover", "focus"]}
    >
      <InfoCircleOutlined
        role="img"
        aria-label={t("healthCheck.helpLabel")}
        title={t("healthCheck.helpLabel")}
        tabIndex={0}
        style={{
          color: colors.muted,
          fontSize: 13,
          marginLeft: 6,
          cursor: "help",
        }}
      />
    </Tooltip>
  );
}

// probeLines describe one check: when, how long, who answered, why it failed.
export function ProbeLines({
  probe,
  replicaName,
}: {
  probe: HealthProbe;
  replicaName?: (uuid: string) => string | undefined;
}) {
  const { t } = useTranslation("common");
  const ago = agoText(t, Math.max(0, (Date.now() - probe.at_ms) / 1000));
  const replica = probe.instance_uuid
    ? (replicaName?.(probe.instance_uuid) ?? probe.instance_uuid.slice(0, 8))
    : "";
  return (
    <>
      <span>
        {t("healthCheck.last", {
          status: t(`healthCheck.status.${probe.status}`),
          time: formatMS(probe.latency_ms),
          ago,
        })}
      </span>
      {replica && <span>{t("healthCheck.lastReplica", { replica })}</span>}
      {probe.http_status ? (
        <span>
          {t("healthCheck.httpStatus", { status: probe.http_status })}
        </span>
      ) : null}
      {probe.message && (
        <span style={{ wordBreak: "break-word" }}>
          {t("healthCheck.message", { message: probe.message })}
        </span>
      )}
    </>
  );
}

// HealthTag is a deployment's health in one line: the status, with the last
// check's time when it answered; hover for the details.
export function HealthTag({
  health,
  detail = false,
}: {
  health: DeploymentHealth | undefined;
  detail?: boolean;
}) {
  const { t } = useTranslation("common");
  if (!health) return <>—</>;
  const text = t(`healthCheck.status.${health.status}`);
  const label =
    health.status === "ok" && health.last
      ? `${text} · ${formatMS(health.last.latency_ms)}`
      : text;
  const status = BADGE[health.status] ?? "default";
  const badge = detail
    ? <StatusBadge variant="plain" size="sm" status={detailBadgeTone[status]}>{label}</StatusBadge>
    : <Badge status={status} text={label} />;
  if (health.status === "off" || health.status === "pending") return badge;
  return (
    <Tooltip
      title={
        <div
          style={{
            display: "flex",
            flexDirection: "column",
            gap: 2,
            maxWidth: 320,
            fontVariantNumeric: "tabular-nums",
          }}
        >
          {health.status === "no_replica" && (
            <span>{t("healthCheck.noReplicaHelp")}</span>
          )}
          {health.last && <ProbeLines probe={health.last} />}
          {health.recent_mean_ms != null ? (
            <span>
              {t("healthCheck.recentMean", {
                count: health.recent.length,
                ok: health.recent_ok,
                time: formatMS(health.recent_mean_ms),
              })}
            </span>
          ) : (
            health.recent.length > 0 && (
              <span>
                {t("healthCheck.recentNone", { count: health.recent.length })}
              </span>
            )
          )}
          <span>
            {t("healthCheck.every", { seconds: health.threshold_sec })}
          </span>
        </div>
      }
    >
      <span tabIndex={0}>{badge}</span>
    </Tooltip>
  );
}
