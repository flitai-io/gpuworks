import type { TFunction } from "i18next";
import { Badge } from "../components/ui/badge";
import { OptionSelect } from "../ui-kit/OptionSelect";
import { message } from "../ui-kit/feedback";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { api, type Deployment, type RoutingPolicyOption } from "../api";
import { apiErrorText } from "../apiError";
import { effectiveDeploymentState, partlyAsleep, detailBadgeTone, stateLabel } from "./state";
import CopyButton from "../components/CopyButton";
import DetailSection from "../components/detail/DetailSection";
import { HealthCheckHelp } from "../components/DeploymentHealth";
import { TokenizeHelp } from "../components/DeploymentTokenize";
import RoutingPolicyHelp from "../components/RoutingPolicyHelp";
import { HealthCheckEditor } from "./DeploymentHealthEditor";
import { DeploymentPreparation } from "./DeploymentPreparation";
import DeploymentInfoField, { DeploymentInfoList } from "./DeploymentInfoField";
import DeploymentPdPanel from "./DeploymentPdPanel";
import { ReplicaRetryAlert } from "./DeploymentLifecycleActions";
import DeploymentSchedulingPolicy from "./DeploymentSchedulingPolicy";
import SleepControls from "./DeploymentSleepControls";
import PlacementModeControl from "./DeploymentPlacementMode";
import { PlacementStatusAlert, RequirementTags } from "./DeploymentPlacementStatus";
import { replicaCount, ReplicasEditor } from "./DeploymentReplicaActions";
import { EditRequirementsButton } from "./DeploymentRequirementsModal";
import { TokenizeEditor } from "./DeploymentTokenizeEditor";

type Props = {
  dep: Deployment;
  onChanged: () => Promise<void>;
  routingPolicies: RoutingPolicyOption[];
  routingOptions: Array<{ value: RoutingPolicyOption["value"]; label: string }>;
};

export default function DeploymentDetailOverview(props: Props) {
  const { dep, onChanged } = props;
  const { t } = useTranslation("deployments");
  const hasNodeConditions = !dep.pd && (dep.nnodes ?? 1) <= 1;
  const placementNotice = <PlacementStatusAlert dep={dep} />;
  return (
    <div className="deployment-overview-groups deployment-service-groups">
      <DetailSection title={t("detail.serviceOverview")}>
        <div className="deployment-overview-columns deployment-paired-fields">
          <DeploymentServiceFields {...props} />
          <DeploymentModelDetails dep={dep} />
        </div>
      </DetailSection>
      <DetailSection title={t("detail.schedulingInformation")}>
        <div className="deployment-scheduling-columns deployment-paired-fields">
          <div className="deployment-fields-column">
            <DeploymentInfoList>
              {nodeLocationFields(dep, t, !hasNodeConditions && placementNotice)}
              {dep.placement_mode && (
                <DeploymentInfoField label={t("unitsPlacement.label")}>
                  <PlacementModeControl dep={dep} onChanged={onChanged} />
                </DeploymentInfoField>
              )}
              {hasNodeConditions && (
                <DeploymentInfoField label={t("detail.placement.requirements")}>
                  <div className="deployment-field-stack">
                    <span className="inline-flex flex-wrap items-center gap-1">
                      <RequirementTags dep={dep} />
                      {dep.placement_strategy && <Badge size="sm" color="blue">{t(`detail.placement.strategy.${dep.placement_strategy}`, { defaultValue: dep.placement_strategy })}</Badge>}
                      <EditRequirementsButton dep={dep} onChanged={onChanged} />
                    </span>
                    {placementNotice}
                  </div>
                </DeploymentInfoField>
              )}
            </DeploymentInfoList>
          </div>
          <DeploymentSchedulingPolicy dep={dep} onChanged={onChanged} />
        </div>
      </DetailSection>
      {dep.pd && <DeploymentPdPanel key={dep.id} dep={dep} onChanged={onChanged} />}
      <DeploymentPreparation rows={dep.model_progress} onChanged={onChanged} />
    </div>
  );
}

function DeploymentServiceFields({ dep, onChanged, routingPolicies, routingOptions }: Props) {
  const { t } = useTranslation("deployments");
  const { t: tCommon } = useTranslation("common");
  const [routingBusy, setRoutingBusy] = useState(false);
  const desired = replicaCount(dep);
  const changeRouting = async (value: string) => {
    const policy = routingPolicies.find((item) => item.value === value);
    if (desired <= 1 || !policy) return;
    setRoutingBusy(true);
    try {
      await api.setDeploymentRouting(dep.id, policy.value);
      await onChanged();
      message.success(t("detail.routingUpdated"));
    } catch (error) {
      message.error(t("detail.routingFailed", { detail: apiErrorText(tCommon, error) }));
    } finally {
      setRoutingBusy(false);
    }
  };

  const badge = stateLabel(effectiveDeploymentState(dep), tCommon);
  const partial = partlyAsleep(dep);
  return (
    <DeploymentInfoList>
      {modelIdentityFields(dep, t)}
      <DeploymentInfoField label={t("detail.state")}>
        <div className="deployment-field-stack">
          <span className="inline-flex min-h-control-xs flex-wrap items-center gap-2">
            <Badge size="sm" variant="plain" status={partial ? "info" : detailBadgeTone[badge.status]}>{badge.text}</Badge>
            {partial && <span className="text-fg-subtle">{t("sleep.partial", { asleep: dep.asleep_replicas, total: desired })}</span>}
          </span>
          {!dep.pd && <ReplicaRetryAlert dep={dep} onChanged={onChanged} />}
        </div>
      </DeploymentInfoField>
      {!dep.pd && (
        <DeploymentInfoField label={t("detail.replicaCapacity")}>
          <ReplicasEditor key={`${dep.id}-${dep.replicas}`} dep={dep} onChanged={onChanged} />
        </DeploymentInfoField>
      )}
      <DeploymentInfoField label={t("detail.health")} hint={<HealthCheckHelp />}>
        <HealthCheckEditor key={`${dep.id}-${dep.health.threshold_sec}-${dep.health.status === "off"}`} dep={dep} onChanged={onChanged} />
      </DeploymentInfoField>
      <DeploymentInfoField label={t("detail.tokenize")} hint={<TokenizeHelp />}>
        <TokenizeEditor dep={dep} onChanged={onChanged} />
      </DeploymentInfoField>
      <DeploymentInfoField label={t("detail.sleepable")}>
        <div className="deployment-sleep-field">
          <span>{dep.sleepable ? t("detail.yes") : t("detail.no")}</span>
          <SleepControls key={dep.id} dep={dep} onDone={onChanged} />
        </div>
      </DeploymentInfoField>
      {desired > 1 && (
        <DeploymentInfoField label={t("detail.routingPolicy")} hint={<RoutingPolicyHelp policies={routingPolicies} />}>
          <OptionSelect size="xs" aria-label={t("detail.routingPolicy")} disabled={routingBusy || routingPolicies.length === 0} value={dep.routing_policy} onChange={changeRouting} options={routingOptions} />
        </DeploymentInfoField>
      )}
    </DeploymentInfoList>
  );
}

function modelIdentityFields(dep: Deployment, t: TFunction) {
  const model = dep.model_name || dep.model_id?.toString();
  return [
    <DeploymentInfoField key="model" label={t("detail.model")}>
      <span className="deployment-copy-value">
        <Link to={`/models?detail=${dep.model_id}`}>{model}</Link>
        {model && <CopyButton size="xs" text={model} label={t("detail.copyModel")} />}
      </span>
    </DeploymentInfoField>,
    <DeploymentInfoField key="servedName" label={t("detail.servedName")}>
      <span className="deployment-copy-value">
        <span>{dep.served_name || "—"}</span>
        {dep.served_name && <CopyButton size="xs" text={dep.served_name} label={t("detail.copyServedName")} />}
      </span>
    </DeploymentInfoField>,
  ];
}

function DeploymentModelDetails({ dep }: Pick<Props, "dep">) {
  const { t } = useTranslation("deployments");
  return (
    <div role="group" aria-label={t("detail.modelAndEngine")} className="deployment-fields-column">
      <DeploymentInfoList>
        <DeploymentInfoField label={t("detail.configuredRuntime")}>
          <div className="deployment-configured-runtime">
            <span>{dep.engine || "—"}</span>
            <span className="deployment-copy-value">
              <span className="text-fg-subtle">{dep.image || t("page.autoImage")}</span>
              {dep.image && <CopyButton size="xs" text={dep.image} label={t("detail.copyConfiguredImage")} />}
            </span>
          </div>
        </DeploymentInfoField>
        {dep.template_id && (
          <DeploymentInfoField label={t("detail.inferenceTemplate")}>
            <Badge size="sm" status={dep.template_deviated ? "warning" : "success"}>{dep.template_deviated ? t("detail.deviated") : t("detail.notDeviated")}</Badge>
            {dep.template_scenario && <span className="text-fg-subtle">{dep.template_scenario}</span>}
          </DeploymentInfoField>
        )}
      </DeploymentInfoList>
    </div>
  );
}

export function DeploymentRuntimeNodeLinks({ dep }: { dep: Pick<Deployment, "runtime_nodes"> }) {
  const { t } = useTranslation("deployments");
  const nodes = dep.runtime_nodes ?? [];
  if (!nodes.length) return <span className="text-fg-subtle">{t("page.runtimeNodes.none")}</span>;
  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      {nodes.map((node) => (
        <Link key={node.node_uuid} to="/nodes" state={{ nodeUUID: node.node_uuid }}>
          {node.node_name || node.node_uuid.slice(0, 8)}
          {nodes.length > 1 && <span className="text-fg-subtle"> ×{node.instances}</span>}
        </Link>
      ))}
    </span>
  );
}

type DeploymentLocation = Pick<Deployment, "node_uuid" | "node_uuids" | "runtime_nodes">;

function targetNodeUUIDs(dep: DeploymentLocation): string[] {
  if (dep.node_uuids?.length) return dep.node_uuids;
  return dep.node_uuid ? [dep.node_uuid] : [];
}

export function actualNodesDiffer(dep: DeploymentLocation): boolean {
  const targets = new Set(targetNodeUUIDs(dep));
  return (dep.runtime_nodes ?? []).some((node) => !targets.has(node.node_uuid));
}

function nodeLocationFields(dep: Deployment, t: TFunction, targetNotice: ReactNode) {
  const targets = targetNodeUUIDs(dep);
  const records = new Map((dep.runtime_nodes ?? []).map((node) => [node.node_uuid, node]));
  const outsideTarget = actualNodesDiffer(dep);
  const sameNodes = records.size > 0 && !outsideTarget && targets.every((uuid) => records.has(uuid));
  const names = targets.length > 1 ? dep.node_names ?? [] : [dep.node_name || dep.node_names?.[0]];
  return [
      <DeploymentInfoField key="target" label={t("detail.targetNode")}>
        <span className="inline-flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
          {targets.length === 0 && "—"}
          {targets.map((uuid, index) => (
            <Link key={`${index}-${uuid}`} to="/nodes" state={{ nodeUUID: uuid }}>
              {names[index] || records.get(uuid)?.node_name || uuid.slice(0, 8)}
              {sameNodes && targets.length > 1 && <span className="text-fg-subtle"> ×{records.get(uuid)?.instances}</span>}
            </Link>
          ))}
          {sameNodes && <span className="text-fg-subtle">{t("detail.nodesMatchRecords")}</span>}
        </span>
        {targetNotice}
      </DeploymentInfoField>,
      !sameNodes && (
        <DeploymentInfoField key="recordedNodes" label={t("detail.node")}>
          <span className="inline-flex flex-wrap items-center gap-x-1.5 gap-y-0.5">
            <DeploymentRuntimeNodeLinks dep={dep} />
            {targets.length > 0 && outsideTarget && <span className="text-fg-subtle">{t("detail.nodesOutsideTarget")}</span>}
          </span>
        </DeploymentInfoField>
      ),
  ];
}
