import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Collapse, Descriptions, Empty, Space, Spin, Typography } from "antd";
import { Alert } from "../pages/DeploymentControls";
import { Badge } from "../components/ui/badge";
import { useTranslation } from "react-i18next";
import {
  api,
  type InstanceLaunchFacts as LaunchFacts,
  type RuntimeContainerObservation,
  type RuntimeImageObservation,
  type RuntimeProcessObservation,
} from "../api";
import { formatTime, formatTimeSec } from "../i18n/datetime";
import { shellQuote } from "./deploy/shellQuote";
import InstanceLaunchValue from "./InstanceLaunchValue";

type LoadState = "idle" | "loading" | "ready" | "error";

function codeValue(value: unknown): React.ReactNode {
  if (value == null || value === "" || (Array.isArray(value) && value.length === 0)) return "—";
  const text = typeof value === "string" ? value : JSON.stringify(value, null, 2);
  return <InstanceLaunchValue>{text}</InstanceLaunchValue>;
}

function argvValue(values?: string[]): React.ReactNode {
  if (!values?.length) return "—";
  return codeValue(values.map(shellQuote).join(" "));
}

function statusTag(status: string, label: string): React.ReactNode {
  const color = status === "current" || status === "available" || status === "observed"
    ? "green" : status === "historical" ? "orange" : status === "unverified" ? "amber" : "gray";
  return <Badge size="sm" color={color}>{label}</Badge>;
}

function UnknownLayer({ reason }: { reason?: string }) {
  const { t } = useTranslation("deployments");
  return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={t(`instance.launchFacts.reason.${reason || "not_collected"}`, { defaultValue: t("instance.launchFacts.reason.not_collected") })} />;
}

function LayerHeading({ title, status, reason }: { title: string; status: string; reason?: string }) {
  const { t } = useTranslation("deployments");
  const label = status === "available"
    ? t("instance.launchFacts.status.available")
    : status === "observed"
      ? t("instance.launchFacts.status.observed")
      : t("instance.launchFacts.status.unknown");
  return (
    <Space wrap>
      <Typography.Text strong>{title}</Typography.Text>
      {statusTag(status, label)}
      {reason ? <Typography.Text type="secondary">{t(`instance.launchFacts.reason.${reason}`, { defaultValue: t("instance.launchFacts.reason.not_collected") })}</Typography.Text> : null}
    </Space>
  );
}

function IntentLayer({ facts }: { facts: LaunchFacts }) {
  const { t } = useTranslation("deployments");
  const layer = facts.intent;
  if (layer.status !== "available" || !layer.value) return <UnknownLayer />;
  const value = layer.value;
  return (
    <Descriptions column={1} size="small" bordered>
      <Descriptions.Item label={t("instance.launchFacts.field.deployment")}>{value.name || `#${value.deployment_id}`}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.engine")}>{value.engine || "—"}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.image")}>{codeValue(value.image)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.servedName")}>{value.served_name || "—"}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.desiredState")}>{value.desired_state}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.replicas")}>{value.replicas}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.environment")}>{codeValue(value.env)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.resourceLimits")}>
        {t("instance.launchFacts.resourceLimits", { vram: value.vram_limit_bytes || 0, sm: value.sm_limit_pct || 0, backend: value.partition_backend || "—" })}
      </Descriptions.Item>
    </Descriptions>
  );
}

function ResolvedLayer({ facts }: { facts: LaunchFacts }) {
  const { t } = useTranslation("deployments");
  const layer = facts.resolved;
  if (layer.status !== "available" || !layer.value) return <UnknownLayer />;
  const value = layer.value;
  return (
    <Descriptions column={1} size="small" bordered>
      <Descriptions.Item label={t("instance.launchFacts.field.entrypoint")}>{argvValue(value.entrypoint)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.arguments")}>{argvValue(value.argv)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.preStart")}>{value.pre_start_script ? codeValue(value.pre_start_script) : argvValue(value.pre_start_argv)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.environment")}>{codeValue(value.env ?? {})}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.mounts")}>{codeValue(value.mounts ?? [])}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.dockerArgs")}>{argvValue(value.docker_args)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.modelMount")}>{codeValue(value.model_mount)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.healthPath")}>{codeValue(value.health_path)}</Descriptions.Item>
    </Descriptions>
  );
}

function observedTime(value?: number): string {
  return value ? formatTimeSec(value) : "—";
}

function ContainerLayer({ value }: { value: RuntimeContainerObservation }) {
  const { t } = useTranslation("deployments");
  if (value.status !== "observed") return <UnknownLayer reason={value.reason} />;
  return (
    <Descriptions column={1} size="small" bordered>
      <Descriptions.Item label={t("instance.launchFacts.field.nodeCollected")}>{observedTime(value.observed_at_unix_ms)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.containerId")}>{codeValue(value.container_id)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.imageId")}>{codeValue(value.image_id)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.entrypoint")}>{argvValue(value.entrypoint)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.arguments")}>{argvValue(value.cmd)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.processPathArgs")}>{argvValue([value.path || "", ...value.args].filter(Boolean))}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.containerConfigEnv")}>{codeValue(value.env)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.mounts")}>{codeValue(value.mounts)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.devices")}>{codeValue(value.devices)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.runtimeNetwork")}>{[value.runtime_name, value.network_mode].filter(Boolean).join(" · ") || "—"}</Descriptions.Item>
    </Descriptions>
  );
}

function ImageLayer({ value }: { value: RuntimeImageObservation }) {
  const { t } = useTranslation("deployments");
  if (value.status !== "observed") return <UnknownLayer reason={value.reason} />;
  return (
    <Descriptions column={1} size="small" bordered>
      <Descriptions.Item label={t("instance.launchFacts.field.nodeCollected")}>{observedTime(value.observed_at_unix_ms)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.imageId")}>{codeValue(value.image_id)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.entrypoint")}>{argvValue(value.entrypoint)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.arguments")}>{argvValue(value.cmd)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.imageConfigEnv")}>{codeValue(value.env)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.volumes")}>{codeValue(value.volumes)}</Descriptions.Item>
    </Descriptions>
  );
}

function ProcessLayer({ value }: { value: RuntimeProcessObservation }) {
  const { t } = useTranslation("deployments");
  if (value.status !== "observed") return <UnknownLayer reason={value.reason} />;
  return (
    <Descriptions column={1} size="small" bordered>
      <Descriptions.Item label={t("instance.launchFacts.field.nodeCollected")}>{observedTime(value.observed_at_unix_ms)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.pid")}>{value.pid || "—"}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.processIdentity")}>{codeValue(value.identity)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.processCommand")}>{argvValue(value.argv)}</Descriptions.Item>
      <Descriptions.Item label={t("instance.launchFacts.field.processEnv")}>{codeValue(value.env ?? {})}</Descriptions.Item>
    </Descriptions>
  );
}

function FactsBody({ facts, refresh }: { facts: LaunchFacts; refresh: () => void }) {
  const { t } = useTranslation("deployments");
  const relation = facts.runtime_observation_relation;
  return (
    <Space direction="vertical" size={12} style={{ display: "flex" }}>
      <Space wrap>
        <Typography.Text>{t("instance.launchFacts.relation.label")}</Typography.Text>
        {statusTag(relation.status, t(`instance.launchFacts.relation.${relation.status}`, { defaultValue: relation.status }))}
        {relation.reason ? <Typography.Text type="secondary">{t(`instance.launchFacts.reason.${relation.reason}`, { defaultValue: relation.reason })}</Typography.Text> : null}
        <Typography.Text type="secondary">{t("instance.launchFacts.serverReceived", { time: formatTime(facts.runtime_observation_received_at) })}</Typography.Text>
        <Button size="small" onClick={refresh}>{t("instance.launchFacts.refresh")}</Button>
      </Space>
      <Typography.Text type="secondary">{t("instance.launchFacts.comparisonHint")}</Typography.Text>
      <Collapse size="small" items={[
        { key: "intent", label: <LayerHeading title={t("instance.launchFacts.layer.intent")} status={facts.intent.status} />, children: <IntentLayer facts={facts} /> },
        { key: "resolved", label: <LayerHeading title={t("instance.launchFacts.layer.resolved")} status={facts.resolved.status} />, children: <ResolvedLayer facts={facts} /> },
        { key: "container", label: <LayerHeading title={t("instance.launchFacts.layer.container")} status={facts.runtime_observation.container.status} reason={facts.runtime_observation.container.reason} />, children: <ContainerLayer value={facts.runtime_observation.container} /> },
        { key: "image", label: <LayerHeading title={t("instance.launchFacts.layer.image")} status={facts.runtime_observation.image.status} reason={facts.runtime_observation.image.reason} />, children: <ImageLayer value={facts.runtime_observation.image} /> },
        { key: "process", label: <LayerHeading title={t("instance.launchFacts.layer.process")} status={facts.runtime_observation.process.status} reason={facts.runtime_observation.process.reason} />, children: <ProcessLayer value={facts.runtime_observation.process} /> },
      ]} />
    </Space>
  );
}

export default function InstanceLaunchFacts({ uuid, expanded }: { uuid: string; expanded: boolean }) {
  const { t } = useTranslation("deployments");
  const [state, setState] = useState<LoadState>("idle");
  const [facts, setFacts] = useState<LaunchFacts | null>(null);
  const requestID = useRef(0);

  const load = useCallback(() => {
    const currentRequest = ++requestID.current;
    setState("loading");
    api.instanceLaunchFacts(uuid).then((next) => {
      if (requestID.current !== currentRequest) return;
      setFacts(next);
      setState("ready");
    }).catch(() => {
      if (requestID.current !== currentRequest) return;
      setState("error");
    });
  }, [uuid]);

  useEffect(() => {
    requestID.current += 1;
    setFacts(null);
    setState("idle");
  }, [uuid]);

  useEffect(() => {
    if (expanded && state === "idle") load();
  }, [expanded, load, state]);

  if (!expanded) return null;
  if (state === "loading") {
    return <div role="status" aria-label={t("instance.launchFacts.loading")} style={{ textAlign: "center", padding: 16 }}><Spin /></div>;
  }
  if (state === "error") {
    return <Alert type="error" showIcon message={t("instance.launchFacts.error")} action={<Button size="small" onClick={load}>{t("instance.launchFacts.retry")}</Button>} />;
  }
  return facts ? <FactsBody facts={facts} refresh={load} /> : null;
}
