import { useIcon } from "../lib/icon-context";
import StartupStatusIcon from "./StartupStatusIcon";
import DeploymentInfoField, { DeploymentInfoList } from "./DeploymentInfoField";
import { productMessageText } from "../apiError";
import { useState, type Ref } from "react";
import { storageDetailPath } from "./storage/storageLinks";
import { launchCommand } from "./deploy/launchCommand";
import { Space, Tooltip, Typography } from "antd";
import { Badge, Button, Checkbox, Progress, Tag } from "../pages/DeploymentControls";
import { WarningFilled } from "@ant-design/icons";
import { type Instance, type InstanceEvent } from "../api";
import { Link, useNavigate } from "react-router-dom";
import { stateLabel } from "./state";
import { Badge as LaunchBadge } from "../components/ui/badge";
import { Button as ZeronButton } from "../components/ui/button";
import DetailSection from "../components/detail/DetailSection";
import VRAMBreakdown from "../components/VRAMBreakdown";
import AIAssistant from "../components/AIAssistant";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { instanceEventStage, phaseDurationText, phaseTransferSize } from "./instanceEvents";
import { formatTimeShort } from "../i18n/datetime";
import TimeRangePicker, { type TimeRangeValue } from "../components/TimeRangePicker";
import InstanceProgressLine from "./InstanceProgressLine";
import InstanceStartupStatus from "./InstanceStartupStatus";
import { useInstanceEvents } from "./useInstanceEvents";
import { LogStream } from "./InstanceLogStream";
import InstanceMetricsTab from "./InstanceMetricsTab";
import InstanceLaunchDetails from "./InstanceLaunchDetails";
import InstanceLaunchValue from "./InstanceLaunchValue";
import InstanceComputeNetworkView from "./InstanceComputeNetwork";
import type { DistRank } from "./instanceGroups";
import { shortImageID } from "./instanceImage";
import { storageTypeLabel } from "../storageDisplay";

function phaseLabel(phase: string | undefined, t: TFunction): string {
  switch (phase) {
    case "image_verify":
    case "image_pull":
    case "image_pull_retry":
    case "image_extract":
    case "container_unpack":
    case "container_prepare":
    case "container_gpu_setup":
    case "container_exec_setup":
    case "container_fixup":
    case "container_start":
    case "container_launch":
    case "health_check":
    case "optimizing":
      return t(`instance.phase.${phase}`);
    default: return phase || t("instance.phase.default");
  }
}


type ReadyModel = { name: string; node?: string };

export type InstanceView = "details" | "metrics";

export default function InstancePanel({ inst, readyModel, sleepable, params, rank, imageDiffers, previousAttempt, nodeName, showNode, metricsRef }: { inst: Instance; readyModel?: ReadyModel; previousAttempt?: boolean; nodeName?: string; showNode?: boolean; metricsRef?: Ref<HTMLDivElement>; sleepable?: boolean; params?: Record<string, unknown>; rank?: DistRank; imageDiffers?: boolean }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const ChevronDown = useIcon("chevron-down");
  const messageText = (inst.runtime_failure ? productMessageText(tCommon, inst.runtime_failure) : inst.message)?.trim();
  const diagnosis = inst.state === "failed" && (
    <AIAssistant task="instance.diagnose"
      subject={{ kind: "instance", uuid: inst.uuid, label: inst.uuid.slice(0, 8) }}
      buttonText={t("instance.diagnoseButton")} buttonType="tertiary" buttonSize="small" />
  );
  const navigate = useNavigate();
  const [followLogs, setFollowLogs] = useState(true);
  const [logRange, setLogRange] = useState<TimeRangeValue>(null);
  const s = stateLabel(inst.state, tCommon);
  const launch = launchCommand(inst);
  const live = inst.state === "running" || inst.state === "starting" || inst.state === "optimizing";
  const schedule = inst.schedule;
  const chosenGpus: number[] = schedule?.explain?.topo_highlight?.gpus ?? [];
  const assignedGpus = inst.gpu_indexes ?? [];
  const showScheduledGpus = chosenGpus.length > 0 &&
    (chosenGpus.length !== assignedGpus.length || chosenGpus.some((gpu) => !assignedGpus.includes(gpu)));
  const sleepLevel = inst.sleep_level ?? 0;
  const sleepOrder = inst.desired_sleep ?? 0;
  const bd = schedule?.breakdown;
  const weightGib = bd?.weight_bytes ? bd.weight_bytes / 1024 ** 3 : undefined;
  const tp = bd?.tensor_parallel && bd.tensor_parallel > 0 ? bd.tensor_parallel : 1;
  const showL1Wake = sleepLevel === 1 || (sleepable && inst.state === "running" && sleepLevel === 0 && sleepOrder !== 2);
  const wakeSecs = showL1Wake && bd?.weight_bytes ? bd.weight_bytes / tp / 14e9 : undefined;
  const hasProgress = inst.state === "pulling" || inst.state === "starting" || inst.state === "optimizing";
  const terminalURL = inst.node_uuid && inst.container_id
    ? `/nodes/${encodeURIComponent(inst.node_uuid)}/terminal?target=container&ref=${encodeURIComponent(inst.container_id)}`
    : "";
  // One timeline read serves both the startup stage list and the log stream's
  // merged system lines, and it is refreshed by the shared SSE stream rather
  // than by every phase change.
  const timeline = useInstanceEvents(inst.uuid);
  const completed = inst.state === "running" || timeline.events.some(event =>
    event.code === "INSTANCE_STATUS_UPDATED" && (event.params?.state === "running" || event.params?.prev_state === "running"));
  const failure = inst.state === "failed" && !completed
    ? timeline.events.find(event => event.code === "INSTANCE_STATUS_UPDATED" && event.params?.state === "failed")
    : undefined;
  const startupSection = !completed && (hasProgress || failure) && (
    <details open className="detail-section instance-startup-progress scroll-mt-12">
      <summary className="deployment-card-disclosure">{t("detailLayout.startup")}<ChevronDown size={14} aria-hidden="true" /></summary>
      <InstanceStartupProgress inst={inst} events={timeline.events} readyModel={readyModel} nodeName={nodeName} failure={failure} />
    </details>
  );
  const detailsContent = (
          <div className="deployment-instance-groups">
            <DetailSection title={t("detail.instanceRuntime")}>
              <DeploymentInfoList className="instance-runtime-fields">
                <DeploymentInfoField label={t("instance.field.instance")}>
                  <span className="inline-flex max-w-full flex-wrap items-center gap-x-2 gap-y-1">
                    <span>{inst.uuid.slice(0, 12)}</span>
                    {live && terminalURL && (
                      <Tooltip title={t("instanceTerminal.hint")} trigger={["hover", "focus"]}>
                        <ZeronButton size="xs" variant="tertiary" onClick={() => window.open(terminalURL, "_blank", "noopener")}>
                          {t("instanceTerminal.open")}
                        </ZeronButton>
                      </Tooltip>
                    )}
                  </span>
                </DeploymentInfoField>
                <DeploymentInfoField label={t("instance.field.state")}>
                  <Space size={4} wrap>
                    <Badge status={s.status} text={s.text} />
                    {previousAttempt && <span>{t("instance.previousAttempt")}</span>}
                    {inst.role && (
                      <Tag color="geekblue">
                        {t(`detail.pd.roleLabel.${inst.role}`, { defaultValue: inst.role })}
                      </Tag>
                    )}
                    {rank && (
                      <Tag color={rank.role === "leader" ? "cyan" : undefined}>
                        {rank.role === "leader"
                          ? t("instance.rank.leader", { rank: rank.rank })
                          : rank.leaderUUID
                            ? t("instance.rank.followerOf", { rank: rank.rank, leader: rank.leaderUUID.slice(0, 8) })
                            : t("instance.rank.follower", { rank: rank.rank })}
                      </Tag>
                    )}
                    {rank?.role === "follower" && <Typography.Text type="secondary">{t("replica.followerHint")}</Typography.Text>}
                    {!messageText && diagnosis}
                  </Space>
                </DeploymentInfoField>
                {showNode && (
                  <DeploymentInfoField label={t("detailLayout.node")}>{nodeName ?? inst.node_uuid}</DeploymentInfoField>
                )}
                <DeploymentInfoField label={t("instance.field.gpu")}>
                  <Space size={4} wrap>
                    {assignedGpus.length ? assignedGpus.join(", ") : "—"}
                    {chosenGpus.length ? <Button size="small" type="link" onClick={() => navigate("/nodes", { state: { nodeUUID: inst.node_uuid, tab: "accelerators", highlight: schedule?.explain?.topo_highlight } })}>{t("instance.topology")}</Button> : null}
                  </Space>
                </DeploymentInfoField>
                <DeploymentInfoField label={t("instance.field.apiPort")}>{inst.api_port || "—"}</DeploymentInfoField>
                {showScheduledGpus && (
                  <DeploymentInfoField label={t("instance.selectedGpu")}>{chosenGpus.join(", ")}</DeploymentInfoField>
                )}
                {(inst.asleep || wakeSecs != null || (sleepLevel > 0 && weightGib != null)) && (
                  <DeploymentInfoField label={t("instance.field.sleep")}>
                    <Space direction="vertical" size={2}>
                      {inst.asleep && (
                        <Tag color="orange">
                          {sleepOrder === 0
                            ? t("instance.waking")
                            : sleepOrder !== sleepLevel
                              ? t("instance.fallingAsleep", { level: sleepOrder })
                              : t("instance.sleeping", { level: sleepLevel, storage: sleepLevel === 1 ? t("instance.memory") : t("instance.disk") })}
                        </Tag>
                      )}
                      {inst.asleep && inst.sleep_held && <Typography.Text type="secondary">{t("instance.held")}</Typography.Text>}
                      {wakeSecs != null && t("instance.wakeEstimate", { seconds: wakeSecs < 1 ? wakeSecs.toFixed(1) : Math.round(wakeSecs) })}
                      {sleepLevel > 0 && weightGib != null && t("instance.sleepWeight", { storage: sleepLevel === 1 ? t("instance.hostMemory") : t("instance.disk"), gib: weightGib.toFixed(1) })}
                    </Space>
                  </DeploymentInfoField>
                )}
                {schedule?.network?.compute && (
                  <DeploymentInfoField label={t("instance.field.computeNetwork")}>
                    <InstanceComputeNetworkView network={schedule.network.compute} />
                  </DeploymentInfoField>
                )}
                {messageText && (!hasProgress || inst.state === "failed") && (
                  <DeploymentInfoField label={t("instance.field.message")}>
                    <div className="min-w-0">
                      <Typography.Text type={inst.state === "failed" ? "danger" : undefined}>{messageText}</Typography.Text>
                      {diagnosis && <>{" "}{diagnosis}</>}
                    </div>
                  </DeploymentInfoField>
                )}
              </DeploymentInfoList>
            </DetailSection>
            <DetailSection title={t("detail.instanceLaunch")}>
              <DeploymentInfoList>
                <DeploymentInfoField label={t("instance.field.image")}>
                  {inst.image ? <InstanceLaunchValue>{inst.image}</InstanceLaunchValue> : "—"}
                </DeploymentInfoField>
                <DeploymentInfoField label={t("instance.field.imageId")}>
                  {inst.image_digest_id ? (
                    <Space size={4}>
                      <InstanceLaunchValue title={inst.image_digest_id} copy={{ label: t("instance.copyImageId"), text: inst.image_digest_id }}>
                        {shortImageID(inst.image_digest_id)}
                      </InstanceLaunchValue>
                      {imageDiffers && (
                        <Tooltip title={t("instance.imageMismatch.tooltip")} trigger={["hover", "focus"]}>
                          <Button
                            type="text"
                            size="small"
                            aria-label={t("instance.imageMismatch.label")}
                            icon={<Typography.Text type="warning"><WarningFilled /></Typography.Text>}
                          />
                        </Tooltip>
                      )}
                    </Space>
                  ) : "—"}
                </DeploymentInfoField>
                {inst.model_files?.length ? (
                  <DeploymentInfoField label={t("instance.field.modelFiles")}>
                    <ul className="deployment-model-files">
                      {inst.model_files.map((file) => (
                        <li key={`${file.model_id}-${file.path}`}>
                          <span className="deployment-file-storage">
                            {file.storage_id ? (
                              <Link to={storageDetailPath(file.storage_id)}>{file.storage_name}</Link>
                            ) : (
                              <Typography.Text type="secondary">{t("instance.storageGone")}</Typography.Text>
                            )}
                            {file.storage_type && <LaunchBadge size="sm">{storageTypeLabel(tCommon, file.storage_type)}</LaunchBadge>}
                          </span>
                          <InstanceLaunchValue copy={{ label: t("instance.copyModelPath") }}>{file.path}</InstanceLaunchValue>
                        </li>
                      ))}
                    </ul>
                  </DeploymentInfoField>
                ) : null}
                <DeploymentInfoField label={t("instance.field.command")}>
                  {launch.command ? (
                    <div className="instance-launch-command">
                      {inst.command_mode === "manual" && (
                        <LaunchBadge size="sm" color="purple">{t("instance.commandMode.manual")}</LaunchBadge>
                      )}
                      <InstanceLaunchValue copy={{ label: t("instance.copyLaunchScript"), text: launch.script }}>
                        {launch.command}
                      </InstanceLaunchValue>
                    </div>
                  ) : "—"}
                </DeploymentInfoField>
              </DeploymentInfoList>
              <InstanceLaunchDetails inst={inst} preparation={launch.preparation} />
            </DetailSection>
            {startupSection}
            <InstanceStartupStatus inst={inst} events={timeline.events} />
          {schedule?.breakdown && (inst.measured?.Found ? (
            <VRAMBreakdown inDetailCard breakdown={{ ...schedule.breakdown, source: "measured" }} measured={inst.measured} />
          ) : (
            <DetailSection title={t("vram.title", { ns: "deploy" })}>
              <Typography.Text type="secondary">{t("instanceNoMeasuredMemory")}</Typography.Text>
            </DetailSection>
          ))}
          <section className="deployment-logs" aria-label={t("instance.tab.logs")}>
            <header className="deployment-log-toolbar">
              <h4 className="deployment-section-title">{t("instance.tab.logs")}</h4>
              <div className="deployment-log-controls">
                <TimeRangePicker deploymentDetail value={logRange} onChange={setLogRange} size="small" className="deployment-log-range"
                  liveLabel={live ? t("timeRange.live") : t("timeRange.recent")} />
                <Checkbox checked={followLogs} onChange={(e) => setFollowLogs(e.target.checked)}>
                  {t("instance.followLogs")}
                </Checkbox>
              </div>
            </header>
            <LogStream uuid={inst.uuid} live={live} follow={followLogs} range={logRange} timeline={timeline}
              runtimeLine={["pulling", "starting", "optimizing"].includes(inst.state) ? messageText : undefined} />
          </section>
          </div>
  );
  const metricsContent = (<div ref={metricsRef} tabIndex={-1}><InstanceMetricsTab inst={inst} params={params} /></div>);
  return (
    <div id={`instance-${inst.uuid}`} style={{ marginBottom: 16 }}>
        <section className="deployment-runtime-section" aria-label={t("instance.tab.details")}>{detailsContent}</section>
        <section className="deployment-metrics-section" aria-label={t("instance.tab.metrics")}>
          <h4 className="deployment-section-title">{t("instance.tab.metrics")}</h4>
          {metricsContent}
        </section>
    </div>
  );
}

function InstanceStartupProgress({ inst, events, readyModel, nodeName, failure }: { inst: Instance; events: InstanceEvent[]; readyModel?: ReadyModel; nodeName?: string; failure?: InstanceEvent }) {
  const { t, i18n } = useTranslation("deployments");
  const durationUnits = i18n.getFixedT("en", "deployments");
  const { t: tCommon } = useTranslation("common");
  const failed = Boolean(failure);
  const lastPhase = failure?.params?.prev_phase;
  const current = (failed && typeof lastPhase === "string" && lastPhase) || inst.phase || inst.state;
  const stages = startupStages(events, current);
  const firstImageStage = stages.find(stage => stage.phase.startsWith("image_"));
  const downloading = inst.state === "pulling" && current === "image_pull" && (inst.progress ?? 0) < 1;
  const percent = typeof inst.progress === "number" && inst.progress > 0 ? Math.min(100, Math.round(inst.progress * 100)) : undefined;
  return <div className="instance-startup-progress-body">
    <ol className="deployment-startup-timeline" aria-label={t("startup.title")}>
      {readyModel && <li className="deployment-startup-row" data-status="done">
        <StartupStatusIcon status="done" />
        <div className="deployment-startup-step col-start-2 -col-end-1">
          <strong>{t("preparation.phase.ready")}</strong>
          {readyModel.name && <span>{readyModel.name}</span>}
          {readyModel.node && <span>{readyModel.node}</span>}
        </div>
      </li>}
      {stages.map(stage => {
        const isCurrent = stage.phase === current;
        const status = isCurrent ? (failed ? "failed" : inst.state === "pending" ? "waiting" : "active") : "done";
        const created = stage.phase === "INSTANCE_CREATED";
        const title = created ? t("startup.instanceCreated") : stage.phase === inst.state ? stateLabel(inst.state, tCommon).text : phaseLabel(stage.phase, t);
        const object = created ? (nodeName ?? readyModel?.node) : stage === firstImageStage ? inst.image : undefined;
        const transferPhase = ["image_pull", "image_pull_retry", "image_extract", "container_unpack"].includes(stage.phase);
        const detail = transferPhase ? undefined : (isCurrent && !failed && inst.message?.trim()) || stage.message;
        const recorded = formatTimeShort(stage.recordedAt, "");
        const duration = stage.endEvent && phaseDurationText(stage.endEvent, durationUnits);
        const transfer = stage.endEvent && phaseTransferSize(stage.endEvent);
        return <li key={stage.phase} className="deployment-startup-row" data-status={status} aria-current={status === "active" ? "step" : undefined}>
          <StartupStatusIcon status={status} />
          <div className="deployment-startup-step">
            <strong>{title}</strong>
            {object && <span>{object}</span>}
            {detail && detail !== title && detail !== object && <span>{detail}</span>}
          </div>
          <div className="deployment-startup-meta">
            <span className={status === "done" ? "sr-only" : undefined}>{t(`startup.${status}`)}</span>
            {transfer && <span>{transfer}</span>}
            {(duration || recorded) && <span>
              {duration}{duration && recorded && " · "}{recorded && <time dateTime={stage.recordedAt}>{recorded}</time>}
            </span>}
          </div>
        </li>;
      })}
    </ol>
    {!failed && (current === "image_pull" || current === "image_extract" || current === "image_pull_retry") && <>
      {percent == null ? <div className="instance-progress-unknown" aria-label={t("instance.progressAria")}><span /></div>
        : <Progress percent={percent} status="active" size="small" />}
      <InstanceProgressLine bytesDone={inst.bytes_done} bytesTotal={inst.bytes_total} rateBps={downloading ? inst.rate_bps : undefined} />
    </>}
  </div>;
}

/**
 * startupStages lists the phases this instance has passed through, in the order
 * it passed through them. The timeline arrives newest-first, so it is walked
 * backwards: a stage list that ran the other way would read as the startup
 * happening in reverse.
 */
function startupStages(events: InstanceEvent[], current: string) {
  type Stage = { phase: string; message?: string; recordedAt?: string; endEvent?: InstanceEvent };
  const stages = new Map<string, Stage>();
  let previous: Stage | undefined;
  for (let index = events.length - 1; index >= 0; index--) {
    const event = events[index];
    if (previous && previous.phase === event.params?.prev_phase) previous.endEvent = event;
    const stage = event.code === "INSTANCE_CREATED" ? { phase: event.code, message: "" } : instanceEventStage(event);
    if (!stage) continue;
    if (previous?.phase === stage.phase) {
      if (stage.message) previous.message = stage.message;
      continue;
    }
    previous = { ...stage, recordedAt: event.created_at };
    stages.set(stage.phase, previous);
  }
  if (previous?.phase !== current) stages.set(current, { phase: current });
  return [...stages.values()];
}
