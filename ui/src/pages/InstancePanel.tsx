import { productMessageText } from "../apiError";
import { useState, type ReactNode } from "react";
import { storageDetailPath } from "./storage/storageLinks";
import { launchCommand } from "./deploy/launchCommand";
import { shellQuote } from "./deploy/shellQuote";
import { Badge, Button, Checkbox, Collapse, Descriptions, Progress, Space, Tabs, Tag, Tooltip, Typography } from "antd";
import { WarningFilled } from "@ant-design/icons";
import { type Instance, type InstanceEvent } from "../api";
import { Link, useNavigate } from "react-router-dom";
import { stateLabel } from "./state";
import VRAMBreakdown from "../components/VRAMBreakdown";
import AIAssistant from "../components/AIAssistant";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { instanceEventStage } from "./instanceEvents";
import TimeRangePicker, { type TimeRangeValue } from "../components/TimeRangePicker";
import InstanceProgressLine from "./InstanceProgressLine";
import InstanceStartupStatus from "./InstanceStartupStatus";
import { useInstanceEvents } from "./useInstanceEvents";
import { LogStream } from "./InstanceLogStream";
import InstanceMetricsTab from "./InstanceMetricsTab";
import InstanceLaunchFacts from "./InstanceLaunchFacts";
import InstanceComputeNetworkView from "./InstanceComputeNetwork";
import type { DistRank } from "./instanceGroups";
import { shortImageID } from "./instanceImage";
import { storageTypeLabel } from "../storageDisplay";

// Under --ipc=host the launch states no /dev/shm size: the container uses the
// host's own (docs/reference/deployment-launch-sources.md).
function sharesHostIPC(args: string[] | undefined): boolean {
  return (args || []).some((arg, index, all) => {
    const token = arg.trim();
    return token === "--ipc=host" || token === "--ipc host" || (token === "--ipc" && all[index + 1]?.trim() === "host");
  });
}

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


// InstancePanel renders one instance: info, the launch command it actually
// runs, and two tabs - a live log stream (EventSource) with instance events
// merged in as system lines, and inference/GPU metrics. rank marks a rank of
// a multi-node replica (leader or follower, and a follower's leader); actions
// are the replica's restart and remove controls. imageDiffers marks a replica
// whose image ID differs from the others' (see instanceImage.ts).
export default function InstancePanel({ inst, params, rank, actions, imageDiffers, previousAttempt }: { inst: Instance; params?: Record<string, unknown>; rank?: DistRank; actions?: ReactNode; imageDiffers?: boolean; previousAttempt?: boolean }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const messageText = inst.runtime_failure ? productMessageText(tCommon, inst.runtime_failure) : inst.message;
  const navigate = useNavigate();
  const [activeTab, setActiveTab] = useState("logs");
  const [followLogs, setFollowLogs] = useState(true);
  const [logRange, setLogRange] = useState<TimeRangeValue>(null);
  const s = stateLabel(inst.state, tCommon);
  const command = [...(inst.entrypoint || []), ...(inst.argv || [])];
  const live = inst.state === "running" || inst.state === "starting" || inst.state === "optimizing";
  const schedule = inst.schedule;
  const chosenGpus: number[] | undefined = schedule?.explain?.topo_highlight?.gpus;
  const sleepLevel = inst.sleep_level ?? 0;
  const sleepOrder = inst.desired_sleep ?? 0;
  const bd = schedule?.breakdown;
  const weightGib = bd?.weight_bytes ? bd.weight_bytes / 1024 ** 3 : undefined;
  const tp = bd?.tensor_parallel && bd.tensor_parallel > 0 ? bd.tensor_parallel : 1;
  const wakeSecs = bd?.weight_bytes ? bd.weight_bytes / tp / 14e9 : undefined;
  const hasProgress = inst.state === "pulling" || inst.state === "starting" || inst.state === "optimizing" || !!inst.phase;
  const percent = typeof inst.progress === "number" && inst.progress > 0 ? Math.min(100, Math.round(inst.progress * 100)) : undefined;
  const terminalURL = inst.node_uuid && inst.container_id
    ? `/nodes/${encodeURIComponent(inst.node_uuid)}/terminal?target=container&ref=${encodeURIComponent(inst.container_id)}`
    : "";
  // One timeline read serves both the startup stage list and the log stream's
  // merged system lines, and it is refreshed by the shared SSE stream rather
  // than by every phase change.
  const timeline = useInstanceEvents(inst.uuid);
  const stages = startupStages(timeline.events);
  return (
    <div id={`instance-${inst.uuid}`} style={{ marginBottom: 16 }}>
      <Descriptions column={2} bordered size="small">
        <Descriptions.Item label={t("instance.field.instance")}>{inst.uuid.slice(0, 12)}</Descriptions.Item>
        <Descriptions.Item label={t("instance.field.state")}>
          <Space size={4} wrap>
            <Badge status={s.status} text={s.text} />
            {/* Plain text: another antd Tag grows the usage inventory. */}
            {previousAttempt && <span>{t("instance.previousAttempt")}</span>}
            {inst.role && (
              <Tag color="geekblue">
                {t(`detail.pd.role.${inst.role}`, { defaultValue: inst.role })}
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
            {inst.asleep && (
              <Tag color="orange">
                {sleepOrder === 0
                  ? t("instance.waking")
                  : sleepOrder !== sleepLevel
                    ? t("instance.fallingAsleep", { level: sleepOrder })
                    : t("instance.sleeping", { level: sleepLevel, storage: sleepLevel === 1 ? t("instance.memory") : t("instance.disk") })}
              </Tag>
            )}
            {inst.asleep && inst.sleep_held && <Tag color="volcano">{t("instance.held")}</Tag>}
          </Space>
        </Descriptions.Item>
        <Descriptions.Item label={t("instance.field.gpu")}>
          <Space size={4} wrap>
            {inst.gpu_indexes?.length ? inst.gpu_indexes.join(", ") : "—"}
            {chosenGpus?.length ? <Button size="small" type="link" onClick={() => navigate("/nodes", { state: { nodeUUID: inst.node_uuid, tab: "accelerators", highlight: schedule?.explain?.topo_highlight } })}>{t("instance.topology")}</Button> : null}
          </Space>
        </Descriptions.Item>
        <Descriptions.Item label={t("instance.field.apiPort")}>{inst.api_port || "—"}</Descriptions.Item>
        {schedule?.network?.compute && (
          <Descriptions.Item label={t("instance.field.computeNetwork")} span={2}>
            <InstanceComputeNetworkView network={schedule.network.compute} />
          </Descriptions.Item>
        )}
        <Descriptions.Item label={t("instance.field.image")} span={2}>
          {inst.image || "—"}
        </Descriptions.Item>
        <Descriptions.Item label={t("instance.field.imageId")} span={2}>
          {inst.image_digest_id ? (
            <Space size={4}>
              <Typography.Text code title={inst.image_digest_id} copyable={{ text: inst.image_digest_id }}>{shortImageID(inst.image_digest_id)}</Typography.Text>
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
        </Descriptions.Item>
        {inst.model_files?.length ? (
          <Descriptions.Item label={t("instance.field.modelFiles")} span={2}>
            <Space direction="vertical" size={2} style={{ display: "flex" }}>
              {inst.model_files.map((file) => (
                <Space key={`${file.model_id}-${file.path}`} size={6} wrap>
                  {file.storage_id ? (
                    <Link to={storageDetailPath(file.storage_id)}>{file.storage_name}</Link>
                  ) : (
                    <Typography.Text type="secondary">{t("instance.storageGone")}</Typography.Text>
                  )}
                  {file.storage_type && <Tag>{storageTypeLabel(tCommon, file.storage_type)}</Tag>}
                  <Typography.Text code copyable style={{ fontSize: 12, wordBreak: "break-all" }}>{file.path}</Typography.Text>
                </Space>
              ))}
            </Space>
          </Descriptions.Item>
        ) : null}
        <Descriptions.Item label={t("instance.field.command")} span={2}>
          {command.length ? (
            <Space direction="vertical" size={2} style={{ display: "flex" }}>
              {inst.command_mode === "manual" && (
                <Space size={4}><Tag color="purple">{t("instance.commandMode.manual")}</Tag></Space>
              )}
              <Typography.Text
                copyable={{ text: launchCommand({ ...inst, command }).script }}
                code
                style={{ fontSize: 12, whiteSpace: "pre-wrap", wordBreak: "break-all", display: "block", padding: 4 }}
              >
                {command.map(shellQuote).join(" ")}
              </Typography.Text>
            </Space>
          ) : "—"}
        </Descriptions.Item>
        {(inst.pre_start_script || inst.pre_start_argv?.length) ? <Descriptions.Item span={2}>
          <details><summary>{t("instance.runtime.preparation")}</summary><Typography.Paragraph copyable style={{ margin: 0, whiteSpace: "pre-wrap", fontFamily: "monospace" }}>{inst.pre_start_script || inst.pre_start_argv?.map(shellQuote).join(" ")}</Typography.Paragraph></details>
        </Descriptions.Item> : null}
        {inst.message && (
          <Descriptions.Item label={t("instance.field.message")} span={2}>
            <Typography.Text type={inst.state === "failed" ? "danger" : undefined}>
              {messageText}
            </Typography.Text>
          </Descriptions.Item>
        )}
      </Descriptions>

      {actions}

      {live && terminalURL && (
        <div style={{ marginTop: 8 }}>
          <Button size="small" onClick={() => window.open(terminalURL, "_blank", "noopener")}>
            {t("instanceTerminal.open")}
          </Button>
          <Typography.Text type="secondary" style={{ marginLeft: 8, fontSize: 12 }}>
            {t("instanceTerminal.hint")}
          </Typography.Text>
        </div>
      )}

      {hasProgress && (
        <div style={{ marginTop: 10, padding: "8px 12px", border: "1px solid #f0f0f0", borderRadius: 2 }}>
          <Space direction="vertical" size={3} style={{ width: "100%" }}>
            <Space style={{ width: "100%", justifyContent: "space-between" }}>
              <Typography.Text strong>{phaseLabel(inst.phase || inst.state, t)}</Typography.Text>
              {percent != null && <Typography.Text type="secondary">{percent}%</Typography.Text>}
            </Space>
            {percent == null ? (
              <div className="instance-progress-unknown" aria-label={t("instance.progressAria")}><span /></div>
            ) : (
              <Progress percent={percent} status={inst.state === "failed" ? "exception" : "active"} showInfo={false} />
            )}
            <InstanceProgressLine
              message={messageText}
              bytesDone={inst.bytes_done}
              bytesTotal={inst.bytes_total}
              rateBps={inst.rate_bps}
              live={inst.state !== "failed" && inst.state !== "stopped"}
            />
            {stages.length > 1 && (
              <Space direction="vertical" size={1} style={{ width: "100%", marginTop: 4 }}>
                {stages.map((stage) => (
                  <Typography.Text key={stage.phase} type={stage.phase === inst.phase ? undefined : "secondary"} style={{ fontSize: 12 }}>
                    {stage.phase === inst.phase ? t("instance.stageRunning") : t("instance.stageDone")}{phaseLabel(stage.phase, t)}{stage.message ? ` · ${stage.message}` : ""}
                  </Typography.Text>
                ))}
              </Space>
            )}
          </Space>
        </div>
      )}

      <InstanceStartupStatus inst={inst} events={timeline.events} />

      {(Object.keys(inst.env || {}).length || inst.mounts?.length || inst.shm_bytes || inst.docker_args?.length) ? (
        <Collapse ghost style={{ marginTop: 8 }} items={[{ key: "runtime", label: t("instance.runtime.title"), children: <div style={{ fontSize: 12 }}>
          <div><Typography.Text strong>{t("instance.runtime.environment")}: </Typography.Text>{Object.entries(inst.env || {}).map(([key, value]) => <Tag key={key}>{key}={value}</Tag>)}</div>
          {!!inst.mounts?.length && <div style={{ marginTop: 4 }}><Typography.Text strong>{t("instance.runtime.mounts")}: </Typography.Text>{inst.mounts.map((mount) => <Tag key={mount.target}>{mount.target}{mount.read_only ? ` (${t("instance.runtime.readOnly")})` : ""}</Tag>)}</div>}
          {!!inst.shm_bytes && <div style={{ marginTop: 4 }}><Typography.Text strong>{t("instance.runtime.shm")}: </Typography.Text><Tag>{t("instance.runtime.shmValue", { size: +(inst.shm_bytes / (1 << 30)).toFixed(2) })}</Tag></div>}
          {!inst.shm_bytes && sharesHostIPC(inst.docker_args) && <div style={{ marginTop: 4 }}><Typography.Text strong>{t("instance.runtime.shm")}: </Typography.Text><Tag>{t("instance.runtime.shmHost")}</Tag></div>}
          {!!inst.docker_args?.length && <div style={{ marginTop: 4 }}><Typography.Text strong>{t("instance.runtime.dockerArgs")}: </Typography.Text><Typography.Text code>{inst.docker_args.join(" ")}</Typography.Text></div>}
          <Typography.Text type="secondary">{t("instance.runtime.dgpu")}</Typography.Text>
        </div> }]} />
      ) : null}

      <InstanceLaunchFacts uuid={inst.uuid} />

      {inst.state === "failed" && (
        <div style={{ marginTop: 8 }}>
          <AIAssistant
            task="instance.diagnose"
            subject={{ kind: "instance", uuid: inst.uuid, label: inst.uuid.slice(0, 8) }}
            buttonText={t("instance.diagnoseButton")}
            buttonType="primary"
            buttonSize="small"
          />
        </div>
      )}

      {wakeSecs != null && (
        <Typography.Paragraph type="secondary" style={{ margin: "8px 0 0", fontSize: 12 }}>
          {t("instance.wakeEstimate", { seconds: wakeSecs < 1 ? wakeSecs.toFixed(1) : Math.round(wakeSecs) })}
        </Typography.Paragraph>
      )}
      {sleepLevel > 0 && weightGib != null && (
        <Typography.Paragraph type="secondary" style={{ margin: "2px 0 0", fontSize: 12 }}>
          {t("instance.sleepWeight", { storage: sleepLevel === 1 ? t("instance.hostMemory") : t("instance.disk"), gib: weightGib.toFixed(1) })}
        </Typography.Paragraph>
      )}

      {schedule?.breakdown && inst.measured?.Found ? (
        <div style={{ marginTop: 12 }}>
          {chosenGpus?.length ? (
            <Typography.Paragraph type="secondary" style={{ marginBottom: 8, fontSize: 12 }}>
              {t("instance.selectedGpu")}
              {chosenGpus.map((g) => (
                <Tag key={g} color="blue" style={{ marginLeft: 4 }}>
                  GPU{g}
                </Tag>
              ))}
            </Typography.Paragraph>
          ) : null}
          <VRAMBreakdown breakdown={{ ...schedule.breakdown, source: "measured" }} measured={inst.measured} />
        </div>
      ) : schedule?.breakdown ? (
        <>
          <Typography.Text type="secondary" style={{ display: "block", marginTop: 12 }}>{t("instanceNoMeasuredMemory")}</Typography.Text>
        </>
      ) : null}

      <Tabs
        style={{ marginTop: 12 }}
        activeKey={activeTab}
        onChange={setActiveTab}
        // The follow toggle only makes sense for the log stream, so keep it
        // out of the tab bar while metrics is selected.
        tabBarExtraContent={
          activeTab === "logs"
            ? {
                right: (
                  <Space>
                    <TimeRangePicker
                      value={logRange}
                      onChange={setLogRange}
                      size="small"
                      liveLabel={live ? t("timeRange.live") : t("timeRange.recent")}
                    />
                    <Checkbox checked={followLogs} onChange={(e) => setFollowLogs(e.target.checked)}>
                      {t("instance.followLogs")}
                    </Checkbox>
                  </Space>
                ),
              }
            : undefined
        }
        items={[
          {
            key: "logs",
            label: t("instance.tab.logs"),
            // Keep the log tab available after a container exits so the
            // persisted tail can be inspected for failure diagnostics. State
            // changes, schedule outcomes and errors are merged in as system
            // lines rather than living on their own tab.
            children: <LogStream uuid={inst.uuid} live={live} follow={followLogs} range={logRange} timeline={timeline} />,
          },
          {
            key: "metrics",
            label: t("instance.tab.metrics"),
            children: <InstanceMetricsTab inst={inst} params={params} />,
          },
        ]}
      />
    </div>
  );
}

/**
 * startupStages lists the phases this instance has passed through, in the order
 * it passed through them. The timeline arrives newest-first, so it is walked
 * backwards: a stage list that ran the other way would read as the startup
 * happening in reverse.
 */
function startupStages(events: InstanceEvent[]): Array<{ phase: string; message: string }> {
  const seen = new Map<string, string>();
  for (let index = events.length - 1; index >= 0; index--) {
    const stage = instanceEventStage(events[index]);
    if (stage) seen.set(stage.phase, stage.message);
  }
  return Array.from(seen, ([phase, message]) => ({ phase, message }));
}
