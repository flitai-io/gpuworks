import { Alert, Button, Modal, Segmented, Space, Table, Tag, Tooltip, Typography } from "antd";
import AIAssistant from "../components/AIAssistant";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import {
  api,
  type Deployment,
  type DeploymentCandidate,
  type DeploymentCandidates,
  type DeploymentCandidateStatus,
  type ScheduleMessage,
} from "../api";
import { apiErrorText } from "../apiError";
import { systemLabelName, systemLabelValue } from "../components/SystemLabelTags";
import { scheduleText } from "./deploy/deploySchedule";
import { PreparationLabel } from "./DeploymentPreparation";
import { isModelFileReady } from "../storageTypes";

const PAGE_SIZE = 50;

type ClosestNode = { node?: string; name?: string; reason?: ScheduleMessage };

// The controller's reason with the refusal it names already rendered: the
// translation interpolates {{first}}, which the server sends as a code.
export function reasonWithFirst(reason: ScheduleMessage, render: (value: ScheduleMessage) => string): ScheduleMessage {
  const first = reason.params?.first_reason as ScheduleMessage | undefined;
  if (!first) return reason;
  return { ...reason, params: { ...reason.params, first: render(first) } };
}

// RequirementTags shows a deployment's node requirements, one tag per key with
// its alternatives: as the node pages show them when the server described the
// values (a node or network by name, a value no node carries flagged), else
// as key=value; "any node" when it states none.
export function RequirementTags({ dep }: { dep: Deployment }) {
  const { t } = useTranslation("deployments");
  const { t: tNodes } = useTranslation("nodes");
  const entries = Object.entries(dep.node_requirements ?? {});
  if (entries.length === 0) return <Typography.Text type="secondary">{t("detail.placement.noRequirements")}</Typography.Text>;
  const described = new Map((dep.node_requirement_labels ?? []).map((label) => [`${label.key}=${label.value}`, label]));
  return (
    <Space size={[4, 4]} wrap>
      {entries.map(([key, values]) => {
        const labels = values.map((value) => described.get(`${key}=${value}`));
        if (labels.some((label) => !label)) return <Tag key={key}>{`${key}=${values.join(" | ")}`}</Tag>;
        const missing = labels.some((label) => label!.nodes === 0);
        const name = key.startsWith("gpuworks.ai/") ? systemLabelName(key, tNodes) : key;
        const shown = labels.map((label) => {
          const text = systemLabelValue(label!, tNodes);
          return label!.nodes === 0 ? `${text} (${t("detail.placement.noNode")})` : text;
        });
        return (
          <Tooltip key={key} title={`${key}=${values.join(" | ")}`}>
            <Tag color={missing ? "warning" : undefined}>{`${name}: ${shown.join(" | ")}`}</Tag>
          </Tooltip>
        );
      })}
    </Space>
  );
}

// A failed model preparation is the known cause of the replicas not starting,
// exactly as a copy still running is: the scheduler's reason describes a
// launch it refused BEFORE the copy failed, so presenting it as the
// explanation points the operator at the wrong thing (a full GPU is not why a
// copy hit "permission denied").
const preparationFailedReason = "MODEL_PREPARATION_FAILED";

// PlacementStatusAlert says how many of an ordinary deployment's replicas
// wait for a node, why (the controller's latest reason and the closest
// nodes), and opens the live candidate table.
export function PlacementStatusAlert({ dep }: { dep: Deployment }) {
  const { t, i18n } = useTranslation("deployments");
  const { t: tDeploy } = useTranslation("deploy");
  const [open, setOpen] = useState(false);
  const status = dep.placement_status;
  if (!status || status.unscheduled <= 0 || dep.pd) return null;
  // A disk block needs operator action even while another model is preparing.
  const diskFull = dep.model_progress?.find((item) => !isModelFileReady(item.state) && item.preparation?.reason === "MODEL_CACHE_DISK_FULL");
  const preparing = diskFull ? undefined : dep.model_progress?.find((item) => !isModelFileReady(item.state) && item.preparation && ["downloading", "syncing", "checking", "copying", "verifying", "installing", "evicting"].includes(item.preparation.phase));
  // A preparation that FAILED reports phase "waiting" with a reason, so it is
  // not "preparing" any more - but it is still the reason no replica starts.
  const failed = diskFull || preparing ? undefined : dep.model_progress?.find((item) => !isModelFileReady(item.state) && item.preparation?.reason === preparationFailedReason);
  const blocked = diskFull ?? failed;
  const render = (value: ScheduleMessage) => scheduleText(tDeploy, value);
  const reason = !preparing && !blocked && status.reason ? render(reasonWithFirst(status.reason, render)) : "";
  const closest = preparing || blocked ? [] : (status.reason?.params?.closest as ClosestNode[] | undefined) ?? [];
  const at = !preparing && !blocked && status.at ? new Date(status.at).toLocaleString(i18n.language) : "";
  const headline = diskFull
    ? t("detail.placement.diskFull", { count: status.unscheduled })
    : failed ? t("detail.placement.preparationFailed", { count: status.unscheduled })
    : t(preparing ? "detail.placement.preparingModel" : "detail.placement.waiting", { count: status.unscheduled });
  return (
    <>
      <Alert
        type={failed ? "error" : preparing && !preparing.preparation?.reason ? "info" : "warning"}
        showIcon
        style={{ marginTop: 16 }}
        message={headline}
        description={
          <Space direction="vertical" size={4} style={{ width: "100%" }}>
            {preparing && <Typography.Text><PreparationLabel item={preparing} /></Typography.Text>}
            {blocked && <Typography.Text><PreparationLabel item={blocked} /></Typography.Text>}
            {/* The agent's own words: the part that says whether a retry can work. */}
            {blocked?.message && <Typography.Text code>{blocked.message}</Typography.Text>}
            {failed && <Typography.Text type="secondary">{t("preparation.noAutoRetry")}</Typography.Text>}
            {reason && <Typography.Text>{reason}</Typography.Text>}
            {closest.length > 0 && (
              <div>
                <Typography.Text type="secondary">{t("detail.placement.closest")}</Typography.Text>
                <ul style={{ margin: "4px 0 0", paddingInlineStart: 20 }}>
                  {closest.map((item) => (
                    <li key={item.node ?? item.name}>
                      <Typography.Text strong>{item.name ?? item.node}</Typography.Text>
                      {item.reason ? ` — ${render(item.reason)}` : ""}
                    </li>
                  ))}
                </ul>
              </div>
            )}
            {at && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("detail.placement.recorded", { time: at })}</Typography.Text>}
          </Space>
        }
        action={
          <Space direction="vertical" size={4}>
            <Button size="small" onClick={() => setOpen(true)}>{t("detail.placement.view")}</Button>
            <AIAssistant task="deployment.unscheduled" subject={{ kind: "deployment", id: dep.id, label: dep.name }} buttonSize="small" />
          </Space>
        }
      />
      {open && <CandidatesModal dep={dep} onClose={() => setOpen(false)} />}
    </>
  );
}

const statusColor: Record<DeploymentCandidateStatus, string> = {
  room: "green",
  busy: "orange",
  unmet: "default",
  cordoned: "orange",
  offline: "red",
};

// CandidatesModal is the cluster as the deployment's next replica sees it,
// one server page at a time: a thousand nodes are never loaded at once.
export function CandidatesModal({ dep, onClose }: { dep: Deployment; onClose: () => void }) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const { t: tDeploy } = useTranslation("deploy");
  const [filter, setFilter] = useState<DeploymentCandidateStatus | "all">("all");
  const [page, setPage] = useState(1);
  const [data, setData] = useState<DeploymentCandidates | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api
      .deploymentCandidates(dep.id, {
        status: filter === "all" ? undefined : filter,
        offset: (page - 1) * PAGE_SIZE,
        limit: PAGE_SIZE,
      })
      .then((result) => {
        if (cancelled) return;
        setData(result);
        setError("");
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(apiErrorText(tCommon, err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [dep.id, filter, page, tCommon]);

  const counts = data?.counts;
  const options = (["all", "room", "busy", "unmet", "cordoned", "offline"] as const).map((value) => ({
    value,
    label:
      value === "all"
        ? `${t("detail.placement.status.all")} ${data?.nodes ?? ""}`.trim()
        : `${t(`detail.placement.status.${value}`)} ${counts?.[value] ?? ""}`.trim(),
  }));

  return (
    <Modal open title={t("detail.placement.candidates")} onCancel={onClose} footer={null} width={860}>
      <Space direction="vertical" style={{ width: "100%" }}>
        <Space size={4} wrap>
          <Typography.Text type="secondary">{t("detail.placement.requirements")}:</Typography.Text>
          <RequirementTags dep={dep} />
          {dep.placement_strategy && <Tag color="blue">{t(`detail.placement.strategy.${dep.placement_strategy}`, { defaultValue: dep.placement_strategy })}</Tag>}
        </Space>
        {data && (
          <Typography.Text type="secondary">
            {t("detail.placement.summary", { nodes: data.nodes, room: counts?.room ?? 0, busy: counts?.busy ?? 0, unmet: counts?.unmet ?? 0, cordoned: counts?.cordoned ?? 0, offline: counts?.offline ?? 0 })}
          </Typography.Text>
        )}
        <Segmented
          options={options}
          value={filter}
          onChange={(value) => {
            setFilter(value as DeploymentCandidateStatus | "all");
            setPage(1);
          }}
        />
        {error && <Alert type="error" showIcon message={t("detail.placement.loadFailed", { detail: error })} />}
        <Table<DeploymentCandidate>
          rowKey="uuid"
          size="small"
          loading={loading}
          dataSource={data?.items ?? []}
          pagination={{ current: page, pageSize: PAGE_SIZE, total: data?.total ?? 0, showSizeChanger: false, onChange: setPage }}
          columns={[
            { title: t("detail.placement.columns.rank"), dataIndex: "rank", width: 64, render: (rank?: number) => rank ?? "—" },
            { title: t("detail.placement.columns.node"), dataIndex: "name" },
            {
              title: t("detail.placement.columns.status"),
              dataIndex: "status",
              render: (value: DeploymentCandidateStatus) => <Tag color={statusColor[value]}>{t(`detail.placement.status.${value}`)}</Tag>,
            },
            { title: t("detail.placement.columns.replicas"), dataIndex: "replicas", width: 96 },
            {
              title: t("detail.placement.columns.gpus"),
              key: "gpus",
              width: 120,
              render: (_: unknown, row: DeploymentCandidate) => `${row.free_gpus} / ${row.gpus}`,
            },
            { title: t("detail.placement.columns.model"), dataIndex: "model", width: 96, render: (value: string) => t(`detail.placement.model.${value}`) },
            {
              title: t("detail.placement.columns.detail"),
              key: "detail",
              render: (_: unknown, row: DeploymentCandidate) => (row.reason ? scheduleText(tDeploy, row.reason) : (row.detail ?? "")),
            },
          ]}
        />
      </Space>
    </Modal>
  );
}
