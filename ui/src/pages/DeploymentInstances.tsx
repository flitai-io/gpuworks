import EmptyState from "../components/EmptyState";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Button } from "../components/ui/button";
import InferenceMetricsPanel from "../components/InferenceMetricsPanel";
import ReplicaCascade from "./ReplicaCascade";
import { deploymentReplicaGroups } from "./replicaGroups";
import { Space, Typography } from "antd";
import { Alert, Tag } from "../pages/DeploymentControls";
import { useTranslation } from "react-i18next";
import type { Deployment, Instance } from "../api";
import { isModelFileReady } from "../storageTypes";
import InstancePanel, { type InstanceView } from "./InstancePanel";
import { distRank, previousAttempts, instanceStatusGroups, instanceSummaryState } from "./instanceGroups";
import { imageMismatch } from "./instanceImage";

function readyModelAtLaunch(dep: Deployment, inst: Instance) {
  const progress = (dep.model_progress ?? []).filter(row => row.node_uuid === inst.node_uuid);
  const mounted = inst.model_files?.some(file => file.model_id === dep.model_id);
  const nodeReady = progress.length ? progress.every(row => isModelFileReady(row.state)) : !!inst.model_progress && isModelFileReady(inst.model_progress.state);
  if (!mounted && !(nodeReady && (!inst.model_progress || isModelFileReady(inst.model_progress.state)))) return;
  const node = progress.find(row => row.node_name)?.node_name
    ?? dep.runtime_nodes?.find(row => row.node_uuid === inst.node_uuid)?.node_name;
  return { name: dep.model_name, node };
}

export default function DeploymentInstances({ dep, focus, focusStartup, initialView, onChanged }: { dep: Deployment; focusStartup?: boolean; initialView?: InstanceView; focus?: string | null; onChanged: () => void }) {
  const { t } = useTranslation("deployments");
  const section = useRef<HTMLElement>(null);
  const logOffset = useRef<number | null>(null);
  const metricsTarget = useRef<HTMLDivElement>(null);
  const pendingFocus = useRef(focus);
  const [selectedUuid, setSelectedUuid] = useState(focus ?? null);
  const [selectedReplica, setSelectedReplica] = useState<string>();
  const [statusFilter, setStatusFilter] = useState("all");
  const [view, setView] = useState(initialView);
  const instances = dep.instances ?? [];
  const replicas = deploymentReplicaGroups(dep);
  const selected = instances.find(inst => inst.uuid === selectedUuid);
  const archived = view === "metrics" && !selected ? selectedUuid : null;
  const matchesFilter = (inst: Instance) => statusFilter === "all" || instanceSummaryState(inst) === statusFilter;
  const matchingReplicas = statusFilter === "all" ? replicas : replicas.filter(group => group.instances.some(matchesFilter));
  if (statusFilter !== "all" && !matchingReplicas.length) setStatusFilter("all");
  const replica = matchingReplicas.find(item => item.key === selectedReplica && (!selectedUuid || item.instances.length > 0))
    ?? matchingReplicas.find(item => selected && item.instances.includes(selected))
    ?? matchingReplicas.find(item => item.instances.length > 0)
    ?? matchingReplicas[0];
  const visible = replica?.instances.filter(matchesFilter) ?? [];
  const current = selected && visible.includes(selected) ? selected
    : visible.find(inst => inst.replica && inst.dist_role !== "follower") ?? instanceStatusGroups([...visible].reverse(), instanceSummaryState)[0]?.[0];
  if (current && !archived) {
    if (replica.key !== selectedReplica) setSelectedReplica(replica.key);
    if (current.uuid !== selectedUuid) setSelectedUuid(current.uuid);
  }
  const role = dep.pd?.roles.find((item) => item.role === current?.role);

  const differing = imageMismatch(instances).mismatched;
  const displayedUuid = archived ?? current?.uuid;
  useEffect(() => {
    if (!pendingFocus.current || pendingFocus.current !== displayedUuid) return;
    const target = focusStartup ? section.current?.querySelector<HTMLElement>(".instance-startup-progress")
      : initialView === "metrics" && !archived ? metricsTarget.current : section.current;
    if (!target) return;
    const frame = requestAnimationFrame(() => {
      pendingFocus.current = null;
      if (initialView === "metrics") target.focus({ preventScroll: true });
      target.scrollIntoView?.({ block: "start" });
    });
    return () => cancelAnimationFrame(frame);
  }, [displayedUuid, initialView, focusStartup, archived]);

  function logViewport() {
    const logs = section.current?.querySelector<HTMLElement>(".deployment-logs");
    if (!logs) return;
    const pane = logs.closest<HTMLElement>("#deployment-instances > section");
    const viewport = pane && getComputedStyle(pane).overflowY === "auto" ? pane : logs.closest<HTMLElement>(".detail-body");
    if (viewport) return { logs, viewport };
  }

  function preserveVisibleLog() {
    logOffset.current = null;
    const view = logViewport();
    if (!view) return;
    const rect = view.logs.getBoundingClientRect();
    const bounds = view.viewport.getBoundingClientRect();
    const bar = section.current?.querySelector(".replica-cascade")?.getBoundingClientRect();
    if (rect.bottom > Math.max(bounds.top, bar?.bottom ?? bounds.top) && rect.top < bounds.bottom) {
      logOffset.current = rect.top - bounds.top;
    }
  }

  useLayoutEffect(() => {
    const offset = logOffset.current;
    logOffset.current = null;
    if (offset === null) return;
    const view = logViewport();
    if (view) view.viewport.scrollTop += view.logs.getBoundingClientRect().top - view.viewport.getBoundingClientRect().top - offset;
  }, [displayedUuid]);

  const chooseReplica = (key: string) => {
    if (key === replica?.key) return;
    preserveVisibleLog();
    pendingFocus.current = null;
    setSelectedReplica(key);
    setSelectedUuid(null);
  };
  if (archived) return (
    <section id="deployment-instances" ref={section} tabIndex={-1}>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <h5 className="m-0 text-body font-semibold">{t("metrics.historicalInstance", { id: archived.slice(0, 12) })}</h5>
        <Button size="sm" variant="tertiary" onClick={() => {
          setSelectedUuid(null);
          setView("details");
        }}>{t("detail.viewInstances")}</Button>
      </div>
      <InferenceMetricsPanel instanceUuid={archived} historical />
    </section>
  );
  return (
    <section id="deployment-instances" ref={section} tabIndex={-1}>
      {instances.length > 0 && replica && <ReplicaCascade dep={dep} replicas={matchingReplicas} replica={replica} current={current}
        visible={visible} filter={statusFilter} onFilter={value => { preserveVisibleLog(); setStatusFilter(value); }} onReplica={chooseReplica} onInstance={uuid => { preserveVisibleLog(); setSelectedUuid(uuid); }} onChanged={onChanged} />}
      <section aria-label={t("detail.instanceDetails")}>
      {differing.size > 0 && (
        <Alert
          type="warning"
          showIcon
          role="status"
          style={{ marginBottom: 12 }}
          message={t("detail.imageMismatch", { count: differing.size })}
        />
      )}
      {role && (
        <Space size={8} style={{ marginBottom: 4 }}>
          <Tag color="geekblue">{t(`detail.pd.roleLabel.${role.role}`)}</Tag>
          <Typography.Text type="secondary">TP {role.tp} · {role.ready}/{role.desired}</Typography.Text>
        </Space>
      )}
      {current ? (
        <InstancePanel
          key={current.uuid}
          nodeName={dep.runtime_nodes?.find(n => n.node_uuid === current.node_uuid)?.node_name}
          showNode={(dep.runtime_nodes?.length ?? 0) > 1}
          metricsRef={metricsTarget}
          inst={current}
          readyModel={readyModelAtLaunch(dep, current)}
          sleepable={dep.sleepable}
          params={dep.quick_params}
          previousAttempt={previousAttempts(instances, dep.replicas).has(current.uuid)}
          rank={distRank(current, instances)}
          imageDiffers={differing.has(current.uuid)}
        />
      ) : (
        <EmptyState kind="deployments" description={t("detail.noInstances")} />
      )}
      </section>
    </section>
  );
}
