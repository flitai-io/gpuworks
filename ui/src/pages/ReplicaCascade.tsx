import { useTranslation } from "react-i18next";
import type { Deployment, Instance } from "../api";
import { Badge } from "../components/ui/badge";
import { useIcon } from "../lib/icon-context";
import { Button } from "../components/ui/button";
import InstanceRecordPicker from "./InstanceRecordPicker";
import { useInstanceChoiceStatus } from "./InstanceSwitcher";
import { instanceStatusGroups, instanceSummaryState } from "./instanceGroups";
import { ReplicaActions } from "./DeploymentReplicaActions";
import { replicaContextLabel, replicaLabel, replicaLeader, replicaStatus, type ReplicaGroup } from "./replicaGroups";

export default function ReplicaCascade({ dep, replicas, replica, filter, visible, current, onFilter, onReplica, onInstance, onChanged }: {
  dep: Deployment; replicas: ReplicaGroup[]; replica: ReplicaGroup; filter: string; visible: Instance[]; current?: Instance;
  onChanged: () => void; onFilter: (value: string) => void; onReplica: (key: string) => void; onInstance: (uuid: string) => void;
}) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const ChevronLeft = useIcon("chevron-left");
  const ChevronRight = useIcon("chevron-right");
  const statusOf = useInstanceChoiceStatus();
  const leader = replicaLeader(replica);
  const allInstances = dep.instances ?? [];
  const index = visible.findIndex(inst => inst.uuid === current?.uuid);
  const states = instanceStatusGroups(allInstances, instanceSummaryState).map(group => ({
    ...statusOf(group[0]), count: group.length,
  }));
  const replicaOptions = replicas.map(group => {
    const status = replicaStatus(group, dep, t, tCommon);
    return { id: group.key, text: [replicaLabel(group, t), replicaContextLabel(group, dep, t, tCommon)].filter(Boolean).join(" · "),
      content: <span className="cascade-choice"><strong>{replicaLabel(group, t)}</strong><Badge variant="plain" size="sm" status={status.tone}>{status.label ?? t("detailLayout.inactive")}</Badge></span> };
  });
  const instanceOptions = visible.map(inst => {
    const {label, tone} = statusOf(inst);
    return { id: inst.uuid, text: inst.uuid + " " + label, content: <span className="cascade-choice">
      <span className="deployment-instance-id">{inst.uuid.slice(0, 12)}</span><Badge variant="plain" size="sm" status={tone}>{label}</Badge></span> };
  });
  return <div className="replica-cascade">
    {allInstances.length > 0 && <div className="cascade-filters" role="group" aria-label={t("detailLayout.recordState")}>
      <Button variant="tertiary" size="xs" className="rounded-full" active={filter === "all"} aria-pressed={filter === "all"} onClick={() => onFilter("all")}>
        {t("detail.allInstances")} {allInstances.length}
      </Button>
      {states.map(({state, label, tone, count}) => <Button key={state} variant="tertiary" size="xs" className="rounded-full"
        labelClassName="inline-flex items-center gap-1" active={filter === state} aria-pressed={filter === state}
        onClick={() => onFilter(state)}><Badge size="sm" variant="plain" status={tone} />{label} {count}</Button>)}
    </div>}
    <div className="cascade-row">
      <div className="cascade-replica-group" role="group" aria-label={t("detailLayout.replicaControls")}>
        <div className="cascade-field"><InstanceRecordPicker label={t("detailLayout.switchReplica")} choices={replicaOptions} selected={replica.key} onSelect={onReplica} /></div>
        {leader && <ReplicaActions key={leader.uuid} dep={dep} inst={leader} label={replicaLabel(replica, t)} onChanged={onChanged} />}
      </div>
      {allInstances.length > 0 && <div className="cascade-instance-group" role="group" aria-label={t("detail.switchInstance")}>
        <div className="cascade-field">
          <InstanceRecordPicker label={t("detail.switchInstance")} choices={instanceOptions} emptyText="—" selected={current?.uuid} onSelect={onInstance} />
        </div>
        <div className="cascade-stepper">
          <Button iconOnly size="sm" variant="tertiary" aria-label={t("detail.previousInstance")} title={t("detail.previousInstance")} disabled={index <= 0} onClick={() => onInstance(visible[index - 1].uuid)}>
            <ChevronLeft aria-hidden="true" />
          </Button>
          <Button iconOnly size="sm" variant="tertiary" aria-label={t("detail.nextInstance")} title={t("detail.nextInstance")} disabled={index < 0 || index >= visible.length - 1} onClick={() => onInstance(visible[index + 1].uuid)}>
            <ChevronRight aria-hidden="true" />
          </Button>
        </div>
      </div>}
    </div>
  </div>;
}
