import { systemConditionsToRequirements } from "../../components/SystemLabelConditions";
import type {
  EngineParamSpec,
  EngineProfile,
  EvaluateResponse,
  InferenceSpec,
  InferenceTemplate,
  Node,
  PartitionCapability,
} from "../../api";
import {
  domesticPartitionCapability,
  readyPartitionCapability,
} from "../../components/partitionCapabilities.ts";
import { visibleParamSpecs } from "./deployParamApplicability";
import { buildPdPlacement, pdActive, unitsPlacementAuto } from "./deployPd";
import { paramsOfValue } from "../yieldPolicy/model";

export { pdActive, unitsPlacementAuto } from "./deployPd";


export {
  EngineDecidesContext,
  effectiveEngineDecidesParamKeys,
  quickParamSpecs,
  useEngineDecides,
  visibleParamSpecs,
} from "./deployParamApplicability";

/** The host of a management address ("host:port", "[v6]:port" or a bare host). */
export function hostOnly(address?: string): string {
  const value = (address || "").trim();
  if (value.startsWith("[")) {
    const end = value.indexOf("]");
    return end > 0 ? value.slice(1, end) : value;
  }
  const colonCount = (value.match(/:/g) || []).length;
  return colonCount === 1 ? value.slice(0, value.lastIndexOf(":")) : value;
}

export interface TemplateSpecSelection {
  spec: InferenceSpec;
}

export function templateSpecSelections(
  template?: InferenceTemplate,
): TemplateSpecSelection[] {
  return (template?.specs || []).map((spec) => ({ spec }));
}

export function findTemplateSpec(
  template: InferenceTemplate | undefined,
  specName?: string,
  scenario?: string,
  specSource?: string,
): TemplateSpecSelection | undefined {
  const selections = templateSpecSelections(template);
  return (
    selections.find(
      (item) =>
        specName &&
        item.spec.name === specName &&
        (!specSource || item.spec.source === specSource),
    ) ||
    selections.find((item) => !scenario || item.spec.scenario === scenario) ||
    selections[0]
  );
}

// The memory KV tier's card position. The tier's size value stays in its field
// while the card is off - the payload simply stops stating it - so recovering
// the draft is one click. This is draft state, not a rule: which flags the
// tier emits is the server's judgement.
export const memoryTierKey = "kv_offloading_size";
export const memoryTierEnabledField = "kv_memory_tier_enabled";
const diskTierKey = "kv_disk_offload";

// The KV tiers as the dictionary publishes them: the memory tier's key, the
// disk tier's, and the companions the disk tier owns through depends_on. One
// reader for the quick form's cards, the expert step's switches and the
// payload projection - the relation is the dictionary's, never re-derived.
export function kvTierSpecs(specs: EngineParamSpec[] | undefined): {
  memory?: EngineParamSpec;
  disk?: EngineParamSpec;
  companions: EngineParamSpec[];
} {
  const published = specs ?? [];
  return {
    memory: published.find((spec) => spec.key === memoryTierKey),
    disk: published.find((spec) => spec.key === diskTierKey),
    companions: published.filter(
      (spec) => spec.depends_on === diskTierKey && spec.key !== diskTierKey,
    ),
  };
}

// projectDraft is the form's one projection onto the parameter dictionary: the
// values the draft states and the keys it tombstones. A control the deployer
// cleared ("") states nothing and removes the template's value too, which is
// what clearing a template-set parser means. An unstated control (undefined)
// states nothing: the server then applies the dictionary default, or leaves
// the key to the engine where the launch may refuse it. The server is the
// judge; the form holds no emission knowledge.
export function projectDraft(
  v: Record<string, unknown>,
  profile?: EngineProfile,
  modalities: string[] = [],
): { quick: Record<string, unknown>; unset: string[] } {
  const engine = String(v.engine || profile?.name || "");
  const published = visibleParamSpecs(profile, engine, modalities);
  const tiers = kvTierSpecs(published);
  const companions = new Set(tiers.companions.map((spec) => spec.key));
  const memoryOn = v[memoryTierEnabledField] !== false;
  // The disk tier depends on the memory tier - the same dependency the server
  // validates - so a memory tier that is off takes the disk keys with it.
  const diskOn = memoryOn && Boolean(tiers.disk && v[diskTierKey]);
  const quick: Record<string, unknown> = {};
  const unset = new Set<string>();
  for (const spec of published) {
    const { key } = spec;
    if (key === memoryTierKey && !memoryOn) continue;
    if ((key === diskTierKey || companions.has(key)) && !diskOn) continue;
    const value = v[key];
    if (value === "") {
      unset.add(key);
      continue;
    }
    if (value == null) continue;
    quick[key] = value;
  }
  // The tier card positions tombstone the keys they switched off, so a
  // template-supplied tier value cannot survive its card being off.
  if (tiers.memory && !memoryOn) {
    unset.add(tiers.memory.key);
    if (tiers.disk) unset.add(tiers.disk.key);
    for (const spec of tiers.companions) unset.add(spec.key);
  } else if (tiers.disk && v[diskTierKey] === false) {
    unset.add(tiers.disk.key);
    for (const spec of tiers.companions) unset.add(spec.key);
  }
  return { quick, unset: [...unset] };
}

// buildQuickParams projects the form onto the parameter dictionary: every
// visible control the deployer stated goes on the wire as-is. Defaults belong
// to the server (evaluate answers them in form_values), tier gating belongs to
// the card positions, and nothing here interprets a value.
export function buildQuickParams(
  v: Record<string, unknown>,
  profile?: EngineProfile,
  modalities: string[] = [],
): Record<string, unknown> {
  return projectDraft(v, profile, modalities).quick;
}

// buildPayload serializes the draft into the evaluate/create body. Projection,
// not derivation: every value originates in the form or an evaluate response.
export function buildPayload(
  v: Record<string, unknown>,
  profile?: EngineProfile,
  modalities: string[] = [],
) {
  const { quick, unset } = projectDraft(v, profile, modalities);
  const placement: Record<string, unknown> = {};
  // A multi-node or PD draft the platform places states no node and no card:
  // only how big each rank is (tp/pp, the PD role's units and TP).
  const unitsAuto = unitsPlacementAuto(v);
  // A PD draft states its instances per role (ADR 0028): it replaces the
  // ordinary tp/pp/gpus placement rather than adding to it.
  const pd = buildPdPlacement(v);
  if (pd) {
    placement.pd = pd;
  } else {
    if (v.tp != null && v.tp !== "") placement.tp = v.tp;
    if (v.pp != null && v.pp !== "") placement.pp = v.pp;
    if (Array.isArray(v.gpu_indexes) && v.gpu_indexes.length && !unitsAuto)
      placement.gpus = v.gpu_indexes;
  }
  const payload: Record<string, unknown> = {};
  // The label control picks one value per key and the hardware control
  // "key=value" pairs; the server's node requirements take a list of
  // alternatives per key.
  const requirements: Record<string, string[]> = systemConditionsToRequirements(v.system_conditions);
  const labels = v.node_labels as Record<string, string> | undefined;
  if (labels && typeof labels === "object")
    for (const [key, value] of Object.entries(labels)) requirements[key] = [value];
  // An automatic single-node draft may list the nodes its replicas are
  // placed among: one more condition. A manual draft names its one node.
  const poolNodes = Array.isArray(v.pool_nodes) ? (v.pool_nodes as unknown[]).filter((uuid): uuid is string => typeof uuid === "string" && uuid !== "") : [];
  if (poolNodes.length && !pd && Number(v.nnodes || 1) <= 1 && v.node_selection_mode !== "manual")
    requirements["gpuworks.ai/node"] = poolNodes;
  if (Object.keys(requirements).length) payload.node_requirements = requirements;
  // The strategy applies to the placements the platform makes: an automatic
  // single-node draft's, and an automatic multi-node or PD draft's.
  if (
    ((!pd && Number(v.nnodes || 1) <= 1) || unitsAuto) &&
    (v.placement_strategy === "spread" || v.placement_strategy === "binpack")
  )
    payload.placement_strategy = v.placement_strategy;
  // What the request says about the model's files: which public weight source
  // to run and the storage a node saves the model into when it has to fetch
  // it. Both travel with the evaluation too, so the server plans disk and
  // cache against what the deployment will actually do.
  const source = v.model_source as Record<string, unknown> | undefined;
  const storageId = Number(source?.storage_id);
  const provider = typeof source?.provider === "string" ? source.provider.trim() : "";
  const modelSource: Record<string, unknown> = {};
  if (provider) modelSource.provider = provider;
  if (source?.storage_id != null && source.storage_id !== "" && Number.isFinite(storageId) && storageId > 0)
    modelSource.storage_id = storageId;
  if (Object.keys(modelSource).length) payload.model_source = modelSource;
  if (v.model_id != null) payload.model_id = v.model_id;
  if (v.served_name != null) payload.served_name = v.served_name;
  if (v.template_id != null) payload.template_id = v.template_id;
  // The routing policy of a deployment with several replicas; an edit that
  // left it out would reset it to the default.
  if (v.routing_policy) payload.routing_policy = v.routing_policy;
  if (v.template_spec_source)
    payload.template_spec_source = v.template_spec_source;
  if (v.template_spec_name) payload.template_spec_name = v.template_spec_name;
  if (v.scenario) payload.scenario = v.scenario;
  if (v.target_vendor) payload.target_vendor = v.target_vendor;
  // A PD draft names its nodes per unit inside placement.pd instead; the
  // server fills the top-level node_uuid from the first decode unit.
  if (v.node_uuid && !pd && !unitsAuto) payload.node_uuid = v.node_uuid;
  payload.engine = v.engine;
  if (v.image) payload.image = v.image;
  if (v.extra_args) payload.extra_args = v.extra_args;
  if (v.sleepable) payload.sleepable = true;
  // The policy chosen is sent as it is: whether the draft may hold it is the
  // server's to judge (it refuses the create, and the form shows the
  // evaluation's reason). Never is what an absent policy means.
  const yieldPolicy = paramsOfValue(v.yield_policy);
  if (yieldPolicy && yieldPolicy.policy !== "never") payload.yield_policy = yieldPolicy;
  if (
    v.vram_limit_gib != null &&
    v.vram_limit_gib !== "" &&
    Number(v.vram_limit_gib) > 0
  ) {
    payload.vram_limit_gib = v.vram_limit_gib;
    if (v.partition_backend) payload.partition_backend = v.partition_backend;
  }
  // Unstated, the server decides (on for a chat or embedding model), which
  // is what the form shows.
  if (typeof v.health_check === "boolean") payload.health_check = v.health_check;
  const healthThreshold = Number(v.health_check_threshold_sec);
  if (Number.isFinite(healthThreshold) && healthThreshold > 0)
    payload.health_check_threshold_sec = Math.round(healthThreshold);
  // Unstated, the server opens tokenize, which is what the form shows.
  if (typeof v.tokenize_enabled === "boolean")
    payload.tokenize_enabled = v.tokenize_enabled;
  const startTimeout = Number(v.start_timeout_sec);
  if (Number.isFinite(startTimeout) && startTimeout > 0)
    payload.start_timeout_sec = Math.round(startTimeout);
  if (Object.keys(quick).length) payload.quick_params = quick;
  if (unset.length) payload.quick_params_unset = unset;
  if (Object.keys(placement).length) payload.placement = placement;
  if (unitsAuto) payload.placement_mode = "auto";
  if (v.nnodes != null) payload.nnodes = v.nnodes;
  if (v.replicas != null) payload.replicas = v.replicas;
  // A PD deployment states no multi-node group of its own (nnodes/node_uuids
  // above describe an ordinary distributed launch, which a PD draft never
  // is - the server rejects nnodes>1 or node_uuids alongside placement.pd).
  if (!pd && Number(v.nnodes || 1) > 1) {
    // An automatic group names no node (the platform picks every rank, and the
    // leader's address is read from the node it picks).
    if (!unitsAuto) {
      if (Array.isArray(v.distributed_nodes) && v.distributed_nodes.length > 0)
        payload.node_uuids = v.distributed_nodes;
      if (v.master_addr) payload.master_addr = v.master_addr;
    }
    if (v.master_port != null) payload.master_port = v.master_port;
    if (v.dist_group) payload.dist_group = v.dist_group;
    payload.dist_role = "leader";
  }
  // The deployment's own container fields (ADR 0031): what the form states is
  // the whole layer, sent only when stated.
  const containerOverrides: Record<string, unknown> = {};
  const modelMount = String(v.container_model_mount_path || "").trim();
  if (modelMount) containerOverrides.model_mount_path = modelMount;
  const envRows = Array.isArray(v.container_env_rows)
    ? (v.container_env_rows as Array<{ key?: string; value?: string }>)
    : [];
  const env = Object.fromEntries(
    envRows
      .filter((row) => String(row?.key || "").trim())
      .map((row) => [String(row.key).trim(), row.value || ""]),
  );
  if (Object.keys(env).length) containerOverrides.env = env;
  const envUnset = Array.isArray(v.container_env_unset)
    ? [
        ...new Set(
          v.container_env_unset
            .map((key: unknown) => String(key ?? "").trim())
            .filter(Boolean),
        ),
      ]
    : [];
  if (envUnset.length) containerOverrides.env_unset = envUnset;
  const mountRows = Array.isArray(v.container_mount_rows)
    ? (v.container_mount_rows as Array<Record<string, unknown>>)
    : [];
  // A row mounts a host path, a named volume or a registered model; one with
  // no source or no container path is still being filled in and not sent.
  const mounts = mountRows
    .filter(
      (row) =>
        (String(row?.host_path || row?.volume || "").trim() ||
          Number(row?.model_id) > 0) &&
        String(row?.container_path || "").trim(),
    )
    .map((row) => {
      const out: Record<string, unknown> = {
        ...row,
        read_only: Boolean(row.read_only),
      };
      if (Number(row.model_id) > 0) {
        out.model_id = Number(row.model_id);
        out.read_only = true;
        delete out.host_path;
        delete out.volume;
      } else {
        delete out.model_id;
      }
      // Switching a row's source leaves the fields it cleared behind as
      // undefined; they state nothing.
      for (const key of ["host_path", "volume"])
        if (out[key] === undefined) delete out[key];
      return out;
    });
  if (mounts.length) containerOverrides.extra_mounts = mounts;
  const dockerArgs = Array.isArray(v.container_docker_args)
    ? v.container_docker_args.filter(
        (arg): arg is string => typeof arg === "string" && arg.trim() !== "",
      )
    : [];
  if (dockerArgs.length) containerOverrides.docker_args = dockerArgs;
  const shmGiB = Number(v.container_shm_gib);
  if (Number.isFinite(shmGiB) && shmGiB > 0)
    containerOverrides.shm_bytes = Math.round(shmGiB * 2 ** 30);
  const entrypoint = String(v.container_entrypoint || "").trim();
  if (entrypoint) containerOverrides.entrypoint_display = entrypoint;
  const cmd = String(v.container_cmd || "").trim();
  if (cmd) containerOverrides.cmd_display = cmd;
  const commandMode = String(v.command_mode || "").trim();
  if (
    commandMode === "manual" &&
    containerOverrides.cmd_display &&
    v.command_includes_preparation === true
  ) {
    containerOverrides.command_includes_preparation = true;
  }
  if (commandMode && commandMode !== "engine")
    containerOverrides.command_mode = commandMode;
  if (Object.keys(containerOverrides).length)
    payload.container_overrides = containerOverrides;
  return { payload, quick, quickParamsUnset: unset, placement };
}

export type ImageSelectionMode = "auto" | "catalog" | "custom";

// submittedImage is the what-you-see-is-what-you-submit rule for the image
// field: everything the form displayed is confirmed by the user. An explicit
// pick (catalog or custom mode) wins; in auto mode the displayed ref is the
// evaluated one, and it travels with the request as an explicit choice, so
// the server deploys what the user saw instead of re-resolving a default.
export function submittedImage(
  formImage: unknown,
  evaluatedImage?: string | null,
): string | undefined {
  const explicit = String(formImage ?? "").trim();
  if (explicit) return explicit;
  const evaluated = String(evaluatedImage ?? "").trim();
  return evaluated || undefined;
}

// applyEvaluateFormValues puts the server's display instruction on the form:
// effective values (defaults and template projection merged) and the argument
// text after the moved flags left it. Keys a previous response applied and
// this one does not carry are cleared, so switching templates cannot leave the
// earlier spec's values behind. Tier card positions and the values of
// disabled tiers are draft state and are never overwritten.
export function applyEvaluateFormValues(
  form: {
    setFieldsValue: (values: Record<string, unknown>) => void;
    getFieldsValue: (all?: boolean) => Record<string, unknown>;
  },
  response: EvaluateResponse | null | undefined,
  previousKeys: string[],
  // Keys whose control the deployer is typing in right now. The answer was
  // built from the draft as it stood when the request left - a field just
  // cleared and about to be retyped reads as "not stated", so the answer
  // carries its default, and writing that into the focused control would
  // splice it into what is typed next (4, then "0" typed, gives 40). Those
  // keys are skipped and returned; the caller evaluates again once the
  // control loses focus.
  editing: (key: string) => boolean = () => false,
): string[] {
  if (!response?.form_values) return [];
  const draft = form.getFieldsValue(true);
  const updates: Record<string, unknown> = {};
  const deferred: string[] = [];
  for (const key of previousKeys) {
    // A disabled memory tier keeps its preserved size out of the response by
    // design; its absence is not staleness.
    if (key === memoryTierKey && draft[memoryTierEnabledField] === false)
      continue;
    if (
      !Object.prototype.hasOwnProperty.call(response.form_values, key) &&
      draft[key] !== undefined
    ) {
      if (editing(key)) deferred.push(key);
      else updates[key] = undefined;
    }
  }
  for (const [key, value] of Object.entries(response.form_values)) {
    if (key === memoryTierKey && draft[memoryTierEnabledField] === false)
      continue;
    // Echoing a value the draft already holds rewrites nothing: writing it
    // anyway would fire the form watch and send a duplicate evaluation of the
    // same draft.
    if (JSON.stringify(draft[key] ?? null) === JSON.stringify(value ?? null))
      continue;
    if (editing(key)) {
      deferred.push(key);
      continue;
    }
    updates[key] = value;
  }
  if (Object.keys(updates).length) form.setFieldsValue(updates);
  return deferred;
}

// deploymentQuery reads the URL scheme a deploy link carries: which template,
// spec and model the link names, and whether it opens an existing deployment
// to edit (edit=<id>, with focus=<field>[,<field>] naming the controls to
// bring into view) or to clone and modify (from=<id>).
export function deploymentQuery(search: string) {
  const query = new URLSearchParams(search);
  const positiveID = (key: string) => {
    const value = Number(query.get(key));
    return Number.isInteger(value) && value > 0 ? value : undefined;
  };
  const queryEditID = positiveID("edit");
  return {
    queryTemplateID: positiveID("template"),
    querySpecName: query.get("spec") || undefined,
    querySpecSource: query.get("spec_source") || undefined,
    queryModelID: positiveID("model"),
    queryEditID,
    // An edit link wins over a clone link.
    queryFromID: queryEditID ? undefined : positiveID("from"),
    queryFocus: (query.get("focus") || "")
      .split(",")
      .map((field) => field.trim())
      .filter(Boolean),
  };
}

export { draftToFormValues } from "./deployDraftValues";

export function nodePartitionCapability(
  node?: Node | null,
): PartitionCapability | undefined {
  return (
    readyPartitionCapability(node) ??
    domesticPartitionCapability(node) ??
    node?.partition_capabilities?.[0]
  );
}

// draftPartitionCapability is the dGPU backend a draft's quota would use: the
// picked node's for a draft that names its node, what the eligible pool offers
// (the evaluation's pool_partition_capability) for an automatic one. An
// automatic draft has no node of its own until a placement fits, and one that
// only fits as a dGPU slice never fits before the quota is set; gating the
// quota on the server's selection kept it off exactly when every card was
// partly taken.
export function draftPartitionCapability(
  mode: string,
  node: Node | null | undefined,
  result: EvaluateResponse | null | undefined,
): PartitionCapability | undefined {
  if (mode === "auto") return result?.pool_partition_capability;
  return nodePartitionCapability(node);
}

// A quota is request intent. Automatic placement has no authoritative node, so
// a stale form selection cannot change it. Manual placement may fill a backend
// only from a capability the selected node explicitly reports as ready.
export function deploymentPartitionDraft(
  values: Record<string, unknown>,
  node: Node | null | undefined,
  automatic: boolean,
): Record<string, unknown> {
  const draft = { ...values };
  if (
    automatic ||
    draft.vram_limit_gib == null ||
    draft.vram_limit_gib === "" ||
    Number(draft.vram_limit_gib) <= 0
  )
    return draft;
  const capability = nodePartitionCapability(node);
  if (capability?.status === "ready")
    draft.partition_backend = capability.backend;
  return draft;
}

export function nodeSupportsPartition(node?: Node | null): boolean {
  return Boolean(readyPartitionCapability(node));
}

// deployDraftPayload serializes the form draft for evaluate: the automatic
// mode states no node and no GPU pinning (the server's selected_node is the
// answer) but keeps the TP/PP the deployer typed, which decide how many GPUs
// the scheduler looks for; the manual mode states exactly what the form holds.
export function deployDraftPayload(
  values: Record<string, unknown>,
  nodes: Node[],
  profile?: EngineProfile,
  modalities: string[] = [],
): Record<string, unknown> | null {
  if (values.model_id == null) return null;
  const pd = pdActive(values);
  const automatic =
    !pd &&
    Number(values.nnodes || 1) <= 1 &&
    values.node_selection_mode !== "manual";
  const node = nodes.find((item) => item.uuid === values.node_uuid);
  const draft = deploymentPartitionDraft(values, node, automatic);
  const built = buildPayload(draft, profile, modalities)
    .payload as Record<string, unknown>;
  if (pd) {
    // PD names its own nodes per unit (or, placed automatically, leaves them
    // to the platform: buildPayload states placement_mode "auto" then).
    delete built.node_uuid;
  } else if (unitsPlacementAuto(values)) {
    // buildPayload stated placement_mode "auto" and no node.
  } else if (automatic) {
    delete built.node_uuid;
    const placement = {
      ...((built.placement as Record<string, unknown> | undefined) || {}),
    };
    delete placement.gpus;
    delete placement.gpu_indexes;
    if (Object.keys(placement).length) built.placement = placement;
    else delete built.placement;
    built.placement_mode = "auto";
  } else {
    built.placement_mode = "manual";
  }
  return built;
}
