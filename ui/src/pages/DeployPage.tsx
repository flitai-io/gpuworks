import { useEffect, useMemo, useRef, useState } from "react";
import { Button, Form, Result, Segmented, Select, Skeleton, Space, Typography } from "antd";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  api,
  usePolling,
  type ImageCatalogEntry,
  type EngineProfile,
  type ImageSeries,
  type List,
  type Model,
  type Node,
} from "../api";
import PageHeader from "../components/PageHeader";
import { APIError, apiErrorText } from "../apiError";
import RoutingPolicyHelp from "../components/RoutingPolicyHelp";
import DeployBasicStep from "./deploy/DeployBasicStep";
import DeployMovedFlagsNotice from "./deploy/DeployMovedFlagsNotice";
import DeployQuickForm from "./deploy/DeployQuickForm";
import DeployRuntimeCommand from "./deploy/DeployRuntimeCommand";
import DeployStepSummary from "./deploy/DeployStepSummary";
import DeployTemplateBar from "./deploy/DeployTemplateBar";
import DeployWeightSourceModal from "./deploy/DeployWeightSourceModal";
import {
  deploymentQuery,
  draftToFormValues,
  EngineDecidesContext,
  effectiveEngineDecidesParamKeys,
  draftPartitionCapability,
  type ImageSelectionMode,
} from "./deploy/deployHelpers";
import DeployEditConfirmModal from "./deploy/DeployEditConfirmModal";
import { DeployEditContext } from "./deploy/DeployEditContext";
import { cloneIdentity, templateSpecKey } from "./deploy/deployDraftValues";
import { DeployEditBack, DeployEditTitle } from "./deploy/DeployEditTitle";
import useDeployDraftSetup from "./deploy/useDeployDraftSetup";
import useDeployEditMode, { type OpenedDeployment } from "./deploy/useDeployEditMode";
import useDeploymentDraft from "./deploy/useDeploymentDraft";
import useDeployEvaluation from "./deploy/useDeployEvaluation";
import useDeploySubmit from "./deploy/useDeploySubmit";
import useDeployPrefill from "./deploy/useDeployPrefill";
import useDeployTemplateSelection from "./deploy/useDeployTemplateSelection";
import useDeployNodeSync from "./deploy/useDeployNodeSync";
import useTemplateWeightSource from "./deploy/useTemplateWeightSource";
import useDeployImageState from "./deploy/useDeployImageState";
import DeployExpertTabs from "./deploy/DeployExpertTabs";
import DeploySubmitBar from "./deploy/DeploySubmitBar";
import DeploySubmissionError from "./deploy/DeploySubmissionError";
import useDeploymentIdentity from "./deploy/useDeploymentIdentity";
import useDeployFormChanges from "./deploy/useDeployFormChanges";
import HealthCheckFields from "./deploy/HealthCheckFields";
import TokenizeField from "./deploy/TokenizeField";
import DeployNoOperationsNotice from "./deploy/DeployNoOperationsNotice";
import { localizedText } from "../i18n/localizedText";
import { useNodeFacets } from "../nodes/useNodeFacets";
import { useNodeLookup } from "../nodes/useNodeLookup";
import "./deploy/deployWizard.css";
import "./deploy/deployRuntimePreview.css";
import "./deploy/deployWizardResponsive.css";

const ONLINE = { state: ["online"] };
import "./deploy/deployLayout.css";
import "./deploy/deployEdit.css";

// DeployPage is the create page. Opened with edit=<id> (edit that deployment)
// or from=<id> (clone it and modify the copy) it first reads the deployment's
// draft, and fills the same form from it.
export default function DeployPage() {
  const { t } = useTranslation("deploy");
  const { t: tCommon } = useTranslation("common");
  const navigate = useNavigate();
  const location = useLocation();
  const { queryEditID, queryFromID } = useMemo(
    () => deploymentQuery(location.search),
    [location.search],
  );
  const openID = queryEditID ?? queryFromID;
  const { state, reload } = useDeploymentDraft(openID);
  if (!openID) return <DeployForm />;
  if (state.status === "error") {
    const missing = state.error instanceof APIError && state.error.status === 404;
    return (
      <div className="deploy-page">
        <Result
          status={missing ? "404" : "error"}
          title={t(missing ? "edit.notFound" : "edit.loadFailed")}
          subTitle={apiErrorText(tCommon, state.error)}
          extra={
            <Button type="primary" onClick={() => navigate("/deployments")}>
              {t("edit.backToList")}
            </Button>
          }
        />
      </div>
    );
  }
  if (state.status !== "ready" || state.id !== openID) {
    return (
      <div className="deploy-page" aria-busy="true">
        <Typography.Text type="secondary" role="status">
          {t("edit.loading")}
        </Typography.Text>
        <Skeleton active paragraph={{ rows: 10 }} />
      </div>
    );
  }
  return (
    <DeployForm
      key={`${queryEditID ? "edit" : "from"}-${openID}`}
      opened={{
        mode: queryEditID ? "edit" : "from",
        id: openID,
        draft: state.draft,
        deployment: state.deployment,
        reload,
      }}
    />
  );
}

function DeployForm({ opened }: { opened?: OpenedDeployment }) {
  const { t, i18n } = useTranslation("deploy");
  const [form] = Form.useForm();
  const watchOptions = { form, preserve: true } as const;
  const [deployMode, setDeployMode] = useState<"quick" | "expert">("quick");
  const navigate = useNavigate();
  const location = useLocation();
  const { queryTemplateID, querySpecName, querySpecSource, queryModelID, queryFocus } =
    useMemo(() => deploymentQuery(location.search), [location.search]);
  // The form of a deployment's draft starts from the draft (a clone starts
  // from a copy with its own names); the dictionary keys the deployment leaves
  // unstated are filled in once the engine profile is known.
  const [initialValues] = useState<Record<string, unknown>>(() => {
    if (!opened) {
      return {
        model_id: queryModelID,
        template_id: queryTemplateID,
        template_spec_source: querySpecSource,
        template_spec_name: querySpecName,
        image_mode: "auto",
        command_mode: "engine",
        // Automatic placement is the server's answer (selected_node with
        // its candidates); the manual option stays one click away.
        node_selection_mode: "auto",
        nnodes: 1,
        replicas: 1,
      };
    }
    const values = draftToFormValues(opened.draft.deployment);
    // A multi-node or PD draft reads as the operator's placement; the
    // deployment itself says when the platform places it.
    if (opened.deployment?.placement_mode === "auto" && values.units_placement)
      values.units_placement = "auto";
    return opened.mode === "from" ? cloneIdentity(values) : values;
  });
  const touchedRef = useRef(false);
  // The form never lists the fleet: what the online nodes carry comes from
  // the server's facets, and the few nodes the draft names are read one by
  // one (knownNodes, below).
  const fleet = useNodeFacets(ONLINE, 15000);
  const knownNodesRef = useRef<Node[]>([]);
  const targetVendor = Form.useWatch("target_vendor", watchOptions) as string | undefined;
  const engines = usePolling<List<EngineProfile>>(
    () => api.engines(targetVendor),
    60000,
    [targetVendor],
  );
  const routingPolicies = usePolling(api.routingPolicies, 0);
  // The cluster's instance start timeout is the placeholder of the form's own
  // start timeout field: an empty field keeps following it.
  const settings = usePolling(api.settings, 0);
  const startTimeoutDefaultSec =
    Number(settings.data?.deployment?.start_timeout_sec) > 0
      ? Number(settings.data?.deployment?.start_timeout_sec)
      : undefined;
  const routingOptions = useMemo(
    () => (routingPolicies.data?.items ?? []).map((policy) => ({ value: policy.value, label: localizedText(policy.label, i18n.language) })),
    [routingPolicies.data, i18n.language],
  );
  const templateIndex = usePolling(api.inferenceTemplateIndex, 0);
  const { selectedTemplate, templates, selectedTemplateSpec } =
    useDeployTemplateSelection({
      form,
      templateIndex: templateIndex.data ?? undefined,
      queryTemplateID,
      querySpecName,
      querySpecSource,
    });
  const images = usePolling<List<ImageCatalogEntry>>(api.imageVersions, 30000);
  const imageSeries = usePolling<List<ImageSeries>>(api.images, 30000);
  const selectedNodeUUID = Form.useWatch("node_uuid", watchOptions);
  // The default engine is the server's answer (evaluate's defaults), not a
  // literal here: an empty engine travels as omitted and the server picks.
  const selectedEngine = (Form.useWatch("engine", watchOptions) ||
    "") as string;
  const engineProfile = engines.loading ? undefined : engines.data?.items.find(
    (engine) => engine.name === selectedEngine,
  );
  useEffect(() => {
    if (!engines.loading && engineProfile && !engineProfile.sleep.supported) {
      form.setFieldValue("sleepable", false);
    }
  }, [engines.loading, engineProfile, form]);
  const nodeSelectionMode =
    Form.useWatch("node_selection_mode", watchOptions) || "manual";
  const formValues = (Form.useWatch([], watchOptions) || {}) as Record<
    string,
    unknown
  >;
  const selectedImage = Form.useWatch("image", watchOptions);
  const imageMode = Form.useWatch("image_mode", watchOptions) as
    ImageSelectionMode | undefined;
  const selectedSeriesID = Form.useWatch("image_series_id", watchOptions) as
    number | undefined;
  const selectedVersionID = Form.useWatch("image_version_id", watchOptions) as
    number | undefined;
  const selectedModelID = Form.useWatch("model_id", watchOptions);
  // The page never lists the models: the picker searches them, and the
  // chosen one is read (and kept fresh) by id.
  const selectedModelRead = usePolling<Model | null>(
    () => (selectedModelID ? api.model(selectedModelID) : Promise.resolve(null)),
    15000,
    [selectedModelID],
  );
  // A model read for an earlier choice never stands in for the current one.
  const selectedModel =
    selectedModelRead.data && selectedModelRead.data.id === selectedModelID
      ? selectedModelRead.data
      : undefined;
  // A form opened from a draft is complete once the profile and the model are
  // in (see useDeployDraftSetup); nothing is evaluated before.
  const draftReady = useDeployDraftSetup({
    form,
    request: opened?.draft.deployment ?? {},
    revision: opened?.draft.revision ?? "",
    profile: engineProfile,
    modalities: selectedModel?.input_modalities ?? [],
    settled: Boolean(opened) && !engines.loading && !selectedModelRead.loading,
  });
  const bodyOfRef = useRef<(values: Record<string, unknown>) => Record<string, unknown> | null>(undefined);
  const evaluationSignal = useMemo(
    () => ({ values: formValues, ready: !opened || draftReady }),
    [formValues, opened, draftReady],
  );
  const evaluation = useDeployEvaluation({
    form,
    signal: evaluationSignal,
    // Without a target accelerator the scheduler would pick any vendor's
    // node, and that answer would linger on screen once one is chosen.
    buildPayload: () => {
      if (!form.getFieldValue("target_vendor")) return null;
      if (opened && !draftReady) return null;
      const body = bodyOfRef.current?.(form.getFieldsValue(true));
      // An edit's preview leaves the deployment's own GPU use out of the plan.
      return body && opened?.mode === "edit"
        ? { ...body, deployment_id: opened.id }
        : body ?? null;
    },
    translate: (key, options) => i18n.t(key, options),
    // An edit keeps the deployment's own values; a clone takes the answer's
    // once the deployer has changed something, as a new deployment would.
    applyFormValues: () => !opened || (opened.mode === "from" && touchedRef.current),
  });
  const { result: evalResult, error: evalErr, evaluating } = evaluation;
  // The node whose facts the page renders: the form's pick in manual mode,
  // and in automatic mode the server's selection (the form field stays empty;
  // display only, the create confirms the selection explicitly).
  const distributedNodes = Form.useWatch("distributed_nodes", watchOptions);
  const knownNodes = useNodeLookup([
    selectedNodeUUID,
    evalResult?.selected_node?.uuid,
    ...(Array.isArray(distributedNodes) ? (distributedNodes as string[]) : []),
  ]);
  const knownList = useMemo(() => [...knownNodes.values()], [knownNodes]);
  knownNodesRef.current = knownList;
  const editMode = useDeployEditMode({
    form,
    opened,
    formValues,
    engineProfile,
    selectedModel,
    nodes: knownList,
    ready: draftReady,
    focus: queryFocus,
    onSaved: () => navigate(`/deployments?deployment=${opened?.id}`),
  });
  bodyOfRef.current = editMode.bodyOf;
  const selectedNode = knownNodes.get(
    (nodeSelectionMode === "manual"
      ? selectedNodeUUID
      : selectedNodeUUID || evalResult?.selected_node?.uuid) ?? "",
  );
  const selectedPartitionCapability = draftPartitionCapability(
    nodeSelectionMode,
    selectedNode,
    evalResult,
  );

  // Keys whose default this launch leaves to the engine: their controls
  // start at "automatic" (EngineDecidesContext), never hidden.
  const engineDecidesParamKeys = effectiveEngineDecidesParamKeys(
    evalResult,
    selectedTemplateSpec?.spec.engine_decides_param_keys,
  );
  const weightSource = useTemplateWeightSource({
    form,
    template: selectedTemplate ?? undefined,
    selection: selectedTemplateSpec,
    engineProfile,
    adoptedKey: opened ? templateSpecKey(opened.draft.deployment) : undefined,
  });
  const { runtimeSeries, autoResolutionText, inferenceSeries } =
    useDeployImageState({
      images: images.data?.items,
      imageSeries: imageSeries.data?.items,
      evalResult,
      selectedEngine,
      selectedVersionID,
      selectedNode,
    });
  useDeployNodeSync({
    form,
    mode: nodeSelectionMode,
    capability: selectedPartitionCapability,
    nodes: knownList,
    nnodes: Form.useWatch("nnodes", watchOptions),
    distributedNodes,
    enabled: opened?.mode !== "edit",
  });
  const onValuesChange = useDeployFormChanges({
    form,
    templates: templateIndex.data?.items ?? [],
    images: images.data?.items ?? [],
  });
  const handleValuesChange = (changed: Record<string, unknown>) => {
    touchedRef.current = true;
    evaluation.noteDraftChanged(changed);
    onValuesChange(changed);
  };
  useDeploymentIdentity(form, selectedModel, opened?.mode !== "edit");

  useDeployPrefill({
    form,
    queryModelID,
    queryTemplateID,
    querySpecName,
    querySpecSource,
    selectedTemplate,
    selectedTemplateSpec,
    nodeSelectionMode,
    defaultsEngine: evalResult?.defaults?.engine,
    fromDraft: Boolean(opened),
  });

  const { submit, submitting, submitError } = useDeploySubmit({
    form,
    nodes: knownList,
    engineProfile,
    selectedModel,
    evalResult,
    evalErr,
    evaluating,
    onCreated: () => navigate("/deployments"),
  });

  return (
    <div className="deploy-page">
      <PageHeader
        title={
          opened ? (
            <DeployEditTitle opened={opened} />
          ) : selectedTemplate ? (
            t("page.deployTemplate", {
              name: selectedTemplate.display_title || selectedTemplate.model_id,
            })
          ) : (
            t("page.title")
          )
        }
        description={opened ? t(`edit.description.${opened.mode}`) : t("page.description")}
        parents={[<Link key="list" to="/deployments">{t("common:navigation.deployments")}</Link>]}
        extra={
          <Space size={16}>
            {opened && <DeployEditBack opened={opened} />}
            <Segmented
              value={deployMode}
              onChange={(value) => setDeployMode(value as "quick" | "expert")}
              options={[
                { value: "quick", label: t("page.mode.quick") },
                { value: "expert", label: t("page.mode.expert") },
              ]}
            />
          </Space>
        }
      />
      <DeployEditContext.Provider value={editMode.context}>
      <EngineDecidesContext.Provider value={engineDecidesParamKeys}>
      <Form
        form={form}
        layout="vertical"
        initialValues={initialValues}
        onValuesChange={handleValuesChange}
      >
        {selectedTemplate && selectedTemplateSpec && (
          <DeployTemplateBar
            selection={selectedTemplateSpec}
            changed={evalResult?.template_deviated}
            evaluatedImage={evalResult?.image}
            weightModel={weightSource.appliedModel}
            onDetails={() =>
              navigate(`/inference-templates/${selectedTemplate.model_id}`)
            }
          />
        )}
        <DeployMovedFlagsNotice
          moved={evalResult?.moved}
          responseSeq={evaluation.responseSeq}
        />
        <DeployNoOperationsNotice
          model={selectedModel}
          templateOperations={selectedTemplateSpec?.spec.api?.operations}
        />
        <fieldset className="deploy-structured-fields">
          <section className="deploy-generic-entry">
            <DeployBasicStep
              templateIndex={templateIndex.data?.items ?? []}
              chips={fleet.data?.gpus ?? []}
              selectedModel={selectedModel}
            />
            <Form.Item noStyle shouldUpdate={(prev, next) => prev.replicas !== next.replicas}>
              {({ getFieldValue }) => getFieldValue("replicas") > 1 ? (
                <Form.Item name="routing_policy" label={<Space size={4}>{t("routing.policy")}<RoutingPolicyHelp policies={routingPolicies.data?.items ?? []} /></Space>} extra={t("routing.help")}>
                  <Select loading={routingPolicies.loading} options={routingOptions} />
                </Form.Item>
              ) : null}
            </Form.Item>
            <HealthCheckFields selectedModel={selectedModel} />
            <TokenizeField />
          </section>
          <div className="deploy-config-layout">
            <div className="deploy-config-inputs">
              {deployMode === "quick" ? (
                <>
                  <DeployQuickForm
                    templates={templates}
                    fleet={fleet.data}
                    knownNodes={knownNodes}
                    selectedNode={selectedNode}
                    partitionCapability={selectedPartitionCapability}
                    eligibleNodes={evalResult?.eligible_nodes}
                    selectedModel={selectedModel}
                    profile={engineProfile}
                    startTimeoutDefaultSec={startTimeoutDefaultSec}
                    engine={selectedEngine}
                    images={images.data?.items ?? []}
                    result={evalResult}
                    loading={evaluating}
                  />
                </>
              ) : (
                <DeployExpertTabs
                  engineProfile={engineProfile}
                  startTimeoutDefaultSec={startTimeoutDefaultSec}
                  templateShmBytes={selectedTemplateSpec?.spec.shm_bytes}
                  selectedEngine={selectedEngine}
                  selectedModel={selectedModel}
                  selectedNode={selectedNode}
                  selectedPartitionCapability={selectedPartitionCapability}
                  engines={{ data: engines.data?.items, loading: engines.loading }}
                  images={images.data?.items ?? []}
                  imageMode={imageMode || "auto"}
                  imageSeriesLoading={imageSeries.loading}
                  selectedSeriesID={selectedSeriesID}
                  selectedVersionID={selectedVersionID}
                  selectedImage={selectedImage}
                  inferenceSeries={inferenceSeries}
                  autoResolutionText={autoResolutionText}
                  runtimeSeries={runtimeSeries}
                  evalResult={evalResult}
                  evaluating={evaluating}
                />
              )}
            </div>
            <aside className="deploy-config-evaluation">
              <DeployStepSummary
                result={evalResult}
                error={evalErr}
                failure={evaluation.failure}
                loading={evaluating}
                onReevaluate={evaluation.reevaluate}
              />
            </aside>
          </div>
        </fieldset>
        <DeployRuntimeCommand
          result={evalResult}
          error={evalErr}
          loading={evaluating}
        />
        <DeploySubmitBar
          selectedTemplate={selectedTemplate}
          submit={editMode.editing ? editMode.submit.open : submit}
          submitting={editMode.editing ? editMode.submit.reviewing : submitting}
          disabled={evaluating || !!evalErr || !evalResult || !selectedModelID || !form.getFieldValue("target_vendor")}
          edit={editMode.editing ? { changes: editMode.changes.length } : undefined}
        />
        {!editMode.editing && <DeploySubmissionError error={submitError} />}
      </Form>
      </EngineDecidesContext.Provider>
      </DeployEditContext.Provider>
      {editMode.submit.review && (
        <DeployEditConfirmModal
          review={editMode.submit.review}
          saving={editMode.submit.saving}
          reloading={editMode.submit.reloading}
          onCancel={editMode.submit.close}
          onSave={(choice) => void editMode.submit.save(choice)}
          onReload={() => void editMode.submit.reloadDraft()}
        />
      )}
      <DeployWeightSourceModal
        open={weightSource.modalOpen}
        loading={weightSource.registering}
        selection={selectedTemplateSpec}
        choice={weightSource.choice}
        matches={weightSource.matches}
        useModel={weightSource.useModel}
        onUseModel={weightSource.setUseModel}
        onChoice={weightSource.setChoice}
        onCancel={() => weightSource.setModalOpen(false)}
        onConfirm={weightSource.confirm}
      />
    </div>
  );
}
