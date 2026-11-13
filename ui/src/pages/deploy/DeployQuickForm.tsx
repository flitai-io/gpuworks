import { useMemo } from "react";
import { Form, Input, InputNumber, Space, Tag, Typography } from "antd";
import { MoonOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import type {
  EligibleNode,
  EngineParamSpec,
  EngineProfile,
  EvaluateResponse,
  ImageCatalogEntry,
  InferenceTemplate,
  Model,
  Node,
  NodeFacets,
  PartitionCapability,
} from "../../api";
import DeployAdvancedSection from "./DeployAdvancedSection";
import DeployImageRow from "./DeployImageRow";
import DeployKvOffloadSection from "./DeployKvOffloadSection";
import DeployComponentAcquisition from "./DeployComponentAcquisition";
import DeployNodeAcquisition from "./DeployNodeAcquisition";
import DeployYieldPolicyField from "./DeployYieldPolicyField";
import DeployParamField from "./DeployParamField";
import DeployParallelFields, { DeployParallelRow } from "./DeployParallelFields";
import FeatureChoice from "./DeployFeatureChoice";
import NodeSection from "./DeployNodeSection";
import { ChoiceCard } from "./DeployChoiceCards";
import { effectiveShmDefaultBytes } from "./shmDefault";
import ContainerOverridesEditor from "./ContainerOverridesEditor";
import DeployWeightSourceFields from "./DeployWeightSourceFields";
import {
  kvTierSpecs,
  quickParamSpecs,
} from "./deployHelpers";
import { capabilityAllowsPartition } from "../../components/partitionCapabilities";
import { LockedGroup, LockedHint, useControlLocked } from "./DeployEditContext";
import { defaultPdMode, defaultPdRoles, pdModeOf, pdPolicyValue } from "./deployPd";

type Props = {
  templates: InferenceTemplate[];
  // What the online nodes carry, and the nodes the draft names (read one by
  // one): the form never lists the fleet.
  fleet?: NodeFacets | null;
  knownNodes: Map<string, Node>;
  // The node whose facts the cards render: the form's pick in manual mode,
  // the server's selection in automatic mode (display only).
  selectedNode?: Node;
  // The dGPU backend a quota would use: the picked node's in manual mode,
  // what the eligible pool offers in automatic mode.
  partitionCapability?: PartitionCapability;
  eligibleNodes?: EligibleNode[];
  selectedModel?: Model;
  profile?: EngineProfile;
  // The cluster default the start timeout field shows while it is empty.
  startTimeoutDefaultSec?: number;
  engine: string;
  images: ImageCatalogEntry[];
  result: EvaluateResponse | null;
  // A re-evaluation is in flight: `result` still answers the previous draft.
  loading: boolean;
};

export default function DeployQuickForm({
  startTimeoutDefaultSec,
  templates,
  fleet,
  knownNodes,
  selectedNode,
  partitionCapability,
  eligibleNodes,
  selectedModel,
  profile,
  engine,
  images,
  result,
  loading,
}: Props) {
  const { t } = useTranslation("deploy");
  const form = Form.useFormInstance();
  const templateID = Form.useWatch("template_id", { form, preserve: true });
  const specSource = Form.useWatch("template_spec_source", {
    form,
    preserve: true,
  });
  const specName = Form.useWatch("template_spec_name", {
    form,
    preserve: true,
  });
  const nodeMode = String(
    Form.useWatch("node_selection_mode", { form, preserve: true }) || "auto",
  );
  const pdMode = nodeMode === "pd";
  const unitsPlacement = Form.useWatch("units_placement", { form, preserve: true });
  const replicasLocked = useControlLocked("replicas");
  const vramLocked = useControlLocked("vram_limit");
  const selectedTemplate = templates.find(
    (template) => template.id === templateID,
  );
  const selectedSpec = specName
    ? selectedTemplate?.specs.find(
        (candidate) =>
          candidate.name === specName &&
          (!specSource || candidate.source === specSource),
      )
    : undefined;
  const selection = selectedSpec ? { spec: selectedSpec } : undefined;
  const specs = quickParamSpecs(
    profile,
    engine,
    selectedModel?.input_modalities,
  );
  const specMap = useMemo(
    () => new Map(specs.map((spec) => [spec.key, spec])),
    [specs],
  );
  // The dictionary owns the quick surface: layout "quick" rows in their
  // declared order, everything else in the advanced fold. The order buckets
  // are the resource row, the KV tiers, then the feature grid.
  const byOrder = (a: EngineParamSpec, b: EngineParamSpec) =>
    (a.order ?? 0) - (b.order ?? 0) || a.key.localeCompare(b.key);
  // The dictionary owns the quick surface through its layout column. A
  // dictionary that predates the column falls back to the profile's
  // quick_form_keys whitelist, so an older server still renders.
  const hasLayout = specs.some(
    (spec) =>
      spec.layout === "quick" ||
      spec.layout === "expert" ||
      spec.layout === "hidden",
  );
  const quickLayoutSpecs = hasLayout
    ? specs.filter((spec) => spec.layout === "quick").sort(byOrder)
    : quickParamSpecs(profile, engine, selectedModel?.input_modalities);
  const quickKeys = new Set(quickLayoutSpecs.map((spec) => spec.key));
  const modelLenSpec = specMap.get("max_model_len");
  const memorySpec =
    specMap.get("gpu_memory_utilization") || specMap.get("mem_fraction_static");
  const epSpec = specMap.get("enable_expert_parallel");
  const { memory: kvMemorySpec, disk: kvDiskSpec } = kvTierSpecs(specs);
  // The surface's rows are key roles, not order ranges: the resource row's
  // lookups, the KV tiers with their depends_on companions, and the feature
  // grid for the rest.
  const rowOrKvKeys = new Set(
    [
      modelLenSpec?.key,
      memorySpec?.key,
      epSpec?.key,
      kvMemorySpec?.key,
      kvDiskSpec?.key,
      ...quickLayoutSpecs
        .filter((spec) => spec.depends_on === "kv_disk_offload")
        .map((spec) => spec.key),
    ].filter(Boolean) as string[],
  );
  const kvDiskCompanionSpecs = kvTierSpecs(quickLayoutSpecs).companions;
  // Feature cards carry switches, selects and comboboxes. A quick-surface row
  // with a plain value control (a fallback dictionary's number field) renders
  // as its ordinary field in the advanced fold instead of a choice card.
  const featureControls = new Set([
    "switch",
    "select",
    "combobox",
    "segmented",
  ]);
  const featureSpecs = quickLayoutSpecs.filter(
    (spec) =>
      !rowOrKvKeys.has(spec.key) &&
      (spec.type === "boolean" || featureControls.has(String(spec.control))),
  );
  const quickValueFieldKeys = new Set(
    quickLayoutSpecs
      .filter(
        (spec) =>
          !rowOrKvKeys.has(spec.key) &&
          spec.type !== "boolean" &&
          !featureControls.has(String(spec.control)),
      )
      .map((spec) => spec.key),
  );
  const advancedSpecs = specs.filter(
    (spec) =>
      (!quickKeys.has(spec.key) || quickValueFieldKeys.has(spec.key)) &&
      spec.layout !== "hidden",
  );
  const epValue = Boolean(
    Form.useWatch(epSpec?.key || "__missing_ep", { form, preserve: true }),
  );
  const sleepable = Boolean(
    Form.useWatch("sleepable", { form, preserve: true }),
  );
  const partition = Form.useWatch("vram_limit_gib", { form, preserve: true });
  // A hand-written command is the launch configuration (ADR 0020 decision 6):
  // the dictionary parameters no longer reach the engine and grey out, while
  // placement — node, GPUs, tensor parallelism, VRAM partition — still comes
  // from this form and stays editable.
  const manualCommand =
    String(Form.useWatch("command_mode", { form, preserve: true }) || "") ===
    "manual";
  // The selected spec's tokens outside the parameter dictionary travel with the
  // deployment in the server's selection snapshot, not in this form. They are
  // shown so the launch is not half hidden, and shown read-only so they cannot
  // be mistaken for the user's own arguments (ADR 0020 decision 4).
  const templateArgs = result?.template_residual_args ?? [];

  const setNodeMode = (mode: "auto" | "manual" | "multi" | "pd") => {
    // Multi-node and PD groups are placed by the platform by default (the
    // same auto / manual switch as a single node's), and pack by default
    // where a single node spreads. A strategy the deployer picked is kept.
    const grouped = (m: string) => m === "multi" || m === "pd";
    const crossing = grouped(String(form.getFieldValue("node_selection_mode") || "auto")) !== grouped(mode);
    const strategy = form.getFieldValue("placement_strategy");
    const placement = {
      ...(grouped(mode)
        ? { units_placement: form.getFieldValue("units_placement") ?? "auto" }
        : {}),
      ...(crossing
        ? {
            placement_strategy:
              form.getFieldValue("placement_strategy_chosen") && strategy
                ? strategy
                : grouped(mode) ? "binpack" : "spread",
          }
        : {}),
    };
    if (mode === "pd") {
      // PD names its own nodes per role unit (DeployPdSection); the ordinary
      // single/multi-node fields do not apply and stay at their rest values.
      const pdMode =
        form.getFieldValue("pd_mode") ||
        defaultPdMode(profile, form.getFieldValue("target_vendor"));
      const mode = pdModeOf(profile, pdMode);
      form.setFieldsValue({
        ...placement,
        node_selection_mode: "pd",
        nnodes: 1,
        replicas: 1,
        node_uuid: undefined,
        distributed_nodes: undefined,
        pd_mode: pdMode,
        // The mode's defaults, unless the deployer already chose one it allows.
        pd_fallback: pdPolicyValue(mode?.fallbacks, form.getFieldValue("pd_fallback")),
        pd_kv_load_failure: pdPolicyValue(mode?.kv_load_failures, form.getFieldValue("pd_kv_load_failure")),
        // A draft the deployer already filled in survives switching away and
        // back; only a first-time entry gets the empty-role defaults.
        pd_roles: form.getFieldValue("pd_roles") || defaultPdRoles(),
      });
      return;
    }
    const nodesCount =
      mode === "multi"
        ? Math.max(2, Number(form.getFieldValue("nnodes") || 2))
        : 1;
    form.setFieldsValue({
      ...placement,
      node_selection_mode: mode,
      nnodes: nodesCount,
      distributed_nodes:
        mode === "multi" ? form.getFieldValue("distributed_nodes") : undefined,
    });
  };

  return (
    <section className="deploy-workbench">
      <NodeSection
        fleet={fleet}
        knownNodes={knownNodes}
        eligibleNodes={eligibleNodes}
        selection={selection}
        selectedNode={selectedNode}
        modelReady={Boolean(selectedModel)}
        result={result}
        loading={loading}
        profile={profile}
        onNodeMode={setNodeMode}
      />
      {nodeMode === "multi" && (
        <DeployParallelRow
          result={result}
          disabled={!selectedModel}
          autoPlaced={unitsPlacement !== "manual"}
        />
      )}
      <DeployImageRow
        acquisition={result?.acquisition}
        evaluated={Boolean(result)}
        images={images}
        warnings={result?.warnings}
      />
      <LockedGroup controls={["model_source"]} hint={false}>
        <DeployWeightSourceFields
          acquisition={result?.acquisition}
          modelSource={selectedModel?.source}
          evaluating={loading}
        />
      </LockedGroup>
      <DeployComponentAcquisition acquisition={result?.acquisition} />
      <DeployNodeAcquisition
        acquisition={result?.acquisition}
        automatic={nodeMode === "auto"}
        loading={loading}
        declaredProviders={selectedSpec?.weight_sources.map((source) => source.provider)}
      />

      <div className="deploy-workbench-row">
        <div className="deploy-workbench-label">
          {t("workbench.resource.title")}
        </div>
        <div className="deploy-workbench-body">
          <div className="deploy-resource-grid">
            <Form.Item
              label={t("catalog.replicas")}
              name="replicas"
              extra={
                pdMode ? t("pd.replicasUnavailable") : t("catalog.replicasHelp")
              }
              rules={[
                {
                  validator: (_, value) =>
                    Number(form.getFieldValue("nnodes") || 1) > 1 &&
                    Number(value || 1) !== 1
                      ? Promise.reject(
                          new Error(t("catalog.replicasDistributed")),
                        )
                      : Promise.resolve(),
                },
              ]}
            >
              <InputNumber
                min={1}
                max={nodeMode === "multi" || pdMode ? 1 : 32}
                disabled={pdMode || replicasLocked}
              />
            </Form.Item>
            {nodeMode !== "multi" && !pdMode && (
              <DeployParallelFields
                result={result}
                multi={false}
                tpLabel={
                  <Space size={5}>
                    {t("workbench.resource.tp")}
                    <Tag color="blue">{t("workbench.automatic")}</Tag>
                  </Space>
                }
                ppLabel={t("workbench.resource.pp")}
                disabled={!selectedModel}
              />
            )}
            {selectedModel && modelLenSpec ? (
              <DeployParamField spec={modelLenSpec} />
            ) : (
              <div className="deploy-unavailable-field">
                <strong>{t("workbench.resource.maxContext")}</strong>
                <span>{t("workbench.parameterUnavailable")}</span>
              </div>
            )}
            {memorySpec ? (
              <DeployParamField spec={memorySpec} />
            ) : (
              <div className="deploy-unavailable-field">
                <strong>{t("workbench.resource.memoryUtilization")}</strong>
                <span>{t("workbench.parameterUnavailable")}</span>
              </div>
            )}
          </div>
          <div className="deploy-choice-strip">
            <ChoiceCard
              active={sleepable}
              title={t("workbench.resource.sleepable")}
              description={
                profile?.sleep.supported
                  ? t("workbench.resource.sleepableDescription")
                  : t("workbench.engineUnsupported")
              }
              icon={<MoonOutlined />}
              disabled={!profile?.sleep.supported}
              onClick={() => form.setFieldValue("sleepable", !sleepable)}
            />
            <ChoiceCard
              active={Boolean(partition)}
              title={t("workbench.resource.partition")}
              description={
                capabilityAllowsPartition(partitionCapability)
                  ? partition
                    ? `${partition} GiB`
                    : t("workbench.resource.fullGpu")
                  : nodeMode === "auto"
                    ? t("workbench.resource.dgpuUnavailablePool")
                    : t("workbench.resource.dgpuUnavailable")
              }
              disabled={!capabilityAllowsPartition(partitionCapability) || vramLocked}
              onClick={() =>
                form.setFieldValue("vram_limit_gib", partition ? undefined : 16)
              }
            />
            {partition && (
              <Form.Item name="vram_limit_gib" noStyle>
                <InputNumber min={1} addonAfter="GiB" disabled={vramLocked} />
              </Form.Item>
            )}
          </div>
          <DeployYieldPolicyField eligibility={result?.yield_policy_eligible} />
          <LockedHint controls={["replicas", "vram_limit"]} />
        </div>
      </div>

      <div className="deploy-workbench-row">
        <div className="deploy-workbench-label">
          {t("workbench.parallel.title")}
        </div>
        <div className="deploy-workbench-body">
          <div className="deploy-choice-strip">
            <ChoiceCard
              active
              title={t("workbench.parallel.tensor")}
              description={
                result?.parallelism
                  ? `TP ${result.parallelism.tp} · PP ${result.parallelism.pp}`
                  : t("workbench.automatic")
              }
            />
            <ChoiceCard
              active={epValue}
              disabled={!epSpec || manualCommand}
              title={t("workbench.parallel.expert")}
              description={
                epSpec
                  ? epValue
                    ? t("workbench.enabled")
                    : t("workbench.clickToEnable")
                  : selectedTemplate?.architecture === "moe"
                    ? t("workbench.engineUnavailable")
                    : t("workbench.parallel.moeOnly")
              }
              onClick={() => epSpec && form.setFieldValue(epSpec.key, !epValue)}
            />
          </div>
          <Typography.Text type="secondary">
            {t("workbench.parallel.help")}
          </Typography.Text>
        </div>
      </div>

      <DeployKvOffloadSection
        kvMemorySpec={kvMemorySpec}
        kvDiskSpec={kvDiskSpec}
        kvDiskCompanionSpecs={kvDiskCompanionSpecs}
        manualCommand={manualCommand}
      />

      <div className="deploy-workbench-row">
        <div className="deploy-workbench-label">
          {t("workbench.features.title")}
        </div>
        <div className="deploy-workbench-body">
          <div className="deploy-choice-strip deploy-feature-choices">
            {featureSpecs.length ? (
              featureSpecs.map((spec) => (
                <FeatureChoice
                  key={spec.key}
                  spec={spec}
                  disabled={manualCommand}
                />
              ))
            ) : (
              <Typography.Text type="secondary">
                {t("workbench.features.empty")}
              </Typography.Text>
            )}
          </div>
        </div>
      </div>

      <div className="deploy-workbench-row">
        <div className="deploy-workbench-label">
          {t("workbench.runtime.title")}
        </div>
        <div className="deploy-workbench-body">
          <Typography.Text type="secondary">
            {t("workbench.runtime.description")}
          </Typography.Text>
          <ContainerOverridesEditor
            shmDefaultBytes={effectiveShmDefaultBytes(
              profile?.shm_bytes,
              selectedSpec?.shm_bytes,
            )}
            // A PD draft resolves a size per role (only the prefill role
            // carries the CPU tier), so there is no single size to promise;
            // while a re-evaluation runs, the result answers the previous draft.
            shmResolvedBytes={
              result?.pd || loading ? undefined : result?.shm_bytes
            }
            startTimeoutDefaultSec={startTimeoutDefaultSec}
          />
        </div>
      </div>

      <DeployAdvancedSection
        advancedSpecs={advancedSpecs}
        manualCommand={manualCommand}
        templateArgs={templateArgs}
      />
      <Form.Item name="name" hidden>
        <Input />
      </Form.Item>
      <Form.Item name="node_selection_mode" hidden>
        <Input />
      </Form.Item>
      {nodeMode !== "multi" && (
        <Form.Item name="nnodes" hidden>
          <InputNumber />
        </Form.Item>
      )}
    </section>
  );
}
