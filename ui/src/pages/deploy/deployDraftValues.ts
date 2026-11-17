import type { EngineProfile } from "../../api";
import { paramsOfValue } from "../yieldPolicy/model";
import { visibleParamSpecs } from "./deployParamApplicability";

const NODE_KEY = "gpuworks.ai/node";
const SYSTEM_PREFIX = "gpuworks.ai/";

type Obj = Record<string, unknown>;

const isObject = (value: unknown): value is Obj =>
  typeof value === "object" && value !== null && !Array.isArray(value);

// requirementsToControls is the inverse of the payload's node requirements:
// system labels to the hardware conditions ("key=value" picks), a node label
// with one value to the label control, the node key to the pool of an
// automatic draft. A label with several alternatives has no place in the
// one-value label control, so it rides in the conditions list, which keeps
// every alternative.
function requirementsToControls(
  requirements: unknown,
  poolable: boolean,
): { system_conditions?: string[]; node_labels?: Record<string, string>; pool_nodes?: string[] } {
  if (!isObject(requirements)) return {};
  const conditions: string[] = [];
  const labels: Record<string, string> = {};
  let pool: string[] | undefined;
  for (const [key, raw] of Object.entries(requirements)) {
    const values = Array.isArray(raw) ? raw.map(String) : [];
    if (!values.length) continue;
    if (key === NODE_KEY && poolable) pool = values;
    else if (key.startsWith(SYSTEM_PREFIX) || values.length > 1)
      conditions.push(...values.map((value) => `${key}=${value}`));
    else labels[key] = values[0];
  }
  return {
    ...(conditions.length ? { system_conditions: conditions } : {}),
    ...(Object.keys(labels).length ? { node_labels: labels } : {}),
    ...(pool ? { pool_nodes: pool } : {}),
  };
}

// overridesToControls is the inverse of the container fields buildPayload
// folds into container_overrides.
function overridesToControls(overrides: unknown): Obj {
  if (!isObject(overrides)) return {};
  const out: Obj = {};
  if (typeof overrides.model_mount_path === "string")
    out.container_model_mount_path = overrides.model_mount_path;
  if (isObject(overrides.env))
    out.container_env_rows = Object.entries(overrides.env).map(([key, value]) => ({
      key,
      value: String(value ?? ""),
    }));
  if (Array.isArray(overrides.env_unset))
    out.container_env_unset = overrides.env_unset.map(String);
  if (Array.isArray(overrides.extra_mounts))
    out.container_mount_rows = overrides.extra_mounts.map((row) => ({ ...(row as Obj) }));
  if (Array.isArray(overrides.docker_args))
    out.container_docker_args = overrides.docker_args.map(String);
  if (Number(overrides.shm_bytes) > 0)
    out.container_shm_gib = Number(overrides.shm_bytes) / 2 ** 30;
  if (typeof overrides.entrypoint_display === "string")
    out.container_entrypoint = overrides.entrypoint_display;
  if (typeof overrides.cmd_display === "string")
    out.container_cmd = overrides.cmd_display;
  if (overrides.command_includes_preparation === true)
    out.command_includes_preparation = true;
  if (typeof overrides.command_mode === "string" && overrides.command_mode)
    out.command_mode = overrides.command_mode;
  return out;
}

// pdToControls is the inverse of buildPdPlacement.
function pdToControls(pd: Obj): Obj {
  const roles: Obj = {};
  if (isObject(pd.roles))
    for (const [role, raw] of Object.entries(pd.roles)) {
      if (!isObject(raw)) continue;
      roles[role] = {
        ...(raw.tp != null ? { tp: raw.tp } : {}),
        ...(raw.pp != null ? { pp: raw.pp } : {}),
        units: Array.isArray(raw.units) ? raw.units : [],
      };
    }
  return {
    node_selection_mode: "pd",
    pd_mode: pd.mode,
    pd_fallback: pd.fallback,
    pd_kv_load_failure: pd.kv_load_failure,
    pd_roles: roles,
  };
}

// draftYieldPolicy reads the create form's yield policy out of a draft: the
// policy with its parameters, none for never (what an absent policy means).
function draftYieldPolicy(policy: unknown): Obj | undefined {
  const params = paramsOfValue(policy);
  return params && params.policy !== "never" ? (params as unknown as Obj) : undefined;
}

// draftToFormValues is buildPayload's inverse: the deploy form's values for a
// stored deployment, read back as a create request (GET /deployments/{id}/draft).
// The deployment's image is put in explicit mode so it is submitted as it is
// stored. A dictionary key the engine profile publishes and the deployment
// does not state is set to "" - the form's cleared-means-unset position - so
// the server adds no default the stored deployment never had.
export function draftToFormValues(
  request: Obj,
  profile?: EngineProfile,
  modalities: string[] = [],
): Obj {
  const placement = isObject(request.placement) ? request.placement : {};
  const pd = isObject(placement.pd) ? placement.pd : undefined;
  const nnodes = Number(request.nnodes || 1);
  const multi = !pd && nnodes > 1;
  const manual = request.placement_mode === "manual";
  const values: Obj = {
    model_id: request.model_id || undefined,
    name: request.name,
    served_name: request.served_name,
    template_id: request.template_id || undefined,
    template_spec_source: request.template_spec_source || undefined,
    template_spec_name: request.template_spec_name || undefined,
    scenario: request.scenario || undefined,
    engine: request.engine || undefined,
    target_vendor: request.target_vendor || undefined,
    extra_args: request.extra_args || undefined,
    sleepable: request.sleepable === true,
    yield_policy: draftYieldPolicy(request.yield_policy),
    replicas: Number(request.replicas) > 0 ? request.replicas : 1,
    routing_policy: request.routing_policy || undefined,
    nnodes: Math.max(nnodes, 1),
    command_mode: "engine",
    ...(request.image
      ? { image: request.image, image_mode: "custom" }
      : { image_mode: "auto" }),
  };
  if (isObject(request.model_source)) values.model_source = { ...request.model_source };
  if (Number(request.vram_limit_gib) > 0) {
    values.vram_limit_gib = request.vram_limit_gib;
    values.partition_backend = request.partition_backend || undefined;
  }
  if (typeof request.health_check === "boolean") values.health_check = request.health_check;
  if (Number(request.health_check_threshold_sec) > 0)
    values.health_check_threshold_sec = request.health_check_threshold_sec;
  if (typeof request.tokenize_enabled === "boolean")
    values.tokenize_enabled = request.tokenize_enabled;
  if (typeof request.priority_scheduling === "boolean")
    values.priority_scheduling = request.priority_scheduling;
  if (Number(request.start_timeout_sec) > 0)
    values.start_timeout_sec = request.start_timeout_sec;
  if (request.placement_strategy) values.placement_strategy = request.placement_strategy;

  // Topology: PD, a distributed group, or one node placed automatically or by name.
  // A multi-node or PD draft is the operator's placement unless it says the
  // platform's: the stored nodes are what a draft without placement_mode names.
  if (pd || multi) values.units_placement = request.placement_mode === "auto" ? "auto" : "manual";
  if (pd) {
    Object.assign(values, pdToControls(pd));
  } else if (multi) {
    values.node_selection_mode = "multi";
    const nodes = Array.isArray(request.node_uuids) ? request.node_uuids.map(String) : [];
    values.distributed_nodes = nodes;
    values.node_uuid = nodes[0] || request.node_uuid || undefined;
    values.master_addr = request.master_addr || undefined;
    if (request.master_port != null) values.master_port = request.master_port;
    values.dist_group = request.dist_group || undefined;
  } else {
    values.node_selection_mode = manual ? "manual" : "auto";
    // An automatic draft's node is the server's preview of it, not the draft's.
    if (manual) values.node_uuid = request.node_uuid || undefined;
  }
  if (!pd) {
    if (placement.tp != null) values.tp = placement.tp;
    if (placement.pp != null) values.pp = placement.pp;
    // An automatic draft names no GPUs: the scheduler picks them again.
    if (Array.isArray(placement.gpus) && placement.gpus.length && (multi || manual))
      values.gpu_indexes = placement.gpus;
  }
  Object.assign(
    values,
    requirementsToControls(request.node_requirements, !pd && !multi && !manual),
  );
  Object.assign(values, overridesToControls(request.container_overrides));

  // The launch configuration: one top-level field per dictionary key.
  const quick = isObject(request.quick_params) ? request.quick_params : {};
  for (const [key, value] of Object.entries(quick)) values[key] = value;
  if (profile) {
    const engine = String(request.engine || profile.name || "");
    for (const spec of visibleParamSpecs(profile, engine, modalities))
      if (!(spec.key in quick)) values[spec.key] = "";
  }
  return values;
}

// cloneIdentity names a clone's copy: the draft's names with a -clone suffix,
// which the deployer changes before creating.
export function cloneIdentity(values: Obj): Obj {
  const suffixed = (value: unknown) =>
    typeof value === "string" && value ? `${value}-clone` : value;
  return {
    ...values,
    name: suffixed(values.name),
    served_name: suffixed(values.served_name),
  };
}

// templateSpecKey names the template spec a draft was made from the way the
// weight source hook keys one (template id, spec source, spec name), so a form
// opened from the draft can tell its own spec from one chosen afterwards.
export function templateSpecKey(request: Obj): string | undefined {
  if (!request.template_id || !request.template_spec_name) return undefined;
  return `${request.template_id}:${request.template_spec_source ?? ""}:${request.template_spec_name}`;
}
